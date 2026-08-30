// Package budget works out how much memory is actually available.
//
// This is the one place where a VM and a container genuinely differ, and getting
// it wrong is silent: a tool that only understands cgroups reports no limit on a
// bare VM and tunes nothing, while a tool that only reads /proc/meminfo inside a
// container sizes against the host's memory and gets OOM-killed. Both are looked
// at here, cgroup first, because inside a container /proc/meminfo shows the host.
package budget

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Source records where a limit came from, so `fpm-tune plan` can show it. An
// operator who disagrees with the budget needs to know which number to change.
type Source string

const (
	SourceCgroupV2 Source = "cgroup v2"

	// SourceCgroupProcess is the limit found on the managed process's OWN
	// cgroup, which on a VM is the only place it lives.
	SourceCgroupProcess Source = "php-fpm's cgroup"
	SourceCgroupV1      Source = "cgroup v1"
	SourceMemInfo       Source = "/proc/meminfo"
	SourceSysctl        Source = "sysctl"
	SourceOverride      Source = "override"
)

// Limits is what the host makes available.
type Limits struct {
	// MemoryBytes is the memory ceiling: the container's limit when there is
	// one, otherwise the machine's physical memory.
	MemoryBytes int64

	// CPUs is the effective core count, including a fractional cgroup quota
	// rounded up — half a core still runs one worker at a time.
	CPUs int

	// Containerized reports whether MemoryBytes came from a cgroup limit rather
	// than from the machine's total.
	Containerized bool

	// Source names where MemoryBytes came from.
	Source Source

	// AvailableBytes is /proc/meminfo's MemAvailable — memory free for new
	// allocations after everything else's use — when it was read. Zero on a cgroup
	// limit (the neighbours are elsewhere), on darwin, and on kernels too old to
	// report it. It is the input to WithNeighbors.
	AvailableBytes int64

	// NeighborBytes is the memory left to OTHER services (mysql, redis, the OS) by
	// WithNeighbors — the difference between the machine total and php-fpm's budget.
	// Zero unless the good-neighbour cap actually reduced the budget, so a non-zero
	// value both drives the report and marks that the cap was applied.
	NeighborBytes int64

	// LookupErr is set when the managed process's OWN limit could not be read,
	// and MemoryBytes therefore fell back to the machine.
	//
	// The two look identical in the number and are opposite situations: a host
	// with no limit anywhere, and a host whose limit could not be reached. The
	// second is the one that sizes a 3GiB service against a 24GiB machine, so it
	// has to be visible — Describe says so, and the caller can decide whether to
	// act on a budget it could not confirm.
	LookupErr error
}

// sysPaths are the files consulted, in a struct so tests can point them at
// fixtures rather than requiring a container to run in.
type sysPaths struct {
	cgroupV2Memory string
	cgroupV2CPU    string
	cgroupV1Memory string
	cgroupV1Quota  string
	cgroupV1Period string
	memInfo        string
}

// cgroupRoot and procRoot are separated out so the per-process lookup can be
// pointed at fixtures.
var (
	cgroupRoot = "/sys/fs/cgroup"
	procRoot   = "/proc"
)

var defaultPaths = sysPaths{
	cgroupV2Memory: "/sys/fs/cgroup/memory.max",
	cgroupV2CPU:    "/sys/fs/cgroup/cpu.max",
	cgroupV1Memory: "/sys/fs/cgroup/memory/memory.limit_in_bytes",
	cgroupV1Quota:  "/sys/fs/cgroup/cpu/cpu.cfs_quota_us",
	cgroupV1Period: "/sys/fs/cgroup/cpu/cpu.cfs_period_us",
	memInfo:        "/proc/meminfo",
}

// implausibleLimit is the ceiling above which a "limit" is really the kernel's
// way of saying unlimited.
//
// cgroup v1 writes a very large number rather than a sentinel, and some v2
// runtimes write the int64 sentinel instead of "max". Taken at face value, that
// becomes a limit of about 8.8 exabytes and every derived setting scales off it.
const implausibleLimit = int64(1) << 50

