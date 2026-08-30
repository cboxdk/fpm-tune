package plan

import (
	"strings"
	"testing"
	"time"

	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
)

// TestWorkloadByNameResolvesAliases: the canonical names and the words an
// operator reaches for first both land on the right class, and an unknown name
// falls back while reporting that it was not recognised.
func TestWorkloadByNameResolvesAliases(t *testing.T) {
	cases := map[string]string{
		"web": "web", "api": "web", "simple": "web",
		"bursty":           "bursty",
		"subprocess-heavy": "subprocess-heavy",
		"subprocess":       "subprocess-heavy",
		"media":            "subprocess-heavy",
		"children":         "subprocess-heavy",
		"  MEDIA  ":        "subprocess-heavy",
	}
	for name, want := range cases {
		w, ok := WorkloadByName(name, WorkloadWeb)
		if !ok {
			t.Errorf("%q was not recognised", name)
		}
		if w.Name != want {
			t.Errorf("%q resolved to %q, want %q", name, w.Name, want)
		}
	}
	if w, ok := WorkloadByName("medai", WorkloadBursty); ok || w.Name != "bursty" {
		t.Errorf("a typo resolved to %q ok=%v, want the fallback and ok=false", w.Name, ok)
	}
}

// TestChildCostPerWorker: the per-worker child cost is the workload's amortised
// guess until a larger cost is measured. Amortised means a bursty pool is NOT
// charged a full child on every worker.
func TestChildCostPerWorker(t *testing.T) {
	cases := []struct {
		w        Workload
		measured int64
		want     int64
	}{
		{WorkloadWeb, 0, 0},
		{WorkloadBursty, 0, 64 * mb},                  // 256MiB × 0.25
		{WorkloadSubprocessHeavy, 0, 512 * mb},        // 512MiB × 1.0
		{WorkloadWeb, 300 * mb, 300 * mb},             // measured on a web pool wins
		{WorkloadBursty, 500 * mb, 500 * mb},          // measured beats the 64MiB guess
		{WorkloadSubprocessHeavy, 100 * mb, 512 * mb}, // guess beats a small measurement
	}
	for _, c := range cases {
		if got := childCostPerWorker(c.w, c.measured); got != c.want {
			t.Errorf("childCostPerWorker(%s, %s) = %s, want %s",
				c.w.Name, budget.HumanBytes(c.measured), budget.HumanBytes(got), budget.HumanBytes(c.want))
		}
	}
}

// TestSubprocessHeavyGetsFewerWorkers: a subprocess-heavy pool costs more per
// worker (own + child), so a budget-bound host gives it fewer workers than the
// same pool declared web — the child memory shows up as fewer workers, never as
// a refusal to plan.
func TestSubprocessHeavyGetsFewerWorkers(t *testing.T) {
	view := observe.PoolView{
		Name: "media", ProcessManager: "dynamic",
		CurrentMaxChildren: 40, MaxChildrenKnown: true, ObservedPeak: 40,
	}
	limits := budget.Limits{MemoryBytes: 8 * gb, CPUs: 8, Source: budget.SourceMemInfo}

	web := mustBuild(t, Input{Limits: limits, Workload: WorkloadWeb, State: state.New(), Views: []observe.PoolView{view}})
	heavy := mustBuild(t, Input{Limits: limits, Workload: WorkloadSubprocessHeavy, State: state.New(), Views: []observe.PoolView{view}})

	if heavy.Plan.Pools[0].MaxChildren >= web.Plan.Pools[0].MaxChildren {
		t.Errorf("subprocess-heavy got %d workers, not fewer than web's %d",
			heavy.Plan.Pools[0].MaxChildren, web.Plan.Pools[0].MaxChildren)
	}
	if heavy.ChildReserve <= 0 {
		t.Error("no child memory was reported as committed for a subprocess-heavy pool")
	}
}

// TestPlanNeverCommitsMoreThanBudget is the safety property, restated for the
// per-worker model: because the child cost is folded into each worker's cost,
// the allocator's own "allocated <= allocatable" invariant now covers children
// too — for EVERY workload, whatever redistribution the allocator did. This is
// what the old host-wide reserve could not guarantee (a pool could be handed
// more workers whose children were unreserved).
func TestPlanNeverCommitsMoreThanBudget(t *testing.T) {
	views := []observe.PoolView{
		{Name: "web1", ProcessManager: "dynamic", CurrentMaxChildren: 30, MaxChildrenKnown: true, ObservedPeak: 30},
		{Name: "media", Workload: "subprocess-heavy", ProcessManager: "dynamic", CurrentMaxChildren: 30, MaxChildrenKnown: true, ObservedPeak: 30},
		{Name: "pdf", Workload: "bursty", ProcessManager: "dynamic", CurrentMaxChildren: 20, MaxChildrenKnown: true, ObservedPeak: 20},
	}
	for _, total := range []int64{2 * gb, 4 * gb, 8 * gb, 16 * gb} {
		res := mustBuild(t, Input{
			Limits:   budget.Limits{MemoryBytes: total, CPUs: 8, Source: budget.SourceMemInfo},
			Workload: WorkloadWeb, State: state.New(), Views: views,
		})
		allocatable := total - res.Reserve
		if res.Plan.AllocatedBytes > allocatable {
			t.Errorf("at %s budget, committed %s to workers-plus-children, over the %s allocatable",
				budget.HumanBytes(total), budget.HumanBytes(res.Plan.AllocatedBytes), budget.HumanBytes(allocatable))
		}
	}
}

