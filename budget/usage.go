package budget

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CgroupUsage is what a process's cgroup has actually used — not its limit.
//
// Both numbers count every process in the cgroup: the master, its workers, and
// every child a worker spawned, because a forked process inherits its parent's
// cgroup and the kernel charges its pages there. That is the whole reason to
// read this. A worker's own RSS, and even its subtree RSS sampled every thirty
// seconds, miss an ffmpeg that lived and died between two scrapes; the cgroup
// does not, because the kernel maintains the charge continuously and it is what
// the OOM killer enforces against.
type CgroupUsage struct {
	// CurrentBytes is the cgroup's resident charge right now: memory.current
	// (v2) or memory.usage_in_bytes (v1).
	CurrentBytes int64

	// PeakBytes is the kernel's own high-water mark: memory.peak (v2, Linux
	// 5.19+) or memory.max_usage_in_bytes (v1). Zero when the kernel offers
	// neither — an older v2 kernel — in which case a high-water has to be built
	// by remembering the largest CurrentBytes seen across rounds.
	PeakBytes int64
}

// CgroupUsageOf reads the usage of the cgroup a process lives in.
//
// ok is false where there is no cgroup at all — a bare VM or a dedicated server
// without one, and macOS — rather than a misleading zero. There, the per-worker
// subtree measurement is the only view of what children cost, and it stands on
// its own.
//
// The reading is the LEAF cgroup, where the processes actually are, not the
// tightest ancestor a limit came from: a limit binds from above, but usage is
// charged where the pages live.
func CgroupUsageOf(pid int) (CgroupUsage, bool) {
	if _, err := os.Stat(procRoot); err != nil {
		// No /proc: macOS, or a Linux host with it unmounted. No cgroup to read.
		return CgroupUsage{}, false
	}

	data, err := os.ReadFile(fmt.Sprintf("%s/%d/cgroup", procRoot, pid))
	if err != nil {
		// Could not look. Unlike a limit — where an unreadable file must never
		// pass for "unlimited" — an unreadable usage is simply an absent signal:
		// it makes the tool fall back to subtree RSS, which is always safe. So
		// this is (nothing, false), not an error.
		return CgroupUsage{}, false
	}

	for _, line := range strings.Split(string(data), "\n") {
		// v2: "0::/system.slice/php-fpm.service"
		// v1: "N:memory:/system.slice/php-fpm.service"
		fields := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(fields) != 3 || fields[2] == "" {
			continue
		}

		var dir, currentFile, peakFile string
		switch {
		case fields[0] == "0" && fields[1] == "":
			dir = filepath.Join(cgroupRoot, fields[2])
			currentFile, peakFile = "memory.current", "memory.peak"
		case strings.Contains(fields[1], "memory"):
			dir = filepath.Join(cgroupRoot, "memory", fields[2])
			currentFile, peakFile = "memory.usage_in_bytes", "memory.max_usage_in_bytes"
		default:
			continue
		}

		current, ok := readBytes(filepath.Join(dir, currentFile))
		if !ok {
			// The line named a controller but its usage file is not there. Keep
			// looking: a host can list both a v1 memory line and a v2 line, and
			// only one of them resolves to real files.
			continue
		}

		usage := CgroupUsage{CurrentBytes: current}
		if peak, ok := readBytes(filepath.Join(dir, peakFile)); ok && peak > current {
			usage.PeakBytes = peak
		}

		return usage, true
	}

	return CgroupUsage{}, false
}

// readBytes reads a single-integer cgroup file. ok is false when the file is
// absent or does not hold a plain number — a usage file never says "max", so
// unlike a limit there is no sentinel to fold, only real bytes or nothing.
func readBytes(path string) (int64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}

	v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || v < 0 {
		return 0, false
	}

	return v, true
}
