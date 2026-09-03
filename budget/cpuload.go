package budget

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// HostCPU is how much CPU the box has spent, cumulative, so two readings a
// scrape apart say how busy it was in between.
//
// It is the other half of the CPU measurement. php-fpm's own figure says what a
// request costs INSIDE the worker; this says what the whole box was doing,
// MySQL, nginx and the kernel included. Regressed against each pool's own CPU
// over time, the two give what a busy worker really costs the host — which on a
// box where half of every request is spent in the database is twice what the
// worker shows.
type HostCPU struct {
	// BusyMicros is CPU time spent, summed over every core, in microseconds.
	// Only the difference between two readings means anything.
	BusyMicros int64

	// At is when it was read.
	At time.Time

	// Source says where it came from: the box's /proc/stat, or php-fpm's
	// cgroup where a CPU quota bounds it — inside a container /proc/stat is the
	// host's, and the quota is the box.
	Source string
}

// HostCPUOf reads the box's cumulative CPU time. Where php-fpm's cgroup carries
// a CPU quota, the cgroup's own usage is the box; otherwise /proc/stat. False
// where neither can be read (macOS, a /proc mounted hidepid), in which case the
// host side of the measurement is simply absent and the plan sizes on
// php-fpm's own figure alone.
func HostCPUOf(masterPID int) (HostCPU, bool) {
	if dir, ok := cgroupV2DirOf(masterPID); ok {
		if _, hasQuota := readCgroupV2CPU(dir + "/cpu.max"); hasQuota {
			if busy, ok := readCgroupV2CPUUsage(dir + "/cpu.stat"); ok {
				return HostCPU{BusyMicros: busy, At: time.Now(), Source: "php-fpm's cgroup"}, true
			}
		}
	}

	busy, ok := readProcStatBusy(procRoot + "/stat")
	if !ok {
		return HostCPU{}, false
	}

	return HostCPU{BusyMicros: busy, At: time.Now(), Source: "/proc/stat"}, true
}

// cgroupV2DirOf resolves a process's cgroup v2 directory under the cgroup root.
func cgroupV2DirOf(pid int) (string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("%s/%d/cgroup", procRoot, pid))
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		// v2: "0::/system.slice/php-fpm.service"
		fields := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(fields) == 3 && fields[0] == "0" && fields[1] == "" && fields[2] != "" {
			return cgroupRoot + fields[2], true
		}
	}

	return "", false
}

// readCgroupV2CPUUsage reads usage_usec from a cgroup's cpu.stat.
func readCgroupV2CPUUsage(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), " "); ok && k == "usage_usec" {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n, err == nil
		}
	}

	return 0, false
}

// readProcStatBusy reads the aggregate "cpu" line of /proc/stat and returns
// the busy time in microseconds: everything but idle and iowait, in USER_HZ
// ticks, which Linux fixes at 100 whatever the kernel's own tick rate.
func readProcStatBusy(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	line, _, _ := strings.Cut(string(data), "\n")

	return parseProcStatBusy(line)
}

func parseProcStatBusy(line string) (int64, bool) {
	fields := strings.Fields(line)
	// cpu user nice system idle iowait irq softirq steal [guest guest_nice]
	if len(fields) < 9 || fields[0] != "cpu" {
		return 0, false
	}
	var busy int64
	for i, f := range fields[1:9] {
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return 0, false
		}
		// idle (3) and iowait (4) are the two that are not work.
		if i == 3 || i == 4 {
			continue
		}
		busy += n
	}

	return busy * 10_000, true
}