// DetectFor reads the limits that apply to a PARTICULAR process.
//
// A pid of zero asks about the host itself, which is the right question only
// when there is no particular process in mind. There used to be a Detect() that
// asked nothing else, and it was removed rather than kept beside this one: it is
// the reading that caused the fault below, and a tool whose entire job is to
// respect one process's ceiling should not have a convenient way to ask about
// somebody else's.
//
// The bare reading looks at /sys/fs/cgroup/memory.max — an absolute path, which
// is the ROOT of the hierarchy. Inside a container that is exactly right: the
// container's own cgroup is what gets mounted there, so the root IS the
// container's limit. On a VM it is the machine, and the machine is never
// limited.
//
// So a php-fpm under a systemd slice with MemoryMax=3G was sized against the
// host's 20GiB. Measured on a five-pool Ubuntu VM: fpm-tune reported "14.7GiB
// available to workers" while php-fpm's own cgroup would OOM-kill its workers at
// 3GiB — driving straight into the ceiling this tool exists to avoid, and
// looking right the whole way, because the number it printed was the machine's
// real memory.
//
// Docker could not surface this. In a container both readings agree, which is
// why the container tests passed throughout.
//
// The limit that applies is the smallest along the process's own path, since a
// cap on any ancestor binds everything below it.
func DetectFor(pid int) Limits {
	limits := detectWith(defaultPaths)
	if pid <= 0 {
		return limits
	}

	bytes, ok, err := cgroupLimitOf(pid)
	if err != nil {
		// Reported, not swallowed. This reading OUTRANKS the machine total when
		// it exists, so failing to take it and silently using the machine total
		// is the widest possible answer to a question about a limit.
		limits.LookupErr = err

		return limits
	}
	if !ok || bytes <= 0 || bytes >= limits.MemoryBytes {
		return limits
	}

	limits.MemoryBytes = bytes
	limits.Containerized = true
	limits.Source = SourceCgroupProcess

	return limits
}

// cgroupLimitOf walks the cgroups a process belongs to and returns the tightest
// memory limit found.
func cgroupLimitOf(pid int) (int64, bool, error) {
	if _, err := os.Stat(procRoot); err != nil {
		// No /proc at all: this is macOS, or a Linux host with it unmounted.
		// Cgroups are not a thing here, so the machine's memory is not a
		// fallback from a failed reading — it is the answer. Reporting an error
		// made the daemon refuse to write on every non-Linux host, which is
		// where it is developed.
		return 0, false, nil
	}

	data, err := os.ReadFile(fmt.Sprintf("%s/%d/cgroup", procRoot, pid))
	if err != nil {
		// COULD NOT LOOK is not the same as FOUND NO LIMIT, and returning the
		// same thing for both is how a 3GiB service got sized against a 24GiB
		// machine with nothing printed. Three ordinary ways in: /proc mounted
		// hidepid=2 while php-fpm runs as root and this does not; this running
		// as a deploy user on a hardened host; and the plain race, since the
		// limit is read AFTER the scrape, so a restart anywhere in that window
		// leaves a pid that has gone.
		return 0, false, fmt.Errorf("cannot read the cgroup of process %d: %w", pid, err)
	}

	best := int64(0)
	for _, line := range strings.Split(string(data), "\n") {
		// v2: "0::/system.slice/php-fpm.service"
		// v1: "N:memory:/system.slice/php-fpm.service"
		fields := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(fields) != 3 || fields[2] == "" {
			continue
		}

		var base string
		var files []string
		switch {
		case fields[0] == "0" && fields[1] == "":
			// memory.high as well as memory.max.
			//
			// systemd's MemoryHigh= is the documented way to say "keep this
			// service under N", and above it the cgroup is throttled into
			// aggressive reclaim rather than killed. That is not an OOM, and it
			// is not room either: a pool sized twelve times past it thrashes
			// instead of serving, which from outside looks like a host that has
			// simply become slow. Reading only memory.max reported the machine.
			base, files = cgroupRoot, []string{"memory.max", "memory.high"}
		case strings.Contains(fields[1], "memory"):
			// v1's soft limit is advisory under pressure rather than a ceiling,
			// so only the hard one counts there.
			base, files = cgroupRoot+"/memory", []string{"memory.limit_in_bytes"}
		default:
			continue
		}

		// Every ancestor, not just the leaf: a cap on a parent slice binds
		// everything under it, and the effective limit is the smallest.
		for path := fields[2]; ; path = filepath.Dir(path) {
			for _, file := range files {
				raw, rerr := readTrimmed(filepath.Join(base, path, file))
				if rerr != nil {
					// The limit exists and cannot be read, which is the case
					// that must not pass for "unlimited".
					return 0, false, fmt.Errorf("cannot read the memory limit at %s: %w",
						filepath.Join(base, path, file), rerr)
				}
				if v, ok := plausible(raw); ok {
					if best == 0 || v < best {
						best = v
					}
				}
			}
			if path == "/" || path == "." {
				break
			}
		}
	}

	return best, best > 0, nil
}

