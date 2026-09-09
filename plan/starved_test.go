package plan

import (
	"testing"
	"time"

	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
)

// A pool that queues while the host's CPU is full must be HELD, not grown:
// another worker finds no core to run on. serve's noteStarved has diagnosed
// exactly this for a while - the plan just kept growing anyway (measured cost
// on a 2-CPU container: 8 -> 21 workers, -16% throughput).
func TestBuild_StarvedPoolIsHeldNotGrown(t *testing.T) {
	mk := func(hostBusy float64, known bool) Input {
		return Input{
			At: time.Now(),
			Limits: budget.Limits{
				MemoryBytes:   1 << 30,
				CPUs:          2,
				CPUMillicores: 2000,
				Source:        budget.SourceCgroupV2,
			},
			Views: []observe.PoolView{{
				Name:               "www",
				CurrentMaxChildren: 8,
				MaxChildrenKnown:   true,
				ObservedPeak:       8,
				QueueDepth:         55,
				ActiveNow:          8,
				Workers: []state.WorkerSample{
					{PID: 1, PSSBytes: 30 << 20, Requests: 100},
					{PID: 2, PSSBytes: 30 << 20, Requests: 100},
					{PID: 3, PSSBytes: 30 << 20, Requests: 100},
					{PID: 4, PSSBytes: 30 << 20, Requests: 100},
				},
			}},
			State:         state.New(),
			HostBusy:      hostBusy,
			HostBusyKnown: known,
		}
	}

	// CPU full and known: the ceiling-hit is suppressed.
	res, err := Build(mk(1.0, true))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.StarvedHeld) != 1 || res.StarvedHeld[0] != "www" {
		t.Fatalf("expected www in StarvedHeld, got %v", res.StarvedHeld)
	}
	for _, p := range res.Plan.Pools {
		if p.Name == "www" && p.MaxChildren > 8 {
			t.Fatalf("starved pool grew to %d; must be held at 8", p.MaxChildren)
		}
	}

	// Host CPU idle: the same queue is real demand and growth resumes.
	res, err = Build(mk(0.10, true))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.StarvedHeld) != 0 {
		t.Fatalf("idle host must not suppress growth, got StarvedHeld=%v", res.StarvedHeld)
	}

	// Busy ratio unknown: behave exactly as before (no suppression).
	res, err = Build(mk(1.0, false))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.StarvedHeld) != 0 {
		t.Fatalf("unknown host busy must not suppress growth, got %v", res.StarvedHeld)
	}
}

// The ceiling-hit that arrives through the max_children_reached COUNTER -
// queue drained between scrapes, QueueDepth back at 0 - must be held on a
// saturated host exactly like the queued case. Gating on the queue snapshot
// let these hits keep growing the pool (measured: 8 -> 21 workers under a
// CPU-saturated microload with the queue reading empty at scrape time).
func TestBuild_StarvedCounterPathIsHeldToo(t *testing.T) {
	in := Input{
		At: time.Now(),
		Limits: budget.Limits{
			MemoryBytes:   1 << 30,
			CPUs:          2,
			CPUMillicores: 2000,
			Source:        budget.SourceCgroupV2,
		},
		Views: []observe.PoolView{{
			Name:               "www",
			CurrentMaxChildren: 8,
			MaxChildrenKnown:   true,
			ObservedPeak:       8,
			QueueDepth:         0,
			MaxChildrenReached: 12,
			ActiveNow:          8,
			Workers: []state.WorkerSample{
				{PID: 1, PSSBytes: 30 << 20, Requests: 100},
				{PID: 2, PSSBytes: 30 << 20, Requests: 100},
			},
		}},
		State:         state.New(),
		HostBusy:      1.0,
		HostBusyKnown: true,
	}
	res, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.StarvedHeld) != 1 {
		t.Fatalf("counter-path ceiling hit on a saturated host must be held, got StarvedHeld=%v", res.StarvedHeld)
	}
	for _, p := range res.Plan.Pools {
		if p.Name == "www" && p.MaxChildren > 8 {
			t.Fatalf("counter-path growth not suppressed: grew to %d", p.MaxChildren)
		}
	}
}
