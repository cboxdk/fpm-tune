package plan

import (
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
		"  MEDIA  ":        "subprocess-heavy", // trimmed and folded
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

// TestSubprocessHeavyReservesFromTheFirstRun is the whole point of workloads: a
// pool declared subprocess-heavy holds memory back for its children before a
// single one has been observed, so it is not sized as if the ffmpeg were free on
// the run where an OOM would otherwise arrive.
func TestSubprocessHeavyReservesFromTheFirstRun(t *testing.T) {
	res, err := Build(Input{
		Limits:   budget.Limits{MemoryBytes: 8 * gb, CPUs: 4, Source: budget.SourceMemInfo},
		Workload: WorkloadSubprocessHeavy,
		State:    state.New(),
		Views: []observe.PoolView{{
			Name: "media", ProcessManager: "dynamic",
			CurrentMaxChildren: 8, MaxChildrenKnown: true, ObservedPeak: 8,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 512MiB per worker (the class's bootstrap guess) × 8 concurrent workers.
	want := int64(512) * mb * 8
	if res.ChildReserve != want {
		t.Errorf("ChildReserve = %s, want %s reserved for children on the first run",
			budget.HumanBytes(res.ChildReserve), budget.HumanBytes(want))
	}
}

// TestWebPoolReservesNothingForChildren: a plain web pool spawns nothing, so it
// must not have memory held back from its workers for children that never exist.
func TestWebPoolReservesNothingForChildren(t *testing.T) {
	res, err := Build(Input{
		Limits:   budget.Limits{MemoryBytes: 8 * gb, CPUs: 4, Source: budget.SourceMemInfo},
		Workload: WorkloadWeb,
		State:    state.New(),
		Views: []observe.PoolView{{
			Name: "app", ProcessManager: "dynamic",
			CurrentMaxChildren: 8, MaxChildrenKnown: true, ObservedPeak: 8,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ChildReserve != 0 {
		t.Errorf("ChildReserve = %s for a web pool, want 0", budget.HumanBytes(res.ChildReserve))
	}
}

// TestMeasurementOverridesAWebDeclaration: a pool an operator marked web, but
// which is observed spawning a 600MiB child, is reserved for anyway — the
// measurement beats the declaration, because being wrong about "web" the unsafe
// way is an OOM.
func TestMeasurementOverridesAWebDeclaration(t *testing.T) {
	st := state.New()
	base := time.Now()
	for i := 0; i < 6; i++ {
		obs := state.Observation{
			Pool: "app", At: base.Add(time.Duration(i) * 2 * time.Minute), ActiveNow: 4,
			Accepted: base.Unix() * 100,
			Workers: []state.WorkerSample{
				{RSSBytes: 90 * mb, SubtreeRSSBytes: 690 * mb, Requests: 500},
				{RSSBytes: 90 * mb, SubtreeRSSBytes: 690 * mb, Requests: 500},
			},
		}
		st.Learn(obs, state.Options{})
	}

	res, err := Build(Input{
		Limits:   budget.Limits{MemoryBytes: 8 * gb, CPUs: 4, Source: budget.SourceMemInfo},
		Workload: WorkloadWeb, // declared web…
		State:    st,
		Views: []observe.PoolView{{
			Name: "app", ProcessManager: "dynamic",
			CurrentMaxChildren: 4, MaxChildrenKnown: true, ObservedPeak: 4,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// …but 600MiB of children were measured, so at least one is reserved for.
	if res.ChildReserve < 600*mb {
		t.Errorf("ChildReserve = %s, want at least the measured 600MiB — a wrong 'web' "+
			"marker must not hide observed children", budget.HumanBytes(res.ChildReserve))
	}
}

// TestChildReserveOnlyEverReducesWorkers is the safety property the whole design
// rests on: reserving for children is additive, so a plan that accounts for them
// can only allocate LESS to workers than one that does not — never more. If this
// ever failed, turning the feature on could overcommit a host it was not
// overcommitting before.
func TestChildReserveOnlyEverReducesWorkers(t *testing.T) {
	view := observe.PoolView{
		Name: "media", ProcessManager: "dynamic",
		CurrentMaxChildren: 8, MaxChildrenKnown: true, ObservedPeak: 8,
	}
	limits := budget.Limits{MemoryBytes: 8 * gb, CPUs: 4, Source: budget.SourceMemInfo}

	web, err := Build(Input{Limits: limits, Workload: WorkloadWeb, State: state.New(), Views: []observe.PoolView{view}})
	if err != nil {
		t.Fatal(err)
	}
	heavy, err := Build(Input{Limits: limits, Workload: WorkloadSubprocessHeavy, State: state.New(), Views: []observe.PoolView{view}})
	if err != nil {
		t.Fatal(err)
	}

	// Never more workers than the un-reserved plan.
	if heavy.Plan.AllocatedBytes > web.Plan.AllocatedBytes {
		t.Errorf("reserving for children allocated MORE to workers (%s) than not reserving (%s)",
			budget.HumanBytes(heavy.Plan.AllocatedBytes), budget.HumanBytes(web.Plan.AllocatedBytes))
	}

	// And the reserve is really taken out of the budget: this pool is small
	// enough that both plans fit the same workers, so the child reserve comes out
	// of free memory instead — exactly childReserve of it. If the reserve were
	// not being subtracted from the budget at all, the two would have identical
	// free memory, which is the mutation this catches.
	if heavy.ChildReserve <= 0 {
		t.Fatal("the subprocess-heavy plan reserved nothing for children")
	}
	if gap := web.Plan.FreeBytes - heavy.Plan.FreeBytes; gap != heavy.ChildReserve {
		t.Errorf("free memory fell by %s between the plans, but the child reserve was %s — "+
			"the reserve is not being held back from the budget",
			budget.HumanBytes(gap), budget.HumanBytes(heavy.ChildReserve))
	}
}

// TestCgroupPeakDrivesTheChildReserve: where the cgroup peaked well above what
// the workers hold, that ground truth — which catches children a per-worker
// sample misses — sets the reserve, over the per-pool estimate.
func TestCgroupPeakDrivesTheChildReserve(t *testing.T) {
	st := state.New()
	base := time.Now()
	for i := 0; i < 6; i++ {
		st.Learn(busy("app", 90*mb, base.Add(time.Duration(i)*2*time.Minute)), state.Options{})
	}

	res, err := Build(Input{
		Limits:   budget.Limits{MemoryBytes: 8 * gb, CPUs: 4, Source: budget.SourceCgroupProcess},
		Workload: WorkloadWeb,
		State:    st,
		Views: []observe.PoolView{{
			Name: "app", ProcessManager: "dynamic",
			CurrentMaxChildren: 8, MaxChildrenKnown: true, ObservedPeak: 8,
		}},
		HasCgroupUsage: true,
		// Workers hold ~90MiB × 8 = 720MiB; the cgroup peaked at 3GiB, so ~2.3GiB
		// went to something other than the workers — children.
		CgroupUsage: budget.CgroupUsage{CurrentBytes: 2 * gb, PeakBytes: 3 * gb},
	})
	if err != nil {
		t.Fatal(err)
	}

	if res.ChildReserve < 2*gb {
		t.Errorf("ChildReserve = %s, want ~2.3GiB from the cgroup high-water beyond the workers",
			budget.HumanBytes(res.ChildReserve))
	}
}

// TestPerPoolMarkerOverridesGlobal: on a mixed host, the global default applies
// to pools that declare nothing, while a pool carrying its own marker is sized
// by that instead — a web pool beside a subprocess-heavy one, under a global
// default of web, reserves for children only on the pool that asked for it.
func TestPerPoolMarkerOverridesGlobal(t *testing.T) {
	res, err := Build(Input{
		Limits:   budget.Limits{MemoryBytes: 16 * gb, CPUs: 8, Source: budget.SourceMemInfo},
		Workload: WorkloadWeb, // global default: reserve nothing…
		State:    state.New(),
		Views: []observe.PoolView{
			{Name: "site", ProcessManager: "dynamic", CurrentMaxChildren: 8, MaxChildrenKnown: true, ObservedPeak: 8},
			// …but this pool declares itself subprocess-heavy.
			{Name: "transcode", Workload: "subprocess-heavy", ProcessManager: "dynamic", CurrentMaxChildren: 4, MaxChildrenKnown: true, ObservedPeak: 4},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Only the transcode pool's declaration reserves: 512MiB × 4 workers.
	want := int64(512) * mb * 4
	if res.ChildReserve != want {
		t.Errorf("ChildReserve = %s, want %s — only the pool that declared subprocess-heavy "+
			"should reserve, not the web pool beside it", budget.HumanBytes(res.ChildReserve), budget.HumanBytes(want))
	}
}

// TestUnmarkedPoolFollowsTheGlobalDefault: a pool with no marker of its own is
// sized by whatever global default the operator chose — so `--workload
// subprocess-heavy` on an all-media host works without annotating every pool.
func TestUnmarkedPoolFollowsTheGlobalDefault(t *testing.T) {
	res, err := Build(Input{
		Limits:   budget.Limits{MemoryBytes: 16 * gb, CPUs: 8, Source: budget.SourceMemInfo},
		Workload: WorkloadSubprocessHeavy, // global default applies to the unmarked pool
		State:    state.New(),
		Views: []observe.PoolView{
			{Name: "site", ProcessManager: "dynamic", CurrentMaxChildren: 6, MaxChildrenKnown: true, ObservedPeak: 6},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ChildReserve != int64(512)*mb*6 {
		t.Errorf("ChildReserve = %s, want the global subprocess-heavy default to apply to an unmarked pool",
			budget.HumanBytes(res.ChildReserve))
	}
}
