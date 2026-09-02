package plan

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
	"github.com/cboxdk/phpfpm"
)

var errDown = errors.New("connection refused")

// TestCPUShape is the classification, table-tested: a pool is called something
// only once it has enough readings, the per-worker cost is the median share in
// millicores, and the fill count is the host's millicores over it, rounded up.
func TestCPUShape(t *testing.T) {
	for _, tc := range []struct {
		name            string
		p50             float64
		known           bool
		host            int
		shape           string
		perWorker, fill int
	}{
		{"too few readings", 0.90, false, 4000, "", 0, 0},
		{"WordPress uncached", 0.70, true, 4000, "cpu-bound", 700, 6},
		{"on the cpu-bound line", 0.50, true, 2000, "cpu-bound", 500, 4},
		{"mixed", 0.35, true, 4000, "mixed", 350, 12},
		{"on the mixed line", 0.20, true, 4000, "mixed", 200, 20},
		{"API waiting on a database", 0.10, true, 4000, "i/o-bound", 100, 40},
		{"below the first bucket", 0, true, 4000, "i/o-bound", 0, 0},
		{"half a core", 0.70, true, 500, "cpu-bound", 700, 1},
		{"host unknown", 0.70, true, 0, "cpu-bound", 700, 0},
	} {
		shape, perWorker, fill := cpuShape(tc.p50, tc.known, tc.host)
		if shape != tc.shape || perWorker != tc.perWorker || fill != tc.fill {
			t.Errorf("%s: cpuShape(%.2f, %v, %d) = (%q, %d, %d), want (%q, %d, %d)",
				tc.name, tc.p50, tc.known, tc.host, shape, perWorker, fill, tc.shape, tc.perWorker, tc.fill)
		}
	}
}

// cpuBusy is a scrape of a pool whose two workers each finished a request at
// the given CPU share. Requests climb per scrape so every reading is new.
func cpuBusy(pool string, share float64, i int, at time.Time) state.Observation {
	obs := busy(pool, 100*mb, at)
	obs.Workers = []state.WorkerSample{
		{RSSBytes: 100 * mb, Requests: 500 + int64(i), PID: 1, Idle: true, LastRequestCPU: share * 100, LastRequestMicros: 300_000},
		{RSSBytes: 100 * mb, Requests: 500 + int64(i), PID: 2, Idle: true, LastRequestCPU: share * 100, LastRequestMicros: 300_000},
	}

	return obs
}

