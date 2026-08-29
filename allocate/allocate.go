// Package allocate divides a memory budget between competing PHP-FPM pools.
//
// It is pure computation: no I/O, no clock, no dependencies outside the standard
// library. Everything it needs is an argument, and the same arguments always
// produce the same plan. That is what makes the interesting cases — forty pools
// against a budget that does not fit, a pool whose workers are three times the
// size of its neighbour's — testable without a machine to run them on, and what
// lets cbox-init embed the decision without inheriting the rest of this project.
//
// The problem is allocation rather than calculation. Sizing one pool against a
// container is arithmetic; the useful question on a host serving many sites is
// how to divide a fixed budget between pools that want different amounts and
// cost different amounts per worker.
package allocate

import (
	"errors"
	"fmt"
	"sort"
)

// Budget is the memory available to PHP-FPM workers.
type Budget struct {
	// TotalBytes is the machine's or container's memory ceiling.
	TotalBytes int64

	// CPUs is the host's effective core count. It bounds how many workers are
	// worth running regardless of how much memory is free — 192GiB of budget
	// against a cheap worker authorised 1.3 million children, which PHP-FPM
	// will accept and then die trying to honour.
	CPUs int

	// ReserveBytes is held back for everything that is not a PHP-FPM worker:
	// the operating system, the web server, opcache's shared segment, and the
	// database if it shares the box. Workers are allocated from what is left.
	ReserveBytes int64
}

// Allocatable is the budget actually available to workers.
func (b Budget) Allocatable() int64 {
	free := b.TotalBytes - b.ReserveBytes
	if free < 0 {
		return 0
	}

	return free
}

// Pool is one pool's input to the allocation.
type Pool struct {
	// Name is the pool's name as PHP-FPM reports it.
	Name string

	// CurrentMaxChildren is the pool's configured pm.max_children, used to
	// decide whether a plan is a change worth applying.
	CurrentMaxChildren int

	// ProcessManager is "static", "dynamic" or "ondemand". Only "dynamic"
	// derives spare-server settings.
	ProcessManager string

	// WorkerBytes is what one worker of this pool costs.
	//
	// This is the number the whole allocation turns on, and getting it from
	// measurement rather than from a table is the reason this package exists.
	// See Measured.
	WorkerBytes int64

	// Measured distinguishes a WorkerBytes that was observed from one that came
	// from a workload profile. It says where the NUMBER came from and nothing
	// else.
	Measured bool

	// Reducible says whether this pool may be taken below its floor when the
	// budget does not stretch.
	//
	// Separate from Measured, and conflating them undid the protection it exists
	// for. A pool can have a real measurement and still not have been watched
	// long enough to justify taking workers away — that is the whole point of
	// the confidence gate — and using "has a measurement" as "may be cut" put
	// exactly those pools first in the queue to give way.
	Reducible bool

	// ObservedPeak is the highest number of workers seen busy at once
	// (pm.max_active_processes). It is the demand signal: a pool that has never
	// had more than three workers busy does not need thirty.
	ObservedPeak int

	// QueueDepth is the pool's current listen queue. Requests waiting means
	// demand the pool could not serve.
	QueueDepth int64

	// HitMaxChildren reports that the pool reached pm.max_children since it was
	// last looked at. Combined with QueueDepth it is the difference between "the
	// ceiling is where it should be" and "the ceiling is what is hurting".
	HitMaxChildren bool

	// Floor is the fewest workers this pool may be given. Zero means
	// Options.DefaultFloor. A pool below its floor cannot serve concurrent
	// requests at all, so the floor is honoured ahead of any proportional share.
	Floor int

	// Ceiling caps this pool regardless of available budget. Zero means no cap.
	Ceiling int

	// Unknown marks a pool whose current configuration could not be read, or
	// which could not be scraped at all.
	//
	// Such a pool still occupies memory and must be allocated for, but nothing
	// may be WRITTEN for it: proposing a new ceiling requires knowing the old
	// one, and a failed `php-fpm -tt` is not evidence about a pool's needs.
	Unknown bool
}

