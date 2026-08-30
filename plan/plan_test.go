package plan

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
	"github.com/cboxdk/phpfpm"
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

	t.Run("history, not yet trusted: the cost is used, the pool is not cut", func(t *testing.T) {
		st := state.New()
		// Real observations, but only a handful over a couple of minutes.
		for i := 0; i < 3; i++ {
			st.Learn(busy("shop", 200*mb, time.Now().Add(time.Duration(i)*time.Minute)), state.Options{})
		}

		res := build(t, st, view)

		// Two separate questions, and this test used to conflate them.
		//
		// What a worker COSTS is answered by whatever measurement exists: 200MB
		// taken from this pool's own workers beats a profile's guess whatever the
		// confidence, because confidence is about how much of the traffic pattern
		// has been seen, not about whether the bytes were real. Falling back to
		// the profile's 48MB here is how a pool gets grown into three times the
		// memory it fits in.
		perWorker := res.Plan.Pools[0].Bytes / int64(res.Plan.Pools[0].MaxChildren)
		if perWorker < 190*mb || perWorker > 210*mb {
			t.Errorf("per-worker cost %dMB; the measurement this pool actually produced "+
				"was discarded for a profile estimate", perWorker/mb)
		}

		// Whether the pool may be CUT is what confidence is for, and three
		// samples over two minutes is no basis for taking workers away.
		if res.Plan.Pools[0].MaxChildren < view.CurrentMaxChildren {
			t.Errorf("a pool with two minutes of history was cut from %d to %d",
				view.CurrentMaxChildren, res.Plan.Pools[0].MaxChildren)
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

	views := []observe.PoolView{
		{Name: "down", Err: errors.New("refused")},
		{Name: "up", Workers: []state.WorkerSample{
			{RSSBytes: 90 * mb, Requests: 400},
			{RSSBytes: 95 * mb, Requests: 400},
		}, MaxChildrenReached: 12},
	}
	LearnFrom(st, views, time.Now(), state.Options{})
	RecordCounters(st, views)

	if _, exists := st.Pools["down"]; exists {
		t.Error("an unreachable pool was recorded as a sample")
	}
	// Its measured cost, not its BusySamples. A single observation has no
	// interval behind it, so there is no request RATE yet and nothing has been
	// established about whether the pool was working — but its workers were read,
	// and that reading is what the allocator sizes against.
	up := st.Pools["up"]
	if up == nil || up.TypicalPeakBytes == 0 {
		t.Fatalf("the healthy pool was not learned from: %+v", up)
	}
	if up.LastMaxChildrenReached != 12 {
		t.Errorf("LastMaxChildrenReached = %d, want 12: the next run has nothing to compare against",
			up.LastMaxChildrenReached)
	}
	if _, exists := st.Pools["down"]; exists {
		t.Error("an unreachable pool had its counter recorded")
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

// busy is a pool under load, and the request counter is what makes it so. A
// smaller worker on a pool serving nothing is an idle worker, not a cheap one,
// and the learner will not take a baseline down on it.
func busy(pool string, rss int64, at time.Time) state.Observation {
	return state.Observation{
		Pool: pool, At: at, ActiveNow: 4,
		Accepted: at.Unix() * 100,
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

// TestCounterSignalActuallyFires is a signal that had never worked.
//
// LearnFrom stored LastMaxChildrenReached and Build then compared the current
// reading against it — but both ran in the same round, LearnFrom first, so the
// stored value WAS the current reading and the delta was always zero. The
// counter half of saturation detection never fired once since it was written,
// and nothing noticed because the listen queue covered for it.
func TestCounterSignalActuallyFires(t *testing.T) {
	st := state.New()
	views := []observe.PoolView{{
		Name: "web", ProcessManager: "dynamic",
		CurrentMaxChildren: 4, MaxChildrenKnown: true,
		ObservedPeak: 4, QueueDepth: 0, // the queue is empty: the counter is the only signal
		MaxChildrenReached: 10,
		Workers: []state.WorkerSample{
			{RSSBytes: 50 << 20, Requests: 400},
			{RSSBytes: 50 << 20, Requests: 400},
		},
	}}

	// Round one, in the order the loop actually runs them.
	LearnFrom(st, views, time.Now(), state.Options{})
	RecordCounters(st, views)

	// Round two: the pool ran out of workers seven more times since.
	views[0].MaxChildrenReached = 17
	LearnFrom(st, views, time.Now().Add(time.Minute), state.Options{})

	if !hitCeiling(views[0], st.Pools["web"]) {
		t.Error("a pool that hit its ceiling seven more times was not detected; " +
			"the counter was compared against itself")
	}

	RecordCounters(st, views)

	// Round three: quiet since.
	if hitCeiling(views[0], st.Pools["web"]) {
		t.Error("a pool that has not hit its ceiling since the last round was " +
			"reported as saturated")
	}
}

// TestAnUnreachablePoolKeepsItsMemory.
//
// The failure this whole tool exists to prevent, reached from the one direction
// nobody was watching. A site configured for twenty workers restarts and its
// socket refuses for a few seconds. The scrape fails, and the view built from
// that failure used to carry nothing at all — so the allocator reserved nothing
// for it, handed the memory to its neighbours, and reloaded them with larger
// ceilings. The moment the pool came back and started forking, the host was
// committed well past its budget.
//
// The reservation logic was there and correct; it was being fed zero.
func TestAnUnreachablePoolKeepsItsMemory(t *testing.T) {
	const worker = 100 * mb

	// "down" is configured for 20 workers and cannot be reached. "up" is busy
	// and would happily take everything going.
	views := []observe.PoolView{
		{
			Name: "down", ProcessManager: "dynamic",
			CurrentMaxChildren: 20, MaxChildrenKnown: true,
			Err: errors.New("connection refused"),
		},
		{
			Name: "up", ProcessManager: "dynamic",
			CurrentMaxChildren: 10, MaxChildrenKnown: true,
			ObservedPeak: 10, QueueDepth: 5, MaxChildrenReached: 40,
			Workers: []state.WorkerSample{
				{RSSBytes: worker, Requests: 500},
				{RSSBytes: worker, Requests: 500},
			},
		},
	}

	st := state.New()
	LearnFrom(st, views, time.Now(), state.Options{})

	result, err := Build(Input{
		Limits: budget.Limits{MemoryBytes: 4 * gb, CPUs: 8},
		Views:  views,
		State:  st,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var down, up allocate.PoolPlan
	for _, pp := range result.Plan.Pools {
		switch pp.Name {
		case "down":
			down = pp
		case "up":
			up = pp
		}
	}

	if down.MaxChildren < 20 {
		t.Errorf("the unreachable pool was allocated %d workers against the 20 it is "+
			"configured for; its memory has been given away", down.MaxChildren)
	}
	if !down.Unknown {
		t.Error("the unreachable pool was not marked unwritable; a pool that could not " +
			"be read must never be resized")
	}
	if up.Bytes+down.Bytes > result.Plan.TotalBytes-result.Plan.ReserveBytes {
		t.Errorf("the plan commits %d bytes of a %d byte budget",
			up.Bytes+down.Bytes, result.Plan.TotalBytes-result.Plan.ReserveBytes)
	}
}

// TestAnUnreachablePoolIsNotReducibleEvenWhenTrusted.
//
// A pool that cannot be written must not have its memory taken either. It was
// being trimmed below the floor reserved for it whenever its baseline happened
// to be trusted — and apply then refuses to WRITE it, because setting a ceiling
// requires knowing the one being replaced. So the memory went to a neighbour
// that did get written, and the unreachable pool came back at the size it always
// had. The host overcommitted, by a pool nobody touched.
func TestAnUnreachablePoolIsNotReducibleEvenWhenTrusted(t *testing.T) {
	st := state.New()
	base := time.Now().Add(-2 * time.Hour)
	for i := 0; i < 30; i++ {
		st.Learn(busy("down", 100*mb, base.Add(time.Duration(i)*2*time.Minute)), state.Options{})
	}
	if !st.Pools["down"].Trusted(state.Options{}.Defaults()) {
		t.Fatal("setting up: the pool was meant to be trusted")
	}

	// Oversubscribed on purpose: the guard only matters when the floors do not
	// fit and something has to give. With room to spare nothing is cut and the
	// test would pass however the code behaved.
	res, err := Build(Input{
		Limits: budget.Limits{MemoryBytes: 3 * gb, CPUs: 4, Source: budget.SourceMemInfo},
		State:  st,
		Views: []observe.PoolView{
			{
				Name: "down", ProcessManager: "dynamic",
				CurrentMaxChildren: 20, MaxChildrenKnown: true,
				Err: errors.New("connection refused"),
			},
			{
				Name: "busy", ProcessManager: "dynamic",
				CurrentMaxChildren: 30, MaxChildrenKnown: true,
				ObservedPeak: 30, QueueDepth: 20, MaxChildrenReached: 500,
			},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var down allocate.PoolPlan
	for _, pp := range res.Plan.Pools {
		if pp.Name == "down" {
			down = pp
		}
	}

	if !down.Unknown {
		t.Fatal("an unreachable pool was not marked unwritable")
	}
	if down.MaxChildren < 20 {
		t.Errorf("a trusted but unreachable pool was cut from 20 to %d; nothing can be "+
			"written for it, so its memory goes to a neighbour that CAN be written and "+
			"the pool comes back at 20 regardless", down.MaxChildren)
	}
}

// TestAnIdleFirstSightingDoesNotUnderpriceThePool.
//
// PHP workers give large allocations back to the operating system, so a pool
// that has been quiet for an hour reads far smaller than it costs under load. If
// the first time this tool ever sees a pool is at three in the morning, that
// small reading is what it establishes — the learner has nothing to compare it
// against, so its refusal to LOWER an estimate on idle evidence cannot help.
//
// The reading is then divided into the budget. A pool configured for 40 workers
// accounted for at 12MiB each reserves 480MiB; its real cost is 4.7GiB. The
// difference is handed to the neighbours, who are grown into it and reloaded,
// and the host is committed past its memory the moment the site wakes up.
func TestAnIdleFirstSightingDoesNotUnderpriceThePool(t *testing.T) {
	st := state.New()

	// Two mature workers — they served traffic earlier in the day — read while
	// the pool is serving nothing at all.
	base := time.Now().Add(-30 * time.Minute)
	for i := 0; i < 30; i++ {
		st.Learn(state.Observation{
			Pool: "shop", At: base.Add(time.Duration(i) * time.Minute),
			ActiveNow: 0,
			Accepted:  500, // frozen
			Workers: []state.WorkerSample{
				{RSSBytes: 12 * mb, Requests: 400},
				{RSSBytes: 12 * mb, Requests: 380},
			},
		}, state.Options{})
	}

	res, err := Build(Input{
		Limits: budget.Limits{MemoryBytes: 8 * gb, CPUs: 8, Source: budget.SourceMemInfo},
		State:  st,
		Views: []observe.PoolView{
			{
				Name: "shop", ProcessManager: "dynamic",
				CurrentMaxChildren: 40, MaxChildrenKnown: true,
				Workers: []state.WorkerSample{{RSSBytes: 12 * mb, Requests: 400}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := res.Plan.Pools[0].WorkerBytes
	if got <= 12*mb {
		t.Errorf("the pool is accounted for at %dMiB a worker on evidence gathered while "+
			"it served nothing; its busy workers cost ten times that, and the difference "+
			"has just been given to its neighbours", got/mb)
	}
}

// TestAMeasuredCheapPoolIsStillBelieved: the floor above must not swallow the
// thing this tool exists for. Once a pool has been watched WORKING, a small
// measurement is a real one — a genuinely cheap application is exactly what
// makes dividing one budget across pools worth doing.
func TestAMeasuredCheapPoolIsStillBelieved(t *testing.T) {
	st := state.New()
	base := time.Now().Add(-time.Hour)

	var accepted int64
	for i := 0; i < 30; i++ {
		accepted += 6000 // 100 req/s across a two-minute interval
		st.Learn(state.Observation{
			Pool: "docs", At: base.Add(time.Duration(i) * 2 * time.Minute),
			ActiveNow: 6, Accepted: accepted,
			Workers: []state.WorkerSample{
				{RSSBytes: 12 * mb, Requests: 400},
				{RSSBytes: 12 * mb, Requests: 380},
			},
		}, state.Options{})
	}

	res, err := Build(Input{
		Limits: budget.Limits{MemoryBytes: 8 * gb, CPUs: 8, Source: budget.SourceMemInfo},
		State:  st,
		Views: []observe.PoolView{
			{
				Name: "docs", ProcessManager: "dynamic",
				CurrentMaxChildren: 40, MaxChildrenKnown: true,
				Workers: []state.WorkerSample{{RSSBytes: 12 * mb, Requests: 400}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := res.Plan.Pools[0].WorkerBytes; got > 16*mb {
		t.Errorf("a pool measured at 12MiB a worker across an hour of real traffic is "+
			"accounted for at %dMiB; the profile's guess has overridden a measurement, "+
			"which is the whole thing this tool is for", got/mb)
	}
}

// TestAnUnreachablePoolIsReservedWhatIsRememberedOfIt.
//
// The Unknown branch returns before the remembered peak is consulted, so a pool
// that is both unreachable AND whose ceiling could not be read fell through to
// the default floor of two workers — while state held a peak of thirty workers
// at 200MiB each. Six gigabytes of live memory accounted for as four hundred
// megabytes, and the difference handed to neighbours that ARE writable and are
// therefore actually grown into it.
//
// It is easy to reach. Discovery parses pm.max_children and discards the parse
// error, so an unreadable value yields zero, which is indistinguishable here
// from a pool configured to allow no workers.
func TestAnUnreachablePoolIsReservedWhatIsRememberedOfIt(t *testing.T) {
	st := state.New()
	base := time.Now().Add(-time.Hour)

	var accepted int64
	for i := 0; i < 30; i++ {
		accepted += 6000
		st.Learn(state.Observation{
			Pool: "big", At: base.Add(time.Duration(i) * 2 * time.Minute),
			ActiveNow: 30, Accepted: accepted,
			Workers: []state.WorkerSample{
				{RSSBytes: 200 * mb, Requests: 500}, {RSSBytes: 200 * mb, Requests: 500},
			},
		}, state.Options{})
	}
	// The concurrency high-water mark, remembered because php-fpm resets its own
	// on reload and this tool reloads.
	st.Pools["big"].ObservePeak(30, time.Now(), state.Options{}.Defaults())

	res, err := Build(Input{
		Limits: budget.Limits{MemoryBytes: 12 * gb, CPUs: 16, Source: budget.SourceMemInfo},
		State:  st,
		Views: []observe.PoolView{
			// Unreachable, and discovery could not read its ceiling either.
			{Name: "big", Err: errors.New("connection refused")},
			{Name: "neighbour", ProcessManager: "dynamic",
				CurrentMaxChildren: 20, MaxChildrenKnown: true, ObservedPeak: 18,
				Workers: []state.WorkerSample{{RSSBytes: 100 * mb, Requests: 400}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, pp := range res.Plan.Pools {
		if pp.Name != "big" {
			continue
		}
		if pp.MaxChildren < 30 {
			t.Errorf("an unreachable pool remembered at 30 busy workers is reserved %d; "+
				"the rest of its memory has just been offered to its neighbours",
				pp.MaxChildren)
		}
		// And not MORE than it can use: it will never be written, so a headroom
		// factor on top of its ceiling reserves memory for workers that cannot
		// exist.
		if pp.MaxChildren > 30 {
			t.Errorf("an unreachable pool was grown to %d workers past the 30 it is known "+
				"to have reached; nothing will be written for it, so that memory is taken "+
				"from pools that can use it", pp.MaxChildren)
		}

		return
	}
	t.Fatal("the unreachable pool is missing from the plan")
}

// TestOneOldRecordIsNotReservedTwice.
//
// A state file written before pools carried a master has one unscoped record
// per pool name. On a host running two PHP versions — both of which call their
// pool `www` — that record belongs to at most one of them, and the fallback
// that exists so an upgrade does not throw away every baseline handed it to
// both.
//
// Neither adopts it while both are unreachable, so it stays unscoped and both
// keep reading it: one pool's remembered thirty workers at 200MiB reserved
// twice, 12GiB of floor out of a single stale entry, on a host that has 12GiB.
func TestOneOldRecordIsNotReservedTwice(t *testing.T) {
	st := state.New()
	base := time.Now().Add(-time.Hour)

	// The legacy shape: no master on the record, keyed by the bare name.
	var accepted int64
	for i := 0; i < 30; i++ {
		accepted += 6000
		st.Learn(state.Observation{
			Pool: "www", At: base.Add(time.Duration(i) * 2 * time.Minute),
			ActiveNow: 30, Accepted: accepted,
			Workers: []state.WorkerSample{
				{RSSBytes: 200 * mb, Requests: 500}, {RSSBytes: 200 * mb, Requests: 500},
			},
		}, state.Options{})
	}
	st.Pools["www"].ObservePeak(30, time.Now(), state.Options{}.Defaults())

	res, err := Build(Input{
		Limits: budget.Limits{MemoryBytes: 12 * gb, CPUs: 16, Source: budget.SourceMemInfo},
		State:  st,
		Views: []observe.PoolView{
			{Name: "www", Target: phpfpm.Target{ConfigPath: "/etc/php/8.2/php-fpm.conf"},
				Err: errors.New("refused")},
			{Name: "www", Target: phpfpm.Target{ConfigPath: "/etc/php/8.3/php-fpm.conf"},
				Err: errors.New("refused")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var reserved int64
	for _, pp := range res.Plan.Pools {
		reserved += pp.Bytes
	}
	if reserved > 2*gb {
		t.Errorf("two pools called www reserved %dMiB between them out of one old record "+
			"of thirty 200MiB workers; that history belongs to at most one of them, and "+
			"the other has just been given it too", reserved/mb)
	}
}

// TestOneTenantsPoolNameDoesNotStopTheHostBeingTuned.
//
// A section name reaches a filename and a section header, so the writer refuses
// one with a path separator or a control character in it — rightly. But that
// refusal aborted the whole change set, so a tenant who can edit their own pool
// file could name it `evil/name` and stop every other site on the host from
// being tuned, indefinitely and with no obvious cause.
//
// One pool's problem should cost one pool. It is reserved conservatively and
// left alone, which is the same answer this code already gives to a pool it
// cannot read.
func TestOneTenantsPoolNameDoesNotStopTheHostBeingTuned(t *testing.T) {
	res, err := Build(Input{
		Limits: budget.Limits{MemoryBytes: 4 * gb, CPUs: 8, Source: budget.SourceMemInfo},
		State:  state.New(),
		Views: []observe.PoolView{
			{
				Name: "evil/name", ProcessManager: "dynamic",
				CurrentMaxChildren: 4, MaxChildrenKnown: true, ObservedPeak: 2,
				Workers: []state.WorkerSample{{RSSBytes: 40 * mb, Requests: 400}},
			},
			{
				Name: "neighbour", ProcessManager: "dynamic",
				CurrentMaxChildren: 4, MaxChildrenKnown: true, ObservedPeak: 20,
				Workers: []state.WorkerSample{{RSSBytes: 40 * mb, Requests: 400}},
			},
		},
	})
	if err != nil {
		t.Fatalf("one badly named pool stopped the whole plan: %v", err)
	}

	byName := map[string]allocate.PoolPlan{}
	for _, pp := range res.Plan.Pools {
		byName[pp.Name] = pp
	}

	if !byName["evil/name"].Unknown {
		t.Error("a pool whose name cannot be written was not marked unwritable, so the " +
			"writer will refuse the whole change set when it reaches it")
	}
	if n := byName["neighbour"]; n.MaxChildren <= n.Current {
		t.Errorf("the neighbour was not grown (%d from %d) even though it is queueing and "+
			"the budget allows it; one tenant's pool name has stopped the host being tuned",
			n.MaxChildren, n.Current)
	}
}

// TestAProfileFlooredNumberIsNotCalledMeasured.
//
// An idle pool's own readings are blocked from lowering its cost below the
// bootstrap profile, so its sizing number is floored up to the profile. Setting
// Measured on that made the plan print "measured 48MiB" while the distribution
// three lines below showed a median of 1MiB — a contradiction that reads as
// broken, exactly on the low-traffic host a first trial runs against.
//
// Measured means the number is the pool's own. A floored one is the guess.
func TestAProfileFlooredNumberIsNotCalledMeasured(t *testing.T) {
	st := state.New()
	base := time.Now().Add(-time.Hour)

	// A pool observed only while idle: its workers read tiny, and the idle-pool
	// rule blocks learning that as its cost. Never busy, so BusySamples stays 0.
	for i := 0; i < 30; i++ {
		st.Learn(state.Observation{
			Pool: "shop", At: base.Add(time.Duration(i) * time.Minute),
			ActiveNow: 0, Accepted: 100, // frozen: no traffic
			Workers: []state.WorkerSample{{RSSBytes: 1 * mb, Requests: 400}},
		}, state.Options{})
	}

	res, err := Build(Input{
		Limits: budget.Limits{MemoryBytes: 4 * gb, CPUs: 8, Source: budget.SourceMemInfo},
		State:  st,
		Views: []observe.PoolView{{
			Name: "shop", ProcessManager: "dynamic",
			CurrentMaxChildren: 4, MaxChildrenKnown: true,
			Workers: []state.WorkerSample{{RSSBytes: 1 * mb, Requests: 400}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	pp := res.Plan.Pools[0]
	if pp.Measured {
		t.Errorf("a pool whose cost was floored to the profile is marked measured; the " +
			"plan will call the profile guess a measurement")
	}
	if strings.Contains(pp.Reason, "measured") {
		t.Errorf("the reason says %q — that number is the profile's, not the pool's", pp.Reason)
	}
	// It must appear in the not-yet-measured list, so the recommendation labels it.
	found := false
	for _, n := range res.Bootstrapped {
		if n == "shop" {
			found = true
		}
	}
	if !found {
		t.Error("the pool is not listed as bootstrapped, so nothing tells the operator " +
			"its number is a guess")
	}
}
