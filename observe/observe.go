// Package observe turns a live PHP-FPM installation into the inputs the
// allocator needs.
//
// It is the only package that talks to a running server, which is what keeps
// allocate free of I/O. The split matters for testing: a PoolView can be built
// by hand, so the interesting allocation cases never need a host with forty
// pools on it.
package observe

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/cboxdk/fpm-tune/state"
	"github.com/cboxdk/phpfpm"
)

// PoolView is one pool as it currently is.
type PoolView struct {
	Name   string
	Target phpfpm.Target

	// Workload is the pool's own workload marker, read from its configuration —
	// "subprocess-heavy", "bursty", "web" — or empty when it declared none, in
	// which case the plan's global default applies. It decides how much memory to
	// keep for the children a worker spawns before any has been observed.
	Workload string

	// CurrentMaxChildren is the configured pm.max_children, read from the
	// effective configuration rather than inferred from the process count — a
	// pool that has never been busy runs fewer workers than it is allowed.
	//
	// Zero means UNKNOWN, not zero: it is also what a failed `php-fpm -tt`
	// produces. See MaxChildrenKnown.
	CurrentMaxChildren int

	// MaxChildrenKnown distinguishes "the pool allows no workers" from "we could
	// not read the configuration".
	//
	// They are the same value and opposite situations. Parsing the effective
	// config shells out to php-fpm, which fails for reasons that have nothing to
	// do with this pool — an unrelated include with a syntax error, the binary
	// mid-upgrade, a permissions problem — and the unknown was then treated as a
	// pool with no configured ceiling. That deleted the bootstrap floor and, for
	// an idle pool, made it look brand new and eligible for the entire budget.
	MaxChildrenKnown bool

	ProcessManager string

	// ObservedPeak is pm.max_active_processes: the most workers this pool has
	// had busy at once since it started.
	ObservedPeak int

	// ActiveNow is how many workers are busy at this instant, and Accepted is
	// how many connections the pool has taken since it started.
	//
	// Together they separate a pool that got CHEAPER from one that got QUIETER.
	// Both show smaller workers — a quiet pool's surviving workers can have
	// returned their memory to the operating system — and only one says anything
	// about what the workload costs.
	//
	// The counter is the reliable half. How many workers happen to be mid-request
	// when a scrape lands depends entirely on how long a request takes: a pool
	// answering in two milliseconds reads as idle almost every time it is looked
	// at, however much traffic it is carrying.
	ActiveNow int
	Accepted  int64

	// QueueDepth is the current listen queue, and MaxChildrenReached is how many
	// times the pool has hit its ceiling. Together they separate "the ceiling is
	// fine" from "the ceiling is what is hurting".
	QueueDepth         int64
	MaxChildrenReached int64

	// Workers feeds the learner. Each carries its request count, because a
	// worker's memory only means something once it has done enough work to have
	// loaded the application.
	Workers []state.WorkerSample

	// Err is set when this pool could not be read. Reported as a value rather
	// than an error return, so one unreachable pool does not hide the others —
	// and so a pool that vanishes is distinguishable from one that was removed.
	Err error
}

// Observation converts the view into something the learner can fold in.
func (v PoolView) Observation() state.Observation {
	return state.Observation{
		Pool:      v.Name,
		Workers:   v.Workers,
		ActiveNow: v.ActiveNow,
		Accepted:  v.Accepted,
	}
}

// Discover finds the pools on this host.
//
// Bounded by the caller's context, because discovery forks `php-fpm -tt` once
// per master: a binary that wedges — an NFS-backed include, an operator's
// wrapper — would otherwise stop the loop above it for good.
func Discover(ctx context.Context, log *slog.Logger) ([]phpfpm.Target, error) {
	found, err := phpfpm.DiscoverContext(ctx, log)
	if err != nil {
		return nil, fmt.Errorf("cannot discover PHP-FPM pools: %w", err)
	}

	// A cancelled context is reported even when the scan happened to complete.
	//
	// With nothing to fork — no master on the host — DiscoverContext returns an
	// empty list and no error however the context is handled, and an empty list
	// with a nil error is a different statement: it says this host runs no
	// php-fpm, which is what the CLI tells the operator. Being interrupted is
	// not knowing.
	if cerr := ctx.Err(); cerr != nil {
		return nil, fmt.Errorf("discovery was interrupted: %w", cerr)
	}

	// One target per (master, pool), because a pool is not two pools just
	// because two processes are serving it.
	//
	// Discovery scans the process table, and a host can carry more than one
	// master for the same configuration: an old one still holding a wedged
	// worker after a restart, or the moment during a daemonized reload when the
	// re-execed master and its predecessor are both up. Each reports the same
	// pools from the same file.
	//
	// Counted twice, every one of those pools was planned twice and the budget
	// was divided among twice as many entries — measured on a five-pool host
	// with a lingering master: ten rows, every pool cut to half the workers it
	// should have had, and an `allocated` figure that agreed with itself.
	//
	// Two masters with DIFFERENT configurations are a different matter entirely,
	// and are refused elsewhere rather than merged here.
	targets := Dedupe(targetsFrom(found))

	// Sorted so that repeated runs report pools in the same order; process-table
	// order is not stable.
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })

	return targets, nil
}

