package allocate

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

const mb = 1024 * 1024

// TestNeverExceedsTheBudget is the invariant the whole package exists to hold.
//
// Over-allocating does not degrade gracefully: the kernel OOM-kills a worker
// mid-request, and on a shared host it may not even be a worker belonging to the
// pool that caused it. Every other property here is negotiable; this one is not.
//
// Swept across pool counts, budget sizes and worker costs, including the
// combinations that do not fit at all.
func TestNeverExceedsTheBudget(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	for _, poolCount := range []int{1, 2, 3, 8, 40} {
		for _, totalMB := range []int64{64, 256, 512, 2048, 8192, 65536} {
			for trial := 0; trial < 8; trial++ {
				pools := make([]Pool, poolCount)
				for i := range pools {
					pools[i] = Pool{
						Name:               fmt.Sprintf("site-%d", i),
						CurrentMaxChildren: 1 + rng.Intn(30),
						ProcessManager:     "dynamic",
						// 16MB to 256MB per worker: a small API and a heavy
						// Magento install on the same box is a real shape.
						WorkerBytes:    int64(16+rng.Intn(240)) * mb,
						Measured:       rng.Intn(2) == 0,
						ObservedPeak:   rng.Intn(40),
						QueueDepth:     int64(rng.Intn(4)),
						HitMaxChildren: rng.Intn(3) == 0,
					}
				}

				budget := Budget{
					TotalBytes:   totalMB * mb,
					ReserveBytes: totalMB * mb / 4,
				}

				plan, err := Compute(budget, pools, Options{})
				if err != nil {
					// Refusing is a valid outcome, and the only honest one when
					// a single worker per pool already exceeds the budget: the
					// alternative is writing a config that OOMs.
					if !errors.Is(err, ErrNoBudget) && !errors.Is(err, ErrCannotFit) {
						t.Fatalf("%d pools / %dMB: %v", poolCount, totalMB, err)
					}
					continue
				}

				var sum int64
				for _, pp := range plan.Pools {
					if pp.MaxChildren < 1 {
						t.Errorf("%d pools / %dMB: %s got %d workers; a pool that cannot serve is not a plan",
							poolCount, totalMB, pp.Name, pp.MaxChildren)
					}
					sum += pp.Bytes
				}

				if sum+budget.ReserveBytes > budget.TotalBytes {
					t.Errorf("%d pools / %dMB: allocated %s + %s reserved exceeds the %s budget",
						poolCount, totalMB,
						humanBytes(sum), humanBytes(budget.ReserveBytes), humanBytes(budget.TotalBytes))
				}
				if sum != plan.AllocatedBytes {
					t.Errorf("AllocatedBytes = %d but the pools sum to %d", plan.AllocatedBytes, sum)
				}
				if plan.FreeBytes < 0 {
					t.Errorf("%d pools / %dMB: FreeBytes is negative (%d)", poolCount, totalMB, plan.FreeBytes)
				}
			}
		}
	}
}

// TestDynamicSettingsSatisfyPHPFPM: PHP-FPM refuses to start when
// min_spare <= start_servers <= max_spare <= max_children does not hold, so an
// allocation that violates it takes the pool down on the reload that applies it.
func TestDynamicSettingsSatisfyPHPFPM(t *testing.T) {
	rng := rand.New(rand.NewSource(2))

	for trial := 0; trial < 300; trial++ {
		pools := []Pool{{
			Name:               "web",
			ProcessManager:     "dynamic",
			WorkerBytes:        int64(16+rng.Intn(200)) * mb,
			CurrentMaxChildren: 1 + rng.Intn(50),
			ObservedPeak:       rng.Intn(60),
			HitMaxChildren:     rng.Intn(2) == 0,
		}}

		plan, err := Compute(Budget{
			TotalBytes:   int64(128+rng.Intn(32000)) * mb,
			ReserveBytes: int64(rng.Intn(256)) * mb,
		}, pools, Options{})
		if err != nil {
			continue
		}

		p := plan.Pools[0]
		switch {
		case p.MinSpare < 1:
			t.Fatalf("min_spare_servers = %d", p.MinSpare)
		case p.MinSpare > p.StartServers:
			t.Fatalf("min_spare_servers (%d) > start_servers (%d)", p.MinSpare, p.StartServers)
		case p.StartServers > p.MaxSpare:
			t.Fatalf("start_servers (%d) > max_spare_servers (%d)", p.StartServers, p.MaxSpare)
		case p.MaxSpare > p.MaxChildren:
			t.Fatalf("max_spare_servers (%d) > max_children (%d)", p.MaxSpare, p.MaxChildren)
		}
	}
}