// Options tunes the allocation. The zero value is usable; Defaults documents
// what each field becomes.
type Options struct {
	// HeadroomFactor is how much room above observed peak concurrency a pool is
	// given, so that ordinary variation does not immediately queue.
	HeadroomFactor float64

	// GrowthFactor bounds how fast a saturated pool grows in one step. A pool
	// that hit its ceiling tells us its demand was clamped, but not by how much
	// — so it grows by a bounded amount and converges over successive runs
	// instead of overshooting into memory it may not need.
	GrowthFactor float64

	// DefaultFloor is the floor for pools that do not set one.
	DefaultFloor int

	// MaxWorkersPerCPU caps a pool regardless of available memory.
	//
	// PHP work is largely I/O-bound, so a generous multiple of the core count is
	// reasonable — but memory alone is not a sufficient authority. Without this,
	// a large host with a cheaply-measured worker produces a max_children in the
	// hundreds of thousands, and a pm.start_servers to match.
	MaxWorkersPerCPU int

	// Spare ratios derive dynamic pm's start/min/max spare servers from
	// max_children.
	StartServersRatio float64
	SpareMinRatio     float64
	SpareMaxRatio     float64
}

// Defaults fills in any unset option.
func (o Options) Defaults() Options {
	if o.HeadroomFactor <= 0 {
		o.HeadroomFactor = 1.25
	}
	if o.GrowthFactor <= 0 {
		o.GrowthFactor = 1.5
	}
	if o.DefaultFloor <= 0 {
		o.DefaultFloor = 2
	}
	if o.MaxWorkersPerCPU <= 0 {
		o.MaxWorkersPerCPU = 50
	}
	if o.StartServersRatio <= 0 {
		o.StartServersRatio = 0.25
	}
	if o.SpareMinRatio <= 0 {
		o.SpareMinRatio = 0.10
	}
	if o.SpareMaxRatio <= 0 {
		o.SpareMaxRatio = 0.50
	}

	return o
}

// PoolPlan is what one pool should be set to.
type PoolPlan struct {
	Name        string
	MaxChildren int

	// Current is the pool's configured pm.max_children as it was OBSERVED, so
	// that the decision to write is made against the running system rather than
	// against a memory of what this tool last did.
	//
	// The two diverge, and when they do the memory is the wrong one: an operator
	// who edits the pool by hand, a deploy that replaces the fragment, a drop-in
	// removed to undo a change. Comparing against state made every one of those
	// invisible — the tool believed the pool was already where it had put it.
	Current int

	// Dynamic-pm settings. Zero for static and ondemand pools.
	StartServers int
	MinSpare     int
	MaxSpare     int

	// Bytes is what this allocation costs: MaxChildren × WorkerBytes.
	Bytes int64

	// WorkerBytes is what one worker of this pool was costed at, carried through
	// so a caller applying part of a plan can work out what that part commits.
	WorkerBytes int64

	// Want is what the pool would have been given with unlimited budget. When it
	// exceeds MaxChildren, the pool is being held back.
	Want int

	// DemandUnmet reports that the pool wanted more than it was given. On its
	// own this is routine — it means the next run may rebalance toward it. Read
	// together with Plan.CapacityExhausted it is the difference between "we can
	// fix this" and "the machine is full".
	DemandUnmet bool

	// Measured records whether the sizing used observed worker memory or a
	// bootstrap estimate, so a reader knows how much to trust the number.
	Measured bool

	// Unknown marks a pool that must not be written. See Pool.Unknown.
	Unknown bool

	// Reason explains the number in one line, for `fpm-tune plan` output.
	Reason string
}

// Plan is the full allocation.
type Plan struct {
	Pools []PoolPlan

	TotalBytes     int64
	ReserveBytes   int64
	AllocatedBytes int64
	FreeBytes      int64

	// CapacityExhausted reports that at least one pool wanted more and there was
	// nowhere left to get it: no free budget, and no other pool holding memory
	// it is not using.
	//
	// This is the distinction that matters when a site slows down. A pool with
	// unmet demand alone is something this tool fixes on the next run, by taking
	// headroom from a quiet neighbour. Unmet demand with the budget exhausted is
	// not a configuration problem at all — the machine needs more memory, or
	// fewer sites.
	CapacityExhausted bool

	Warnings []string
}

