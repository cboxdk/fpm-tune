package metrics

import (
	"strconv"
	"strings"
	"testing"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/plan"
	"github.com/cboxdk/fpm-tune/state"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const mb = 1024 * 1024

// TestRemovedPoolStopsReporting is the stale-series trap.
//
// A pool deleted from the host would otherwise keep publishing its last value
// forever, and a stale fpm_tune_pool_demand_unmet is the alert nobody can
// silence: it describes a pool that no longer exists, so no configuration change
// makes it go away.
func TestRemovedPoolStopsReporting(t *testing.T) {
	c := New()

	c.Update(result(
		poolPlan("keeps-running", 10, false),
		poolPlan("about-to-go", 4, true),
	), state.New(), state.Options{}, 1000)

	if !exposes(t, c, `fpm_tune_pool_demand_unmet{pool="about-to-go"}`) {
		t.Fatal("test setup: the pool was never published")
	}

	// The next evaluation no longer has it.
	c.Update(result(poolPlan("keeps-running", 10, false)), state.New(), state.Options{}, 2000)

	if exposes(t, c, `pool="about-to-go"`) {
		t.Error("a removed pool is still publishing; its demand_unmet alert can never be silenced")
	}
	if !exposes(t, c, `pool="keeps-running"`) {
		t.Error("the surviving pool stopped publishing")
	}
}

// TestSaturationSeriesAreDistinct: the whole point of publishing both. Unmet
// demand alone means a later run can rebalance; unmet demand with the budget
// committed means no configuration change helps and the host needs more memory.
func TestSaturationSeriesAreDistinct(t *testing.T) {
	c := New()

	t.Run("wants more, room available", func(t *testing.T) {
		r := result(poolPlan("shop", 8, true))
		r.Plan.CapacityExhausted = false
		r.Plan.FreeBytes = 2048 * mb
		c.Update(r, state.New(), state.Options{}, 1)

		if got := testutil.ToFloat64(c.capacityExhausted); got != 0 {
			t.Errorf("fpm_tune_capacity_exhausted = %v with free budget", got)
		}
		if !exposes(t, c, `fpm_tune_pool_demand_unmet{pool="shop"} 1`) {
			t.Error("unmet demand was not published")
		}
	})

	t.Run("wants more, nothing left", func(t *testing.T) {
		r := result(poolPlan("shop", 8, true))
		r.Plan.CapacityExhausted = true
		r.Plan.FreeBytes = 0
		c.Update(r, state.New(), state.Options{}, 2)

		if got := testutil.ToFloat64(c.capacityExhausted); got != 1 {
			t.Errorf("fpm_tune_capacity_exhausted = %v with an exhausted budget", got)
		}
	})
}

// TestBaselineIsPublishedWithItsConfidence: a reader has to be able to tell a
// measured number from a guess, or the recommendation means nothing.
func TestBaselineIsPublishedWithItsConfidence(t *testing.T) {
	st := state.New()
	st.Pools["shop"] = &state.PoolState{
		Pool: "shop", TypicalPeakBytes: 90 * mb, HighWaterBytes: 140 * mb,
		BusySamples: 30,
	}

	c := New()
	c.Update(result(poolPlan("shop", 12, false)), st, state.Options{}, 1)

	for _, want := range []string{
		`fpm_tune_pool_worker_rss_bytes{pool="shop",quantile="typical_peak"}`,
		`fpm_tune_pool_worker_rss_bytes{pool="shop",quantile="high_water"}`,
		`fpm_tune_pool_baseline_confidence{pool="shop"}`,
		`fpm_tune_pool_measured{pool="shop"}`,
	} {
		if !exposes(t, c, want) {
			t.Errorf("%s is not published", want)
		}
	}
}

// TestBudgetIsPublishedByState so the free headroom can be graphed alongside
// what was allocated.
func TestBudgetIsPublishedByState(t *testing.T) {
	c := New()
	r := result(poolPlan("shop", 4, false))
	r.Plan.TotalBytes = 8192 * mb
	r.Reserve = 2048 * mb
	r.Plan.AllocatedBytes = 1024 * mb
	r.Plan.FreeBytes = 5120 * mb
	c.Update(r, state.New(), state.Options{}, 1)

	for _, st := range []string{"total", "reserved", "allocated", "free"} {
		if !exposes(t, c, `fpm_tune_budget_bytes{state="`+st+`"}`) {
			t.Errorf("budget state %q is not published", st)
		}
	}
}

// TestLastRunAdvances: a loop that has stalled shows up in no other series here
// — every gauge simply keeps its last value, which looks like a healthy steady
// state.
func TestLastRunAdvances(t *testing.T) {
	c := New()

	c.Update(result(poolPlan("shop", 4, false)), state.New(), state.Options{}, 1000)
	first := testutil.ToFloat64(c.lastRun)

	c.Update(result(poolPlan("shop", 4, false)), state.New(), state.Options{}, 2000)
	second := testutil.ToFloat64(c.lastRun)

	if second <= first {
		t.Errorf("last run did not advance: %v then %v", first, second)
	}
}

// TestTwoCollectorsDoNotShareState: each has its own registry, so one test's
// assertions cannot leak into the next.
func TestTwoCollectorsDoNotShareState(t *testing.T) {
	a, b := New(), New()

	a.Update(result(poolPlan("only-in-a", 5, false)), state.New(), state.Options{}, 1)

	if exposes(t, b, "only-in-a") {
		t.Error("two collectors are sharing series")
	}
}

func poolPlan(name string, workers int, unmet bool) allocate.PoolPlan {
	return allocate.PoolPlan{
		Name: name, MaxChildren: workers, DemandUnmet: unmet,
		Bytes: int64(workers) * 48 * mb,
	}
}

func result(pools ...allocate.PoolPlan) plan.Result {
	views := make([]observe.PoolView, 0, len(pools))
	for _, p := range pools {
		views = append(views, observe.PoolView{Name: p.Name, CurrentMaxChildren: p.MaxChildren})
	}

	return plan.Result{
		Plan:   allocate.Plan{Pools: pools, TotalBytes: 4096 * mb},
		Budget: budget.Limits{MemoryBytes: 4096 * mb},
		Views:  views,
	}
}

// exposes reports whether the rendered exposition contains a fragment.
func exposes(t *testing.T, c *Collectors, want string) bool {
	t.Helper()

	families, err := c.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	var b strings.Builder
	for _, f := range families {
		for _, m := range f.GetMetric() {
			b.WriteString(f.GetName())
			b.WriteString("{")
			for i, l := range m.GetLabel() {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(l.GetName() + `="` + l.GetValue() + `"`)
			}
			b.WriteString("} ")
			if g := m.GetGauge(); g != nil {
				b.WriteString(trimFloat(g.GetValue()))
			}
			b.WriteString("\n")
		}
	}

	return strings.Contains(b.String(), want)
}

func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