// TestTheReportSaysWhatEachPoolRunsOutOfFirst is the question the whole
// measurement exists to answer: memory or CPU. A cpu-bound pool sized on
// memory to more workers than fill the cores is CPU-limited however much RAM
// there is; an i/o-bound pool is memory-limited; a pool with too few readings
// is listed so the operator sees the measurement is running.
func TestTheReportSaysWhatEachPoolRunsOutOfFirst(t *testing.T) {
	st := state.New()
	base := time.Now()
	for i := 0; i < 25; i++ {
		at := base.Add(time.Duration(i) * 2 * time.Minute)
		st.Learn(cpuBusy("shop", 0.72, i, at), state.Options{})
		st.Learn(cpuBusy("api", 0.08, i, at), state.Options{})
	}
	st.Learn(busy("blog", 40*mb, base), state.Options{})

	views := []observe.PoolView{
		{Name: "shop", ProcessManager: "dynamic", CurrentMaxChildren: 40, MaxChildrenKnown: true, ObservedPeak: 12},
		{Name: "api", ProcessManager: "dynamic", CurrentMaxChildren: 10, MaxChildrenKnown: true, ObservedPeak: 2},
		{Name: "blog", ProcessManager: "dynamic", CurrentMaxChildren: 10, MaxChildrenKnown: true, ObservedPeak: 2},
	}
	in := Input{
		Limits: budget.Limits{MemoryBytes: 16 * gb, CPUs: 4, CPUMillicores: 4000, Source: budget.SourceMemInfo},
		Views:  views, State: st,
	}

	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CPU) != 3 {
		t.Fatalf("CPU has %d rows, want one per pool: %+v", len(res.CPU), res.CPU)
	}
	api, blog, shop := res.CPU[0], res.CPU[1], res.CPU[2]

	if blog.Name != "blog" || blog.Samples != 0 || blog.Shape != "" || blog.Limit != "" {
		t.Errorf("blog = %+v, want a listed pool with no readings and no verdict", blog)
	}
	// 72% lands in the 70-75% bucket: 700m per busy worker, 4000/700 = 5.7 → 6.
	if shop.Shape != "cpu-bound" || shop.MillicoresPerWorker != 700 || shop.FillWorkers != 6 {
		t.Errorf("shop = %+v, want cpu-bound at 700m per worker, 6 fill 4 cores", shop)
	}
	if shop.Limit != "cpu" || shop.Allowed <= shop.FillWorkers {
		t.Errorf("shop = %+v: memory allows %d, the CPU fills at %d, so the limit is cpu", shop, shop.Allowed, shop.FillWorkers)
	}
	if shop.Capped {
		t.Error("shop was capped without --cpu")
	}
	if api.Shape != "i/o-bound" || api.Limit != "memory" {
		t.Errorf("api = %+v, want i/o-bound and memory-limited", api)
	}

	// The host line adds the known pools up against the real CPU.
	if res.HostCPU.Known != 2 || res.HostCPU.Millicores != 4000 {
		t.Errorf("HostCPU = %+v", res.HostCPU)
	}
	if want := 40*700 + 10*50; res.HostCPU.NeededNow != want {
		t.Errorf("NeededNow = %d, want %d (40×700m + 10×50m)", res.HostCPU.NeededNow, want)
	}
	if want := shop.Allowed*700 + api.Allowed*50; res.HostCPU.NeededAtPlan != want {
		t.Errorf("NeededAtPlan = %d, want %d", res.HostCPU.NeededAtPlan, want)
	}

	var b strings.Builder
	if err := res.Render(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"CPU per request, as measured:",
		"cpu-bound; ~6 busy workers fill 4 core(s); plan allows",
		"(now 40)",
		"too few readings yet",
		"against 4 core(s).",
		"pass --cpu to hold it there",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered plan lacks %q:\n%s", want, out)
		}
	}
	// The first bucket is "<5%", never "0%": that would claim a measurement of
	// nothing.
	if strings.Contains(out, "  0%") {
		t.Errorf("a share in the first bucket printed as 0%%:\n%s", out)
	}
}

// TestTheCPUCeilingBindsOnlyWhenAskedAndOnlyOnATrustedPool.
//
// --cpu lets the measurement cap a pool, and a cap below the configured ceiling
// is a cut — so it waits for the same confidence the memory path needs before
// it may cut. Twenty readings say what shape the requests have; they are not
// permission to take workers away from a pool this tool has not watched
// through its traffic pattern.
func TestTheCPUCeilingBindsOnlyWhenAskedAndOnlyOnATrustedPool(t *testing.T) {
	view := observe.PoolView{
		Name: "shop", ProcessManager: "dynamic", CurrentMaxChildren: 40, MaxChildrenKnown: true, ObservedPeak: 30,
	}
	limits := budget.Limits{MemoryBytes: 16 * gb, CPUs: 4, CPUMillicores: 4000, Source: budget.SourceMemInfo}

	trusted := state.New()
	base := time.Now()
	for i := 0; i < 25; i++ {
		trusted.Learn(cpuBusy("shop", 0.72, i, base.Add(time.Duration(i)*2*time.Minute)), state.Options{})
	}
	// Enough CPU readings to know the shape, but three scrapes over two
	// minutes is no basis for a cut.
	young := state.New()
	for i := 0; i < 3; i++ {
		obs := cpuBusy("shop", 0.72, i, base.Add(time.Duration(i)*time.Minute))
		var w []state.WorkerSample
		for pid := 1; pid <= 10; pid++ {
			w = append(w, state.WorkerSample{RSSBytes: 100 * mb, Requests: 500 + int64(i), PID: pid, Idle: true, LastRequestCPU: 72, LastRequestMicros: 300_000})
		}
		obs.Workers = w
		young.Learn(obs, state.Options{})
	}

	t.Run("not asked: reported, not applied", func(t *testing.T) {
		res, err := Build(Input{Limits: limits, Views: []observe.PoolView{view}, State: trusted})
		if err != nil {
			t.Fatal(err)
		}
		if p := res.Plan.Pools[0]; p.CPUBound || p.MaxChildren <= 6 {
			t.Errorf("without --cpu the pool was capped: %+v", p)
		}
		if res.CPU[0].Limit != "cpu" {
			t.Errorf("the report still has to say the limit is cpu: %+v", res.CPU[0])
		}
	})

	t.Run("asked, trusted: held at the fill count", func(t *testing.T) {
		res, err := Build(Input{Limits: limits, Views: []observe.PoolView{view}, State: trusted, CPUCeiling: true})
		if err != nil {
			t.Fatal(err)
		}
		p := res.Plan.Pools[0]
		if !p.CPUBound || p.MaxChildren != 6 {
			t.Errorf("with --cpu a trusted cpu-bound pool should be held at 6: %+v", p)
		}
		if !strings.Contains(p.Reason, "cpu-bound") {
			t.Errorf("the plan row does not say why: %q", p.Reason)
		}
		if c := res.CPU[0]; !c.Capped || c.Limit != "cpu" {
			t.Errorf("the report does not say it was held: %+v", c)
		}
		var b strings.Builder
		if err := res.Render(&b); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(b.String(), "--cpu is on") || !strings.Contains(b.String(), "held there (now 40)") {
			t.Errorf("rendered plan does not say the ceiling is on:\n%s", b.String())
		}
	})

	t.Run("asked, not trusted: the shape is known, the cut waits", func(t *testing.T) {
		if !young.Pools["shop"].CPUShapeKnown(state.Options{}) {
			t.Fatal("fixture: the young pool should have enough CPU readings")
		}
		res, err := Build(Input{Limits: limits, Views: []observe.PoolView{view}, State: young, CPUCeiling: true})
		if err != nil {
			t.Fatal(err)
		}
		if p := res.Plan.Pools[0]; p.CPUBound || p.MaxChildren < 40 {
			t.Errorf("a pool with two minutes of history was capped from 40 to %d on CPU evidence: %+v", p.MaxChildren, p)
		}
		if c := res.CPU[0]; c.Limit != "cpu" || c.Capped {
			t.Errorf("the report should still say cpu, and not held: %+v", c)
		}
	})
}

