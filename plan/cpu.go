package plan

import (
	"fmt"
	"math"
	"sort"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
)

// PoolCPU is how CPU-bound one pool's requests have measured, and what that
// means beside the number the plan gives it.
//
// It answers the question the memory sizing cannot: which of the two does
// this pool run out of first? A pool whose busy workers fill the CPU before
// they reach the memory-sized ceiling is CPU-limited, however much RAM the
// host has; past that point another worker makes every request slower rather
// than serving one more.
type PoolCPU struct {
	Name string

	// P50 and P90 are the share of a request's wall time spent on CPU, as
	// fractions: 0.70 is a request that computed for 70% of its duration. P50
	// is what everything below is built on; P90 says how much heavier the
	// heavy requests are.
	P50, P90 float64
	Samples  int64

	// Shape classifies the pool, or is empty when there are too few readings
	// to call it: "cpu-bound", "mixed", "i/o-bound".
	Shape string

	// MillicoresPerWorker is what one busy worker of this pool costs in CPU:
	// P50 in thousandths of a core, the unit a container quota is written in.
	// The CPU twin of a worker's bytes. Zero until the shape is known.
	MillicoresPerWorker int

	// FillWorkers is how many of this pool's workers, all busy at once, fill
	// the host's CPU: the host's millicores over MillicoresPerWorker, rounded
	// up. It is a per-pool bound, not a share — every pool is measured against
	// the whole host, and the host line below is where they add up. Zero
	// until the shape is known.
	FillWorkers int

	// Current is the pm.max_children in effect now, and Allowed the one this
	// plan gives the pool. Zero when the pool is not written.
	Current, Allowed int

	// Limit names what this pool runs out of first, "cpu" or "memory", or is
	// empty until the shape is known. "cpu" when the memory-sized ceiling is
	// above FillWorkers — or when the measurement was allowed to bind and did.
	Limit string

	// CPUBound reports that the allocation was actually held at FillWorkers,
	// which happens only when the operator passed --cpu. The same fact as
	// allocate.PoolPlan.CPUBound, under the same name.
	CPUBound bool
}

// HostCPU is the host's CPU against what the plan would need of it.
type HostCPU struct {
	// Millicores is the CPU available: the cgroup quota where there is one,
	// otherwise the machine's cores.
	Millicores int

	// NeededAtPlan is what every pool with a known shape would draw if it ran
	// its planned ceiling busy at once, and NeededNow the same for the
	// ceilings in effect now. Both are the worst case, not a prediction: they
	// say whether the ceilings, taken together, could ever fit the CPU. Zero
	// when no pool has a shape yet, or none costs a millicore.
	NeededAtPlan, NeededNow int
}

// The shape thresholds, on the median share. Coarse on purpose: the point is
// to separate a pool whose workers compute from one whose workers wait, and
// finer distinctions than that are not what anyone sizes by hand on.
const (
	cpuBoundFrom = 0.50
	mixedFrom    = 0.20
)

// cpuShape classifies a pool from its median share and prices a busy worker
// in millicores. A pure function of the share, so it can be table-tested; the
// caller decides whether there are enough readings to ask.
func cpuShape(p50 float64) (shape string, perWorker int) {
	switch {
	case p50 >= cpuBoundFrom:
		shape = "cpu-bound"
	case p50 >= mixedFrom:
		shape = "mixed"
	default:
		shape = "i/o-bound"
	}

	return shape, int(math.Round(p50 * 1000))
}

// fillWorkers is how many busy workers at the given per-worker cost fill the
// host's CPU, rounded up; zero when either side is unknown.
func fillWorkers(perWorker, hostMillicores int) int {
	if hostMillicores <= 0 || perWorker <= 0 {
		return 0
	}

	return int(math.Ceil(float64(hostMillicores) / float64(perWorker)))
}

// cpuCeilingFor is the ceiling plan hands the allocator for one pool: its
// fill count, but only for a pool that has been watched long enough to be CUT
// on memory evidence too. Twenty readings say what shape a pool's requests
// have; they are not permission to take workers away. See cpu.md, "What
// --cpu does".
func cpuCeilingFor(ps *state.PoolState, opts state.Options, hostMillicores int) int {
	if ps == nil || !ps.Trusted(opts) || !ps.CPUShapeKnown(opts) {
		return 0
	}
	_, perWorker := cpuShape(ps.CPUShare(0.50))

	return fillWorkers(perWorker, hostMillicores)
}