// ErrNoBudget reports a budget that cannot host anything.
var ErrNoBudget = errors.New("no memory available to allocate")

// ErrCannotFit reports that the pools cannot run on this host at all: one worker
// each already costs more than the budget.
//
// This is a failure rather than a reduced plan because there is nothing to
// reduce to. PHP-FPM's own minimum is one worker per pool, so a plan that fitted
// would have to leave a pool with none — and a caller that wrote such a config
// would take the site down to avoid an OOM that had not happened yet. Saying so
// leaves the running configuration alone and puts the decision where it belongs.
// maxPlausibleChildren bounds a worker count before the arithmetic that
// multiplies it by a per-worker cost is reached. PHP-FPM will not run anything
// near this; a number above it came from a misparse, and int64 wraps on it.
const maxPlausibleChildren = 100_000

var ErrCannotFit = errors.New("pools do not fit on this host")

// Compute divides the budget between the pools.
//
// The allocation runs in two passes, and the order is the point. Floors are
// satisfied first, so a busy pool cannot starve a quiet one into being unable to
// serve at all. Only what remains is distributed toward demand — which is where
// the multi-pool gain lives: a site whose workers sit idle gives up the headroom
// it is not using, rather than holding memory a neighbour is queueing for.
func Compute(budget Budget, pools []Pool, opts Options) (Plan, error) {
	opts = opts.Defaults()

	plan := Plan{
		TotalBytes:   budget.TotalBytes,
		ReserveBytes: budget.ReserveBytes,
		Pools:        make([]PoolPlan, 0, len(pools)),
	}

	allocatable := budget.Allocatable()
	if allocatable <= 0 {
		return plan, fmt.Errorf("%w: %d bytes total with %d reserved",
			ErrNoBudget, budget.TotalBytes, budget.ReserveBytes)
	}
	if len(pools) == 0 {
		plan.FreeBytes = allocatable

		return plan, nil
	}

	for _, p := range pools {
		if p.WorkerBytes <= 0 {
			return plan, fmt.Errorf("pool %q has no per-worker cost: nothing can be sized against zero", p.Name)
		}
		// Guarded here rather than only at the observation boundary, because this
		// package is meant to be embedded and an embedder does not inherit that
		// boundary's validation. `granted * WorkerBytes` is int64 and a worker
		// count in the millions wraps it — to zero, in the case that was
		// demonstrated, which reads as a free pool and passes every budget check
		// below.
		if p.Floor > maxPlausibleChildren || p.Ceiling > maxPlausibleChildren {
			return plan, fmt.Errorf(
				"pool %q asks for %d workers with a ceiling of %d: past %d this is a bad "+
					"reading rather than a configuration, and multiplying it by a per-worker "+
					"cost overflows",
				p.Name, p.Floor, p.Ceiling, maxPlausibleChildren)
		}
	}

	// What each pool would take if the budget were unlimited.
	wants := make([]int, len(pools))
	floors := make([]int, len(pools))
	cpuCap := cpuCeiling(budget, opts)
	for i, p := range pools {
		floors[i] = poolFloor(p, opts)
		wants[i] = poolWant(p, floors[i], opts)
		if cpuCap > 0 && wants[i] > cpuCap {
			wants[i] = cpuCap
		}
		if cpuCap > 0 && floors[i] > cpuCap {
			floors[i] = cpuCap
		}
	}

	granted, remaining, reduced, err := allocateFloors(pools, floors, allocatable, &plan)
	if err != nil {
		return plan, err
	}

	// The demand pass runs even when the floors had to be reduced. Scaling the
	// floors is where the allocation STARTS, not where it ends: skipping the
	// distribution left budget unspent while pools sat below the floor it had
	// just been decided they could not have.
	// The return value was consumed by canStillGive, which was unreachable and
	// is gone. What is left of the budget is FreeBytes, computed below from what
	// the plan actually commits, which is the same number arrived at from the
	// side that cannot drift.
	_ = allocateDemand(pools, wants, granted, remaining)

	var allocated int64
	unmet := false
	for i, p := range pools {
		pp := PoolPlan{
			Name:        p.Name,
			MaxChildren: granted[i],
			Current:     p.CurrentMaxChildren,
			Bytes:       int64(granted[i]) * p.WorkerBytes,
			WorkerBytes: p.WorkerBytes,
			Want:        wants[i],
			DemandUnmet: granted[i] < wants[i],
			Measured:    p.Measured,
			Unknown:     p.Unknown,
			Reason:      reason(p, granted[i], wants[i], floors[i], reduced),
		}
		if pp.DemandUnmet {
			unmet = true
		}
		applyProcessManager(&pp, p, opts)

		allocated += pp.Bytes
		plan.Pools = append(plan.Pools, pp)
	}

	plan.AllocatedBytes = allocated
	plan.FreeBytes = allocatable - allocated

	// The one invariant this package exists to hold. Every branch above is
	// supposed to guarantee it; one of them did not, and the plan was published
	// with FreeBytes negative — 3.8GiB past the budget on an eight-pool host,
	// eating the OS reserve whole. An error is a bad outcome; a plan that
	// overcommits the machine is a worse one, and it looks like success.
	if allocated > allocatable {
		return Plan{}, fmt.Errorf(
			"%w: the plan commits %s against %s available; this is a bug in the "+
				"allocator and nothing should be written from it",
			ErrCannotFit, humanBytes(allocated), humanBytes(allocatable))
	}

	// Unmet demand in a FINISHED plan already means it could not be met.
	//
	// This used to be qualified by canStillGive, on the reading that a pool
	// could be short now and topped up on the next run. Both of its tests are
	// unreachable. The first is the exact negation of the demand pass's own
	// candidate filter, and that pass only returns when no candidate is left; the
	// second needs a pool granted MORE than it wants, and wants are clamped at or
	// above floors on every path. Measured across 277,684 generated plans: of
	// 909,297 pool-plans with unmet demand, not one had this come out false.
	//
	// So the two signals carry the same news at different granularity — which
	// pool, and whether any — and saying so is better than a qualifier that
	// reads like a second opinion and is not one.
	plan.CapacityExhausted = unmet

	if plan.CapacityExhausted {
		// The free budget belongs in the message. "Fully committed" was printed
		// with 30% of the budget free, because free budget that is smaller than
		// ONE worker of the pool that needs one buys nothing — true, and not
		// what those words say to someone reading a log at two in the morning.
		short := cheapestUnmet(pools, granted, wants)
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"at least one pool is short of workers, and the %s left in the budget is less "+
				"than the %s one more worker would cost it: no rearrangement helps, the host "+
				"needs more memory or fewer pools",
			humanBytes(plan.FreeBytes), humanBytes(short)))
	}

	return plan, nil
}