func targetsFrom(found []phpfpm.Discovered) []phpfpm.Target {
	out := make([]phpfpm.Target, 0, len(found))
	for _, d := range found {
		out = append(out, phpfpm.TargetFromDiscovered(d))
	}

	return out
}

// Dedupe keeps one target per (master, pool).
//
// A pool is not two pools because two processes are serving it. A host can
// carry more than one master for the same configuration — an old one still
// holding a wedged worker after a restart, or the moment during a daemonized
// reload when the re-execed master and its predecessor are both up — and each
// reports the same pools from the same file.
//
// Counted twice, every one of those pools is planned twice and the budget is
// divided among twice as many entries. Measured on a five-pool host with a
// lingering master: ten rows, every pool cut to half the workers it should have
// had, and an `allocated` figure that agreed with itself all the way down.
//
// Two masters with DIFFERENT configurations are a different matter entirely,
// and are refused elsewhere rather than merged here.
func Dedupe(targets []phpfpm.Target) []phpfpm.Target {
	seen := make(map[string]bool, len(targets))
	out := make([]phpfpm.Target, 0, len(targets))
	for _, t := range targets {
		key := filepath.Clean(t.ConfigPath) + "\x00" + t.Name
		if seen[key] {
			continue
		}
		seen[key] = true

		out = append(out, t)
	}

	return out
}

// Sample scrapes every target and returns one view per pool, in the order given.
//
// A pool that cannot be reached comes back with Err set rather than being
// dropped. Dropping it would make an unreachable pool indistinguishable from a
// deleted one, and the allocator would hand its memory to the others — which is
// precisely wrong if the pool is merely restarting.
func Sample(ctx context.Context, targets []phpfpm.Target, log *slog.Logger) []PoolView {
	outcomes, _ := phpfpm.ScrapeAll(ctx, targets, log)

	views := make([]PoolView, 0, len(outcomes))
	for i, outcome := range outcomes {
		target := phpfpm.Target{}
		if i < len(targets) {
			target = targets[i]
		}

		if outcome.Err != nil {
			// Carrying what discovery already knew. A pool that could not be
			// scraped still occupies whatever it is configured for, and a view
			// that reports nothing makes the allocator reserve nothing — so a
			// site restarting for five seconds has its memory handed to its
			// neighbours, who are then reloaded with larger ceilings, and the
			// host is overcommitted the moment it comes back.
			ceiling := boundedCeiling(target.MaxChildren)
			views = append(views, PoolView{
				Name:               outcome.Name,
				Target:             target,
				Workload:           target.Workload,
				CurrentMaxChildren: ceiling,
				MaxChildrenKnown:   ceiling > 0,
				ProcessManager:     target.ProcessManager,
				Err:                outcome.Err,
			})

			continue
		}

		views = append(views, viewFromOutcome(outcome, target))
	}

	return views
}

