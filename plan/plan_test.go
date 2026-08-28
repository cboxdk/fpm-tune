package plan

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
)

const (
	mb = 1024 * 1024
	gb = 1024 * mb
)

// TestBootstrapUntilTrusted is the switch this package exists to make.
//
// Sizing a pool DOWN on a number that has not been watched through a real
// traffic pattern is how a tool like this causes the outage it was installed to
// prevent. Until the baseline is trusted the profile estimate stands — the same
// guess a hand-written config makes, so bootstrapping is never worse than what
// it replaces.
func TestBootstrapUntilTrusted(t *testing.T) {
	view := observe.PoolView{
		Name: "shop", ProcessManager: "dynamic",
		CurrentMaxChildren: 10, MaxChildrenKnown: true, ObservedPeak: 6,
	}

	t.Run("no history", func(t *testing.T) {
		res := build(t, state.New(), view)

		if len(res.Bootstrapped) != 1 || res.Bootstrapped[0] != "shop" {
			t.Errorf("Bootstrapped = %v, want [shop]", res.Bootstrapped)
		}
		if res.Plan.Pools[0].Measured {
			t.Error("a pool with no history was reported as measured")
		}
	})

	t.Run("history, not yet trusted", func(t *testing.T) {
		st := state.New()
		// Real observations, but only a handful over a couple of minutes.
		for i := 0; i < 3; i++ {
			st.Learn(busy("shop", 200*mb, time.Now().Add(time.Duration(i)*time.Minute)), state.Options{})
		}

		res := build(t, st, view)

		if res.Plan.Pools[0].Measured {
			t.Error("three samples over two minutes was treated as a trusted baseline")
		}
		// And crucially the 200MB observation is NOT used, or the pool would be
		// sized on a number nobody has confidence in yet.
		if got := res.Plan.Pools[0].Bytes / int64(res.Plan.Pools[0].MaxChildren); got >= 200*mb {
			t.Errorf("per-worker cost %dMB came from the untrusted baseline", got/mb)
		}
	})

	t.Run("trusted", func(t *testing.T) {
		st := state.New()
		base := time.Now()
		for i := 0; i < 25; i++ {
			st.Learn(busy("shop", 200*mb, base.Add(time.Duration(i)*2*time.Minute)), state.Options{})
		}

		res := build(t, st, view)

		if !res.Plan.Pools[0].Measured {
			t.Fatal("a pool watched for 50 minutes was still bootstrapping")
		}
		if len(res.Bootstrapped) != 0 {
			t.Errorf("Bootstrapped = %v, want empty", res.Bootstrapped)
		}
		perWorker := res.Plan.Pools[0].Bytes / int64(res.Plan.Pools[0].MaxChildren)
		if perWorker < 190*mb || perWorker > 210*mb {
			t.Errorf("per-worker cost %dMB, want the measured ~200MB", perWorker/mb)
		}
	})
}

// TestUnreachablePoolKeepsItsAllocation: a pool that is restarting must not have
// its memory handed to its neighbours. Treating "could not read" as "idle" is
// how a deploy turns into a cascade.
func TestUnreachablePoolKeepsItsAllocation(t *testing.T) {
	res := build(t, state.New(),
		observe.PoolView{
			Name: "restarting", ProcessManager: "dynamic",
			CurrentMaxChildren: 20, MaxChildrenKnown: true, Err: errors.New("connection refused"),
		},
		observe.PoolView{
			Name: "healthy", ProcessManager: "dynamic",
			CurrentMaxChildren: 5, MaxChildrenKnown: true, ObservedPeak: 5, QueueDepth: 10,
		},
	)

	if len(res.Unreachable) != 1 || res.Unreachable[0] != "restarting" {
		t.Errorf("Unreachable = %v, want [restarting]", res.Unreachable)
	}

	var down *int
	for i, p := range res.Plan.Pools {
		if p.Name == "restarting" {
			down = &res.Plan.Pools[i].MaxChildren
		}
	}
	if down == nil {
		t.Fatal("the unreachable pool disappeared from the plan")
	}
	if *down < 20 {
		t.Errorf("the restarting pool was cut from 20 to %d while it was down", *down)
	}
}