// allocateFloors gives every pool its floor before anything is distributed by
// demand.
//
// When the floors themselves do not fit, the host is oversubscribed before
// tuning even begins. Rather than refuse — which would leave whatever is
// currently configured in place, and that is worse — floors are scaled back
// proportionally, never below one worker, and the caller is told capacity is
// exhausted.
func allocateFloors(pools []Pool, floors []int, allocatable int64, plan *Plan) (granted []int, remaining int64, exhausted bool, err error) {
	granted = make([]int, len(pools))

	var need, minimum int64
	for i, p := range pools {
		need += int64(floors[i]) * p.WorkerBytes
		minimum += p.WorkerBytes
	}

	if need <= allocatable {
		for i := range pools {
			granted[i] = floors[i]
		}

		return granted, allocatable - need, false, nil
	}

	// Below PHP-FPM's own minimum of one worker per pool there is no
	// configuration to write, only a smaller number that is still too large.
	if minimum > allocatable {
		return nil, 0, false, fmt.Errorf(
			"%w: %d pools need at least %s for one worker each, but only %s is available",
			ErrCannotFit, len(pools), humanBytes(minimum), humanBytes(allocatable))
	}

	// Pools with a TRUSTED baseline give way first.
	//
	// A pool without one sits at its own configured setting, and there is no
	// evidence for taking workers away from it. Scaling everything uniformly cut
	// healthy pools on a guess: a first install on a tight host, with real
	// workers cheaper than the profile assumes, queued traffic on sites nobody
	// had any reason to touch.
	// Reducible, not Measured. A pool whose cost is known but whose baseline has
	// not been watched through a real traffic pattern is protected exactly like
	// one with no measurement at all: there is no more evidence for cutting it.
	//
	// Unknown pools are excluded here as well as by the caller. Nothing can be
	// written for them, so taking their memory only ever hands it to a pool that
	// CAN be written — and the untouched pool comes back at the size it always
	// had.
	var protectedNeed, reducibleNeed int64
	for i, p := range pools {
		if p.Reducible && !p.Unknown {
			reducibleNeed += int64(floors[i]) * p.WorkerBytes
		} else {
			protectedNeed += int64(floors[i]) * p.WorkerBytes
		}
	}

	var reducibleMinimum int64
	for _, p := range pools {
		if p.Reducible && !p.Unknown {
			reducibleMinimum += p.WorkerBytes
		}
	}

	var used int64
	if reducibleNeed > 0 && protectedNeed+reducibleMinimum <= allocatable {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"the configuration needs %s but only %s is available: the pools with a "+
				"trusted baseline were reduced to fit, and the ones without one were left "+
				"alone",
			humanBytes(need), humanBytes(allocatable)))

		scale := float64(allocatable-protectedNeed) / float64(reducibleNeed)
		for i, p := range pools {
			if !p.Reducible || p.Unknown {
				granted[i] = floors[i]
				used += int64(floors[i]) * p.WorkerBytes

				continue
			}
			n := int(float64(floors[i]) * scale)
			if n < 1 {
				n = 1
			}
			granted[i] = n
			used += int64(n) * p.WorkerBytes
		}

		// The same trim the path below performs, and for the same reason:
		// rounding each pool up to at least one worker can put the total back
		// over the budget. Omitting it here made the branch that exists to be
		// SAFER the one that overcommits — asymmetric floors are enough to do it,
		// two measured pools at 100MiB with floors of 100 and 1 against 250MiB
		// committing 300MiB.
		//
		// Only reducible pools are trimmed, because only they were reduced.
		used, ok := trimToFit(pools, granted, used, allocatable, func(i int) bool {
			return pools[i].Reducible && !pools[i].Unknown
		})
		if !ok {
			return nil, 0, false, fmt.Errorf(
				"%w: after reducing every pool with a trusted baseline to one worker, "+
					"%s is still committed against %s available",
				ErrCannotFit, humanBytes(used), humanBytes(allocatable))
		}

		return granted, allocatable - used, true, nil
	}

	// Even here, a pool that cannot be WRITTEN keeps its floor.
	//
	// This is the path taken when nothing is reducible at all, and protecting
	// unwritable pools only in the branch above left the same fault reachable by
	// a different route. Cutting a pool nothing can be written for frees memory
	// on paper only: apply skips it, the bytes go to a pool that IS written, and
	// the untouched pool comes back at the size it always had — leaving the host
	// over its budget by exactly the fiction.
	var unwritableNeed, writableMinimum int64
	for i, p := range pools {
		if p.Unknown {
			unwritableNeed += int64(floors[i]) * p.WorkerBytes

			continue
		}
		writableMinimum += p.WorkerBytes
	}

	// The guard has to be the SAME shape as the one above: what cannot be cut,
	// plus one worker for everything that can.
	//
	// It was `unwritableNeed >= allocatable` alone, and that is not the question.
	// An unwritable pool is held at its FLOOR, not at one worker, so the global
	// check at the top of Compute — one worker per pool — does not imply this
	// one. With the floors of the unwritable pools taking almost the whole
	// budget, scaling the rest and rounding each up to one worker produced a
	// total the trim below could not bring down, and the plan was published over
	// the budget with a negative FreeBytes. A randomised sweep put it at 3.5% of
	// valid inputs, all of them with an unwritable pool — and a pool whose
	// config could not be read is the ordinary case on a first run, not a corner.
	if unwritableNeed+writableMinimum > allocatable {
		return nil, 0, false, fmt.Errorf(
			"%w: %s is needed by pools whose configuration could not be read, and one "+
				"worker each for the rest needs %s, against %s available — nothing can be "+
				"written for the pools holding the memory, so no plan here would help",
			ErrCannotFit, humanBytes(unwritableNeed), humanBytes(writableMinimum),
			humanBytes(allocatable))
	}

	plan.Warnings = append(plan.Warnings, fmt.Sprintf(
		"the minimum viable configuration for %d pools needs %s but only %s is available: "+
			"floors have been reduced and every pool is undersized, including pools with "+
			"no trusted baseline to justify it",
		len(pools), humanBytes(need), humanBytes(allocatable)))

	scale := float64(allocatable-unwritableNeed) / float64(need-unwritableNeed)
	for i, p := range pools {
		if p.Unknown {
			granted[i] = floors[i]
			used += int64(floors[i]) * p.WorkerBytes

			continue
		}

		n := int(float64(floors[i]) * scale)
		if n < 1 {
			n = 1
		}
		granted[i] = n
		used += int64(n) * p.WorkerBytes
	}

	// Rounding up to one worker each can push the total back over the budget.
	used, ok := trimToFit(pools, granted, used, allocatable, func(i int) bool {
		return !pools[i].Unknown
	})
	if !ok {
		return nil, 0, false, fmt.Errorf(
			"%w: after reducing every writable pool to one worker, %s is still "+
				"committed against %s available",
			ErrCannotFit, humanBytes(used), humanBytes(allocatable))
	}

	return granted, allocatable - used, true, nil
}

