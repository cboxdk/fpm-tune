// Package plan turns observations and remembered baselines into an allocation.
//
// It is the seam where bootstrap becomes learned. Everything below it is either
// pure (allocate) or purely mechanical (observe, budget, state); the judgement
// about whether a pool has been watched long enough to be believed lives here,
// in one place, so it can be read and argued with.
package plan

import (
	"fmt"
	"sort"
	"time"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
)

// Profile is the starting estimate for a pool with no history.
//
// It is a guess, and it is labelled as one everywhere it surfaces. Its job is to
// produce a survivable configuration on the first run and then get out of the
// way — every pool that keeps running replaces it with measurement.
type Profile struct {
	Name string

	// WorkerBytes is the assumed cost of one worker. The default is sized for a
	// PHP application with opcache enabled: the runtime plus per-request data,
	// with the compiled code in shared memory rather than in each worker.
	WorkerBytes int64

	// ReserveFraction and ReserveFloorBytes decide how much of the host is held
	// back from workers, for the OS, the web server and opcache's shared
	// segment. The floor matters on small hosts, where a fraction of very little
	// is not enough to run anything.
	ReserveFraction   float64
	ReserveFloorBytes int64
}

// DefaultProfile is used when no other is given.
var DefaultProfile = Profile{
	Name:              "default",
	WorkerBytes:       48 << 20,
	ReserveFraction:   0.25,
	ReserveFloorBytes: 256 << 20,
}

// Input is everything needed to produce a plan.
type Input struct {
	Limits  budget.Limits
	Views   []observe.PoolView
	State   *state.State
	Profile Profile

	// ReserveBytes overrides the profile's reserve when non-zero.
	ReserveBytes int64

	StateOptions    state.Options
	AllocateOptions allocate.Options
}

// Result is a plan plus the reasoning that produced it.
type Result struct {
	Plan   allocate.Plan
	Budget budget.Limits

	// Reserve is what was held back from workers, and why.
	Reserve       int64
	ReserveReason string

	// Bootstrapped names the pools sized from a profile rather than from
	// measurement, so the output can say which numbers are guesses.
	Bootstrapped []string

	// Unreachable names pools that could not be scraped. Their memory is left
	// allocated to them: a pool that is merely restarting must not have its
	// budget handed to its neighbours.
	Unreachable []string

	// Views is what was observed, kept so the rendered plan can show each pool's
	// current setting beside the proposed one. A plan that shows only the new
	// number gives an operator nothing to judge it against.
	Views []observe.PoolView
}

// Build produces an allocation from what is known.
func Build(in Input) (Result, error) {
	profile := in.Profile
	if profile.WorkerBytes <= 0 {
		profile = DefaultProfile
	}
	stateOpts := in.StateOptions.Defaults()

	reserve, reason := reserveFor(in.Limits, profile, in.ReserveBytes)

	result := Result{
		Budget:        in.Limits,
		Reserve:       reserve,
		ReserveReason: reason,
	}

	pools := make([]allocate.Pool, 0, len(in.Views))
	for _, view := range in.Views {
		if view.Err != nil {
			result.Unreachable = append(result.Unreachable, view.Name)
		}

		pool, bootstrapped := poolFor(view, in.State, profile, stateOpts)
		if bootstrapped {
			result.Bootstrapped = append(result.Bootstrapped, view.Name)
		}
		pools = append(pools, pool)
	}

	sort.Strings(result.Bootstrapped)
	sort.Strings(result.Unreachable)

	allocation, err := allocate.Compute(allocate.Budget{
		TotalBytes:   in.Limits.MemoryBytes,
		ReserveBytes: reserve,
	}, pools, in.AllocateOptions)
	if err != nil {
		return result, err
	}

	result.Plan = allocation
	result.Views = in.Views

	return result, nil
}