func viewFromOutcome(outcome phpfpm.PoolOutcome, target phpfpm.Target) PoolView {
	view := PoolView{Name: outcome.Name, Target: target, Workload: target.Workload}

	// What discovery already knew, carried before anything can return early.
	//
	// Sample's own failure branch does this and this one did not, so a pool that
	// came back with no result at all was accounted for as having no configured
	// ceiling — and a pool with no known ceiling has its memory reserved at the
	// default floor while it actually runs forty workers. It is the file's own
	// doctrine, applied to one of two failure paths.
	if c := boundedCeiling(target.MaxChildren); c > 0 {
		view.CurrentMaxChildren, view.MaxChildrenKnown = c, true
	}
	if target.ProcessManager != "" {
		view.ProcessManager = target.ProcessManager
	}

	if outcome.Result == nil {
		view.Err = fmt.Errorf("pool %s returned no result", outcome.Name)

		return view
	}

	// The pool this outcome is ABOUT, by name.
	//
	// It used to take whatever the range reached first and break. There is one
	// pool per scrape today, so the map has one entry and the two are the same
	// thing — but "the same thing" rests on an invariant of another module, and
	// map iteration order is random, so the day that stops holding this reads
	// one pool's workers under another pool's name and nothing anywhere says so.
	// Driven with three pools, the view labelled `alpha` carried the wrong
	// peak 31% of the time.
	pool, ok := outcome.Result.Pools[outcome.Name]
	if !ok {
		// Fall back to the single entry when the names disagree, because
		// php-fpm's own name is the more authoritative of the two and the
		// outcome may be labelled from discovery.
		if len(outcome.Result.Pools) != 1 {
			view.Err = fmt.Errorf("the scrape reported %d pools and none of them is %q",
				len(outcome.Result.Pools), outcome.Name)

			return view
		}
		for _, only := range outcome.Result.Pools {
			// The VALUE, not the name. view.Name stays what discovery read out
			// of the root-owned configuration.
			//
			// Taking the name from the response is what makes a status page able
			// to relabel itself: a tenant who can set pm.status_listen for their
			// own pool can point it at a socket they control, and a response
			// claiming to be another pool would have been learned under that
			// pool's name and then rendered into the drop-in as `[victim]`. The
			// library refuses a mismatched name before it gets here, and this is
			// the same rule at the layer that would have to act on it.
			pool = only
		}
	}

	{
		view.ProcessManager = pool.ProcessManager
		// Through the same clamp as the configured ceiling: ObservedPeak is a
		// scraped number that becomes a pool's Floor/Ceiling for an Unknown pool,
		// so a garbage value must not slip past the bound the configured ceiling
		// already respects.
		view.ObservedPeak = boundedCeiling(int(pool.MaxActiveProcesses))
		view.ActiveNow = int(pool.ActiveProcesses)
		view.Accepted = pool.AcceptedConnections
		view.QueueDepth = pool.ListenQueue
		view.MaxChildrenReached = pool.MaxChildrenReached
		view.CurrentMaxChildren, view.MaxChildrenKnown = configuredMaxChildren(pool)
		if c := boundedCeiling(target.MaxChildren); !view.MaxChildrenKnown && c > 0 {
			// Discovery parsed this out of the effective configuration and the
			// scrape did not report it. Falling back was already done for a
			// FAILED scrape and not for a successful one that simply lacked the
			// key — so a pool actually configured for forty was accounted for at
			// the default floor, its memory handed to a neighbour, and the
			// neighbour written.
			view.CurrentMaxChildren, view.MaxChildrenKnown = c, true
		}

		view.Workers = make([]state.WorkerSample, 0, len(pool.Processes))
		for _, proc := range pool.Processes {
			view.Workers = append(view.Workers, state.WorkerSample{
				RSSBytes:        proc.CurrentRSS,
				PSSBytes:        proc.CurrentPSS,
				SubtreeRSSBytes: proc.SubtreeRSS,
				Requests:        proc.Requests,

				// Carried on every scrape whether or not CPU is being measured:
				// the numbers are already in the status response, and the
				// learner's option decides what to do with them. php-fpm only
				// fills in the CPU figure once the request has finished and the
				// worker is back to Idle; while it is Running the field reads 0,
				// which is not a measurement, so the state travels with it.
				PID:               proc.PID,
				Idle:              strings.EqualFold(proc.State, "Idle"),
				LastRequestCPU:    proc.LastRequestCPU,
				LastRequestMicros: proc.RequestDuration,
			})
		}

	}

	return view
}

// SubtreeRSS sums php-fpm's own current memory across the views — each worker plus
// the processes it spawned. It is what budget.WithNeighbors adds back so the
// good-neighbour budget is "free memory plus php-fpm's own", not just the free
// memory a neighbour's growth would otherwise ratchet it down by.
func SubtreeRSS(views []PoolView) int64 {
	var total int64
	for _, v := range views {
		for _, w := range v.Workers {
			rss := w.SubtreeRSSBytes
			if rss <= 0 {
				rss = w.RSSBytes
			}
			total += rss
		}
	}

	return total
}

// configuredMaxChildren reads pm.max_children from the effective configuration.
//
// The live process count is not a substitute: a pool that has never been busy
// runs far fewer workers than it is allowed, and sizing against that would
// ratchet every quiet pool down to nothing.
func configuredMaxChildren(pool phpfpm.Pool) (int, bool) {
	raw, ok := pool.Config["pm.max_children"]
	if !ok {
		return 0, false
	}

	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 0, false
	}

	// A configured ceiling in the millions is a typo or a parse gone wrong, not
	// an instruction. Believing it overflows the byte arithmetic downstream.
	if n > maxPlausibleChildren {
		return 0, false
	}

	return n, true
}

// maxPlausibleChildren bounds what is accepted as a configured ceiling. Beyond
// this the number is not a configuration, and multiplying it by a per-worker
// cost wraps int64.
const maxPlausibleChildren = 100_000

// boundedCeiling rejects an implausible pm.max_children. The scrape path clamps
// in configuredMaxChildren; this is the same clamp for the fallback paths that
// read target.MaxChildren directly — which discovery parsed with a discarded
// error and no upper bound, so a garbage value there could otherwise become a
// pool's ceiling and reach the sizing arithmetic.
func boundedCeiling(n int) int {
	if n <= 0 || n > maxPlausibleChildren {
		return 0
	}

	return n
}
