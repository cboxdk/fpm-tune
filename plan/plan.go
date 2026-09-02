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
	"strings"
	"time"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/apply"
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
//
// ReserveFraction 0.15 targets ~85% of the budget for workers — a stable
// utilisation that leaves headroom for the OS, page cache and per-request spikes
// without stranding memory. It applies to php-fpm's budget: on a bare VM that is
// already what the good-neighbour cap left after other services (budget.WithNeighbors),
// and the per-worker cost the allocator divides it by folds in the memory a
// worker's spawned children use. Raise it with --reserve for a more cautious host,
// or set a fixed amount.
var DefaultProfile = Profile{
	Name:              "default",
	WorkerBytes:       48 << 20,
	ReserveFraction:   0.15,
	ReserveFloorBytes: 256 << 20,
}

// Input is everything needed to produce a plan.
type Input struct {
	Limits  budget.Limits
	Views   []observe.PoolView
	State   *state.State
	Profile Profile

	// Workload is the default assumption about what pools do — whether their
	// workers spawn subprocesses — used to reserve for children before any has
	// been observed. A pool overrides it with a marker in its own config
	// (PoolView.Workload). The zero value reserves nothing, which is WorkloadWeb
	// and the behaviour before workloads existed.
	Workload Workload

	// CgroupUsage is what the master's cgroup has actually used, where there is
	// a cgroup. HasCgroupUsage is false on a bare VM or dedicated host without
	// one — there the per-worker subtree measurement stands alone. Reporting
	// only: it is carried to Result for the metrics and the recommendation, not
	// used to size anything (yet).
	CgroupUsage    budget.CgroupUsage
	HasCgroupUsage bool

	// ReserveBytes overrides the profile's reserve when non-zero.
	ReserveBytes int64

	// ReserveFraction overrides the profile's reserve fraction when non-zero, so an
	// operator can set the target utilisation (e.g. 0.20 to keep 20% back, 80% for
	// workers) without a fixed byte amount. Ignored when ReserveBytes is set.
	ReserveFraction float64

	StateOptions    state.Options
	AllocateOptions allocate.Options

	// At is the round's clock, shared with LearnFrom.
	//
	// Build reads and refreshes the remembered concurrency peak, and it used to
	// stamp that with its own time.Now() while the same round's learning used an
	// explicit timestamp — two clocks in one round, in a package whose decay is
	// measured in time. Zero means now.
	At time.Time
}

// PoolDistribution is what one pool's workers have measured.
type PoolDistribution struct {
	Name          string
	P50, P95, P99 int64

	// WorstSeen is the largest worker in the DISTRIBUTION, not PoolState's
	// HighWaterBytes — that field is a sizing input, fed only by scrapes the
	// sizing path accepts, so a pool could report a dozen readings and a
	// largest-worker-ever of zero.
	WorstSeen int64
	Samples   int64

	// SubtreeHighWater is the largest a single worker's whole footprint — itself
	// plus everything it spawned — was ever seen. ChildPerWorker is the child
	// memory sizing adds to each worker: the high-water of a scrape's total child
	// memory divided by its workers, so it already reflects how many workers ran a
	// child at once. Both are zero until a child was observed.
	SubtreeHighWater int64
	ChildPerWorker   int64
}