// trimToFit takes workers back until the total fits.
//
// From the pools with the most workers first, so the damage is spread rather
// than falling entirely on the smallest site. eligible limits which pools may be
// trimmed; nil means all of them.
func trimToFit(pools []Pool, granted []int, used, allocatable int64, eligible func(int) bool) (int64, bool) {
	for used > allocatable {
		i := largestGrantedAmong(granted, eligible)
		if i < 0 || granted[i] <= 1 {
			// Everything eligible is down to a single worker and the total is
			// still over. Returning the number anyway is how an over-budget plan
			// used to escape: silence here reads at the call site exactly like a
			// fit. The callers guard against reaching this, and say so if they do.
			return used, false
		}
		granted[i]--
		used -= pools[i].WorkerBytes
	}

	return used, true
}

// allocateDemand distributes what is left after floors, proportionally to each
// pool's unmet want.
//
// Proportional rather than first-come: with a fixed order, the pool that happens
// to sort first would take everything it wants before a busier pool later in the
// list is considered at all.
func allocateDemand(pools []Pool, wants, granted []int, remaining int64) int64 {
	for {
		// Pools that still want more and can still be afforded.
		var totalGap int64
		type candidate struct {
			i      int
			gap    int
			urgent bool
		}
		var cands []candidate
		for i, p := range pools {
			gap := wants[i] - granted[i]
			if gap > 0 && p.WorkerBytes <= remaining {
				cands = append(cands, candidate{
					i: i, gap: gap,
					urgent: p.HitMaxChildren || p.QueueDepth > 0,
				})
				totalGap += int64(gap)
			}
		}
		if len(cands) == 0 || totalGap == 0 {
			return remaining
		}

		// Pools that are actually HURTING come first, then larger gaps.
		//
		// The gap alone put the priority the wrong way round. A gap is how far a
		// pool is from what it would like; a listen queue is requests waiting
		// right now, and a pool at its ceiling is turning that queue into
		// latency someone is measuring. Sorting on the gap gave a cheap pool
		// wanting 399 more workers its pick of the budget ahead of a saturated
		// one wanting 2 — measured, with 100MiB free: the cheap pool took 40
		// workers and the queueing pool got one.
		//
		// This is also what QueueDepth is for. It was carried from the scrape,
		// through the plan, into this struct, documented as the difference
		// between "the ceiling is where it should be" and "the ceiling is what
		// is hurting" — and then read by nothing.
		sort.SliceStable(cands, func(a, b int) bool {
			if cands[a].urgent != cands[b].urgent {
				return cands[a].urgent
			}
			if cands[a].urgent {
				// Among pools that are queueing, the SMALLEST gap first: it is
				// the cheapest site to take out of the queue, and serving it
				// first resolves the most pools with the budget available.
				return cands[a].gap < cands[b].gap
			}

			return cands[a].gap > cands[b].gap
		})

		// Every share is computed against the budget as it stood at the START of
		// the round, not against what the pools before it have left.
		//
		// remaining is mutated inside this loop, so measuring each share against
		// it meant the first candidate took its proportion of the whole
		// remainder and everyone after it divided the leftovers. Sorted
		// largest-first, that is the first-come behaviour this pass exists to
		// avoid, arrived at by the ordering that was supposed to prevent it.
		pot := remaining

		progressed := false
		for _, c := range cands {
			p := pools[c.i]

			// A queueing pool gets what it is SHORT, not its proportion of it.
			//
			// The gap of a saturated pool is the number of workers between it
			// and requests no longer waiting — a small, specific number bounded
			// by the evidence cap in poolWant. Handing it a fraction of that,
			// proportional to a gap that is small precisely because the need is
			// modest, is how a pool two workers short of working spent round
			// after round one worker short instead.
			share := c.gap
			if !c.urgent {
				share = int(float64(pot) * (float64(c.gap) / float64(totalGap)) / float64(p.WorkerBytes))
			}
			if share < 1 {
				share = 1
			}
			if share > c.gap {
				share = c.gap
			}
			cost := int64(share) * p.WorkerBytes
			if cost > remaining {
				share = int(remaining / p.WorkerBytes)
				cost = int64(share) * p.WorkerBytes
			}
			if share <= 0 {
				continue
			}

			granted[c.i] += share
			remaining -= cost
			progressed = true
		}

		if !progressed {
			return remaining
		}
	}
}

