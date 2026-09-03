package state

import (
	"math"
	"testing"
	"time"
)

// points is a fit's sample count rounded: the forgetting factor shaves a
// fraction off on every add, so N is 1.9975 after two points, not 2.
func points(f Fit) int { return int(math.Round(f.N)) }

func cpuWorker(pid int, ticks int64) WorkerSample {
	return WorkerSample{RSSBytes: 50 * mb, Requests: 100, PID: pid, CPUTicks: ticks}
}

// TestPoolCPUTicksAreADifferencePerWorker: a worker's counter is cumulative,
// so the pool's CPU this interval is the sum of each worker's rise since the
// last scrape. A worker never seen before contributes its whole life, which
// began inside the interval; a counter that went down is a recycled pid, a new
// worker wearing an old number, and is treated the same way; a worker that
// exited takes what it spent since the last scrape with it.
func TestPoolCPUTicksAreADifferencePerWorker(t *testing.T) {
	ps := &PoolState{}

	if got := ps.poolCPUTicks([]WorkerSample{cpuWorker(1, 100), cpuWorker(2, 50)}); got != 150 {
		t.Errorf("first scrape = %d ticks, want 150: a pid never seen contributes its whole counter", got)
	}
	if got := ps.poolCPUTicks([]WorkerSample{cpuWorker(1, 130), cpuWorker(2, 50)}); got != 30 {
		t.Errorf("second scrape = %d ticks, want 30: only the rise since last time", got)
	}
	// pid 2 gone, pid 1 recycled at 7 ticks, pid 3 new at 20.
	if got := ps.poolCPUTicks([]WorkerSample{cpuWorker(1, 7), cpuWorker(3, 20)}); got != 27 {
		t.Errorf("third scrape = %d ticks, want 27: a recycled pid and a new one both count in full", got)
	}
	if _, still := ps.CPUTicksSeen[2]; still {
		t.Error("an exited worker is still remembered")
	}
	if got := ps.poolCPUTicks(nil); got != 0 || ps.CPUTicksSeen != nil {
		t.Errorf("an empty scrape gave %d ticks and left %v", got, ps.CPUTicksSeen)
	}
}

// TestTheFitRecoversALine: y = 0.3 + 2.1x, fed with spread, comes back as
// slope 2.1 and intercept 0.3 — the box spends 2.1 cores for every core the
// pool does, on top of 0.3 cores of base load.
func TestTheFitRecoversALine(t *testing.T) {
	var f Fit
	for i := 0; i < 200; i++ {
		x := float64(i%10) / 4 // 0 .. 2.25 cores
		f.Add(x, 0.3+2.1*x)
	}
	slope, intercept, sd, ok := f.Line()
	if !ok {
		t.Fatal("no line from two hundred points with spread")
	}
	if math.Abs(slope-2.1) > 0.01 || math.Abs(intercept-0.3) > 0.01 {
		t.Errorf("line = %.3f x + %.3f, want 2.1 x + 0.3", slope, intercept)
	}
	if sd < 0.5 {
		t.Errorf("sdX = %.2f, want the spread of 0..2.25", sd)
	}

	// No spread, no slope: every point at the same x.
	var flat Fit
	for i := 0; i < 50; i++ {
		flat.Add(1, 2+float64(i%3)*0.1)
	}
	if _, _, _, ok := flat.Line(); ok {
		t.Error("a fit through points at one x gave a slope")
	}
}

// TestBoxOverheadIsGated: the slope is believed only with enough points and
// enough spread, is clamped up to one (the box cannot spend less than the
// pool did), and is refused past twenty (that is not this pool's traffic).
func TestBoxOverheadIsGated(t *testing.T) {
	line := func(n int, spread, slope float64) *PoolState {
		ps := &PoolState{}
		for i := 0; i < n; i++ {
			x := spread * float64(i%5) / 4
			ps.BoxCost.Add(x, 0.2+slope*x)
		}

		return ps
	}
	if _, ok := line(10, 2, 2).BoxOverhead(Options{}); ok {
		t.Error("ten points were believed; the default asks for thirty")
	}
	if _, ok := line(100, 0.1, 2).BoxOverhead(Options{}); ok {
		t.Error("a spread of a tenth of a core was believed; the default asks for a fifth")
	}
	if a, ok := line(100, 2, 2.1).BoxOverhead(Options{}); !ok || math.Abs(a-2.1) > 0.02 {
		t.Errorf("overhead = %.2f ok=%v, want 2.1", a, ok)
	}
	if a, ok := line(100, 2, 0.9).BoxOverhead(Options{}); !ok || a != 1 {
		t.Errorf("a slope below one came out as %.2f ok=%v; want clamped to 1", a, ok)
	}
	if _, ok := line(100, 2, 25).BoxOverhead(Options{}); ok {
		t.Error("a slope of 25 was believed; that is not this pool's traffic")
	}
	if a, ok := line(100, 2, 2).BoxOverhead(Options{MinBoxCostSamples: 500}); ok {
		t.Errorf("the sample option was not honoured: %.2f", a)
	}
}

