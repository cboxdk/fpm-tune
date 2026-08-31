package state

import "testing"

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