// cheapestUnmet is what one more worker would cost the least expensive pool
// that wanted one and did not get it — the number that says how far off the
// budget is, rather than merely that it is.
func cheapestUnmet(pools []Pool, granted, wants []int) int64 {
	var cheapest int64
	for i, p := range pools {
		if granted[i] >= wants[i] {
			continue
		}
		if cheapest == 0 || p.WorkerBytes < cheapest {
			cheapest = p.WorkerBytes
		}
	}

	return cheapest
}

// cpuCeiling is the most workers one pool may be given regardless of memory.
//
// Zero when the core count is unknown, in which case memory is the only bound
// available and the caller gets the old behaviour.
func cpuCeiling(budget Budget, opts Options) int {
	if budget.CPUs <= 0 {
		return 0
	}

	return budget.CPUs * opts.MaxWorkersPerCPU
}

// poolFloor is the fewest workers a pool may be reduced to.
func poolFloor(p Pool, opts Options) int {
	floor := p.Floor
	if floor <= 0 {
		floor = opts.DefaultFloor
	}
	if p.Ceiling > 0 && floor > p.Ceiling {
		floor = p.Ceiling
	}
	if floor < 1 {
		floor = 1
	}

	return floor
}

// poolWant is what a pool would take with unlimited budget.
//
// A pool that has never been saturated wants its observed peak plus headroom:
// what it has actually needed, with room for ordinary variation. A pool that HAS
// been saturated has had its demand clamped, and we cannot see how far — so it
// grows by a bounded factor and converges over successive runs rather than
// guessing a number that may be badly wrong in either direction.
func poolWant(p Pool, floor int, opts Options) int {
	want := int(float64(p.ObservedPeak) * opts.HeadroomFactor)

	if p.HitMaxChildren {
		base := p.CurrentMaxChildren
		if p.ObservedPeak > base {
			base = p.ObservedPeak
		}
		grown := int(float64(base) * opts.GrowthFactor)

		// Bounded by evidence. The growth branch exists because a saturated
		// pool's demand is clamped and we cannot see how far past the ceiling it
		// goes — but the base is the pool's own previous size, so without a
		// bound this is positive feedback with no reference to demand at all.
		// A pool may grow past what it has been seen to need, but not without
		// limit: one growth step beyond the headroom it already has.
		if evidence := int(float64(p.ObservedPeak) * opts.HeadroomFactor * opts.GrowthFactor); grown > evidence {
			grown = evidence
		}

		if grown > want {
			want = grown
		}
	}

	if want < floor {
		want = floor
	}
	if p.Ceiling > 0 && want > p.Ceiling {
		want = p.Ceiling
	}

	return want
}