// TestStaticPoolsGetNoSpareSettings: pm.start_servers and the spare settings are
// only meaningful for dynamic. Writing them for a static pool is a config error.
func TestStaticPoolsGetNoSpareSettings(t *testing.T) {
	for _, pm := range []string{"static", "ondemand"} {
		plan := mustCompute(t, Budget{TotalBytes: 4096 * mb, ReserveBytes: 512 * mb}, []Pool{{
			Name: "w", ProcessManager: pm, WorkerBytes: 40 * mb, ObservedPeak: 10,
		}})

		p := plan.Pools[0]
		if p.StartServers != 0 || p.MinSpare != 0 || p.MaxSpare != 0 {
			t.Errorf("%s pool got dynamic settings: start=%d min=%d max=%d",
				pm, p.StartServers, p.MinSpare, p.MaxSpare)
		}
	}
}

// TestHeadroomMovesFromIdleToBusy is the multi-pool gain, and the reason this is
// an allocator rather than a per-pool calculator: a quiet site gives up the
// memory it is not using instead of holding it while a neighbour queues.
func TestHeadroomMovesFromIdleToBusy(t *testing.T) {
	// Both pools currently sized at 20. One is idle, one is saturated.
	plan := mustCompute(t, Budget{TotalBytes: 4096 * mb, ReserveBytes: 1024 * mb}, []Pool{
		{
			Name: "idle-blog", ProcessManager: "dynamic", WorkerBytes: 60 * mb,
			CurrentMaxChildren: 20, ObservedPeak: 2,
		},
		{
			Name: "busy-shop", ProcessManager: "dynamic", WorkerBytes: 60 * mb,
			CurrentMaxChildren: 20, ObservedPeak: 20, QueueDepth: 12, HitMaxChildren: true,
		},
	})

	idle, busy := plan.Pools[0], plan.Pools[1]

	if idle.MaxChildren >= 20 {
		t.Errorf("the idle pool kept %d workers despite peaking at 2; its headroom never moved",
			idle.MaxChildren)
	}
	if busy.MaxChildren <= 20 {
		t.Errorf("the saturated pool stayed at %d despite queueing", busy.MaxChildren)
	}
	if plan.CapacityExhausted {
		t.Error("capacity reported exhausted while a rebalance was available")
	}
}

// TestCapacityExhaustedOnlyWhenNothingCanBeGiven: the distinction the metrics
// are built on. Unmet demand with budget free is routine; unmet demand with
// nothing left is the signal that no configuration change helps.
func TestCapacityExhaustedOnlyWhenNothingCanBeGiven(t *testing.T) {
	saturated := Pool{
		Name: "shop", ProcessManager: "dynamic", WorkerBytes: 100 * mb,
		CurrentMaxChildren: 8, ObservedPeak: 8, QueueDepth: 40, HitMaxChildren: true,
	}

	t.Run("room to grow", func(t *testing.T) {
		plan := mustCompute(t, Budget{TotalBytes: 8192 * mb, ReserveBytes: 512 * mb}, []Pool{saturated})
		if plan.CapacityExhausted {
			t.Errorf("exhausted with %s free", humanBytes(plan.FreeBytes))
		}
	})

	t.Run("no room left", func(t *testing.T) {
		// Enough for the floor and essentially nothing more.
		plan := mustCompute(t, Budget{TotalBytes: 700 * mb, ReserveBytes: 400 * mb}, []Pool{saturated})
		if !plan.CapacityExhausted {
			t.Errorf("a saturated pool with %s free was not reported as a capacity problem",
				humanBytes(plan.FreeBytes))
		}
		if len(plan.Warnings) == 0 {
			t.Error("capacity exhaustion produced no warning for the operator")
		}
	})
}

