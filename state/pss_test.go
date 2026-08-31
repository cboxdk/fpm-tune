package state

import (
	"testing"
	"time"
)

// TestSizingPrefersPSSButChildDeltaStaysRSS.
//
// A warm worker's RSS double-counts the shared opcache segment and shared
// libraries: summed across a pool, that shared memory is charged once per worker.
// So the pool is sized from PSS when the kernel reports it. The child delta
// (subtree minus the worker) deliberately stays on RSS, because it is a
// difference of two RSS reads — subtracting a PSS worker would credit the shared
// pages the worker no longer carries to its children and over-reserve for them.
func TestSizingPrefersPSSButChildDeltaStaysRSS(t *testing.T) {
	s := New()

	s.Learn(Observation{
		Pool: "app",
		At:   time.Now(),
		Workers: []WorkerSample{
			// 200MiB RSS, but 120MiB once the shared pages are divided among
			// sharers; shelled out to a 600MiB child (subtree 800MiB).
			{RSSBytes: 200 * mb, PSSBytes: 120 * mb, SubtreeRSSBytes: 800 * mb, Requests: 500},
		},
	}, Options{})

	ps := s.Pools["app"]
	if ps.HighWaterBytes != 120*mb {
		t.Errorf("sized on %dMiB, want 120MiB (PSS), not 200MiB (RSS)", ps.HighWaterBytes/mb)
	}
	if ps.TypicalPeakBytes != 120*mb {
		t.Errorf("typical peak %dMiB, want 120MiB (PSS)", ps.TypicalPeakBytes/mb)
	}
	// 800 - 200 = 600 (subtree minus RSS), NOT 800 - 120 = 680.
	if ps.ChildPerWorkerHighWaterBytes != 600*mb {
		t.Errorf("child cost %dMiB, want 600MiB (subtree minus RSS, not minus PSS)",
			ps.ChildPerWorkerHighWaterBytes/mb)
	}
}

// TestSizingFallsBackToRSSWithoutPSS: on a kernel too old for smaps_rollup, or
// without permission to read it, the sample carries PSSBytes == 0 and sizing must
// use RSS exactly as before.
func TestSizingFallsBackToRSSWithoutPSS(t *testing.T) {
	s := New()

	s.Learn(Observation{
		Pool: "app",
		At:   time.Now(),
		Workers: []WorkerSample{
			{RSSBytes: 150 * mb, PSSBytes: 0, SubtreeRSSBytes: 150 * mb, Requests: 500},
		},
	}, Options{})

	if got := s.Pools["app"].HighWaterBytes; got != 150*mb {
		t.Errorf("fallback sized on %dMiB, want 150MiB (RSS)", got/mb)
	}
}
