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

// TestCPUShape is the classification, table-tested: the per-worker cost is
// the median share in millicores, and the fill count is the host's millicores
// over it, rounded up.
func TestCPUShape(t *testing.T) {
	for _, tc := range []struct {
		name            string
		p50             float64
		host            int
		shape           string
		perWorker, fill int
	}{
		{"WordPress uncached", 0.70, 4000, "cpu-bound", 700, 6},
		{"on the cpu-bound line", 0.50, 2000, "cpu-bound", 500, 4},
		{"mixed", 0.35, 4000, "mixed", 350, 12},
		{"on the mixed line", 0.20, 4000, "mixed", 200, 20},
		{"API waiting on a database", 0.10, 4000, "i/o-bound", 100, 40},
		{"below the first bucket", 0, 4000, "i/o-bound", 0, 0},
		{"half a core", 0.70, 500, "cpu-bound", 700, 1},
		{"ffmpeg on eight cores", 8.0, 4000, "cpu-bound", 8000, 1},
		{"host unknown", 0.70, 0, "cpu-bound", 700, 0},
	} {
		shape, perWorker := cpuShape(tc.p50)
		fill := fillWorkers(perWorker, tc.host)
		if shape != tc.shape || perWorker != tc.perWorker || fill != tc.fill {
			t.Errorf("%s: cpuShape(%.2f) on %dm = (%q, %d, fill %d), want (%q, %d, fill %d)",
				tc.name, tc.p50, tc.host, shape, perWorker, fill, tc.shape, tc.perWorker, tc.fill)
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
	if shop.CPUBound {
		t.Error("shop was capped without --cpu")
	}
	if api.Shape != "i/o-bound" || api.Limit != "memory" {
		t.Errorf("api = %+v, want i/o-bound and memory-limited", api)
	}

	// The host line adds the known pools up against the real CPU.
	if res.HostCPU.Millicores != 4000 {
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
		"cpu-bound; ~6 busy workers fill 4 core(s) by PHP's own CPU (the rest of the box not measured yet); ceiling 12 at 2× headroom; plan allows",
		"(now 40)",
		"too few readings yet",
		"against 4 core(s).",
		"pass --cpu to hold it at the ceiling shown",
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

	t.Run("asked, trusted: held at the ceiling", func(t *testing.T) {
		res, err := Build(Input{Limits: limits, Views: []observe.PoolView{view}, State: trusted, CPUCeiling: true})
		if err != nil {
			t.Fatal(err)
		}
		// Six busy workers fill four cores at 700m; the ceiling is twice that
		// by default, room for I/O waits and bursts.
		p := res.Plan.Pools[0]
		if !p.CPUBound || p.MaxChildren != 12 {
			t.Errorf("with --cpu a trusted cpu-bound pool should be held at 12 (6 fill × 2 headroom): %+v", p)
		}
		if !strings.Contains(p.Reason, "cpu-bound") {
			t.Errorf("the plan row does not say why: %q", p.Reason)
		}
		if c := res.CPU[0]; !c.CPUBound || c.Limit != "cpu" {
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
		if young.Pools["shop"].Trusted(state.Options{}) {
			t.Fatal("fixture: three scrapes over two minutes must not be a trusted baseline")
		}
		res, err := Build(Input{Limits: limits, Views: []observe.PoolView{view}, State: young, CPUCeiling: true})
		if err != nil {
			t.Fatal(err)
		}
		if p := res.Plan.Pools[0]; p.CPUBound || p.MaxChildren < 40 {
			t.Errorf("a pool with two minutes of history was capped from 40 to %d on CPU evidence: %+v", p.MaxChildren, p)
		}
		if c := res.CPU[0]; c.Limit != "cpu" || c.CPUBound {
			t.Errorf("the report should still say cpu, and not held: %+v", c)
		}
	})

	t.Run("asked, not trusted, small ceiling: the gate is the only thing standing", func(t *testing.T) {
		// Configured for 4, busy at 30: the floor is 4, below the fill count of
		// 6, so the allocator's floor guard would let the ceiling bind. Only
		// plan's confidence gate keeps an untrusted pool from being held at 6
		// while memory would have grown it to 37.
		small := view
		small.CurrentMaxChildren = 4
		res, err := Build(Input{Limits: limits, Views: []observe.PoolView{small}, State: young, CPUCeiling: true})
		if err != nil {
			t.Fatal(err)
		}
		if p := res.Plan.Pools[0]; p.CPUBound || p.MaxChildren <= 12 {
			t.Errorf("an untrusted pool was held at the CPU ceiling: %+v", p)
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
		{Name: "shop", ProcessManager: "dynamic", CurrentMaxChildren: 40, MaxChildrenKnown: true, Err: errDown},
		{Name: "new", ProcessManager: "dynamic", Err: errDown},
	}
	res, err := Build(Input{
		Limits: budget.Limits{MemoryBytes: 16 * gb, CPUs: 4, CPUMillicores: 4000, Source: budget.SourceMemInfo},
		Views:  views, State: st,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CPU) != 1 || res.CPU[0].Name != "shop" || res.CPU[0].Shape != "cpu-bound" {
		t.Fatalf("CPU = %+v, want shop's remembered row and nothing for a pool never seen", res.CPU)
	}
	// The plan does not write an unreachable pool, so it keeps its 40: that is
	// the ceiling to compare against, and what the host sums count.
	if c := res.CPU[0]; c.Limit != "cpu" || res.HostCPU.NeededAtPlan != 40*700 || res.HostCPU.NeededNow != 40*700 {
		t.Errorf("an unreachable pool at 40 was not counted at 40: %+v host=%+v", c, res.HostCPU)
	}
}

// TestLimitsWithoutMillicoresStillDivide: Limits built by hand carry a core
// count and no millicores. A core is a thousand of them; dividing by zero
// would call every pool memory-limited and make --cpu a silent no-op.
func TestLimitsWithoutMillicoresStillDivide(t *testing.T) {
	st := state.New()
	base := time.Now()
	for i := 0; i < 25; i++ {
		st.Learn(cpuBusy("shop", 0.72, i, base.Add(time.Duration(i)*2*time.Minute)), state.Options{})
	}
	res, err := Build(Input{
		Limits: budget.Limits{MemoryBytes: 16 * gb, CPUs: 4, Source: budget.SourceMemInfo},
		Views:  []observe.PoolView{{Name: "shop", ProcessManager: "dynamic", CurrentMaxChildren: 40, MaxChildrenKnown: true, ObservedPeak: 30}},
		State:  st, CPUCeiling: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c := res.CPU[0]; c.FillWorkers != 6 || c.Limit != "cpu" || !c.CPUBound || res.HostCPU.Millicores != 4000 {
		t.Errorf("four cores without millicores: %+v host=%+v", c, res.HostCPU)
	}
}

// TestAShareAboveOneCoreRendersAsSuch: php-fpm counts the children a request
// waited for, so a transcode on eight cores is a share of 800%, and the whole
// path — histogram, per-worker cost, fill count, host line, the rendered
// text — has to carry it rather than fold it back under 100%.
func TestAShareAboveOneCoreRendersAsSuch(t *testing.T) {
	st := state.New()
	base := time.Now()
	for i := 0; i < 25; i++ {
		st.Learn(cpuBusy("media", 8.0, i, base.Add(time.Duration(i)*2*time.Minute)), state.Options{})
	}
	res, err := Build(Input{
		Limits: budget.Limits{MemoryBytes: 16 * gb, CPUs: 4, CPUMillicores: 4000, Source: budget.SourceMemInfo},
		Views:  []observe.PoolView{{Name: "media", ProcessManager: "dynamic", CurrentMaxChildren: 10, MaxChildrenKnown: true, ObservedPeak: 8}},
		State:  st,
	})
	if err != nil {
		t.Fatal(err)
	}
	// One busy worker fills the box; the ceiling is the floor of cores + 1,
	// because a cap below the core count would be a fault, not a cap.
	c := res.CPU[0]
	if c.P50 != 8.0 || c.MillicoresPerWorker != 8000 || c.FillWorkers != 1 || c.Ceiling != 5 || c.Limit != "cpu" {
		t.Errorf("media = %+v", c)
	}
	if res.HostCPU.NeededNow != 10*8000 {
		t.Errorf("NeededNow = %d, want 80000", res.HostCPU.NeededNow)
	}
	var b strings.Builder
	if err := res.Render(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"800%", "8000m", "~1 busy worker fill 4 core(s)", "80 core(s) now"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("rendered plan lacks %q:\n%s", want, b.String())
		}
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

// TestCPUCeilingCarriesHeadroomAndAFloor: the fill count is the throughput
// optimum and nothing more, so the ceiling is the fill count times the
// headroom, and never fewer workers than cores plus one.
func TestCPUCeilingCarriesHeadroomAndAFloor(t *testing.T) {
	for _, tc := range []struct {
		fill, host int
		headroom   float64
		want       int
	}{
		{0, 4000, 2, 0},   // no fill, no ceiling
		{6, 4000, 2, 12},  // cbox-web by PHP's own figure
		{3, 4000, 2, 6},   // the same with the box measured
		{3, 4000, 1.5, 5}, // 4.5 rounded up
		{1, 4000, 2, 5},   // floor: cores + 1
		{2, 500, 2, 4},    // half a core: floor is 1 + 1, headroom gives 4
		{6, 4000, 0.5, 6}, // headroom below one is one
	} {
		if got := cpuCeiling(tc.fill, tc.host, tc.headroom); got != tc.want {
			t.Errorf("cpuCeiling(%d, %dm, %.2g) = %d, want %d", tc.fill, tc.host, tc.headroom, got, tc.want)
		}
	}
}

// TestTheBoxCostChangesTheFillCount: with the box-cost fit believed, a busy
// worker is priced at what the whole box spends for it, not what PHP alone
// does. cbox-web: 700m in PHP, 2.1× that on the box, so three busy workers
// fill four cores rather than six, and the report says which figure it used.
func TestTheBoxCostChangesTheFillCount(t *testing.T) {
	st := state.New()
	base := time.Now()
	for i := 0; i < 25; i++ {
		st.Learn(cpuBusy("shop", 0.72, i, base.Add(time.Duration(i)*2*time.Minute)), state.Options{})
	}
	ps := st.Pools["shop"]
	for i := 0; i < 100; i++ {
		x := 2 * float64(i%5) / 4
		ps.BoxCost.Add(x, 0.2+2.1*x)
	}

	view := observe.PoolView{Name: "shop", ProcessManager: "dynamic", CurrentMaxChildren: 22, MaxChildrenKnown: true, ObservedPeak: 17}
	limits := budget.Limits{MemoryBytes: 8 * gb, CPUs: 4, CPUMillicores: 4000, Source: budget.SourceMemInfo}

	res, err := Build(Input{Limits: limits, Views: []observe.PoolView{view}, State: st})
	if err != nil {
		t.Fatal(err)
	}
	c := res.CPU[0]
	if !c.BoxMeasured || c.BoxMillicoresPerWorker != 1470 || c.FillWorkers != 3 || c.Ceiling != 6 {
		t.Errorf("shop = %+v, want the box measured at 1470m per worker, 3 fill, ceiling 6", c)
	}
	if !strings.Contains(c.Why(4000), "with MySQL, nginx and the kernel counted (2.1× PHP's own); ceiling 6 at 2× headroom") {
		t.Errorf("Why = %q", c.Why(4000))
	}
	if res.HostCPU.NeededNow != 22*1470 {
		t.Errorf("NeededNow = %d, want 22 × 1470m: the host line prices workers at the box cost", res.HostCPU.NeededNow)
	}

	// The headroom is the operator's: at 1.5 the ceiling is 5, the floor.
	res, err = Build(Input{Limits: limits, Views: []observe.PoolView{view}, State: st, CPUHeadroom: 1.5, CPUCeiling: true})
	if err != nil {
		t.Fatal(err)
	}
	if p := res.Plan.Pools[0]; !p.CPUBound || p.MaxChildren != 5 {
		t.Errorf("at 1.5× headroom the pool should be held at 5 (3 × 1.5 = 4.5, and never below cores + 1): %+v", p)
	}
	if c := res.CPU[0]; !strings.Contains(c.Why(4000), "at 1.5× headroom") {
		t.Errorf("Why = %q", c.Why(4000))
	}
}

// TestTheHostHeadroomIsBoundedToo: Input.CPUHeadroom is held to the range a
// pool's marker is, so a caller that skips the command's flag check cannot
// saturate the ceiling either, and the ceiling itself clamps whatever reaches
// it. Six workers fill a 4-core box here; the largest headroom gives 600, and
// 1e19 gives the same rather than the floor of five.
func TestTheHostHeadroomIsBoundedToo(t *testing.T) {
	for in, want := range map[float64]float64{0: 2, -1: 2, 0.5: 1, 1.5: 1.5, 100: 100, 101: 100, 1e19: 100} {
		if got := hostHeadroom(in); got != want {
			t.Errorf("hostHeadroom(%g) = %g, want %g", in, got, want)
		}
	}
	if got := cpuCeiling(6, 4000, 1e19); got != 600 {
		t.Errorf("cpuCeiling(6, 4000m, 1e19) = %d, want 600: the ceiling must clamp, not saturate", got)
	}
}

// TestAPoolCanCarryItsOwnHeadroom: env[FPM_TUNE_CPU_HEADROOM] on the pool wins
// over the host's --cpu-headroom for that pool alone, the report says so, and
// a marker that does not read as a number from one to MaxCPUHeadroom is a
// warning, not a silent default. 1e19 is the value that used to saturate the
// ceiling's float-to-int conversion.
func TestAPoolCanCarryItsOwnHeadroom(t *testing.T) {
	for marker, want := range map[string]struct {
		headroom float64
		fromPool bool
		ok       bool
	}{
		"":      {2, false, true},
		"3":     {3, true, true},
		" 1.5 ": {1.5, true, true},
		"100":   {100, true, true},
		"101":   {2, false, false},
		"1e19":  {2, false, false},
		"0.5":   {2, false, false},
		"abc":   {2, false, false},
		"-1":    {2, false, false},
		"NaN":   {2, false, false},
		"+Inf":  {2, false, false},
	} {
		h, fromPool, ok := headroomFor(marker, 2)
		if h != want.headroom || fromPool != want.fromPool || ok != want.ok {
			t.Errorf("headroomFor(%q, 2) = (%.2g, %v, %v), want (%.2g, %v, %v)", marker, h, fromPool, ok, want.headroom, want.fromPool, want.ok)
		}
	}

	st := state.New()
	base := time.Now()
	for i := 0; i < 25; i++ {
		at := base.Add(time.Duration(i) * 2 * time.Minute)
		st.Learn(cpuBusy("shop", 0.72, i, at), state.Options{})
		st.Learn(cpuBusy("api", 0.72, i, at), state.Options{})
	}
	limits := budget.Limits{MemoryBytes: 16 * gb, CPUs: 4, CPUMillicores: 4000, Source: budget.SourceMemInfo}
	views := []observe.PoolView{
		{Name: "shop", ProcessManager: "dynamic", CurrentMaxChildren: 40, MaxChildrenKnown: true, ObservedPeak: 30, CPUHeadroom: "3"},
		{Name: "api", ProcessManager: "dynamic", CurrentMaxChildren: 40, MaxChildrenKnown: true, ObservedPeak: 30, CPUHeadroom: "lots"},
	}
	res, err := Build(Input{Limits: limits, Views: views, State: st, CPUCeiling: true})
	if err != nil {
		t.Fatal(err)
	}
	api, shop := res.CPU[0], res.CPU[1]
	// Six fill the box at 700m; shop asked for three times that, api's marker
	// is unreadable so it gets the host's two.
	if shop.Ceiling != 18 || !shop.HeadroomFromPool || !strings.Contains(shop.Why(4000), "at 3× headroom (the pool's own)") {
		t.Errorf("shop = %+v\nwhy = %s", shop, shop.Why(4000))
	}
	if api.Ceiling != 12 || api.HeadroomFromPool {
		t.Errorf("api = %+v; an unreadable marker should fall back to the host's headroom", api)
	}
	for _, p := range res.Plan.Pools {
		if p.Name == "shop" && (!p.CPUBound || p.MaxChildren != 18) {
			t.Errorf("shop's own headroom did not reach the allocator: %+v", p)
		}
	}
	found := false
	for _, w := range res.Plan.Warnings {
		if strings.Contains(w, HeadroomMarker) && strings.Contains(w, `api ("lots")`) {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning about api's unreadable marker in %v", res.Plan.Warnings)
	}
}