// Result is a plan plus the reasoning that produced it.
type Result struct {
	Plan   allocate.Plan
	Budget budget.Limits

	// Reserve is what was held back from workers for the system, and why.
	Reserve       int64
	ReserveReason string

	// ChildReserve is the memory this plan committed to the processes pool
	// workers spawn — the sum over pools of each pool's per-worker child cost
	// times the workers it was given. It is not held back as a separate reserve;
	// it is folded into each worker's cost, so the allocator's own budget
	// invariant already covers it. Zero on a host whose pools spawn nothing.
	// Reporting only.
	ChildReserve       int64
	ChildReserveReason string

	// Bootstrapped names the pools sized from a profile rather than from
	// measurement, so the output can say which numbers are guesses.
	Bootstrapped []string

	// Unreachable names pools that could not be scraped. Their memory is left
	// allocated to them: a pool that is merely restarting must not have its
	// budget handed to its neighbours.
	Unreachable []string

	// Distribution is what each pool's workers were actually MEASURED at, as
	// opposed to the single number the budget is divided by.
	//
	// Reporting only. It exists for the person deciding by hand — a pool whose
	// median worker is 60MiB and whose p99 is 400MiB wants a different decision
	// from one that sits flat at 90MiB, and the sizing number cannot tell them
	// apart because it is not trying to.
	Distribution []PoolDistribution

	// CPU is how CPU-bound each pool's requests have measured. Reporting only,
	// and present only when the operator asked for it (StateOptions.MeasureCPU):
	// sizing stays on memory, and this is the dimension memory cannot see.
	CPU []PoolCPU

	// Ambiguous names pool names that appear more than once in this round,
	// because two masters each have one. Nothing keyed on the name alone can
	// represent them — not the metrics labels, not the plan table — so both say
	// so rather than showing whichever came last.
	Ambiguous []string

	// WorstCaseBytes is what this plan costs if every pool fills its ceiling
	// with the most expensive worker ever seen from it. Advisory: sizing to it
	// would pin the host to its worst minute.
	WorstCaseBytes int64

	// CgroupUsage is what the master's cgroup has actually used — every process
	// in it, children included — and HasCgroupUsage says whether there was a
	// cgroup to read. It is the ground truth the OOM killer enforces against, and
	// the one number that catches a child a per-worker sample missed. Reporting
	// only. Absent on a bare VM or dedicated host, where subtree RSS stands alone.
	CgroupUsage    budget.CgroupUsage
	HasCgroupUsage bool

	// Views is what was observed, kept so the rendered plan can show each pool's
	// current setting beside the proposed one. A plan that shows only the new
	// number gives an operator nothing to judge it against.
	Views []observe.PoolView

	// Advice is mode suggestions: pools whose measured shape points toward a
	// different pm mode than the one they run. Advisory only — fpm-tune sizes
	// within the chosen mode and never writes pm itself. Usually empty.
	Advice []ModeAdvice
}