// Percent prints a share as a whole percentage, or a dash until there are
// enough readings to call the shape — a number beside "too few readings" would
// assert and disclaim in one line. The first bucket prints as "<5%": its floor
// is 0, and "0%" claims a measurement of nothing.
func (c PoolCPU) Percent(share float64) string {
	if c.Shape == "" {
		return "-"
	}
	if share == 0 {
		return "<5%"
	}

	return fmt.Sprintf("%.0f%%", share*100)
}

// PerWorker prints the per-worker cost in millicores, or a dash when there is
// none to give.
func (c PoolCPU) PerWorker() string {
	if c.MillicoresPerWorker == 0 {
		return "-"
	}

	return fmt.Sprintf("%dm", c.MillicoresPerWorker)
}

// Why is the sentence beside the numbers: the shape, the arithmetic, and the
// ceilings the arithmetic is compared with, so the gap between what fills the
// CPU and what the pool is allowed is on one line. The plan and the
// recommendation file both print it.
func (c PoolCPU) Why(hostMillicores int) string {
	if c.Shape == "" {
		return "too few readings yet"
	}
	if c.FillWorkers == 0 {
		return c.Shape
	}

	workers := "workers"
	if c.FillWorkers == 1 {
		workers = "worker"
	}
	line := fmt.Sprintf("%s; ~%d busy %s fill %s", c.Shape, c.FillWorkers, workers, budget.HumanMillicores(hostMillicores))
	switch {
	case c.CPUBound:
		line += fmt.Sprintf("; held there (now %d)", c.Current)
	case c.Allowed > 0:
		line += fmt.Sprintf("; plan allows %d (now %d)", c.Allowed, c.Current)
	case c.Current > 0:
		line += fmt.Sprintf("; now %d", c.Current)
	}

	return line
}

// cpuOf builds the CPU report: one row for every pool this round, whether or
// not it has readings yet. A pool listed with "too few readings" tells the
// operator the measurement is running, where an absent row would not — and a
// pool that could not be reached this round keeps its row, because what it
// measured last week is still what its requests look like.
func cpuOf(
	views []observe.PoolView,
	st *state.State,
	opts state.Options,
	hostMillicores int,
	allocation allocate.Plan,
	ambiguous map[string]bool,
) ([]PoolCPU, HostCPU) {
	plans := make(map[string]allocate.PoolPlan, len(allocation.Pools))
	for _, pp := range allocation.Pools {
		plans[pp.Name] = pp
	}

	host := HostCPU{Millicores: hostMillicores}
	var out []PoolCPU
	for _, v := range views {
		if ambiguous[v.Name] {
			// Two masters share this name; nothing keyed on it can say which
			// one's readings these are. Build already warns about the name.
			continue
		}

		var ps *state.PoolState
		if st != nil {
			ps = st.Lookup(v.Target.ConfigPath, v.Name)
		}
		if ps == nil && v.Err != nil {
			// Unreachable, and nothing remembered: there is nothing to say.
			continue
		}

		row := PoolCPU{Name: v.Name, Current: v.CurrentMaxChildren}
		if pp, ok := plans[v.Name]; ok && !pp.Unknown {
			row.Allowed = pp.MaxChildren
			row.CPUBound = pp.CPUBound
		}
		// A pool the plan does not write keeps the ceiling it has, so that is
		// the ceiling to compare against and to count in the host sums.
		atPlan := row.Allowed
		if atPlan == 0 {
			atPlan = row.Current
		}
		if ps != nil {
			row.P50 = ps.CPUShare(0.50)
			row.P90 = ps.CPUShare(0.90)
			row.Samples = ps.CPUSamples
			if ps.CPUShapeKnown(opts) {
				row.Shape, row.MillicoresPerWorker = cpuShape(row.P50)
				row.FillWorkers = fillWorkers(row.MillicoresPerWorker, hostMillicores)
			}
		}
		if row.Shape != "" {
			row.Limit = "memory"
			if row.CPUBound || (row.FillWorkers > 0 && atPlan > row.FillWorkers) {
				row.Limit = "cpu"
			}

			host.NeededAtPlan += atPlan * row.MillicoresPerWorker
			host.NeededNow += row.Current * row.MillicoresPerWorker
		}

		out = append(out, row)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })

	return out, host
}