// applyProcessManager derives the dynamic-pm settings, keeping PHP-FPM's own
// ordering constraint intact: min_spare <= start_servers <= max_spare <=
// max_children. PHP-FPM refuses to start when it does not hold.
func applyProcessManager(pp *PoolPlan, p Pool, opts Options) {
	if p.ProcessManager != "dynamic" {
		return
	}

	pp.StartServers = ceilRatio(pp.MaxChildren, opts.StartServersRatio)
	pp.MinSpare = ceilRatio(pp.MaxChildren, opts.SpareMinRatio)
	pp.MaxSpare = ceilRatio(pp.MaxChildren, opts.SpareMaxRatio)

	if pp.MinSpare < 1 {
		pp.MinSpare = 1
	}
	if pp.StartServers < pp.MinSpare {
		pp.StartServers = pp.MinSpare
	}
	if pp.MaxSpare < pp.StartServers {
		pp.MaxSpare = pp.StartServers
	}
	if pp.MaxSpare > pp.MaxChildren {
		pp.MaxSpare = pp.MaxChildren
	}
	if pp.StartServers > pp.MaxChildren {
		pp.StartServers = pp.MaxChildren
	}
	if pp.MinSpare > pp.MaxSpare {
		pp.MinSpare = pp.MaxSpare
	}
}