// poolFor decides what one pool costs and what it wants.
//
// This is the bootstrap-to-learned switch. A baseline is used only once it is
// trusted — samples and elapsed time both — because sizing a pool DOWN on a
// number that has not been watched through a real traffic pattern is how a tool
// like this causes the outage it was installed to prevent. Until then the
// profile's estimate stands, which is the same guess a hand-written config
// makes, so bootstrapping is never worse than what it replaces.
func poolFor(view observe.PoolView, st *state.State, profile Profile, opts state.Options) (allocate.Pool, bool) {
	pool := allocate.Pool{
		Name:               view.Name,
		CurrentMaxChildren: view.CurrentMaxChildren,
		ProcessManager:     view.ProcessManager,
		ObservedPeak:       view.ObservedPeak,
		QueueDepth:         view.QueueDepth,
		WorkerBytes:        profile.WorkerBytes,
	}

	var ps *state.PoolState
	if st != nil {
		ps = st.Pools[view.Name]
	}

	trusted := ps != nil && ps.Trusted(opts) && ps.TypicalPeakBytes > 0
	if trusted {
		pool.WorkerBytes = ps.TypicalPeakBytes
		pool.Measured = true
	} else if pool.CurrentMaxChildren > 0 {
		// While a pool is still bootstrapping it may GROW but must not be cut.
		//
		// The trust gate covers the per-worker cost, but the demand signal needs
		// it just as much: max_active_processes is a high-water mark since the
		// master started, so a pool observed for thirty seconds looks idle
		// whatever it does at nine in the morning. Cutting a site from twenty
		// workers to two on that evidence is precisely the outage this tool is
		// installed to prevent. Holding the floor at the current setting means
		// the first run can only ever help.
		pool.Floor = pool.CurrentMaxChildren
	}

	// A pool that could not be reached keeps what it has. Its peak is unknown,
	// not zero, and treating an unreachable pool as idle would hand its memory
	// to its neighbours while it is merely restarting.
	if view.Err != nil {
		if pool.CurrentMaxChildren > 0 {
			pool.Floor = pool.CurrentMaxChildren
			pool.ObservedPeak = pool.CurrentMaxChildren
		}

		return pool, !pool.Measured
	}

	pool.HitMaxChildren = hitCeiling(view, ps)

	return pool, !pool.Measured
}

// hitCeiling reports whether the pool has run out of workers.
//
// On a one-shot run the counter is all there is: non-zero means it has happened
// at some point since PHP-FPM started. With state to compare against, the delta
// is used instead — a pool that hit its ceiling once last Tuesday is not the same
// as one hitting it now, and only the second should be growing.
func hitCeiling(view observe.PoolView, ps *state.PoolState) bool {
	if ps != nil && ps.LastMaxChildrenReached > 0 {
		return view.MaxChildrenReached > ps.LastMaxChildrenReached
	}

	return view.MaxChildrenReached > 0 || view.QueueDepth > 0
}

// reserveFor decides how much of the host is held back from workers.
func reserveFor(limits budget.Limits, profile Profile, override int64) (int64, string) {
	if override > 0 {
		return override, "set explicitly"
	}

	fraction := int64(float64(limits.MemoryBytes) * profile.ReserveFraction)
	if fraction >= profile.ReserveFloorBytes {
		return fraction, fmt.Sprintf("%.0f%% of %s",
			profile.ReserveFraction*100, budget.HumanBytes(limits.MemoryBytes))
	}

	// On a small host a fraction of very little is not enough to run the
	// operating system and a web server, so the floor wins — even though it
	// leaves proportionally little for workers. A host that small is going to be
	// told it is oversubscribed, which is the correct answer.
	if profile.ReserveFloorBytes < limits.MemoryBytes {
		return profile.ReserveFloorBytes, fmt.Sprintf("%s floor (a %.0f%% share would be only %s)",
			budget.HumanBytes(profile.ReserveFloorBytes), profile.ReserveFraction*100,
			budget.HumanBytes(fraction))
	}

	// Smaller than the floor itself: keep half and let the allocator report what
	// that means, rather than reserving the entire host and allocating nothing.
	half := limits.MemoryBytes / 2

	return half, fmt.Sprintf("half of %s (below the %s reserve floor)",
		budget.HumanBytes(limits.MemoryBytes), budget.HumanBytes(profile.ReserveFloorBytes))
}

// LearnFrom folds a round of observations into the store and records the
// ceiling counters the next run compares against.
func LearnFrom(st *state.State, views []observe.PoolView, at time.Time, opts state.Options) {
	for _, view := range views {
		if view.Err != nil {
			continue
		}

		obs := view.Observation()
		obs.At = at
		st.Learn(obs, opts)

		if ps := st.Pools[view.Name]; ps != nil {
			ps.LastMaxChildrenReached = view.MaxChildrenReached
		}
	}
}
