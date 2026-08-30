package budget

import (
	"os"
	"path/filepath"
	"testing"
)

const gib = int64(1) << 30

// TestWithNeighborsRecordsNonWorkerMemory: on a bare VM, the memory other services
// and the OS are using is recorded so the plan can leave room for it. The figure is
// MemTotal − MemAvailable − php-fpm's own.
func TestWithNeighborsRecordsNonWorkerMemory(t *testing.T) {
	l := Limits{MemoryBytes: 8 * gib, AvailableBytes: 3 * gib, Source: SourceMemInfo}

	got := l.WithNeighbors(1 * gib)

	if got.NeighborBytes != 4*gib {
		t.Errorf("NeighborBytes = %s, want 4GiB (8 − 3 available − 1 php-fpm)", HumanBytes(got.NeighborBytes))
	}
	if got.MemoryBytes != 8*gib {
		t.Errorf("MemoryBytes = %s, want the host total 8GiB left unchanged", HumanBytes(got.MemoryBytes))
	}
}

// TestWithNeighborsIsANoOpOnADedicatedHost: when almost everything is free, there
// is no neighbour to reserve for, and the host is left exactly as it was.
func TestWithNeighborsIsANoOpOnADedicatedHost(t *testing.T) {
	l := Limits{MemoryBytes: 8 * gib, AvailableBytes: 7*gib + 512*(1<<20), Source: SourceMemInfo}

	got := l.WithNeighbors(512 * (1 << 20)) // php-fpm using 512MiB; total accounted ≈ host

	if got.NeighborBytes != 0 {
		t.Errorf("NeighborBytes = %s, want 0 on a host with nothing else using memory", HumanBytes(got.NeighborBytes))
	}
}

// TestWithNeighborsDoesNotTouchACgroupLimit: a cgroup limit already excludes the
// neighbours — they run in other cgroups — so the good-neighbour reserve would
// double-count. It is a no-op there, and off /proc/meminfo entirely.
func TestWithNeighborsDoesNotTouchACgroupLimit(t *testing.T) {
	for _, src := range []Source{SourceCgroupV2, SourceCgroupProcess, SourceOverride} {
		l := Limits{MemoryBytes: 4 * gib, AvailableBytes: 2 * gib, Source: src, Containerized: true}
		if got := l.WithNeighbors(1 * gib); got.NeighborBytes != 0 {
			t.Errorf("source %s: NeighborBytes = %s, want 0 (the cap already excludes neighbours)",
				src, HumanBytes(got.NeighborBytes))
		}
	}
}

// TestWithNeighborsNeedsMemAvailable: a kernel too old to report MemAvailable gives
// nothing to reason from, so it falls back to the dedicated assumption.
func TestWithNeighborsNeedsMemAvailable(t *testing.T) {
	l := Limits{MemoryBytes: 8 * gib, AvailableBytes: 0, Source: SourceMemInfo}
	if got := l.WithNeighbors(1 * gib); got.NeighborBytes != 0 {
		t.Errorf("NeighborBytes = %s, want 0 when MemAvailable is unknown", HumanBytes(got.NeighborBytes))
	}
}

// TestReadMemInfoReadsAvailable parses both MemTotal and MemAvailable.
func TestReadMemInfoReadsAvailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meminfo")
	body := "MemTotal:        8388608 kB\nMemFree:         1048576 kB\nMemAvailable:    3145728 kB\nBuffers:          200000 kB\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	total, available, ok := readMemInfo(path)
	if !ok {
		t.Fatal("readMemInfo reported no total")
	}
	if total != 8388608*1024 {
		t.Errorf("total = %d, want %d", total, 8388608*1024)
	}
	if available != 3145728*1024 {
		t.Errorf("available = %d, want %d", available, 3145728*1024)
	}
}