func ceilRatio(n int, ratio float64) int {
	v := int(float64(n) * ratio)
	if float64(v) < float64(n)*ratio {
		v++
	}

	return v
}

// largestGrantedAmong is largestGranted restricted to the pools a caller is
// willing to take from.
func largestGrantedAmong(granted []int, eligible func(int) bool) int {
	best, bestN := -1, 0
	for i, n := range granted {
		if eligible != nil && !eligible(i) {
			continue
		}
		if n > bestN {
			best, bestN = i, n
		}
	}

	return best
}

// reason explains one pool's number, for the plan output.
func reason(p Pool, granted, want, floor int, exhausted bool) string {
	source := "estimated"
	if p.Measured {
		source = "measured"
	}

	switch {
	case p.Unknown:
		return "current configuration could not be read; left alone"
	case exhausted:
		return fmt.Sprintf("host oversubscribed; held at %d (floor %d), %s %s/worker",
			granted, floor, source, humanBytes(p.WorkerBytes))
	case granted < want:
		return fmt.Sprintf("wants %d, budget allows %d; %s %s/worker",
			want, granted, source, humanBytes(p.WorkerBytes))
	case p.HitMaxChildren:
		return fmt.Sprintf("hit its ceiling; grown to %d, %s %s/worker",
			granted, source, humanBytes(p.WorkerBytes))
	case granted > p.CurrentMaxChildren:
		return fmt.Sprintf("peak %d workers busy; raised to %d, %s %s/worker",
			p.ObservedPeak, granted, source, humanBytes(p.WorkerBytes))
	case granted < p.CurrentMaxChildren:
		return fmt.Sprintf("peak %d workers busy; %d is enough, %s %s/worker",
			p.ObservedPeak, granted, source, humanBytes(p.WorkerBytes))
	case !p.Reducible && granted == p.CurrentMaxChildren && p.ObservedPeak*2 < granted:
		// The commonest "why is this pool not moving" question, and nothing
		// answered it. A pool with plenty of headroom that is not being cut is
		// being held by the confidence gate, and from outside that is
		// indistinguishable from the tool ignoring it.
		return fmt.Sprintf("peak %d workers busy, but not yet watched under load; held at "+
			"its configured %d, %s %s/worker",
			p.ObservedPeak, granted, source, humanBytes(p.WorkerBytes))
	default:
		return fmt.Sprintf("unchanged at %d; %s %s/worker",
			granted, source, humanBytes(p.WorkerBytes))
	}
}

// humanBytes formats a byte count for operator-facing output.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}

	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 3; n /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.0f%ciB", float64(b)/float64(div), "KMGT"[exp])
}
