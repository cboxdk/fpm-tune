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
func Discover(log *slog.Logger) ([]phpfpm.Target, error) {
	found, err := phpfpm.Discover(log)
	if err != nil {
		return nil, fmt.Errorf("cannot discover PHP-FPM pools: %w", err)
	}

	targets := make([]phpfpm.Target, 0, len(found))
	for _, d := range found {
		targets = append(targets, phpfpm.TargetFromDiscovered(d))
	}

	// Sorted so that repeated runs report pools in the same order; process-table
	// order is not stable.
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })

	return targets, nil
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
			views = append(views, PoolView{
				Name:               outcome.Name,
				Target:             target,
				CurrentMaxChildren: target.MaxChildren,
				MaxChildrenKnown:   target.MaxChildren > 0,
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
	view := PoolView{Name: outcome.Name, Target: target}

	if outcome.Result == nil {
		view.Err = fmt.Errorf("pool %s returned no result", outcome.Name)

		return view
	}

	for name, pool := range outcome.Result.Pools {
		if view.Name == "" {
			view.Name = name
		}

		view.ProcessManager = pool.ProcessManager
		view.ObservedPeak = int(pool.MaxActiveProcesses)
		view.ActiveNow = int(pool.ActiveProcesses)
		view.Accepted = pool.AcceptedConnections
		view.QueueDepth = pool.ListenQueue
		view.MaxChildrenReached = pool.MaxChildrenReached
		view.CurrentMaxChildren, view.MaxChildrenKnown = configuredMaxChildren(pool)
		if !view.MaxChildrenKnown && target.MaxChildren > 0 {
			// Discovery parsed this out of the effective configuration and the
			// scrape did not report it. Falling back was already done for a
			// FAILED scrape and not for a successful one that simply lacked the
			// key — so a pool actually configured for forty was accounted for at
			// the default floor, its memory handed to a neighbour, and the
			// neighbour written.
			view.CurrentMaxChildren, view.MaxChildrenKnown = target.MaxChildren, true
		}

		view.Workers = make([]state.WorkerSample, 0, len(pool.Processes))
		for _, proc := range pool.Processes {
			view.Workers = append(view.Workers, state.WorkerSample{
				RSSBytes: proc.CurrentRSS,
				Requests: proc.Requests,
			})
		}

		// One pool per scrape; the map shape comes from the status endpoint.
		break
	}

	return view
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
