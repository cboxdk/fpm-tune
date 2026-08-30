package budget

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeCgroupUsage lays out a fake cgroup for pid at leaf, with the given files.
func writeCgroupUsage(t *testing.T, pid int, cgroupLine, leaf string, files map[string]string) (cg, proc string) {
	t.Helper()
	root := t.TempDir()

	proc = filepath.Join(root, "proc")
	pdir := filepath.Join(proc, strconv.Itoa(pid))
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "cgroup"), []byte(cgroupLine), 0o644); err != nil {
		t.Fatal(err)
	}

	cg = filepath.Join(root, "cgroup")
	dir := filepath.Join(cg, leaf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return cg, proc
}

// TestCgroupUsageV2WithPeak: a modern v2 kernel exposes memory.peak, and it is
// the high-water mark that catches the ffmpeg a sample missed.
func TestCgroupUsageV2WithPeak(t *testing.T) {
	cg, proc := writeCgroupUsage(t, 4242,
		"0::/system.slice/php-fpm.service\n",
		"system.slice/php-fpm.service",
		map[string]string{
			"memory.current": "1073741824", // 1GiB now
			"memory.peak":    "4294967296", // 4GiB at its worst
		})
	defer swapRoots(cg, proc)()

	usage, ok := CgroupUsageOf(4242)
	if !ok {
		t.Fatal("a cgroup with usage files read as no cgroup")
	}
	if usage.CurrentBytes != 1073741824 {
		t.Errorf("CurrentBytes = %s, want 1GiB", HumanBytes(usage.CurrentBytes))
	}
	if usage.PeakBytes != 4294967296 {
		t.Errorf("PeakBytes = %s, want the 4GiB high-water — the number that catches "+
			"a transient child a sample missed", HumanBytes(usage.PeakBytes))
	}
}

// TestCgroupUsageV2WithoutPeak: an older v2 kernel has memory.current but no
// memory.peak. The current reading still comes back; the peak is left zero for
// the caller to build from the running maximum.
func TestCgroupUsageV2WithoutPeak(t *testing.T) {
	cg, proc := writeCgroupUsage(t, 7,
		"0::/system.slice/php-fpm.service\n",
		"system.slice/php-fpm.service",
		map[string]string{"memory.current": "2147483648"})
	defer swapRoots(cg, proc)()

	usage, ok := CgroupUsageOf(7)
	if !ok {
		t.Fatal("a cgroup with only memory.current read as no cgroup")
	}
	if usage.CurrentBytes != 2147483648 {
		t.Errorf("CurrentBytes = %s, want 2GiB", HumanBytes(usage.CurrentBytes))
	}
	if usage.PeakBytes != 0 {
		t.Errorf("PeakBytes = %s, want 0 when the kernel offers no high-water", HumanBytes(usage.PeakBytes))
	}
}

// TestCgroupUsageV1: cgroup v1 keeps its own high-water in
// memory.max_usage_in_bytes.
func TestCgroupUsageV1(t *testing.T) {
	cg, proc := writeCgroupUsage(t, 99,
		"5:memory:/system.slice/php-fpm.service\n",
		"memory/system.slice/php-fpm.service",
		map[string]string{
			"memory.usage_in_bytes":     "536870912",  // 512MiB now
			"memory.max_usage_in_bytes": "1610612736", // 1.5GiB peak
		})
	defer swapRoots(cg, proc)()

	usage, ok := CgroupUsageOf(99)
	if !ok {
		t.Fatal("a v1 memory cgroup read as no cgroup")
	}
	if usage.CurrentBytes != 536870912 {
		t.Errorf("CurrentBytes = %s, want 512MiB", HumanBytes(usage.CurrentBytes))
	}
	if usage.PeakBytes != 1610612736 {
		t.Errorf("PeakBytes = %s, want the 1.5GiB v1 high-water", HumanBytes(usage.PeakBytes))
	}
}

// TestCgroupUsageNoCgroup: a bare VM or dedicated host has no usage file for the
// process. That is (nothing, false), not a zero that would read as "used no
// memory" — the subtree measurement stands alone there.
func TestCgroupUsageNoCgroup(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "proc", "1")
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	// A cgroup line pointing at a hierarchy with no usage files present.
	if err := os.WriteFile(filepath.Join(proc, "cgroup"),
		[]byte("0::/system.slice/php-fpm.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer swapRoots(filepath.Join(root, "cgroup"), filepath.Join(root, "proc"))()

	if _, ok := CgroupUsageOf(1); ok {
		t.Error("a host with no cgroup usage files reported a usage; the caller would " +
			"treat a bare VM as having a zero-byte pool")
	}
}
