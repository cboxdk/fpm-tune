package state

import (
	"testing"
	"time"
)

// The aggregate share must classify a fast CPU-heavy pool that the
// per-request histogram never sees: sub-50ms requests inform nothing, and a
// saturated pool has no idle worker to sample. Tick deltas see everything.
func TestAggregateCPUShare_FastCPUHeavyPool(t *testing.T) {
	opts := Options{}.Defaults()
	ps := &PoolState{Pool: "www"}

	// Six intervals, each: 2 busy workers spending ~1.9 cores between them -
	// a cpu-bound pool (share ~0.95) made of 3ms requests.
	for i := 0; i < 6; i++ {
		ps.learnAggregateCPU(1.9, 2)
	}
	share, ok := ps.AggregateCPUShare(opts)
	if !ok {
		t.Fatalf("aggregate not trusted after 6 active rounds (rounds=%d busy=%.1f)", ps.AggCPURounds, ps.AggCPUBusy)
	}
	if share < 0.5 {
		t.Fatalf("cpu-bound pool read as share %.2f; want >= 0.50", share)
	}

	// And the classification the plan will draw from it.
	if got := share; got < 0.5 {
		t.Fatalf("shape threshold not reached: %v", got)
	}
}

// Idle intervals must not drag the share toward zero, and too few rounds
// must not be trusted.
func TestAggregateCPUShare_Guards(t *testing.T) {
	opts := Options{}.Defaults()
	ps := &PoolState{Pool: "www"}

	for i := 0; i < 3; i++ {
		ps.learnAggregateCPU(1.9, 2)
	}
	if _, ok := ps.AggregateCPUShare(opts); ok {
		t.Fatal("trusted after only 3 rounds; MinAggCPURounds default is 5")
	}

	before := ps.AggCPUCores
	ps.learnAggregateCPU(0.0, 0) // idle interval: below aggMinCores
	if ps.AggCPUCores != before {
		t.Fatal("idle interval changed the aggregate")
	}

	// An IO-shaped pool: 6 busy workers holding ~0.4 cores between them.
	io := &PoolState{Pool: "io"}
	for i := 0; i < 6; i++ {
		io.learnAggregateCPU(0.4, 6)
	}
	share, ok := io.AggregateCPUShare(opts)
	if !ok || share > 0.20 {
		t.Fatalf("io-shaped pool: share=%.3f ok=%v; want trusted and < 0.20", share, ok)
	}
	_ = time.Now
}