// TestFloorsAreSatisfiedBeforeDemand: a busy pool must not starve a quiet one
// into being unable to serve at all. Ordering, not arithmetic.
func TestFloorsAreSatisfiedBeforeDemand(t *testing.T) {
	plan := mustCompute(t, Budget{TotalBytes: 2048 * mb, ReserveBytes: 512 * mb}, []Pool{
		{
			Name: "hungry", ProcessManager: "dynamic", WorkerBytes: 50 * mb,
			CurrentMaxChildren: 30, ObservedPeak: 30, QueueDepth: 99, HitMaxChildren: true,
		},
		{Name: "small-a", ProcessManager: "dynamic", WorkerBytes: 50 * mb, ObservedPeak: 0, Floor: 3},
		{Name: "small-b", ProcessManager: "dynamic", WorkerBytes: 50 * mb, ObservedPeak: 0, Floor: 3},
	})

	for _, p := range plan.Pools[1:] {
		if p.MaxChildren < 3 {
			t.Errorf("%s got %d workers, below its floor of 3: a hungry neighbour starved it",
				p.Name, p.MaxChildren)
		}
	}
}

// TestOversubscribedHostIsReportedNotRefused: when the floors alone do not fit,
// returning an error would leave whatever is currently configured in place —
// which is the situation that produced the OOM. Reduce, warn, and say so.
func TestOversubscribedHostIsReportedNotRefused(t *testing.T) {
	pools := make([]Pool, 20)
	for i := range pools {
		pools[i] = Pool{
			Name: fmt.Sprintf("site-%d", i), ProcessManager: "dynamic",
			WorkerBytes: 80 * mb, Floor: 5,
		}
	}

	// One worker each is 20 × 80MB = 1600MB, which fits in 2560MB; the floors
	// are 20 × 5 × 80MB = 8000MB, which does not. That gap is this test: there
	// IS a configuration, just not a comfortable one.
	plan := mustCompute(t, Budget{TotalBytes: 3072 * mb, ReserveBytes: 512 * mb}, pools)

	if !plan.CapacityExhausted {
		t.Error("20 pools × 5 workers × 80MB into 2560MB was not reported as exhausted")
	}
	if len(plan.Warnings) == 0 {
		t.Error("no warning for an oversubscribed host")
	}

	var sum int64
	for _, p := range plan.Pools {
		if p.MaxChildren < 1 {
			t.Errorf("%s was reduced to %d workers", p.Name, p.MaxChildren)
		}
		sum += p.Bytes
	}
	if sum+512*mb > 3072*mb {
		t.Errorf("reduced allocation still exceeds the budget: %s", humanBytes(sum))
	}
}

// TestMeasuredAndEstimatedBothAllocate: a pool with no history still has to be
// sized, and the plan has to say which number it used so a reader knows how much
// to trust it.
func TestMeasuredAndEstimatedBothAllocate(t *testing.T) {
	plan := mustCompute(t, Budget{TotalBytes: 4096 * mb, ReserveBytes: 512 * mb}, []Pool{
		{Name: "known", ProcessManager: "dynamic", WorkerBytes: 90 * mb, Measured: true, ObservedPeak: 10},
		{Name: "new", ProcessManager: "dynamic", WorkerBytes: 40 * mb, Measured: false, ObservedPeak: 0},
	})

	if !plan.Pools[0].Measured {
		t.Error("a measured pool was not marked as such")
	}
	if plan.Pools[1].Measured {
		t.Error("a bootstrap estimate was reported as measured")
	}
	for _, p := range plan.Pools {
		if p.Reason == "" {
			t.Errorf("%s has no rationale; the plan output would not explain itself", p.Name)
		}
	}
}

// TestZeroWorkerCostIsRejected: nothing can be divided by a per-worker cost of
// zero, and silently treating it as free would allocate unlimited workers.
func TestZeroWorkerCostIsRejected(t *testing.T) {
	_, err := Compute(Budget{TotalBytes: 1024 * mb}, []Pool{{Name: "w", WorkerBytes: 0}}, Options{})
	if err == nil {
		t.Error("a pool with no per-worker cost was accepted")
	}
}