// TestLearnCPULoadAttributesTheBoxToThePoolThatWasBusy is the whole mechanism
// in one run: two pools, the box's counter and the workers' counters read
// twice thirty seconds apart. The pool that did the PHP work gets the point;
// the idle neighbour gets nothing; a quiet interval gives both the base load;
// and a queue while the box was full is counted against the pool that queued.
func TestLearnCPULoadAttributesTheBoxToThePoolThatWasBusy(t *testing.T) {
	s := New()
	t0 := time.Now()
	scrape := func(at time.Time, busyMicros int64, shopTicks, apiTicks int64, shopQueue int64) {
		s.LearnCPULoad([]Observation{
			{Pool: "shop", Workers: []WorkerSample{cpuWorker(1, shopTicks)}, QueueDepth: shopQueue},
			{Pool: "api", Workers: []WorkerSample{cpuWorker(2, apiTicks)}},
		}, CPULoadSample{BusyMicros: busyMicros, Millicores: 4000, At: at})
	}

	// First reading: nothing to compare against.
	scrape(t0, 0, 0, 0, 0)
	if points(s.Pools["shop"].BoxCost) != 0 {
		t.Fatal("the first reading produced a point")
	}

	// Thirty seconds in which shop's worker spent 30 CPU seconds (3000 ticks:
	// one full core) and api's spent nothing, while the box spent 63 core
	// seconds: 2.1 cores, of which one was shop's own.
	scrape(t0.Add(30*time.Second), 63_000_000, 3000, 0, 0)
	shop, api := s.Pools["shop"], s.Pools["api"]
	if points(shop.BoxCost) != 1 || points(api.BoxCost) != 0 {
		t.Fatalf("shop got %.0f points and api %.0f; want 1 and 0", shop.BoxCost.N, api.BoxCost.N)
	}
	if math.Abs(shop.BoxCost.SumX-1.0) > 0.01 || math.Abs(shop.BoxCost.SumY-2.1) > 0.01 {
		t.Errorf("shop's point = (%.2f, %.2f), want (1.0 cores, 2.1 cores)", shop.BoxCost.SumX, shop.BoxCost.SumY)
	}

	// A quiet interval: no PHP work, the box at 0.2 cores. Both pools learn
	// the base load as (0, 0.2).
	scrape(t0.Add(60*time.Second), 63_000_000+6_000_000, 3000, 0, 0)
	if points(shop.BoxCost) != 2 || points(api.BoxCost) != 1 {
		t.Errorf("after a quiet interval shop has %.0f points and api %.0f; want 2 and 1", shop.BoxCost.N, api.BoxCost.N)
	}

	// Shop queued while the box was full (3.9 of 4 cores): starved. api did
	// not queue.
	scrape(t0.Add(90*time.Second), 63_000_000+6_000_000+117_000_000, 6000, 0, 4)
	if shop.CPUStarvedRounds != 1 || api.CPUStarvedRounds != 0 {
		t.Errorf("starved rounds shop=%d api=%d, want 1 and 0", shop.CPUStarvedRounds, api.CPUStarvedRounds)
	}

	// A hole longer than five minutes teaches nothing across it, and a
	// counter that went backwards (a reboot) neither.
	before := points(shop.BoxCost)
	scrape(t0.Add(20*time.Minute), 500_000_000, 9000, 0, 0)
	scrape(t0.Add(20*time.Minute+30*time.Second), 1_000, 9100, 0, 0)
	if points(shop.BoxCost) != before {
		t.Errorf("a long hole or a reset counter added points: %d -> %d", before, points(shop.BoxCost))
	}
	// The host reading is still remembered, so the next ordinary interval works.
	scrape(t0.Add(21*time.Minute), 1_000+30_000_000, 9100+3000, 0, 0)
	if points(shop.BoxCost) != before+1 {
		t.Errorf("the interval after a reset was not learned: %d", points(shop.BoxCost))
	}
}

// TestBoxCostSurvivesTheStateFile: the fit and the counters round-trip, so a
// daemon restart continues the line rather than starting it over.
func TestBoxCostSurvivesTheStateFile(t *testing.T) {
	s := New()
	t0 := time.Now()
	for i := 0; i < 3; i++ {
		s.LearnCPULoad([]Observation{{Pool: "shop", Workers: []WorkerSample{cpuWorker(1, int64(3000*i))}}},
			CPULoadSample{BusyMicros: int64(63_000_000 * i), Millicores: 4000, At: t0.Add(time.Duration(i) * 30 * time.Second)})
	}
	path := t.TempDir() + "/state.json"
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.HostCPU == nil || loaded.HostCPU.BusyMicros != 126_000_000 {
		t.Errorf("host reading did not survive: %+v", loaded.HostCPU)
	}
	if ps := loaded.Pools["shop"]; points(ps.BoxCost) != 2 || ps.CPUTicksSeen[1] != 6000 {
		t.Errorf("fit or counters did not survive: N=%d seen=%v", points(ps.BoxCost), ps.CPUTicksSeen)
	}
}
