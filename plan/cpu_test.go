package plan

import (
	"strings"
	"testing"
	"time"

	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
)

// TestCPUShape is the classification, table-tested: a pool is called something
// only once it has enough readings, and the worker arithmetic is cores over
// the median share, rounded up.
func TestCPUShape(t *testing.T) {
	for _, tc := range []struct {
		name       string
		p50        float64
		samples    int64
		cores      int
		shape      string
		saturating int
	}{
		{"too few readings", 0.90, 19, 4, "", 0},
		{"just enough", 0.90, 20, 4, "cpu-bound", 5},
		{"WordPress uncached", 0.72, 1000, 4, "cpu-bound", 6},
		{"on the cpu-bound line", 0.50, 100, 2, "cpu-bound", 4},
		{"mixed", 0.35, 100, 4, "mixed", 12},
		{"on the mixed line", 0.20, 100, 4, "mixed", 20},
		{"API waiting on a database", 0.10, 100, 4, "i/o-bound", 40},
		{"below the first bucket", 0, 100, 4, "i/o-bound", 0},
		{"cores unknown", 0.72, 100, 0, "cpu-bound", 0},
	} {
		shape, saturating := cpuShape(tc.p50, tc.samples, tc.cores)
		if shape != tc.shape || saturating != tc.saturating {
			t.Errorf("%s: cpuShape(%.2f, %d, %d) = (%q, %d), want (%q, %d)",
				tc.name, tc.p50, tc.samples, tc.cores, shape, saturating, tc.shape, tc.saturating)
		}
	}
}

// TestCPUReportIsOptIn: without MeasureCPU the plan carries no CPU section at
// all — not an empty one. With it, every pool with a record is listed, including
// one with too few readings, so the operator can see the measurement is running.
func TestCPUReportIsOptIn(t *testing.T) {
	st := state.New()
	base := time.Now()
	for i := 0; i < 25; i++ {
		obs := busy("shop", 100*mb, base.Add(time.Duration(i)*2*time.Minute))
		obs.Workers = []state.WorkerSample{
			{RSSBytes: 100 * mb, Requests: 500 + int64(i), PID: 1, Idle: true, LastRequestCPU: 72, LastRequestMicros: 300_000},
			{RSSBytes: 100 * mb, Requests: 500 + int64(i), PID: 2, Idle: true, LastRequestCPU: 75, LastRequestMicros: 300_000},
		}
		st.Learn(obs, state.Options{MeasureCPU: true})
	}
	st.Learn(busy("api", 40*mb, base), state.Options{MeasureCPU: true})

	views := []observe.PoolView{
		{Name: "shop", ProcessManager: "dynamic", CurrentMaxChildren: 10, MaxChildrenKnown: true, ObservedPeak: 6},
		{Name: "api", ProcessManager: "dynamic", CurrentMaxChildren: 10, MaxChildrenKnown: true, ObservedPeak: 2},
	}
	in := Input{
		Limits: budget.Limits{MemoryBytes: 4 * gb, CPUs: 4, Source: budget.SourceMemInfo},
		Views:  views, State: st,
	}

	t.Run("off", func(t *testing.T) {
		res, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		if res.CPU != nil {
			t.Errorf("CPU = %+v without MeasureCPU, want none", res.CPU)
		}
	})

	t.Run("on", func(t *testing.T) {
		in.StateOptions = state.Options{MeasureCPU: true}
		res, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.CPU) != 2 {
			t.Fatalf("CPU has %d pools, want 2: %+v", len(res.CPU), res.CPU)
		}

		api, shop := res.CPU[0], res.CPU[1]
		if api.Name != "api" || api.Samples != 0 || api.Shape != "" {
			t.Errorf("api = %+v, want a listed pool with no readings and no shape", api)
		}
		if shop.Name != "shop" || shop.Shape != "cpu-bound" || shop.Samples != 50 {
			t.Errorf("shop = %+v, want cpu-bound on 50 readings", shop)
		}
		// 72% lands in the 70-75% bucket, reported as 0.70; 4 cores / 0.70 = 5.7,
		// rounded up.
		if shop.SaturatingWorkers != 6 {
			t.Errorf("shop saturates at %d workers, want 6", shop.SaturatingWorkers)
		}
		if shop.Allowed == 0 {
			t.Error("shop's planned ceiling is missing from the report")
		}

		var b strings.Builder
		if err := res.Render(&b); err != nil {
			t.Fatal(err)
		}
		out := b.String()
		for _, want := range []string{
			"CPU per request, as measured (--cpu):",
			"cpu-bound: ~6 busy workers saturate 4 core(s); plan allows",
			"too few readings yet",
			"Sizing does not use this.",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("rendered plan lacks %q:\n%s", want, out)
			}
		}
	})
}
