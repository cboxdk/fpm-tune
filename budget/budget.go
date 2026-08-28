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
	"runtime"
	"strconv"
	"strings"
)

// Source records where a limit came from, so `fpm-tune plan` can show it. An
// operator who disagrees with the budget needs to know which number to change.
type Source string

const (
	SourceCgroupV2 Source = "cgroup v2"
	SourceCgroupV1 Source = "cgroup v1"
	SourceMemInfo  Source = "/proc/meminfo"
	SourceSysctl   Source = "sysctl"
	SourceOverride Source = "override"
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
	where := "host"
	if l.Containerized {
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