// TestAnUnreachablePoolKeepsItsCPURow: what it measured last week is still what
// its requests look like, and a pool that vanishes from the report during a
// restart is a warning the operator went looking for and did not find.
func TestAnUnreachablePoolKeepsItsCPURow(t *testing.T) {
	st := state.New()
	base := time.Now()
	for i := 0; i < 25; i++ {
		st.Learn(cpuBusy("shop", 0.72, i, base.Add(time.Duration(i)*2*time.Minute)), state.Options{})
	}
	views := []observe.PoolView{
		{Name: "shop", ProcessManager: "dynamic", Err: errDown},
		{Name: "new", ProcessManager: "dynamic", Err: errDown},
	}
	res, err := Build(Input{
		Limits: budget.Limits{MemoryBytes: 4 * gb, CPUs: 4, CPUMillicores: 4000, Source: budget.SourceMemInfo},
		Views:  views, State: st,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CPU) != 1 || res.CPU[0].Name != "shop" || res.CPU[0].Shape != "cpu-bound" {
		t.Errorf("CPU = %+v, want shop's remembered row and nothing for a pool never seen", res.CPU)
	}
}

// TestAmbiguousNamesStayOutOfTheCPUReport: two masters each with a `www`
// cannot be told apart by name, and a row that attributes one master's
// readings to the other is worse than no row. Build already warns.
func TestAmbiguousNamesStayOutOfTheCPUReport(t *testing.T) {
	views := []observe.PoolView{
		{Name: "www", Target: phpfpm.Target{ConfigPath: "/etc/php/8.2/fpm/php-fpm.conf"}, ProcessManager: "dynamic", CurrentMaxChildren: 10, MaxChildrenKnown: true},
		{Name: "www", Target: phpfpm.Target{ConfigPath: "/etc/php/8.3/fpm/php-fpm.conf"}, ProcessManager: "dynamic", CurrentMaxChildren: 10, MaxChildrenKnown: true},
	}
	res, err := Build(Input{
		Limits: budget.Limits{MemoryBytes: 4 * gb, CPUs: 4, CPUMillicores: 4000, Source: budget.SourceMemInfo},
		Views:  views, State: state.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CPU) != 0 {
		t.Errorf("CPU = %+v, want no rows for an ambiguous name", res.CPU)
	}
}
