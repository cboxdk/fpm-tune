package state

import (
	"testing"
	"time"
)

// TestSizingBytesAtCatchesADeployInOneScrape: a percentile of a decaying histogram
// reacts to a real increase only slowly, so the default basis is floored by the most
// recent round's peak. A deploy that makes every worker heavier is caught in one
// scrape, before the histogram has shifted, so the pool is never under-sized through
// the deploy window.
func TestSizingBytesAtCatchesADeployInOneScrape(t *testing.T) {
	ps := &PoolState{}
	for range 200 {
		ps.observeRSS(100 << 20) // long steady at 100MiB
	}
	// The deploy: every worker is now 220MiB. The histogram still reads 100; the
	// most recent round's peak is already 220.
	ps.LastPeakBytes = 220 << 20

	if got := ps.SizingBytesAt(0.95, 0.10); got != 220<<20 {
		t.Errorf("SizingBytesAt = %dMiB, want the deploy peak 220MiB (the histogram lags, the floor does not)", got>>20)
	}
}

// TestSizingBytesAtDoesNotHoldARareMonster: a single monster request is the very top
// of the distribution, so it does not move p95, and once it is over the most recent
// round's peak has dropped back. The pool is not sized forever on its worst minute
// the way the pure peak-follower is.
func TestSizingBytesAtDoesNotHoldARareMonster(t *testing.T) {
	ps := &PoolState{}
	for range 200 {
		ps.observeRSS(100 << 20)
	}
	ps.observeRSS(400 << 20)     // one monster in the histogram
	ps.LastPeakBytes = 100 << 20 // ...but the most recent round is back to normal

	if got := ps.SizingBytesAt(0.95, 0.10); got > 130<<20 {
		t.Errorf("SizingBytesAt = %dMiB, want ~p95 (~110MiB); a rare monster must not inflate the steady size", got>>20)
	}
}

// TestHybridReflectsAMonsterThenReleasesIt drives the learner round by round: the
// monster is reflected the scrape it happens (the pool really needs that memory
// then), but is gone the next scrape rather than held.
func TestHybridReflectsAMonsterThenReleasesIt(t *testing.T) {
	s := New()
	now := time.Now()
	steady := func(i int) Observation {
		return Observation{Pool: "app", At: now.Add(time.Duration(i) * time.Minute), ActiveNow: 2,
			Workers: []WorkerSample{
				{RSSBytes: 100 << 20, Requests: 500},
				{RSSBytes: 100 << 20, Requests: 500},
			}}
	}
	for i := range 30 {
		s.Learn(steady(i), Options{})
	}
	base := s.Pools["app"].SizingBytesAt(0.95, 0.10)

	s.Learn(Observation{Pool: "app", At: now.Add(31 * time.Minute), ActiveNow: 2,
		Workers: []WorkerSample{
			{RSSBytes: 400 << 20, Requests: 500}, // a monster this scrape
			{RSSBytes: 100 << 20, Requests: 500},
		}}, Options{})
	during := s.Pools["app"].SizingBytesAt(0.95, 0.10)

	s.Learn(steady(32), Options{})
	after := s.Pools["app"].SizingBytesAt(0.95, 0.10)

	if during <= base {
		t.Errorf("during the monster the size (%dMiB) did not rise above the steady base (%dMiB)", during>>20, base>>20)
	}
	if after > base+(20<<20) {
		t.Errorf("after the monster the size (%dMiB) stayed elevated; it must release back toward p95 (~%dMiB)", after>>20, base>>20)
	}
}

// TestSizingBytesAtIsBelowThePeakForATightDistribution: the whole point of the
// percentile basis is to size below the peak-follower when a pool's workers sit at
// a stable size with only rare spikes.
func TestSizingBytesAtIsBelowThePeakForATightDistribution(t *testing.T) {
	ps := &PoolState{}
	for range 200 {
		ps.observeRSS(200 << 20) // steady 200MiB
	}
	ps.observeRSS(256 << 20) // a rare spike
	ps.TypicalPeakBytes = 256 << 20

	peak := ps.SizingBytes()
	p95 := ps.SizingBytesAt(0.95, 0.10)

	if p95 >= peak {
		t.Errorf("p95 sizing %d is not below the peak-follower %d on a tight distribution", p95, peak)
	}
	if want := int64(float64(ps.Percentile(0.95)) * 1.10); p95 != want {
		t.Errorf("SizingBytesAt = %d, want p95 x 1.10 = %d", p95, want)
	}
}

// TestSizingBytesAtFallsBackWithNoDistribution: a cold pool with no reading yet must
// not be sized at zero — the peak-follower stands in until the histogram fills.
func TestSizingBytesAtFallsBackWithNoDistribution(t *testing.T) {
	ps := &PoolState{TypicalPeakBytes: 100 << 20}
	if got := ps.SizingBytesAt(0.95, 0.10); got != ps.SizingBytes() {
		t.Errorf("with no distribution SizingBytesAt = %d, want the peak-follower %d", got, ps.SizingBytes())
	}
}
