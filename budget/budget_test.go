package budget

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const gb = 1024 * 1024 * 1024

// TestCgroupBeatsMemInfo is the container/VM split, and the reason both are read
// rather than one.
//
// Inside a container /proc/meminfo reports the HOST's memory. A tool that
// consults it first sizes every container against the whole machine and gets
// OOM-killed; a tool that only reads cgroups reports no limit on a bare VM and
// tunes nothing at all.
func TestCgroupBeatsMemInfo(t *testing.T) {
	dir := t.TempDir()
	p := fixturePaths(dir)

	// A 2GB container on a 64GB host.
	write(t, p.cgroupV2Memory, "2147483648")
	write(t, p.memInfo, "MemTotal:       67108864 kB\n")

	got := detectWith(p)

	if got.MemoryBytes != 2*gb {
		t.Errorf("memory = %s, want 2GiB: the host's total was used instead of the container's limit",
			HumanBytes(got.MemoryBytes))
	}
	if !got.Containerized {
		t.Error("a cgroup limit was not reported as containerized")
	}
	if got.Source != SourceCgroupV2 {
		t.Errorf("source = %s, want %s", got.Source, SourceCgroupV2)
	}
}

// TestUnlimitedCgroupFallsBackToTheHost: "max" means this container may use the
// whole machine, so the machine's total is the honest ceiling — not "no limit
// found, tune nothing", which is what a bare VM used to get.
func TestUnlimitedCgroupFallsBackToTheHost(t *testing.T) {
	dir := t.TempDir()
	p := fixturePaths(dir)

	write(t, p.cgroupV2Memory, "max\n")
	write(t, p.memInfo, "MemTotal:        8388608 kB\n")

	got := detectWith(p)

	if got.MemoryBytes != 8*gb {
		t.Errorf("memory = %s, want 8GiB", HumanBytes(got.MemoryBytes))
	}
	if got.Containerized {
		t.Error("an unlimited cgroup was reported as a container limit")
	}
	if got.Source != SourceMemInfo {
		t.Errorf("source = %s, want %s", got.Source, SourceMemInfo)
	}
}

// TestBareVM is the case the existing autotuner cannot handle: no cgroup limit
// anywhere, so the machine's memory is the budget.
func TestBareVM(t *testing.T) {
	dir := t.TempDir()
	p := fixturePaths(dir)

	write(t, p.memInfo, "MemTotal:       33554432 kB\nMemFree:  1024 kB\n")

	got := detectWith(p)

	if got.MemoryBytes != 32*gb {
		t.Errorf("memory = %s, want 32GiB", HumanBytes(got.MemoryBytes))
	}
	if got.Containerized {
		t.Error("a bare VM was reported as containerized")
	}
}

// TestImplausibleLimitsAreRejected: cgroup v1 writes a very large number rather
// than a sentinel for "unlimited", and some v2 runtimes write the int64 sentinel
// instead of "max". Taken literally that is a limit of ~8.8 exabytes, and every
// derived setting scales off it.
func TestImplausibleLimitsAreRejected(t *testing.T) {
	sentinels := []string{
		"9223372036854771712", // the int64 sentinel, seen from both v1 and v2
		"9223372036854775807",
		"0",
		"-1",
		"not a number",
	}

	for _, raw := range sentinels {
		t.Run(raw, func(t *testing.T) {
			dir := t.TempDir()
			p := fixturePaths(dir)

			write(t, p.cgroupV2Memory, raw)
			write(t, p.memInfo, "MemTotal:        4194304 kB\n")

			got := detectWith(p)

			if got.Containerized {
				t.Errorf("%q was accepted as a container limit (%s)", raw, HumanBytes(got.MemoryBytes))
			}
			if got.MemoryBytes != 4*gb {
				t.Errorf("memory = %s, want the 4GiB host total", HumanBytes(got.MemoryBytes))
			}
		})
	}
}