// TestHitCeilingUsesTheDelta: max_children_reached is a running total since the
// master started. A pool that ran out once last Tuesday should not still be
// growing because of it.
func TestHitCeilingUsesTheDelta(t *testing.T) {
	tests := map[string]struct {
		lastSeen int64
		now      int64
		queue    int64
		peak     int
		maxKids  int
		want     bool
	}{
		"first run, never hit": {0, 0, 0, 0, 10, false},
		"first run, has hit":   {0, 7, 0, 0, 10, true},
		"hit again since":      {7, 9, 0, 0, 10, true},
		"unchanged since":      {7, 7, 0, 0, 10, false},
		"old history only":     {40, 40, 0, 0, 10, false},

		// A backlog is the instantaneous accept queue, and it reads 1-3 on any
		// busy pool that is nowhere near its ceiling. Treating that alone as
		// saturation compounded into a pool with real demand of 10 growing to
		// 614 workers, starving its neighbours. It counts only when the pool is
		// ALSO at its ceiling.
		"queue backing, not at the ceiling": {0, 0, 3, 4, 20, false},
		"queue backing, at the ceiling":     {0, 0, 3, 20, 20, true},
		"queue backing, ceiling unknown":    {0, 0, 3, 0, 0, false},

		// The counter reset still counts on its own — that path does not depend
		// on the queue.
		"counter reset, saturated since": {40, 3, 0, 0, 10, true},
		"counter reset, quiet since":     {40, 0, 0, 0, 10, false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var ps *state.PoolState
			if tt.lastSeen > 0 {
				ps = &state.PoolState{LastMaxChildrenReached: tt.lastSeen}
			}

			got := hitCeiling(observe.PoolView{
				MaxChildrenReached: tt.now, QueueDepth: tt.queue,
				ObservedPeak: tt.peak, CurrentMaxChildren: tt.maxKids,
			}, ps)

			if got != tt.want {
				t.Errorf("hitCeiling = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReserveScalesWithTheHost: a quarter of a big machine is plenty; a quarter
// of a small one is not enough to run an operating system.
func TestReserveScalesWithTheHost(t *testing.T) {
	profile := DefaultProfile

	t.Run("large host uses the fraction", func(t *testing.T) {
		got, why := reserveFor(budget.Limits{MemoryBytes: 32 * gb}, profile, 0)
		if got != 8*gb {
			t.Errorf("reserve = %s, want 8GiB", budget.HumanBytes(got))
		}
		if !strings.Contains(why, "25%") {
			t.Errorf("reason %q does not explain the fraction", why)
		}
	})

	t.Run("small host uses the floor", func(t *testing.T) {
		got, why := reserveFor(budget.Limits{MemoryBytes: 512 * mb}, profile, 0)
		if got != 256*mb {
			t.Errorf("reserve = %s, want the 256MiB floor", budget.HumanBytes(got))
		}
		if !strings.Contains(why, "floor") {
			t.Errorf("reason %q does not mention the floor", why)
		}
	})

	t.Run("host smaller than the floor keeps half", func(t *testing.T) {
		// Reserving the whole host would allocate nothing at all; half lets the
		// allocator report the real problem instead.
		got, _ := reserveFor(budget.Limits{MemoryBytes: 128 * mb}, profile, 0)
		if got != 64*mb {
			t.Errorf("reserve = %s, want half of 128MiB", budget.HumanBytes(got))
		}
		if got >= 128*mb {
			t.Error("the entire host was reserved, leaving nothing to allocate")
		}
	})

	t.Run("explicit override wins", func(t *testing.T) {
		got, why := reserveFor(budget.Limits{MemoryBytes: 32 * gb}, profile, 2*gb)
		if got != 2*gb {
			t.Errorf("reserve = %s, want the 2GiB override", budget.HumanBytes(got))
		}
		if !strings.Contains(why, "explicit") {
			t.Errorf("reason %q does not say it was set explicitly", why)
		}
	})
}

// TestLearnFromSkipsUnreachablePools: a pool that could not be read has nothing
// to teach, and recording it would count as a sample toward confidence.
func TestLearnFromSkipsUnreachablePools(t *testing.T) {
	st := state.New()

	LearnFrom(st, []observe.PoolView{
		{Name: "down", Err: errors.New("refused")},
		{Name: "up", Workers: []state.WorkerSample{
			{RSSBytes: 90 * mb, Requests: 400},
			{RSSBytes: 95 * mb, Requests: 400},
		}, MaxChildrenReached: 12},
	}, time.Now(), state.Options{})

	if _, exists := st.Pools["down"]; exists {
		t.Error("an unreachable pool was recorded as a sample")
	}
	up := st.Pools["up"]
	if up == nil || up.BusySamples != 1 {
		t.Fatalf("the healthy pool was not learned from: %+v", up)
	}
	if up.LastMaxChildrenReached != 12 {
		t.Errorf("LastMaxChildrenReached = %d, want 12: the next run has nothing to compare against",
			up.LastMaxChildrenReached)
	}
}

// TestRenderExplainsItself: this tool proposes changes to a running server. An
// operator who cannot see why a pool is being cut has no basis for allowing it.
func TestRenderExplainsItself(t *testing.T) {
	st := state.New()
	base := time.Now()
	for i := 0; i < 25; i++ {
		st.Learn(busy("measured-pool", 100*mb, base.Add(time.Duration(i)*2*time.Minute)), state.Options{})
	}

	res := build(t, st,
		observe.PoolView{Name: "measured-pool", ProcessManager: "dynamic", CurrentMaxChildren: 30, MaxChildrenKnown: true, ObservedPeak: 4},
		observe.PoolView{Name: "new-pool", ProcessManager: "dynamic", CurrentMaxChildren: 5, MaxChildrenKnown: true, ObservedPeak: 5},
	)

	var out strings.Builder
	if err := res.Render(&out); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := out.String()

	for _, want := range []string{
		"measured-pool", "new-pool",
		"NOW", "PLAN", "WHY",
		"measured",             // the sizing source is stated
		"Estimated, not yet",   // the bootstrap pool is flagged
		"available to workers", // the budget is shown, not just the result
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered plan does not mention %q:\n%s", want, got)
		}
	}

	// The current setting has to appear beside the proposal, or there is nothing
	// to judge the proposal against.
	if !strings.Contains(got, "30") {
		t.Errorf("the pool's current 30 workers are not shown:\n%s", got)
	}
}

// TestRenderHandlesAnEmptyHost without panicking on the tabwriter.
func TestRenderHandlesAnEmptyHost(t *testing.T) {
	res, err := Build(Input{Limits: budget.Limits{MemoryBytes: 4 * gb}})
	if err != nil {
		t.Fatalf("Build with no pools: %v", err)
	}

	var out strings.Builder
	if err := res.Render(&out); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out.String(), "No PHP-FPM pools") {
		t.Errorf("an empty host rendered as:\n%s", out.String())
	}
}

func build(t *testing.T, st *state.State, views ...observe.PoolView) Result {
	t.Helper()

	res, err := Build(Input{
		Limits: budget.Limits{MemoryBytes: 8 * gb, CPUs: 4, Source: budget.SourceMemInfo},
		Views:  views,
		State:  st,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	return res
}

func busy(pool string, rss int64, at time.Time) state.Observation {
	return state.Observation{
		Pool: pool, At: at,
		Workers: []state.WorkerSample{
			{RSSBytes: rss, Requests: 500},
			{RSSBytes: rss, Requests: 500},
		},
	}
}

// TestBootstrappingPoolIsNeverCut is the asymmetry that makes a first run safe.
//
// max_active_processes is a high-water mark since the master started, so a pool
// observed for thirty seconds looks idle whatever it does at nine in the
// morning. A tool that cuts a site from twenty workers to two on that evidence
// causes exactly the outage it was installed to prevent — so while a pool is
// bootstrapping it may grow, but it may not shrink.
func TestBootstrappingPoolIsNeverCut(t *testing.T) {
	// Looks idle: peaked at 2 while configured for 30.
	view := observe.PoolView{
		Name: "quiet-looking", ProcessManager: "dynamic",
		CurrentMaxChildren: 30, MaxChildrenKnown: true, ObservedPeak: 2,
	}

	t.Run("bootstrapping holds the line", func(t *testing.T) {
		res := build(t, state.New(), view)

		if got := res.Plan.Pools[0].MaxChildren; got < 30 {
			t.Errorf("an unmeasured pool was cut from 30 to %d on thirty seconds of evidence", got)
		}
	})

	t.Run("trusted may cut", func(t *testing.T) {
		st := state.New()
		base := time.Now()
		for i := 0; i < 25; i++ {
			st.Learn(busy("quiet-looking", 40*mb, base.Add(time.Duration(i)*2*time.Minute)), state.Options{})
		}

		res := build(t, st, view)

		if got := res.Plan.Pools[0].MaxChildren; got >= 30 {
			t.Errorf("a pool measured for 50 minutes as needing 2 workers kept %d; "+
				"its headroom can never move to a neighbour", got)
		}
	})
}

// TestBootstrappingPoolMayStillGrow: holding the floor must not also cap it. A
// new pool that is queueing needs help on the first run, not after an hour.
func TestBootstrappingPoolMayStillGrow(t *testing.T) {
	res := build(t, state.New(), observe.PoolView{
		Name: "struggling", ProcessManager: "dynamic",
		CurrentMaxChildren: 4, MaxChildrenKnown: true, ObservedPeak: 4,
		QueueDepth: 25, MaxChildrenReached: 60,
	})

	if got := res.Plan.Pools[0].MaxChildren; got <= 4 {
		t.Errorf("a queueing pool stayed at %d on its first run", got)
	}
}

// TestUnreadableConfigIsTreatedAsUnknownNotZero.
//
// Parsing the effective configuration shells out to php-fpm, which fails for
// reasons that have nothing to do with the pool being sized — an unrelated
// include with a syntax error, the binary mid-upgrade, a permissions problem.
// The result was zero, which is also what "this pool allows no workers" looks
// like. They are opposite situations and were indistinguishable.
//
// The consequence was not subtle: the unknown deleted the bootstrap floor, so a
// pool configured for 40 workers was resized on a reading that did not exist —
// and if it happened to be idle at that moment it also looked brand new and was
// handed a share of the entire budget.
func TestUnreadableConfigIsTreatedAsUnknownNotZero(t *testing.T) {
	t.Run("config readable", func(t *testing.T) {
		res := build(t, state.New(), observe.PoolView{
			Name: "web", ProcessManager: "dynamic",
			CurrentMaxChildren: 40, MaxChildrenKnown: true, ObservedPeak: 3,
		})

		if got := res.Plan.Pools[0].MaxChildren; got != 40 {
			t.Errorf("max_children = %d; a bootstrapping pool must keep its 40", got)
		}
	})

	t.Run("config unreadable is never written", func(t *testing.T) {
		res := build(t, state.New(), observe.PoolView{
			// Same pool, same traffic; php-fpm -tt just failed this round.
			Name: "web", ProcessManager: "dynamic",
			CurrentMaxChildren: 0, MaxChildrenKnown: false, ObservedPeak: 3,
		})

		// The plan still reserves memory for it — a neighbour must not take it
		// while the pool is merely unreadable — but it is marked so nothing is
		// written. Proposing a ceiling requires knowing the one being replaced.
		if !res.Plan.Pools[0].Unknown {
			t.Error("a pool with an unreadable configuration was not marked; " +
				"apply would resize it on a reading that does not exist")
		}
		if res.Plan.Pools[0].Reason == "" {
			t.Error("no rationale; the operator cannot see why nothing happened")
		}
	})

	t.Run("unreadable and idle is not a new pool", func(t *testing.T) {
		res := build(t, state.New(), observe.PoolView{
			Name: "web", ProcessManager: "dynamic",
			CurrentMaxChildren: 0, MaxChildrenKnown: false, ObservedPeak: 0,
		})

		// A genuinely new pool would be seeded a share of the budget. This one
		// must not be — we know nothing about it, which is not the same as
		// knowing it is new.
		if got := res.Plan.Pools[0].MaxChildren; got > 10 {
			t.Errorf("an unreadable idle pool was seeded %d workers as if it were new", got)
		}
	})
}

// TestAPoolWithNoHistoryKeepsWhatItHas documents the real cold start.
//
// There was briefly a seedColdPools step here that handed a pool with "no
// evidence at all" a share of the whole budget, because a probe showed the
// allocator proposing two workers where memory allowed sixty-four. That probe
// was built by hand with CurrentMaxChildren of zero — and PHP-FPM requires
// pm.max_children for every pool, so a readable configuration always has one.
// The case it fixed cannot occur; zero means the config could not be read, which
// is now handled as Unknown and is the opposite of "help yourself to the budget".
//
// The genuine cold start is this: a container boots with whatever ceiling its
// image ships, no history, and the pool keeps that ceiling until there is a
// reason to change it.
func TestAPoolWithNoHistoryKeepsWhatItHas(t *testing.T) {
	res := build(t, state.New(), observe.PoolView{
		Name: "www", ProcessManager: "dynamic",
		CurrentMaxChildren: 5, MaxChildrenKnown: true, ObservedPeak: 0,
	})

	if got := res.Plan.Pools[0].MaxChildren; got != 5 {
		t.Errorf("max_children = %d on first sight of a pool configured for 5; "+
			"an unmeasured pool is neither cut nor inflated", got)
	}

	// And it can still grow the moment it shows it needs to.
	saturated := build(t, state.New(), observe.PoolView{
		Name: "www", ProcessManager: "dynamic",
		CurrentMaxChildren: 5, MaxChildrenKnown: true, ObservedPeak: 5,
		QueueDepth: 20, MaxChildrenReached: 40,
	})
	if got := saturated.Plan.Pools[0].MaxChildren; got <= 5 {
		t.Errorf("a saturated pool stayed at %d on its first round", got)
	}
}