// Build produces an allocation from what is known.
func Build(in Input) (Result, error) {
	profile := in.Profile
	if profile.WorkerBytes <= 0 {
		profile = DefaultProfile
	}
	stateOpts := in.StateOptions.Defaults()

	at := in.At
	if at.IsZero() {
		at = time.Now()
	}

	// The operator can retarget the utilisation without a fixed byte amount.
	if in.ReserveFraction > 0 {
		profile.ReserveFraction = in.ReserveFraction
	}

	reserve, reason := reserveFor(in.Limits, profile, in.ReserveBytes)

	// The good-neighbour reserve: on top of the percentage headroom, hold back what
	// other services and the OS are using, so the host as a whole stays under the
	// target — not just php-fpm's own share. Zero unless budget.WithNeighbors found
	// non-php-fpm memory in use, and skipped entirely when the operator set a fixed
	// reserve, which is their own total.
	if in.ReserveBytes == 0 && in.Limits.NeighborBytes > 0 {
		reserve += in.Limits.NeighborBytes
	}

	result := Result{
		Budget:        in.Limits,
		Reserve:       reserve,
		ReserveReason: reason,
	}

	// Pool names that appear more than once this round, because two masters can
	// each have a `www` and the store's legacy fallback would hand both the same
	// old record.
	ambiguous := ambiguousNames(in.Views)

	// The child cost is folded into each pool's per-worker cost, not held back as
	// a host-wide reserve. That is what keeps it safe: the allocator sizes every
	// pool by dividing the budget by a bounded per-worker cost, so a worker that
	// also runs an ffmpeg simply costs more and the pool is given fewer of them —
	// the cost scales with the workers actually planned, the allocator's own
	// bounds cap it, and accounting for children can never turn into "no plan at
	// all". A single number set aside up front had none of those properties.
	childPerWorker := map[string]int64{}
	var badMarkers []string
	pools := make([]allocate.Pool, 0, len(in.Views))
	for _, view := range in.Views {
		if view.Err != nil {
			result.Unreachable = append(result.Unreachable, view.Name)
		}

		pool, bootstrapped := poolFor(view, in.State, profile, stateOpts, at, ambiguous[view.Name])
		if bootstrapped {
			result.Bootstrapped = append(result.Bootstrapped, view.Name)
		}

		workload, known := WorkloadByName(view.Workload, in.Workload)
		if !known {
			// A typo in a pool's own marker silently reserving nothing is the
			// exact OOM this feature exists to prevent, so it is surfaced rather
			// than swallowed — the flag path already warns, and the per-pool path
			// is the recommended one.
			badMarkers = append(badMarkers, fmt.Sprintf("%s (%q)", view.Name, view.Workload))
		}
		if child := childCostPerWorker(workload, measuredChildPerWorker(in.State, view)); child > 0 && pool.WorkerBytes > 0 {
			childPerWorker[view.Name] = child
			pool.WorkerBytes += child
			pool.ChildBytes = child
		}

		pools = append(pools, pool)
	}

	sort.Strings(result.Bootstrapped)
	sort.Strings(result.Unreachable)

	allocation, err := allocate.Compute(allocate.Budget{
		TotalBytes:   in.Limits.MemoryBytes,
		ReserveBytes: reserve,
		CPUs:         in.Limits.CPUs,
	}, pools, in.AllocateOptions)
	if err != nil {
		return result, err
	}

	// What the plan actually committed to children: the per-worker child cost of
	// each pool times the workers it was given. Reporting only.
	for _, pp := range allocation.Pools {
		if c := childPerWorker[pp.Name]; c > 0 {
			result.ChildReserve += c * int64(pp.MaxChildren)
		}
	}
	if result.ChildReserve > 0 {
		result.ChildReserveReason = "folded into each worker's cost, sized to the workers planned"
	}
	if len(badMarkers) > 0 {
		sort.Strings(badMarkers)
		allocation.Warnings = append(allocation.Warnings, fmt.Sprintf(
			"unknown env[FPM_TUNE_WORKLOAD] on %s — reserving nothing for their children; "+
				"known values are %s", strings.Join(badMarkers, ", "), KnownWorkloads))
	}

	result.Plan = allocation
	result.Views = in.Views
	result.WorstCaseBytes = worstCase(allocation, in.State, mastersOf(in.Views))
	result.Distribution = distributionOf(in.Views, in.State)
	result.Advice = adviceFor(in.Views, in.State, allocation)
	if stateOpts.MeasureCPU {
		allowed := make(map[string]int, len(allocation.Pools))
		for _, p := range allocation.Pools {
			if !p.Unknown {
				allowed[p.Name] = p.MaxChildren
			}
		}
		result.CPU = cpuOf(in.Views, in.State, in.Limits.CPUs, allowed)
	}
	result.CgroupUsage = in.CgroupUsage
	result.HasCgroupUsage = in.HasCgroupUsage
	for name := range ambiguous {
		result.Ambiguous = append(result.Ambiguous, name)
	}
	sort.Strings(result.Ambiguous)

	return result, nil
}

// ambiguousNames reports which pool names appear more than once in a round.
//
// Only possible unscoped, on a host running several masters — which is warned
// about loudly and cannot be applied — but the reporting and the legacy
// fallback both key on the name, and both are wrong when it is not unique.
func ambiguousNames(views []observe.PoolView) map[string]bool {
	seen := make(map[string]string, len(views))
	dup := map[string]bool{}
	for _, v := range views {
		if first, ok := seen[v.Name]; ok && first != v.Target.ConfigPath {
			dup[v.Name] = true

			continue
		}
		seen[v.Name] = v.Target.ConfigPath
	}

	return dup
}

