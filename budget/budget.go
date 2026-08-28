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

// Detect reads the host's limits.
func Detect() Limits {
	return detectWith(defaultPaths)
}

// DetectFor reads the limits that apply to a PARTICULAR process, which on a VM
// is not the same question.
//
// Detect reads /sys/fs/cgroup/memory.max — an absolute path, which is the ROOT
// of the hierarchy. Inside a container that is exactly right: the container's
// own cgroup is what gets mounted there, so the root IS the container's limit.
// On a VM it is the machine, and the machine is never limited.
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

	bytes, ok := cgroupLimitOf(pid)
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
func cgroupLimitOf(pid int) (int64, bool) {
	data, err := os.ReadFile(fmt.Sprintf("%s/%d/cgroup", procRoot, pid))
	if err != nil {
		return 0, false
	}

	best := int64(0)
	for _, line := range strings.Split(string(data), "\n") {
		// v2: "0::/system.slice/php-fpm.service"
		// v1: "N:memory:/system.slice/php-fpm.service"
		fields := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(fields) != 3 || fields[2] == "" {
			continue
		}

		var base, file string
		switch {
		case fields[0] == "0" && fields[1] == "":
			base, file = cgroupRoot, "memory.max"
		case strings.Contains(fields[1], "memory"):
			base, file = cgroupRoot+"/memory", "memory.limit_in_bytes"
		default:
			continue
		}

		// Every ancestor, not just the leaf: a cap on a parent slice binds
		// everything under it, and the effective limit is the smallest.
		for path := fields[2]; ; path = filepath.Dir(path) {
			if v, ok := plausible(readTrimmed(filepath.Join(base, path, file))); ok {
				if best == 0 || v < best {
					best = v
				}
			}
			if path == "/" || path == "." {
				break
			}
		}
	}

	return best, best > 0
}

func readTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(data))
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
	} else if bytes, ok := readMemInfo(p.memInfo); ok {
		limits.MemoryBytes, limits.Source = bytes, SourceMemInfo
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

	return fmt.Sprintf("%s memory %s, %d CPU(s) (via %s)",
		where, HumanBytes(l.MemoryBytes), l.CPUs, l.Source)
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

func readMemInfo(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}

	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || kb <= 0 {
			return 0, false
		}

		return kb * 1024, true
	}

	return 0, false
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