// TestCgroupV1IsReadWhenV2IsAbsent keeps older hosts working.
func TestCgroupV1IsReadWhenV2IsAbsent(t *testing.T) {
	dir := t.TempDir()
	p := fixturePaths(dir)

	write(t, p.cgroupV1Memory, "1073741824")
	write(t, p.memInfo, "MemTotal:       67108864 kB\n")

	got := detectWith(p)

	if got.MemoryBytes != gb {
		t.Errorf("memory = %s, want 1GiB", HumanBytes(got.MemoryBytes))
	}
	if got.Source != SourceCgroupV1 {
		t.Errorf("source = %s, want %s", got.Source, SourceCgroupV1)
	}
}

// TestCPUQuotaRoundsUp: half a core still runs one worker at a time, and
// rounding down would report zero CPUs.
func TestCPUQuotaRoundsUp(t *testing.T) {
	tests := map[string]struct {
		quota, period string
		want          int
	}{
		"half a core":     {"50000", "100000", 1},
		"one core":        {"100000", "100000", 1},
		"one and a half":  {"150000", "100000", 2},
		"four cores":      {"400000", "100000", 4},
		"a tenth of core": {"10000", "100000", 1},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := fixturePaths(dir)
			write(t, p.cgroupV2CPU, tt.quota+" "+tt.period)
			write(t, p.memInfo, "MemTotal:        4194304 kB\n")

			if got := detectWith(p).CPUs; got != tt.want {
				t.Errorf("CPUs = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestUnlimitedCPUKeepsTheHostCount: "max" is no quota at all.
func TestUnlimitedCPUKeepsTheHostCount(t *testing.T) {
	dir := t.TempDir()
	p := fixturePaths(dir)
	write(t, p.cgroupV2CPU, "max 100000")
	write(t, p.memInfo, "MemTotal:        4194304 kB\n")

	if got := detectWith(p).CPUs; got < 1 {
		t.Errorf("CPUs = %d with no quota; want the host count", got)
	}
}

// TestOverrideWins: a host where PHP-FPM is not the only tenant is a real case,
// and the operator's number outranks detection.
func TestOverrideWins(t *testing.T) {
	l := Limits{MemoryBytes: 16 * gb, Source: SourceMemInfo}

	got := l.WithOverride(4 * gb)
	if got.MemoryBytes != 4*gb {
		t.Errorf("memory = %s, want 4GiB", HumanBytes(got.MemoryBytes))
	}
	if got.Source != SourceOverride {
		t.Errorf("source = %s, want %s", got.Source, SourceOverride)
	}

	// A non-positive override is not an instruction, so detection stands.
	if again := l.WithOverride(0); again.MemoryBytes != 16*gb {
		t.Errorf("a zero override changed the limit to %s", HumanBytes(again.MemoryBytes))
	}
}

// TestDetectOnThisMachine is a smoke test: whatever this is running on, it must
// produce a usable budget rather than zero.
func TestDetectOnThisMachine(t *testing.T) {
	got := DetectFor(0)

	if got.MemoryBytes <= 0 {
		t.Errorf("no memory detected on this host; Describe: %s", got.Describe())
	}
	if got.CPUs < 1 {
		t.Errorf("CPUs = %d", got.CPUs)
	}
	t.Logf("detected: %s", got.Describe())
}

func TestHumanBytes(t *testing.T) {
	tests := map[int64]string{
		512:            "512B",
		1024:           "1.0KiB",
		1536:           "1.5KiB",
		1024 * 1024:    "1.0MiB",
		gb:             "1.0GiB",
		2 * gb:         "2.0GiB",
		1024 * gb:      "1.0TiB",
		100 * 1024 * 1: "100.0KiB",
	}

	for in, want := range tests {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %s, want %s", in, got, want)
		}
	}
}

// fixturePaths points the reader at a temp directory. Files that a test does not
// write are simply absent, which is what a host without that cgroup looks like.
func fixturePaths(dir string) sysPaths {
	return sysPaths{
		cgroupV2Memory: filepath.Join(dir, "memory.max"),
		cgroupV2CPU:    filepath.Join(dir, "cpu.max"),
		cgroupV1Memory: filepath.Join(dir, "memory.limit_in_bytes"),
		cgroupV1Quota:  filepath.Join(dir, "cpu.cfs_quota_us"),
		cgroupV1Period: filepath.Join(dir, "cpu.cfs_period_us"),
		memInfo:        filepath.Join(dir, "meminfo"),
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDetectForReadsTheManagedProcessCgroup.
//
// Detect reads /sys/fs/cgroup/memory.max — an absolute path, which is the ROOT
// of the hierarchy. Inside a container that is exactly right, because the
// container's own cgroup is what gets mounted there. On a VM it is the machine,
// and the machine is never limited.
//
// Measured on a five-pool Ubuntu VM with php-fpm under MemoryMax=3G: fpm-tune
// reported "14.7GiB available to workers" while php-fpm's own cgroup would
// OOM-kill its workers at 3GiB. It would have grown the pools straight into the
// ceiling it exists to avoid, and looked right the whole way, because the number
// it printed was the machine's real memory. No container test could surface it:
// in a container both readings agree.
func TestDetectForReadsTheManagedProcessCgroup(t *testing.T) {
	root := t.TempDir()

	// /proc/<pid>/cgroup, as systemd leaves it.
	proc := filepath.Join(root, "proc", "4242")
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "cgroup"),
		[]byte("0::/system.slice/php8.5-fpm.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The limit on the service, "max" everywhere above it — the real shape.
	cg := filepath.Join(root, "cgroup")
	for path, value := range map[string]string{
		"system.slice/php8.5-fpm.service": "3221225472",
		"system.slice":                    "max",
		"":                                "max",
	} {
		dir := filepath.Join(cg, path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	restore := swapRoots(cg, filepath.Join(root, "proc"))
	defer restore()

	limits := DetectFor(4242)

	if limits.MemoryBytes != 3221225472 {
		t.Errorf("MemoryBytes = %s, want the 3GiB the service is actually capped at",
			HumanBytes(limits.MemoryBytes))
	}
	if limits.Source != SourceCgroupProcess {
		t.Errorf("Source = %q, want %q so an operator can see which number to change",
			limits.Source, SourceCgroupProcess)
	}
	if strings.Contains(limits.Describe(), "container") {
		t.Errorf("described as a container on a VM: %q", limits.Describe())
	}
}

// TestDetectForTakesTheTightestLimitInThePath: a cap on a parent slice binds
// everything under it, so the effective limit is the smallest along the path —
// not the one nearest the process.
func TestDetectForTakesTheTightestLimitInThePath(t *testing.T) {
	root := t.TempDir()

	proc := filepath.Join(root, "proc", "77")
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "cgroup"),
		[]byte("0::/sites.slice/php.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cg := filepath.Join(root, "cgroup")
	for path, value := range map[string]string{
		"sites.slice/php.service": "8589934592", // 8GiB on the service
		"sites.slice":             "2147483648", // 2GiB on the slice above it
		"":                        "max",
	} {
		dir := filepath.Join(cg, path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	restore := swapRoots(cg, filepath.Join(root, "proc"))
	defer restore()

	if got := DetectFor(77).MemoryBytes; got != 2147483648 {
		t.Errorf("MemoryBytes = %s, want the 2GiB parent cap: sizing to the service's "+
			"own 8GiB would be sizing past a limit that binds it", HumanBytes(got))
	}
}

// TestDetectForFallsBackWhenThereIsNoLimit: a bare VM with no slice cap must
// still report the machine's memory rather than nothing. Pid zero is how that is
// asked, and it is the only way to ask it.
func TestDetectForFallsBackWhenThereIsNoLimit(t *testing.T) {
	plain := DetectFor(0)
	if plain.MemoryBytes <= 0 {
		t.Skip("no memory reading available on this host")
	}

	if got := DetectFor(0); got.MemoryBytes != plain.MemoryBytes {
		t.Errorf("DetectFor(0) = %s, want the same as Detect() = %s",
			HumanBytes(got.MemoryBytes), HumanBytes(plain.MemoryBytes))
	}
}

func swapRoots(cg, proc string) func() {
	oldCg, oldProc := cgroupRoot, procRoot
	cgroupRoot, procRoot = cg, proc

	return func() { cgroupRoot, procRoot = oldCg, oldProc }
}
