package plan

import (
	"testing"
	"time"

	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
)

// A starved pool that is OVERSIZED - more workers than the cores they
// saturate can ever run in parallel - must be pulled DOWN to its measured
// parallelism, not merely held where it is. The share cannot say this:
// under saturation every worker reads "busy" while queuing for a core, so
// the share's denominator absorbs the oversubscription factor and a
// share-based fill circles back to whatever the current size happens to be.
// The kernel's tick deltas cannot be inflated by workers that only wait -
// the cores the pool actually drives ARE its fill (#14 phase 2).
func TestBuild_StarvedOversizedPoolCutToMeasuredCores(t *testing.T) {
	primed := func(aggRounds int64) *state.State {
		st := state.New()
		base := time.Now().Add(-2 * time.Hour)
		for i := 0; i < 30; i++ {
			st.Learn(busy("www", 30*mb, base.Add(time.Duration(i)*2*time.Minute)), state.Options{})
		}
		ps := st.Pools["www"]
		if !ps.Trusted(state.Options{}.Defaults()) {
			t.Fatal("setup: the pool was meant to be trusted")
		}
		// The pool saturates ~1.9 of the box's 2 cores with 15 workers busy on
		// average - a pure-CPU flood on a comically oversized pool.
		ps.AggCPUCores, ps.AggCPUBusy, ps.AggCPURounds = 1.9, 15, aggRounds
		return st
	}

	mkInput := func(st *state.State) Input {
		return Input{
			At: time.Now(),
			Limits: budget.Limits{
				MemoryBytes:   2 << 30,
				CPUs:          2,
				CPUMillicores: 2000,
				Source:        budget.SourceCgroupV2,
			},
			Views: []observe.PoolView{{
				Name:               "www",
				ProcessManager:     "dynamic",
				CurrentMaxChildren: 16,
				MaxChildrenKnown:   true,
				ObservedPeak:       16,
				QueueDepth:         55,
				ActiveNow:          16,
				Workers: []state.WorkerSample{
					{PID: 1, PSSBytes: 30 << 20, Requests: 900},
					{PID: 2, PSSBytes: 30 << 20, Requests: 900},
					{PID: 3, PSSBytes: 30 << 20, Requests: 900},
					{PID: 4, PSSBytes: 30 << 20, Requests: 900},
				},
			}},
			State:         st,
			CPUCeiling:    true,
			HostBusy:      0.98,
			HostBusyKnown: true,
		}
	}

	// Aggregate trusted: ceiling comes from measured cores x headroom -
	// ceil(1.9) = 2 cores, default headroom 2.0 -> 4 - NOT from the current
	// size of 16.
	res, err := Build(mkInput(primed(10)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.StarvedHeld) != 1 || res.StarvedHeld[0] != "www" {
		t.Fatalf("expected www starved-held, got %v", res.StarvedHeld)
	}
	for _, p := range res.Plan.Pools {
		if p.Name != "www" {
			continue
		}
		if p.MaxChildren > 4 {
			t.Errorf("MaxChildren = %d; an oversized starved pool must converge to its measured ceiling of 4 (ceil(1.9 cores) x headroom 2.0), not stay at 16", p.MaxChildren)
		}
		if !p.CPUBound {
			t.Error("the pool should be marked CPU-bound: its ceiling, not memory, decided the size")
		}
	}

	// Aggregate NOT yet trusted (too few rounds): phase-1 behavior stands -
	// the pool is held at its current size, never cut on an unproven signal.
	res, err = Build(mkInput(primed(1)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, p := range res.Plan.Pools {
		if p.Name == "www" && p.MaxChildren != 16 {
			t.Errorf("MaxChildren = %d; without a trusted aggregate the starved pool is HELD at 16, never cut", p.MaxChildren)
		}
	}

	// Without --cpu the cut does not bind: a cap below the configured
	// ceiling IS a cut, and cuts are opt-in. The pool is held, phase-1 style.
	noFlag := mkInput(primed(10))
	noFlag.CPUCeiling = false
	res, err = Build(noFlag)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, p := range res.Plan.Pools {
		if p.Name == "www" && p.MaxChildren != 16 {
			t.Errorf("MaxChildren = %d; without --cpu the starved pool is HELD at 16", p.MaxChildren)
		}
	}

	// The report tells the same story as the allocator.
	res, err = Build(mkInput(primed(10)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, c := range res.CPU {
		if c.Name != "www" {
			continue
		}
		if !c.SaturationMeasured {
			t.Error("report row should be marked SaturationMeasured")
		}
		if c.FillWorkers != 2 {
			t.Errorf("report FillWorkers = %d; measured cores say 2", c.FillWorkers)
		}
	}
}

// cpuCeilingFor must fall back to the aggregate share when per-request
// sampling never fired (sub-50ms requests): before this, only the REPORT
// used the aggregate and the allocator's ceiling stayed 0 for exactly the
// workloads --cpu exists for.
func TestCPUCeilingFor_AggregateFallback(t *testing.T) {
	st := state.New()
	base := time.Now().Add(-2 * time.Hour)
	for i := 0; i < 30; i++ {
		st.Learn(busy("www", 30*mb, base.Add(time.Duration(i)*2*time.Minute)), state.Options{})
	}
	ps := st.Pools["www"]
	opts := state.Options{}.Defaults()
	if ps.CPUShapeKnown(opts) {
		t.Fatal("setup: no per-request CPU readings were meant to exist")
	}

	// No aggregate either: no ceiling, exactly as before.
	if got := cpuCeilingFor(ps, opts, 2000, 2.0); got != 0 {
		t.Fatalf("without any shape signal the ceiling must stay 0, got %d", got)
	}

	// Aggregate primed: a CPU-heavy share yields a real ceiling.
	ps.AggCPUCores, ps.AggCPUBusy, ps.AggCPURounds = 1.9, 2, 10
	if got := cpuCeilingFor(ps, opts, 2000, 2.0); got <= 0 {
		t.Fatalf("aggregate share should produce a ceiling, got %d", got)
	}
}