// readTrimmed returns a file's contents, and whether it could not be read for a
// reason other than not being there.
//
// The distinction is the whole point. A cgroup with no limit set simply has no
// such file, and that is an answer. A limit file that exists and returns EACCES
// — this process is not root, the host is hardened — is NOT an answer, and
// collapsing both to "" meant a 3GiB service was sized against a 32GiB machine
// with nothing anywhere saying the number had not been confirmed.
func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}

		return "", err
	}

	return strings.TrimSpace(string(data)), nil
}

func detectWith(p sysPaths) Limits {
	limits := Limits{CPUs: runtime.NumCPU()}

	// cgroup before /proc/meminfo: inside a container meminfo reports the
	// HOST's memory, so consulting it first would size every container against
	// the whole machine.
	if bytes, ok := readCgroupV2Memory(p.cgroupV2Memory); ok {
		limits.MemoryBytes, limits.Containerized, limits.Source = bytes, true, SourceCgroupV2
	} else if bytes, ok := readCgroupV1Memory(p.cgroupV1Memory); ok {
		limits.MemoryBytes, limits.Containerized, limits.Source = bytes, true, SourceCgroupV1
	} else if total, available, ok := readMemInfo(p.memInfo); ok {
		limits.MemoryBytes, limits.AvailableBytes, limits.Source = total, available, SourceMemInfo
	} else if bytes, ok := readSysctlMemory(); ok {
		limits.MemoryBytes, limits.Source = bytes, SourceSysctl
	}

	if cpus, ok := readCgroupV2CPU(p.cgroupV2CPU); ok {
		limits.CPUs = cpus
	} else if cpus, ok := readCgroupV1CPU(p.cgroupV1Quota, p.cgroupV1Period); ok {
		limits.CPUs = cpus
	}

	return limits
}

// WithOverride replaces the detected memory ceiling.
//
// Detection is right for the common case and wrong for a specific one worth
// supporting: a host where PHP-FPM is not the only tenant. An operator who wants
// half the box given to workers says so rather than being argued with.
func (l Limits) WithOverride(memoryBytes int64) Limits {
	if memoryBytes <= 0 {
		return l
	}

	l.MemoryBytes = memoryBytes
	l.Source = SourceOverride

	// The operator gave the number, so a failed detection no longer matters:
	// the whole reason to flag it was that nobody had confirmed the budget.
	l.LookupErr = nil

	return l
}

// WithNeighbors records the memory in use OUTSIDE php-fpm's own workers — the OS,
// the page cache that cannot be reclaimed, and other services like MySQL and Redis —
// so the plan reserves it and the host as a whole stays under the target
// utilisation, not just php-fpm's own share of it.
//
// This is the good-neighbour default on a bare VM. It is deliberately a no-op where
// it would be wrong: a cgroup limit already excludes the neighbours (they run in
// other cgroups), an explicit --memory is the operator's own number, and a kernel
// that does not report MemAvailable gives nothing to reason from.
//
// The figure is `MemTotal − MemAvailable − phpfpmRSS`: everything used, less what
// the kernel can hand out and less php-fpm's own (which the reserve must not
// double-count, since the allocator is about to size the workers that hold it).
//
// It measures what neighbours use NOW. A service still warming up — MySQL's InnoDB
// buffer pool filling toward its configured maximum — uses less now than it will, so
// on a shared VM the honest hard guarantee is still a cgroup cap on php-fpm (systemd
// MemoryMax) or an explicit --reserve.
func (l Limits) WithNeighbors(phpfpmRSS int64) Limits {
	if l.Source != SourceMemInfo || l.AvailableBytes <= 0 || phpfpmRSS < 0 {
		return l
	}

	nonWorker := l.MemoryBytes - l.AvailableBytes - phpfpmRSS
	if nonWorker <= 0 {
		// Nothing meaningful in use outside php-fpm: a dedicated host, left as it was.
		return l
	}
	l.NeighborBytes = nonWorker

	return l
}