// distributionOf collects what each pool's workers have measured, for the
// pools that have measured anything.
func distributionOf(views []observe.PoolView, st *state.State) []PoolDistribution {
	if st == nil {
		return nil
	}

	var out []PoolDistribution
	for _, v := range views {
		ps := st.Lookup(v.Target.ConfigPath, v.Name)
		if ps == nil || ps.RSSSamples == 0 {
			continue
		}
		out = append(out, PoolDistribution{
			Name: v.Name,
			P50:  ps.Percentile(0.50),
			P95:  ps.Percentile(0.95),
			P99:  ps.Percentile(0.99),
			// From the distribution, not from HighWaterBytes: that field is a
			// sizing input and is only fed by scrapes the sizing path accepts,
			// so a pool could report a dozen readings and a
			// largest-worker-ever of zero. Two numbers describing the same
			// thing and disagreeing is worse than either alone.
			WorstSeen: ps.Percentile(1),
			Samples:   ps.RSSSamples,

			SubtreeHighWater: ps.SubtreeHighWaterBytes,
			ChildPerWorker:   ps.ChildPerWorkerHighWaterBytes,
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })

	return out
}

// adviceFor collects the mode suggestions for a plan, joining what was observed
// (mode, ceiling, peak concurrency, queue) with the resulting per-pool plan
// (measured cost, whether demand went unmet). Ambiguous pools — two masters
// sharing a name — are skipped, because nothing keyed on the name alone can
// attribute their numbers.
func adviceFor(views []observe.PoolView, st *state.State, p allocate.Plan) []ModeAdvice {
	plans := make(map[string]allocate.PoolPlan, len(p.Pools))
	for _, pp := range p.Pools {
		plans[pp.Name] = pp
	}
	dup := ambiguousNames(views)

	var out []ModeAdvice
	for _, v := range views {
		if dup[v.Name] || v.Err != nil {
			continue
		}
		pp, ok := plans[v.Name]
		if !ok || pp.Unknown {
			continue
		}

		// The busiest concurrency we can stand behind: FPM's since-start peak
		// resets on a master restart, so a remembered peak that survived one is
		// the safer of the two.
		peak := v.ObservedPeak
		if st != nil {
			if ps := st.Lookup(v.Target.ConfigPath, v.Name); ps != nil && ps.PeakWorkers > peak {
				peak = ps.PeakWorkers
			}
		}

		advice, ok := adviseMode(adviceInput{
			Pool:        v.Name,
			Mode:        v.ProcessManager,
			Current:     v.CurrentMaxChildren,
			Peak:        peak,
			MaxKnown:    v.MaxChildrenKnown,
			Measured:    pp.Measured,
			Queue:       v.QueueDepth,
			DemandUnmet: pp.DemandUnmet,
			WorkerBytes: pp.WorkerBytes,
		})
		if ok {
			out = append(out, advice)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Pool < out[b].Pool })

	return out
}

// mastersOf maps each pool to the configuration it belongs to, so a lookup by
// name alone cannot reach another master's record.
func mastersOf(views []observe.PoolView) map[string]string {
	out := make(map[string]string, len(views))
	dup := ambiguousNames(views)
	for _, v := range views {
		if dup[v.Name] {
			continue
		}
		out[v.Name] = v.Target.ConfigPath
	}

	return out
}

// worstCase is what the plan would cost if every pool filled its ceiling with
// the most expensive worker this tool has ever seen from it.
//
// Sizing to the high-water mark would pin every host to its worst minute, which
// is why the estimate follows the typical peak instead. But the number is kept,
// and nothing looked at it — so a pool with a rare 700MiB export endpoint and a
// 90MiB typical cost was planned at ninety-odd workers with nothing anywhere
// noting that those workers, all busy on the export at once, do not fit on the
// machine.
//
// It is not a sizing input and must not become one. It is the sentence an
// operator wants when the host OOMs anyway.
func worstCase(p allocate.Plan, st *state.State, masters map[string]string) int64 {
	if st == nil {
		return 0
	}

	var total int64
	for _, pp := range p.Pools {
		cost := pp.WorkerBytes
		// Scoped, and skipped entirely when the name is not unique: a
		// worst-case figure attributed to the wrong master is worse than one
		// that is missing, because the number is only ever read when something
		// has already gone wrong.
		//
		// The worst a single worker was ever seen at is its whole subtree —
		// itself plus every child it spawned — not its own RSS alone. Using the
		// subtree high-water keeps this coherent with the folded per-worker cost:
		// both count children, so the worst case is not silently smaller than the
		// number the plan was built from.
		if master, ok := masters[pp.Name]; ok {
			if ps := st.LookupScoped(master, pp.Name); ps != nil {
				worst := ps.HighWaterBytes
				if ps.SubtreeHighWaterBytes > worst {
					worst = ps.SubtreeHighWaterBytes
				}
				if worst > cost {
					cost = worst
				}
			}
		}
		total += int64(pp.MaxChildren) * cost
	}

	return total
}

// poolFor decides what one pool costs and what it wants.
//
// This is the bootstrap-to-learned switch, and it turns on two separate
// questions rather than one.
//
// The per-worker COST comes from any measurement there is. A number taken from
// this pool's own workers beats a profile's guess whatever the confidence, and
// gating it meant a measured 160MiB reverted to an estimated 48MiB — enough to
// grow a pool into three times the memory it fits in.
//
// Whether the pool may be CUT is what confidence decides. Sizing a pool DOWN on
// a baseline that has not been watched through a real traffic pattern is how a
// tool like this causes the outage it was installed to prevent, so until then
// its floor holds at whatever it is configured for and the first run can only
// ever help.
func poolFor(
	view observe.PoolView,
	st *state.State,
	profile Profile,
	opts state.Options,
	at time.Time,
	ambiguous bool,
) (allocate.Pool, bool) {
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
		if ambiguous {
			// No legacy fallback: the old unscoped record belongs to at most one
			// of the pools sharing this name, and giving it to both reserves the
			// same history twice.
			ps = st.LookupScoped(view.Target.ConfigPath, view.Name)
		} else {
			ps = st.Lookup(view.Target.ConfigPath, view.Name)
		}
	}

	// Two separate questions, and conflating them was a way to overcommit.
	//
	// What does a worker COST is answered by any measurement there is: a number
	// taken from this pool's own workers beats a profile estimate whatever the
	// confidence, because confidence is about how much of the traffic pattern has
	// been seen, not about whether the bytes were real. Gating the cost on it
	// meant a pool carrying a measured 160MiB reverted to the profile's 48MiB —
	// on an upgrade, on a state file with no busy timestamps — and could then be
	// grown into three times the memory it fits in.
	//
	// May the pool be CUT is the question confidence is for. A baseline that has
	// not been watched through a real traffic pattern is no basis for taking
	// workers away, so until it has, the floor holds at what the pool is
	// configured for and the first run can only ever help.
	// The peak-follower by default; a percentile of the distribution when the
	// operator has chosen the less-conservative basis for this host.
	var sized int64
	if ps != nil {
		if opts.Sizing.Percentile > 0 {
			sized = ps.SizingBytesAt(opts.Sizing.Percentile, opts.Sizing.Margin)
		} else {
			sized = ps.SizingBytes()
		}
	}

	if ps != nil && sized > 0 {
		measured := sized

		// An unproven measurement may RAISE the cost, never lower it below the
		// profile's guess.
		//
		// A pool's workers shrink when it is idle — PHP returns large
		// allocations to the operating system — so a pool first observed at
		// three in the morning reads at 12MiB a worker when its busy cost is
		// 120MiB. That number is then divided into the budget: forty workers
		// accounted for at 480MiB, the neighbours grown into the difference, and
		// the host committed past its memory the moment the site wakes up.
		//
		// The learner already refuses to LOWER an established estimate on an
		// idle reading. It could not refuse to establish one, because there was
		// nothing to compare against — so the floor has to come from here. Once
		// the pool has been seen working the measurement stands on its own,
		// however small: a genuinely cheap application is exactly what this tool
		// exists to notice.
		flooredToProfile := false
		if ps.BusySamples == 0 && measured < profile.WorkerBytes {
			measured = profile.WorkerBytes
			flooredToProfile = true
		}

		pool.WorkerBytes = measured

		// Measured means the number is the pool's OWN, not a profile guess.
		//
		// When the value above was floored up to the profile, it is the guess,
		// not a measurement — so calling it "measured" would print "measured
		// 48MiB" in the plan while the distribution table three lines down shows
		// a median of 1MiB, which reads as broken. The word, the "not yet
		// measured" list, and the fpm_tune_pool_measured metric all key on this,
		// and all three were wrong for an idle first-run pool.
		pool.Measured = !flooredToProfile
	}

	// Reducible is the OTHER question, and it travels separately. Handing
	// "Measured" to the allocator as permission to cut put pools with a real
	// measurement but no trusted baseline first in the queue to give way — which
	// is the opposite of what the confidence gate is for.
	pool.Reducible = ps != nil && ps.Trusted(opts)

	if !pool.Reducible && view.MaxChildrenKnown && pool.CurrentMaxChildren > 0 {
		pool.Floor = pool.CurrentMaxChildren
	}

	// A pool whose configured ceiling could not be read is in the same position
	// as one that could not be reached: we know nothing about it, and acting on
	// nothing is how a failed `php-fpm -tt` turns into a resize.
	//
	// And one this tool cannot safely WRITE, for the same reason from the other
	// end. A section name with a path separator or a control character in it is
	// refused by the writer — rightly, since the name reaches a filename and a
	// section header — but that refusal used to abort the whole change set, so
	// one tenant naming their pool `evil/name` stopped every other site on the
	// host being tuned. Reserved conservatively and left alone is the same
	// answer this branch already gives to a pool it cannot read, and it is the
	// right one: one pool's problem should cost one pool.
	if view.Err != nil || !view.MaxChildrenKnown || apply.UnsafePoolName(view.Name) {
		pool.Unknown = true

		// And not reducible, whatever its confidence says. A trusted pool that
		// happens to be unreachable was still being trimmed below the floor
		// reserved for it — and apply then refuses to WRITE it, because writing a
		// ceiling requires knowing the old one. So its memory went to a
		// neighbour that did get written, and the pool came back at the size it
		// always had: the host overcommitted, by a pool nobody touched.
		pool.Reducible = false

		// Reserve for it conservatively so its memory is not handed to a
		// neighbour, but never write it: proposing a new ceiling requires
		// knowing the old one.
		//
		// What state REMEMBERS counts here, and it used not to. This branch
		// returns before the remembered peak is consulted, so a pool that is
		// unreachable AND whose ceiling could not be read fell back to the
		// default floor of two — while state was holding a peak of thirty
		// workers at 200MiB each. Six gigabytes of live memory reserved as four
		// hundred megabytes, handed to the neighbours, who are writable and are
		// therefore actually grown into it. Discovery reaches that state easily:
		// an unparsed pm.max_children yields zero, which is indistinguishable
		// here from a pool that allows no workers.
		reserve := pool.CurrentMaxChildren
		if pool.ObservedPeak > reserve {
			reserve = pool.ObservedPeak
		}
		if ps != nil && ps.PeakWorkers > reserve {
			reserve = ps.PeakWorkers
		}
		if reserve > 0 {
			pool.Floor = reserve

			// Its want is its floor, and no more.
			//
			// Setting ObservedPeak here put the pool through the demand pass,
			// where the headroom factor asked for 25% MORE than the ceiling it
			// cannot exceed — memory reserved for workers that cannot exist,
			// taken from pools that can. A pool nothing can be written for is
			// not a candidate for growth; it is a thing to make room for.
			pool.ObservedPeak = 0
			pool.Ceiling = reserve
		}

		return pool, !pool.Measured
	}

	pool.HitMaxChildren = hitCeiling(view, ps)

	// PHP-FPM resets max_active_processes on reload, and this tool reloads — so
	// the peak has to be remembered here rather than read fresh each time. See
	// PoolState.ObservePeak.
	if ps != nil {
		pool.ObservedPeak = ps.ObservePeak(view.ObservedPeak, at, opts)
	}

	return pool, !pool.Measured
}

