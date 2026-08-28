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
	CurrentMaxChildren int
	ProcessManager     string

	// ObservedPeak is pm.max_active_processes: the most workers this pool has
	// had busy at once since it started.
	ObservedPeak int

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
	return state.Observation{Pool: v.Name, Workers: v.Workers}
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
			views = append(views, PoolView{Name: outcome.Name, Target: target, Err: outcome.Err})

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
		view.QueueDepth = pool.ListenQueue
		view.MaxChildrenReached = pool.MaxChildrenReached
		view.CurrentMaxChildren = configuredMaxChildren(pool)

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
func configuredMaxChildren(pool phpfpm.Pool) int {
	raw, ok := pool.Config["pm.max_children"]
	if !ok {
		return 0
	}

	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0
	}

	return n
}