// TestMeasuredChildDrivesSizing: a pool marked web but observed spawning a child
// is sized for it — the measured per-worker child, already averaged over how
// many workers ran one at once, folds into each worker's cost.
func TestMeasuredChildDrivesSizing(t *testing.T) {
	st := state.New()
	base := time.Now()
	for i := 0; i < 6; i++ {
		// Eight workers, two of them each carrying a 600MiB child in this scrape:
		// child total 1200MiB over 8 workers = 150MiB per worker.
		workers := make([]state.WorkerSample, 8)
		for w := range workers {
			workers[w] = state.WorkerSample{RSSBytes: 90 * mb, SubtreeRSSBytes: 90 * mb, Requests: 500}
		}
		workers[0].SubtreeRSSBytes = 690 * mb
		workers[1].SubtreeRSSBytes = 690 * mb
		st.Learn(state.Observation{
			Pool: "app", At: base.Add(time.Duration(i) * 2 * time.Minute), ActiveNow: 8,
			Accepted: base.Unix() * 100, Workers: workers,
		}, state.Options{})
	}

	if got := measuredChildPerWorker(st, observe.PoolView{Name: "app"}); got != 150*mb {
		t.Fatalf("measured child per worker = %s, want 150MiB (1200MiB over 8 workers)", budget.HumanBytes(got))
	}

	web := mustBuild(t, Input{
		Limits:   budget.Limits{MemoryBytes: 8 * gb, CPUs: 8, Source: budget.SourceMemInfo},
		Workload: WorkloadWeb, State: state.New(),
		Views: []observe.PoolView{{Name: "app", CurrentMaxChildren: 30, MaxChildrenKnown: true, ObservedPeak: 8, ProcessManager: "dynamic"}},
	})
	measured := mustBuild(t, Input{
		Limits:   budget.Limits{MemoryBytes: 8 * gb, CPUs: 8, Source: budget.SourceMemInfo},
		Workload: WorkloadWeb, State: st,
		Views: []observe.PoolView{{Name: "app", CurrentMaxChildren: 30, MaxChildrenKnown: true, ObservedPeak: 8, ProcessManager: "dynamic"}},
	})
	if measured.ChildReserve <= web.ChildReserve {
		t.Errorf("a web pool observed spawning children committed no more to children (%s) than one that never did (%s)",
			budget.HumanBytes(measured.ChildReserve), budget.HumanBytes(web.ChildReserve))
	}
}

// TestPerPoolMarkerOverridesGlobal: on a mixed host the global default applies to
// unmarked pools, and a pool carrying its own marker is sized by that instead.
func TestPerPoolMarkerOverridesGlobal(t *testing.T) {
	res := mustBuild(t, Input{
		Limits:   budget.Limits{MemoryBytes: 16 * gb, CPUs: 8, Source: budget.SourceMemInfo},
		Workload: WorkloadWeb,
		State:    state.New(),
		Views: []observe.PoolView{
			{Name: "site", ProcessManager: "dynamic", CurrentMaxChildren: 8, MaxChildrenKnown: true, ObservedPeak: 8},
			{Name: "transcode", Workload: "subprocess-heavy", ProcessManager: "dynamic", CurrentMaxChildren: 8, MaxChildrenKnown: true, ObservedPeak: 8},
		},
	})
	// Only the transcode pool commits to children.
	if res.ChildReserve <= 0 {
		t.Error("the subprocess-heavy pool committed nothing to children")
	}
	// The web pool's workers were not inflated: its own cost is unchanged.
	for _, pp := range res.Plan.Pools {
		if pp.Name == "site" && pp.WorkerBytes > 64*mb {
			t.Errorf("the web pool's per-worker cost was inflated to %s; a child cost leaked onto it",
				budget.HumanBytes(pp.WorkerBytes))
		}
	}
}

// TestUnknownPerPoolMarkerWarns: a typo in a pool's own env[FPM_TUNE_WORKLOAD]
// must not silently reserve nothing — it surfaces as a plan warning naming the
// pool, so the operator sees the marker did not take.
func TestUnknownPerPoolMarkerWarns(t *testing.T) {
	res := mustBuild(t, Input{
		Limits:   budget.Limits{MemoryBytes: 8 * gb, CPUs: 8, Source: budget.SourceMemInfo},
		Workload: WorkloadWeb,
		State:    state.New(),
		Views: []observe.PoolView{
			{Name: "transcode", Workload: "subprocess_heavy", ProcessManager: "dynamic", CurrentMaxChildren: 8, MaxChildrenKnown: true, ObservedPeak: 8},
		},
	})
	found := false
	for _, w := range res.Plan.Warnings {
		if strings.Contains(w, "transcode") && strings.Contains(w, "FPM_TUNE_WORKLOAD") {
			found = true
		}
	}
	if !found {
		t.Errorf("a typo'd per-pool marker produced no warning; it silently reserved nothing.\nwarnings: %v", res.Plan.Warnings)
	}
}

func mustBuild(t *testing.T, in Input) Result {
	t.Helper()
	res, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	return res
}
