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
		millicores    int
	}{
		"half a core":     {"50000", "100000", 1, 500},
		"one core":        {"100000", "100000", 1, 1000},
		"one and a half":  {"150000", "100000", 2, 1500},
		"four cores":      {"400000", "100000", 4, 4000},
		"a tenth of core": {"10000", "100000", 1, 100},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := fixturePaths(dir)
			write(t, p.cgroupV2CPU, tt.quota+" "+tt.period)
			write(t, p.memInfo, "MemTotal:        4194304 kB\n")

			got := detectWith(p)
			if got.CPUs != tt.want {
				t.Errorf("CPUs = %d, want %d", got.CPUs, tt.want)
			}
			// The fraction survives alongside: anything that divides by the
			// CPU must not be told a half-core quota is a core.
			if got.CPUMillicores != tt.millicores {
				t.Errorf("CPUMillicores = %d, want %d", got.CPUMillicores, tt.millicores)
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

	// The machine the service runs on, larger than its cgroup cap — otherwise
	// the cgroup limit is not the tighter of the two and there is nothing to
	// prove. On Linux this is the only source of a base reading (no sysctl), so
	// without it the base is zero and the cgroup override has nothing to beat.
	write(t, filepath.Join(root, "proc", "meminfo"), "MemTotal:       67108864 kB\n")

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

	// A machine larger than either cap, so the tightest cgroup limit is what wins.
	write(t, filepath.Join(root, "proc", "meminfo"), "MemTotal:       67108864 kB\n")

	restore := swapRoots(cg, filepath.Join(root, "proc"))
	defer restore()

	if got := DetectFor(77).MemoryBytes; got != 2147483648 {
		t.Errorf("MemoryBytes = %s, want the 2GiB parent cap: sizing to the service's "+
			"own 8GiB would be sizing past a limit that binds it", HumanBytes(got))
	}
}

// TestDetectForFallsBackWhenThereIsNoLimit: a bare VM with no cgroup cap
// anywhere must report the machine's memory rather than nothing.
//
// Against a fixture, because the previous version compared DetectFor(0) with
// DetectFor(0) — a leftover from the refactor that removed Detect(), and an
// equality no change to the code could break. It also left the "max" fall-
// through and the guard that prefers the tighter of the two readings covered by
// nothing at all.
func TestDetectForFallsBackWhenThereIsNoLimit(t *testing.T) {
	root := t.TempDir()

	proc := filepath.Join(root, "proc", "4242")
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "cgroup"),
		[]byte("0::/system.slice/php8.5-fpm.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The bare-VM shape: a cgroup hierarchy with no limit set anywhere in it.
	cg := filepath.Join(root, "cgroup")
	for _, path := range []string{"system.slice/php8.5-fpm.service", "system.slice", ""} {
		dir := filepath.Join(cg, path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte("max\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "meminfo"),
		[]byte("MemTotal:       16777216 kB\nMemFree:         8388608 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	defer swapRoots(cg, filepath.Join(root, "proc"))()

	got := DetectFor(4242)
	if got.Source != SourceMemInfo {
		t.Errorf("Source = %q, want %q: with no limit anywhere, the machine's own memory "+
			"is the only honest answer", got.Source, SourceMemInfo)
	}
	if want := int64(16777216) * 1024; got.MemoryBytes != want {
		t.Errorf("MemoryBytes = %s, want %s", HumanBytes(got.MemoryBytes), HumanBytes(want))
	}
}

// TestACgroupLimitLargerThanTheMachineIsIgnored: a slice capped above the
// machine's own memory is not a budget, it is the absence of one, and sizing to
// it commits memory the host does not have.
func TestACgroupLimitLargerThanTheMachineIsIgnored(t *testing.T) {
	root := t.TempDir()

	proc := filepath.Join(root, "proc", "4242")
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "cgroup"),
		[]byte("0::/system.slice/php8.5-fpm.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cg := filepath.Join(root, "cgroup")
	for path, value := range map[string]string{
		// 64GiB on a 16GiB machine, which is what MemoryMax=infinity-adjacent
		// settings and container defaults look like.
		"system.slice/php8.5-fpm.service": "68719476736",
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
	if err := os.WriteFile(filepath.Join(root, "proc", "meminfo"),
		[]byte("MemTotal:       16777216 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	defer swapRoots(cg, filepath.Join(root, "proc"))()

	got := DetectFor(4242)
	if want := int64(16777216) * 1024; got.MemoryBytes != want {
		t.Errorf("MemoryBytes = %s from a 64GiB cgroup limit on a 16GiB machine, want %s",
			HumanBytes(got.MemoryBytes), HumanBytes(want))
	}
}

// swapRoots points the whole detection at a fixture tree.
//
// defaultPaths as well as the two roots, and the difference matters: it holds
// absolute literals, so a test that swapped only the roots still read the
// RUNNER's /proc/meminfo for the host fallback. Two tests written against a
// 16GiB fixture therefore asserted a number no real machine reports exactly,
// and would have failed the first time CI ran them on Linux — while passing
// locally, because darwin skips them.
func swapRoots(cg, proc string) func() {
	oldCg, oldProc, oldPaths := cgroupRoot, procRoot, defaultPaths
	cgroupRoot, procRoot = cg, proc
	defaultPaths = sysPaths{
		cgroupV2Memory: filepath.Join(cg, "memory.max"),
		cgroupV2CPU:    filepath.Join(cg, "cpu.max"),
		cgroupV1Memory: filepath.Join(cg, "memory", "memory.limit_in_bytes"),
		cgroupV1Quota:  filepath.Join(cg, "cpu", "cpu.cfs_quota_us"),
		cgroupV1Period: filepath.Join(cg, "cpu", "cpu.cfs_period_us"),
		memInfo:        filepath.Join(proc, "meminfo"),
	}

	return func() { cgroupRoot, procRoot, defaultPaths = oldCg, oldProc, oldPaths }
}

// TestAProcessWhoseLimitCannotBeReadIsNotReportedAsUnlimited.
//
// "Could not look" and "found no limit" produced the same answer and the same
// silence: the machine's memory, with no error, no flag and nothing in
// Describe. So a php-fpm capped at 3GiB was sized against a 24GiB machine —
// eight times its real ceiling — and the output was indistinguishable from a
// genuinely unlimited host.
//
// Three ordinary routes in, none exotic: /proc mounted hidepid=2 while php-fpm
// runs as root and this does not; this running as a deploy user on a hardened
// host; and the plain race, since the limit is read AFTER the scrape, so a
// restart anywhere in that window leaves a pid that has gone.
func TestAProcessWhoseLimitCannotBeReadIsNotReportedAsUnlimited(t *testing.T) {
	root := t.TempDir()
	// A /proc with no entry for the pid at all, which is what a dead process and
	// a hidden one both look like from here.
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer swapRoots(filepath.Join(root, "cgroup"), filepath.Join(root, "proc"))()

	got := DetectFor(4242)

	if got.LookupErr == nil {
		t.Fatal("a process whose cgroup could not be read reported a budget with no " +
			"reservation at all; that number is the machine's, and it is the widest " +
			"possible answer to a question about a limit")
	}
	if !strings.Contains(got.Describe(), "WARNING") {
		t.Errorf("Describe says nothing about it:\n%s", got.Describe())
	}
	if !strings.Contains(got.Describe(), "too large") {
		t.Errorf("the warning does not say which direction the error is in:\n%s",
			got.Describe())
	}
}

// TestAProcessWithNoLimitAnywhereIsNotAnError is the other half: a genuinely
// unlimited host must not be reported as a failed lookup, or the warning
// becomes noise and stops being read.
func TestAProcessWithNoLimitAnywhereIsNotAnError(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "proc", "4242")
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "cgroup"),
		[]byte("0::/system.slice/php-fpm.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cg := filepath.Join(root, "cgroup")
	for _, p := range []string{"system.slice/php-fpm.service", "system.slice", ""} {
		dir := filepath.Join(cg, p)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte("max\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "meminfo"),
		[]byte("MemTotal:       16777216 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	defer swapRoots(cg, filepath.Join(root, "proc"))()

	if got := DetectFor(4242); got.LookupErr != nil {
		t.Errorf("a host with no limit anywhere was reported as a failed lookup: %v",
			got.LookupErr)
	}
}

// TestASoftCeilingCountsToo.
//
// systemd's MemoryHigh= is the documented way to say "keep this service under
// N". Above it the cgroup is throttled into aggressive reclaim rather than
// killed — so it is not an OOM line, and it is not room either: a pool sized
// twelve times past it thrashes instead of serving, which from outside looks
// like a host that has simply gone slow.
//
// Reading only memory.max reported the whole machine for a service that had
// been given a ceiling in the one way systemd documents for it.
func TestASoftCeilingCountsToo(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "proc", "4242")
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "cgroup"),
		[]byte("0::/system.slice/php-fpm.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "meminfo"),
		[]byte("MemTotal:       16777216 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cg := filepath.Join(root, "cgroup")
	for path, files := range map[string]map[string]string{
		"system.slice/php-fpm.service": {"memory.max": "max", "memory.high": "2147483648"},
		"system.slice":                 {"memory.max": "max", "memory.high": "max"},
		"":                             {"memory.max": "max", "memory.high": "max"},
	} {
		dir := filepath.Join(cg, path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, value := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	defer swapRoots(cg, filepath.Join(root, "proc"))()

	got := DetectFor(4242)
	if want := int64(2) << 30; got.MemoryBytes != want {
		t.Errorf("MemoryBytes = %s for a service with MemoryHigh=2G, want %s: the pools "+
			"are about to be sized against the machine, and above the soft ceiling this "+
			"host reclaims rather than serves",
			HumanBytes(got.MemoryBytes), HumanBytes(want))
	}
}

// TestTheTighterOfTheTwoCeilingsWins: MemoryMax and MemoryHigh can both be set,
// and the one that binds first is the smaller. Sizing to the larger is sizing
// past a line the kernel enforces.
func TestTheTighterOfTheTwoCeilingsWins(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "proc", "4242")
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "cgroup"),
		[]byte("0::/system.slice/php-fpm.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "meminfo"),
		[]byte("MemTotal:       16777216 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cg := filepath.Join(root, "cgroup")
	dir := filepath.Join(cg, "system.slice/php-fpm.service")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A hard cap of 4G with a soft one of 1G: the service is reclaimed at 1G.
	for name, value := range map[string]string{
		"memory.max": "4294967296", "memory.high": "1073741824",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	defer swapRoots(cg, filepath.Join(root, "proc"))()

	if got := DetectFor(4242); got.MemoryBytes != 1<<30 {
		t.Errorf("MemoryBytes = %s with MemoryMax=4G and MemoryHigh=1G, want 1.0GiB",
			HumanBytes(got.MemoryBytes))
	}
}

// TestALimitThatCannotBeReadIsNotAnAbsentLimit.
//
// A cgroup with no limit set simply has no such file, and that is an answer. A
// limit file that exists and returns EACCES — this process is not root, the
// host is hardened — is not an answer, and collapsing both to an empty string
// meant a 3GiB service was sized against a 32GiB machine with nothing anywhere
// saying the number had not been confirmed.
//
// The earlier fix covered an unreadable /proc/<pid>/cgroup. This is the other
// half: the path is readable and names a slice, and the slice's own limit file
// is not.
func TestALimitThatCannotBeReadIsNotAnAbsentLimit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file with mode 000")
	}

	root := t.TempDir()
	proc := filepath.Join(root, "proc", "4242")
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "cgroup"),
		[]byte("0::/system.slice/php-fpm.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "meminfo"),
		[]byte("MemTotal:       33554432 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cg := filepath.Join(root, "cgroup")
	dir := filepath.Join(cg, "system.slice/php-fpm.service")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	limit := filepath.Join(dir, "memory.max")
	if err := os.WriteFile(limit, []byte("3221225472\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(limit, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(limit, 0o644) })

	defer swapRoots(cg, filepath.Join(root, "proc"))()

	got := DetectFor(4242)
	if got.LookupErr == nil {
		t.Errorf("a 3GiB limit that could not be read was reported as %s with no "+
			"reservation at all; that is the machine's memory, and apply is about to "+
			"write from it", HumanBytes(got.MemoryBytes))
	}
}

// TestAHostWithNoProcIsNotAFailedLookup.
//
// Reporting a failed lookup when php-fpm's own limit cannot be read is right on
// Linux, where /proc/<pid>/cgroup missing means the process is gone or hidden.
// It is wrong everywhere /proc does not exist at all — macOS, or a Linux host
// with it unmounted — because there are no cgroups there and the machine's
// memory is not a fallback from a failed reading, it is the answer.
//
// Caught by the end-to-end suite rather than by a unit test: the daemon refused
// to apply on every run, on the machine this is developed on.
func TestAHostWithNoProcIsNotAFailedLookup(t *testing.T) {
	root := t.TempDir()
	// A cgroup root that exists, and no /proc beside it.
	cg := filepath.Join(root, "cgroup")
	if err := os.MkdirAll(cg, 0o755); err != nil {
		t.Fatal(err)
	}

	defer swapRoots(cg, filepath.Join(root, "does-not-exist"))()

	if got := DetectFor(4242); got.LookupErr != nil {
		t.Errorf("a host with no /proc reported a failed lookup (%v); there are no "+
			"cgroups to read there, and the tool now refuses to write on every such host",
			got.LookupErr)
	}
}

// TestProcStatBusyCountsWorkNotWaiting: the aggregate cpu line, everything but
// idle and iowait, in USER_HZ ticks turned into microseconds.
func TestProcStatBusyCountsWorkNotWaiting(t *testing.T) {
	//                 user  nice system idle   iowait irq softirq steal guest gnice
	line := "cpu  1000 10   500    90000  400    5   15      20    0     0"
	got, ok := parseProcStatBusy(line)
	if !ok {
		t.Fatal("a well-formed cpu line was refused")
	}
	if want := int64(1000+10+500+5+15+20) * 10_000; got != want {
		t.Errorf("busy = %d µs, want %d: user+nice+system+irq+softirq+steal", got, want)
	}
	for _, bad := range []string{"", "cpu0 1 2 3 4 5 6 7 8", "cpu 1 2 3", "cpu a b c d e f g h"} {
		if _, ok := parseProcStatBusy(bad); ok {
			t.Errorf("%q was accepted", bad)
		}
	}
}