// TestBudgetSmallerThanItsReserve is a misconfiguration that must not produce a
// plan sized against a negative number.
func TestBudgetSmallerThanItsReserve(t *testing.T) {
	_, err := Compute(Budget{TotalBytes: 100 * mb, ReserveBytes: 512 * mb},
		[]Pool{{Name: "w", WorkerBytes: 40 * mb}}, Options{})
	if !errors.Is(err, ErrNoBudget) {
		t.Errorf("err = %v, want ErrNoBudget", err)
	}
}

// TestNoPoolsIsNotAnError: a host with PHP-FPM installed and nothing configured
// is a legitimate state, and the free budget is still worth reporting.
func TestNoPoolsIsNotAnError(t *testing.T) {
	plan, err := Compute(Budget{TotalBytes: 2048 * mb, ReserveBytes: 512 * mb}, nil, Options{})
	if err != nil {
		t.Fatalf("no pools: %v", err)
	}
	if plan.FreeBytes != 1536*mb {
		t.Errorf("FreeBytes = %s, want 1536MiB", humanBytes(plan.FreeBytes))
	}
}

// TestCeilingIsRespected: an operator cap outranks available budget.
func TestCeilingIsRespected(t *testing.T) {
	plan := mustCompute(t, Budget{TotalBytes: 65536 * mb, ReserveBytes: 1024 * mb}, []Pool{{
		Name: "capped", ProcessManager: "dynamic", WorkerBytes: 40 * mb,
		ObservedPeak: 200, QueueDepth: 50, HitMaxChildren: true, Ceiling: 12,
	}})

	if got := plan.Pools[0].MaxChildren; got > 12 {
		t.Errorf("max_children = %d, above the ceiling of 12", got)
	}
}

// TestDeterministic: the same inputs must produce the same plan. A plan that
// varies between runs would reload PHP-FPM for no reason.
func TestDeterministic(t *testing.T) {
	pools := []Pool{
		{Name: "a", ProcessManager: "dynamic", WorkerBytes: 55 * mb, ObservedPeak: 9, CurrentMaxChildren: 10},
		{Name: "b", ProcessManager: "dynamic", WorkerBytes: 130 * mb, ObservedPeak: 4, HitMaxChildren: true},
		{Name: "c", ProcessManager: "static", WorkerBytes: 22 * mb, ObservedPeak: 30, QueueDepth: 7},
	}
	budget := Budget{TotalBytes: 3072 * mb, ReserveBytes: 768 * mb}

	first := mustCompute(t, budget, pools)
	for i := 0; i < 20; i++ {
		again := mustCompute(t, budget, pools)
		for j := range first.Pools {
			if first.Pools[j] != again.Pools[j] {
				t.Fatalf("run %d differs for %s:\n  %+v\n  %+v", i, first.Pools[j].Name, first.Pools[j], again.Pools[j])
			}
		}
	}
}