// Describe renders the limits for operator-facing output.
func (l Limits) Describe() string {
	// "container" is wrong on a VM, where the same limit comes from a systemd
	// slice — and an operator reading "container memory 3.0GiB" on a bare VM has
	// good reason to distrust the rest of the output.
	where := "host"
	switch {
	case l.Source == SourceCgroupProcess:
		where = "php-fpm's"
	case l.Containerized:
		where = "container"
	}

	base := fmt.Sprintf("%s memory %s, %d CPU(s) (via %s)",
		where, HumanBytes(l.MemoryBytes), l.CPUs, l.Source)

	if l.LookupErr != nil {
		// Said out loud, because this number and a genuinely unlimited host's
		// number are the same number. Sizing against the machine when php-fpm is
		// capped below it is how a service gets grown into a ceiling it never
		// sees.
		return base + fmt.Sprintf("\n  WARNING: php-fpm's own limit could not be read "+
			"(%v), so this is the MACHINE's memory. If php-fpm is capped below it — a "+
			"systemd MemoryMax, a container — this budget is too large.", l.LookupErr)
	}

	return base
}

func readCgroupV2Memory(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}

	raw := strings.TrimSpace(string(data))
	if raw == "max" {
		// No limit set on this cgroup: the container can use the whole host, so
		// the host's own total is the honest answer and meminfo provides it.
		return 0, false
	}

	return plausible(raw)
}

func readCgroupV1Memory(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}

	return plausible(strings.TrimSpace(string(data)))
}

// plausible parses a byte count and rejects the values that mean "unlimited".
func plausible(raw string) (int64, bool) {
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 || v >= implausibleLimit {
		return 0, false
	}

	return v, true
}

// readMemInfo returns the machine's total memory and, when the kernel reports it
// (3.14+), MemAvailable — the memory free for new allocations after what every
// other process is using, which is how the good-neighbour budget leaves room for
// services like MySQL and Redis on a bare VM. available is 0 when the field is
// absent, and the caller falls back to the dedicated assumption there.
func readMemInfo(path string) (total, available int64, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kb, perr := strconv.ParseInt(fields[1], 10, 64)
		if perr != nil || kb < 0 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			available = kb * 1024
		}
	}

	return total, available, total > 0
}

// readSysctlMemory covers macOS, where there is no /proc. Development happens
// there even though production does not.
func readSysctlMemory() (int64, bool) {
	if runtime.GOOS != "darwin" {
		return 0, false
	}

	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}

	return v, true
}

func readCgroupV2CPU(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}

	// "max 100000" means no quota; otherwise "<quota> <period>".
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) != 2 || fields[0] == "max" {
		return 0, false
	}

	return quotaToCPUs(fields[0], fields[1])
}

func readCgroupV1CPU(quotaPath, periodPath string) (int, bool) {
	quota, err := os.ReadFile(quotaPath)
	if err != nil {
		return 0, false
	}
	period, err := os.ReadFile(periodPath)
	if err != nil {
		return 0, false
	}

	return quotaToCPUs(strings.TrimSpace(string(quota)), strings.TrimSpace(string(period)))
}

// quotaToCPUs converts a CFS quota/period pair to a core count, rounding up:
// half a core still runs one worker at a time, and rounding down would report
// zero.
func quotaToCPUs(quotaRaw, periodRaw string) (int, bool) {
	quota, err := strconv.ParseInt(quotaRaw, 10, 64)
	if err != nil || quota <= 0 {
		return 0, false
	}
	period, err := strconv.ParseInt(periodRaw, 10, 64)
	if err != nil || period <= 0 {
		return 0, false
	}

	cpus := int((quota + period - 1) / period)
	if cpus < 1 {
		cpus = 1
	}

	return cpus, true
}

// HumanBytes formats a byte count for operator-facing output.
func HumanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}

	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 3; n /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGT"[exp])
}
