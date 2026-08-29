package serve

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
	"github.com/cboxdk/phpfpm"
)

const mb = 1 << 20

// TestTheCeilingSignalFiresAcrossRounds.
//
// The loop must learn, then plan, then record counters — in that order. The
// ceiling counter is a running total since the master started, so what says a
// pool is queueing NOW is the difference between this round's value and the last
// one. Storing this round's value before the plan reads it makes the difference
// zero, every round, for ever: the growth signal never fired once in any round
// between the day it was written and the day that was noticed.
//
// It was asserted by a comment. The unit test named for the signal calls the two
// functions in its own body, in the right order, so it tests the comparison and
// not the loop — moving the call back to where the bug was left every package
// green.
func TestTheCeilingSignalFiresAcrossRounds(t *testing.T) {
	targets := []phpfpm.Target{{Name: "shop", MaxChildren: 10, ProcessManager: "dynamic"}}

	// Two rounds of the same pool, with the ceiling counter climbing between
	// them the way a pool that keeps hitting pm.max_children makes it climb.
	var round int
	view := func(reached int64) observe.PoolView {
		return observe.PoolView{
			Name: "shop", ProcessManager: "dynamic",
			CurrentMaxChildren: 10, MaxChildrenKnown: true,
			ActiveNow: 10, ObservedPeak: 10,
			MaxChildrenReached: reached,
			Accepted:           100_000 * int64(round+1),
			Workers: []state.WorkerSample{
				{RSSBytes: 40 * mb, Requests: 500},
				{RSSBytes: 42 * mb, Requests: 500},
			},
		}
	}

	loop, err := New(Config{
		StatePath:      filepath.Join(t.TempDir(), "state.json"),
		MetricsAddr:    "",
		MemoryOverride: 4096 * mb,
		Discover: func(context.Context) ([]phpfpm.Target, error) {
			return targets, nil
		},
		Sample: func(context.Context, []phpfpm.Target) []observe.PoolView {
			round++

			return []observe.PoolView{view(int64(round) * 5)}
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { loop.Close() })

	loop.round(context.Background())

	// After the first round the counter has nothing to be compared against, so
	// the state has only stored it.
	if got := loop.State().Pools["shop"].LastMaxChildrenReached; got != 5 {
		t.Fatalf("after round 1 the stored counter is %d, want 5", got)
	}

	loop.round(context.Background())

	// The plan built in round two must have SEEN 10 against a stored 5 — and
	// then stored 10 for round three. If the counter were recorded before the
	// plan, the plan would compare 10 against 10.
	if got := loop.State().Pools["shop"].LastMaxChildrenReached; got != 10 {
		t.Errorf("after round 2 the stored counter is %d, want 10", got)
	}
	// The plan built in round two is what the metrics carry. A pool whose
	// ceiling counter rose between the rounds is saturated, and a saturated pool
	// with budget to spare is grown past the 10 it is configured for. If the
	// counter had been recorded before the plan, the plan would have compared 10
	// against 10, seen no movement, and left it where it was.
	// Measured against 12, not against 10.
	//
	// Without the signal the pool still grows — to its observed peak plus the
	// headroom factor, which is 12 — so "more than the 10 it had" passes either
	// way and proves nothing. The saturation branch takes it past that, and the
	// exact number is bounded by this host's core count, so the assertion is the
	// part that does not depend on the machine running it.
	if got := recommended(t, loop, "shop"); got <= 12 {
		t.Errorf("a pool whose ceiling counter rose from 5 to 10 between rounds was "+
			"planned at %v workers. 12 is what headroom alone gives, which means the "+
			"plan saw no movement in the counter — what recording it before the plan "+
			"does is compare the round's own reading against itself", got)
	}
}

// recommended reads a pool's planned worker count back off the metrics, which
// is where this loop's output goes.
func recommended(t *testing.T, l *Loop, pool string) float64 {
	t.Helper()

	families, err := l.Metrics().Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "fpm_tune_pool_workers_recommended" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, lab := range m.GetLabel() {
				if lab.GetName() == "pool" && lab.GetValue() == pool {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("no recommendation published for %q", pool)

	return 0
}