func mustCompute(t *testing.T, b Budget, pools []Pool) Plan {
	t.Helper()

	plan, err := Compute(b, pools, Options{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	return plan
}

// TestCannotFitIsAnErrorNotAnOverAllocation: when a single worker per pool
// already exceeds the budget there is no configuration to write — PHP-FPM's own
// minimum is one worker. Producing a plan anyway would mean either exceeding the
// budget (the OOM this exists to prevent) or giving a pool zero workers (taking
// the site down for an OOM that has not happened).
func TestCannotFitIsAnErrorNotAnOverAllocation(t *testing.T) {
	// Two pools at 256MB per worker, 300MB allocatable.
	_, err := Compute(
		Budget{TotalBytes: 400 * mb, ReserveBytes: 100 * mb},
		[]Pool{
			{Name: "heavy-a", ProcessManager: "dynamic", WorkerBytes: 256 * mb},
			{Name: "heavy-b", ProcessManager: "dynamic", WorkerBytes: 256 * mb},
		},
		Options{},
	)

	if !errors.Is(err, ErrCannotFit) {
		t.Fatalf("err = %v, want ErrCannotFit", err)
	}
	// The message has to name the shortfall: an operator reading it needs to
	// know whether to add memory or remove a site.
	if !strings.Contains(err.Error(), "one worker each") {
		t.Errorf("error does not explain the shortfall: %v", err)
	}
}

// TestExactlyOneWorkerEachFits is the boundary immediately above: it fits, so it
// must produce a plan rather than an error.
func TestExactlyOneWorkerEachFits(t *testing.T) {
	plan := mustCompute(t,
		Budget{TotalBytes: 612 * mb, ReserveBytes: 100 * mb},
		[]Pool{
			{Name: "heavy-a", ProcessManager: "dynamic", WorkerBytes: 256 * mb},
			{Name: "heavy-b", ProcessManager: "dynamic", WorkerBytes: 256 * mb},
		},
	)

	for _, p := range plan.Pools {
		if p.MaxChildren != 1 {
			t.Errorf("%s got %d workers, want exactly 1", p.Name, p.MaxChildren)
		}
	}
	if !plan.CapacityExhausted {
		t.Error("a host reduced to one worker per pool was not reported as exhausted")
	}
}

// TestRoutineBacklogDoesNotCompoundIntoARunaway is the worst failure this
// package has had, and it was an outage the tool caused rather than prevented.
//
// PHP-FPM's listen queue is the instantaneous socket accept backlog; it reads
// 1-3 on any busy pool that is nowhere near its ceiling. Reading that as "out of
// workers" made every scrape look saturated, and because the growth base is the
// pool's own current size, each round multiplied the last: a pool whose real
// demand never exceeded 10 concurrent workers grew to 614 over nine rounds and
// took 24GiB of a 32GiB host. On a shared host it starved three pools that
// genuinely needed 25 workers down to 10 to pay for it.
func TestRoutineBacklogDoesNotCompoundIntoARunaway(t *testing.T) {
	current := 20
	for round := 0; round < 12; round++ {
		plan := mustComputeWith(t,
			Budget{TotalBytes: 32 << 30, ReserveBytes: 8 << 30, CPUs: 8},
			[]Pool{{
				Name: "noisy", ProcessManager: "dynamic", WorkerBytes: 40 * mb,
				CurrentMaxChildren: current,
				ObservedPeak:       10, // real demand, and it never moves
				QueueDepth:         1,  // a routine accept backlog
			}})
		current = plan.Pools[0].MaxChildren
	}

	if current > 20 {
		t.Errorf("twelve rounds of a 1-deep backlog grew the pool from 20 to %d "+
			"while demand stayed at 10", current)
	}
}

// TestGenuineSaturationStillGrows: the fix above must not disable growth. A pool
// whose concurrency is clamped AT its ceiling with requests queueing is really
// out of workers, and that is the case the growth branch exists for.
func TestGenuineSaturationStillGrows(t *testing.T) {
	current := 4
	for round := 0; round < 10; round++ {
		plan := mustComputeWith(t,
			Budget{TotalBytes: 32 << 30, ReserveBytes: 8 << 30, CPUs: 8},
			[]Pool{{
				Name: "busy", ProcessManager: "dynamic", WorkerBytes: 40 * mb,
				CurrentMaxChildren: current,
				ObservedPeak:       current, // clamped by the ceiling
				QueueDepth:         12,
				HitMaxChildren:     true,
			}})
		current = plan.Pools[0].MaxChildren
	}

	if current <= 4 {
		t.Errorf("a genuinely saturated pool stayed at %d over ten rounds", current)
	}
}

// TestCPUsBoundWhatMemoryWouldAllow: memory alone is not sufficient authority.
// A large host with a cheaply-measured worker authorised a max_children in the
// hundreds of thousands — and a pm.start_servers to match, which PHP-FPM accepts
// and then dies trying to honour.
func TestCPUsBoundWhatMemoryWouldAllow(t *testing.T) {
	plan := mustComputeWith(t,
		Budget{TotalBytes: 256 << 30, ReserveBytes: 64 << 30, CPUs: 2},
		[]Pool{{
			Name: "cheap", ProcessManager: "dynamic", WorkerBytes: 64 << 10,
			CurrentMaxChildren: 10, ObservedPeak: 500000,
			QueueDepth: 5, HitMaxChildren: true,
		}})

	p := plan.Pools[0]
	if p.MaxChildren > 2*50 {
		t.Errorf("max_children = %d on a 2-core host; memory was the only bound", p.MaxChildren)
	}
	if p.StartServers > p.MaxChildren {
		t.Errorf("start_servers %d exceeds max_children %d", p.StartServers, p.MaxChildren)
	}
}

// TestUnknownCPUCountKeepsTheOldBehaviour: a host whose core count could not be
// detected must still get a plan, bounded by memory alone.
func TestUnknownCPUCountKeepsTheOldBehaviour(t *testing.T) {
	plan := mustComputeWith(t,
		Budget{TotalBytes: 8 << 30, ReserveBytes: 2 << 30, CPUs: 0},
		[]Pool{{
			Name: "w", ProcessManager: "dynamic", WorkerBytes: 50 * mb, ObservedPeak: 20,
		}})

	if plan.Pools[0].MaxChildren < 20 {
		t.Errorf("max_children = %d with an unknown core count; memory allowed far more",
			plan.Pools[0].MaxChildren)
	}
}

func mustComputeWith(t *testing.T, b Budget, pools []Pool) Plan {
	t.Helper()

	plan, err := Compute(b, pools, Options{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	return plan
}

// TestAnEstimatedPoolIsNotCutToPayForAMeasuredOne.
//
// An unmeasured pool's floor is its own current setting, held there so a first
// run can only ever help, and its cost is a profile estimate rather than an
// observation. Scaling every floor uniformly cut healthy pools on that guess: a
// first install on a tight host, with real workers cheaper than the profile
// assumes, queued traffic on sites nobody had any evidence to touch.
func TestAnEstimatedPoolIsNotCutToPayForAMeasuredOne(t *testing.T) {
	plan, err := Compute(
		Budget{TotalBytes: 1200 * mb},
		[]Pool{
			// Measured, and expensive: this is what there is evidence to reduce.
			{Name: "measured", CurrentMaxChildren: 20, WorkerBytes: 100 * mb, Measured: true,
				ProcessManager: "dynamic", Floor: 20},
			// Still an estimate, sitting at what the operator configured.
			{Name: "guessed", CurrentMaxChildren: 10, WorkerBytes: 48 * mb,
				ProcessManager: "dynamic", Floor: 10},
		},
		Options{},
	)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	byName := map[string]PoolPlan{}
	for _, pp := range plan.Pools {
		byName[pp.Name] = pp
	}

	if got := byName["guessed"].MaxChildren; got < 10 {
		t.Errorf("the pool whose cost is still a guess was cut from 10 to %d to pay for "+
			"one whose cost is known; there is no evidence it needed touching", got)
	}
	if got := byName["measured"].MaxChildren; got >= 20 {
		t.Errorf("the measured pool was not reduced (%d); something has to give when the "+
			"budget does not fit", got)
	}

	var total int64
	for _, pp := range plan.Pools {
		total += pp.Bytes
	}
	if total > plan.TotalBytes-plan.ReserveBytes {
		t.Errorf("the plan commits %d of %d bytes", total, plan.TotalBytes-plan.ReserveBytes)
	}
}

// TestEverythingGivesWayWhenTheMeasuredPoolsAreNotEnough: the protection above
// cannot become a refusal to fit. If reducing every measured pool to one worker
// still does not fit, the estimated ones have to give too — and the plan says so.
func TestEverythingGivesWayWhenTheMeasuredPoolsAreNotEnough(t *testing.T) {
	plan, err := Compute(
		Budget{TotalBytes: 300 * mb},
		[]Pool{
			{Name: "measured", CurrentMaxChildren: 20, WorkerBytes: 100 * mb, Measured: true,
				ProcessManager: "dynamic", Floor: 20},
			{Name: "guessed", CurrentMaxChildren: 20, WorkerBytes: 48 * mb,
				ProcessManager: "dynamic", Floor: 20},
		},
		Options{},
	)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	var total int64
	for _, pp := range plan.Pools {
		total += pp.Bytes
	}
	if total > plan.TotalBytes-plan.ReserveBytes {
		t.Errorf("the plan commits %d of %d bytes; nothing gave way",
			total, plan.TotalBytes-plan.ReserveBytes)
	}
	if len(plan.Warnings) == 0 {
		t.Error("pools were cut below what anyone asked for and the plan says nothing")
	}
}