// hitCeiling reports whether the pool has run out of workers.
//
// This is the signal that lets a pool recover from having been cut too far. Once
// max_children is lowered, the pool can no longer demonstrate that it wanted more
// — its observed concurrency is capped by the very number in question — so
// saturation is the only evidence left that the cut was wrong.
//
// max_children_reached is a running total that PHP-FPM RESETS on reload, and this
// tool reloads. A plain delta therefore goes blind at exactly the wrong moment:
// straight after a cut, the counter restarts at zero, `now > previous` is false,
// and the pool that is now queueing looks content. A counter that went backwards
// means it was reset, so anything non-zero since is saturation.
//
// The listen queue is checked in every case: it is an instantaneous depth rather
// than a counter, so it survives the reset that hides everything else.
func hitCeiling(view observe.PoolView, ps *state.PoolState) bool {
	// A backlog on its own is NOT saturation. QueueDepth is PHP-FPM's listen
	// queue — the instantaneous socket accept backlog — and it reads 1 to 3 on
	// any busy pool that is nowhere near its ceiling. Treating that as "out of
	// workers" made every scrape look saturated, and because the growth base is
	// the pool's own current size, it compounded: a pool with real demand of 10
	// grew to 614 workers over nine rounds and took 24GiB of a 32GiB host, while
	// three pools that genuinely needed 25 workers were starved to 10.
	//
	// It means "out of workers" only when the pool is also at its ceiling.
	if view.QueueDepth > 0 && view.CurrentMaxChildren > 0 &&
		view.ObservedPeak >= view.CurrentMaxChildren {
		return true
	}
	if view.MaxChildrenReached == 0 {
		return false
	}

	if ps == nil || ps.LastMaxChildrenReached == 0 {
		return true
	}

	if view.MaxChildrenReached < ps.LastMaxChildrenReached {
		// The counter went backwards: the master was reloaded. Everything it
		// has counted since is new.
		return true
	}

	return view.MaxChildrenReached > ps.LastMaxChildrenReached
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

// LearnFrom folds a round of observations into the store.
//
// It deliberately does NOT record the ceiling counters — see RecordCounters.
func LearnFrom(st *state.State, views []observe.PoolView, at time.Time, opts state.Options) {
	for _, view := range views {
		if view.Err != nil {
			continue
		}

		obs := view.Observation()
		obs.At = at
		// Which master this pool belongs to, so a daemon scoped to one of them
		// does not forget the other's pools out of a shared state file.
		obs.MasterConfig = view.Target.ConfigPath
		st.Learn(obs, opts)
	}
}

// RecordCounters stores the ceiling counters for the NEXT round to compare
// against. It must be called AFTER Build.
//
// Splitting this out of LearnFrom is the whole point. Both ran before the plan
// was built, so LastMaxChildrenReached was overwritten with the current reading
// and hitCeiling then compared that reading against itself: the delta was always
// zero and the counter signal never fired once, in any round, since it was
// written. Saturation detection rested entirely on the listen queue without
// anyone noticing there was supposed to be a second signal.
func RecordCounters(st *state.State, views []observe.PoolView) {
	for _, view := range views {
		if view.Err != nil {
			continue
		}
		if ps := st.Lookup(view.Target.ConfigPath, view.Name); ps != nil {
			ps.LastMaxChildrenReached = view.MaxChildrenReached
		}
	}
}
