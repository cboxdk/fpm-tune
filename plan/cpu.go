package plan

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

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

	// BoxMillicoresPerWorker is what one busy worker costs the whole box —
	// MySQL, nginx and the kernel included — once the box-cost fit has enough
	// spread to say (BoxMeasured); until then it equals MillicoresPerWorker
	// and the report says the rest of the box is not measured yet. Overhead
	// is the ratio between the two: 2.1 for a pool that spends as much again
	// outside PHP as in it.
	BoxMillicoresPerWorker int
	BoxMeasured            bool
	Overhead               float64

	// FillWorkers is how many of this pool's workers, all busy at once, fill
	// the host's CPU: the host's millicores over BoxMillicoresPerWorker,
	// rounded up. It is a per-pool bound, not a share — every pool is measured
	// against the whole host, and the host line below is where they add up.
	// Zero until the shape is known.
	FillWorkers int

	// Ceiling is the number --cpu holds the pool at: FillWorkers with Headroom
	// on top, never below one worker per core plus one. The headroom is the
	// one figure here that is a judgement rather than a measurement: it is
	// how many workers may sit waiting on I/O or absorbing a burst past the
	// point where the CPU is full. HeadroomFromPool says the pool set its own
	// (env[FPM_TUNE_CPU_HEADROOM]) rather than taking the host's. Zero until
	// the shape is known.
	Ceiling          int
	Headroom         float64
	HeadroomFromPool bool

	// StarvedRounds is how many scrapes found requests queued while the box
	// was full: the direct observation that another worker would not have
	// helped.
	StarvedRounds int

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

// DefaultCPUHeadroom is the factor on the fill count a pool is held at with
// --cpu. Two, because the fill count is the throughput optimum and nothing
// more: past it another worker serves no more requests, but a pool held at
// exactly it has no worker to spare for a request stuck on an upstream, and
// short requests wait behind long ones in the listen queue rather than sharing
// the CPU. Operators who size cpu-bound PHP by hand land between one and a half
// and two workers per core; two is the generous end.
const DefaultCPUHeadroom = 2.0

// HeadroomMarker is the key a pool sets in its own configuration to carry more
// or less headroom than the host: a pool with a slow payment API behind it
// wants workers to wait in while the CPU is full, and can say so without every
// other pool getting the same.
const HeadroomMarker = "env[FPM_TUNE_CPU_HEADROOM]"

// MaxCPUHeadroom is the most headroom a pool or the host may ask for. A
// hundred times the fill count is already far past anything a real pool wants
// (memory caps it long before), and the bound is what keeps the ceiling an
// honest int: the product of a fill count and an unbounded float saturates
// when it is converted, to the largest int on arm64 and the smallest on amd64,
// where the floor check then holds the pool at the SMALLEST ceiling for asking
// for the largest. A refused value is a warning the operator can act on; a
// wrapped one is a silent cap.
const MaxCPUHeadroom = 100.0

// headroomFor resolves a pool's headroom: its own marker when it set one that
// reads as a number from one to MaxCPUHeadroom, otherwise the host's. ok is
// false for a marker that could not be read, so the caller can warn — a typo
// should tell the operator, not quietly hand the pool the default.
func headroomFor(marker string, host float64) (headroom float64, fromPool, ok bool) {
	marker = strings.TrimSpace(marker)
	if marker == "" {
		return host, false, true
	}
	v, err := strconv.ParseFloat(marker, 64)
	if err != nil || !headroomInRange(v) {
		return host, false, false
	}

	return v, true, true
}

// headroomInRange says whether a headroom is one the ceiling can carry: a
// finite number from one to MaxCPUHeadroom. NaN fails both comparisons, so it
// needs no case of its own.
func headroomInRange(v float64) bool {
	return v >= 1 && v <= MaxCPUHeadroom
}

// hostHeadroom is the headroom the host's flag resolves to inside plan: the
// default when unset, and otherwise held to the same range a pool's marker is.
// The command validates the flag before it gets here, so this is the guard for
// callers of the package that do not: the same saturation headroomFor refuses
// on a pool would otherwise arrive through Input.CPUHeadroom.
func hostHeadroom(v float64) float64 {
	switch {
	case v <= 0 || math.IsNaN(v):
		return DefaultCPUHeadroom
	case v < 1:
		return 1
	case v > MaxCPUHeadroom:
		return MaxCPUHeadroom
	}

	return v
}

// cpuCeiling is the number --cpu holds a pool at: the fill count with headroom
// on top, never below one worker per core plus one — a cap that leaves a box
// fewer workers than cores is not a cap on concurrency, it is a fault.
func cpuCeiling(fill, hostMillicores int, headroom float64) int {
	if fill <= 0 {
		return 0
	}
	// Clamped here too, so the conversion below can never saturate whatever
	// path a headroom arrived by.
	headroom = math.Min(math.Max(headroom, 1), MaxCPUHeadroom)
	ceiling := int(math.Ceil(float64(fill) * headroom))
	if floor := (hostMillicores+999)/1000 + 1; ceiling < floor {
		ceiling = floor
	}

	return ceiling
}

// boxCost prices a busy worker for the whole box: PHP's own millicores times
// the measured overhead when the fit can be believed, PHP's own alone
// otherwise.
func boxCost(ps *state.PoolState, opts state.Options, phpMillicores int) (millicores int, overhead float64, measured bool) {
	if a, ok := ps.BoxOverhead(opts); ok {
		return int(math.Round(float64(phpMillicores) * a)), a, true
	}

	return phpMillicores, 0, false
}

// cpuCeilingFor is the ceiling plan hands the allocator for one pool, but only
// for a pool that has been watched long enough to be CUT on memory evidence
// too. Twenty readings say what shape a pool's requests have; they are not
// permission to take workers away. See cpu.md, "What --cpu does".
func cpuCeilingFor(ps *state.PoolState, opts state.Options, hostMillicores int, headroom float64) int {
	if ps == nil || !ps.Trusted(opts) || !ps.CPUShapeKnown(opts) {
		return 0
	}
	_, php := cpuShape(ps.CPUShare(0.50))
	box, _, _ := boxCost(ps, opts, php)

	return cpuCeiling(fillWorkers(box, hostMillicores), hostMillicores, headroom)
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
	if c.BoxMeasured {
		line += fmt.Sprintf(" with MySQL, nginx and the kernel counted (%.1f× PHP's own)", c.Overhead)
	} else {
		line += " by PHP's own CPU (the rest of the box not measured yet)"
	}
	if c.Ceiling > 0 {
		line += fmt.Sprintf("; ceiling %d at %.2g× headroom", c.Ceiling, c.Headroom)
		if c.HeadroomFromPool {
			line += " (the pool's own)"
		}
	}
	switch {
	case c.CPUBound:
		line += fmt.Sprintf("; held there (now %d)", c.Current)
	case c.Allowed > 0:
		line += fmt.Sprintf("; plan allows %d (now %d)", c.Allowed, c.Current)
	case c.Current > 0:
		line += fmt.Sprintf("; now %d", c.Current)
	}
	if c.StarvedRounds > 0 {
		line += fmt.Sprintf("; queued in %d rounds while the box was full", c.StarvedRounds)
	}

	return line
}

// BoxPerWorker prints the all-in per-worker cost, or a dash until the box-cost
// fit can say.
func (c PoolCPU) BoxPerWorker() string {
	if !c.BoxMeasured {
		return "-"
	}

	return fmt.Sprintf("%dm", c.BoxMillicoresPerWorker)
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
	headroom float64,
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
			row.StarvedRounds = ps.CPUStarvedRounds
			if ps.CPUShapeKnown(opts) {
				row.Shape, row.MillicoresPerWorker = cpuShape(row.P50)
				row.BoxMillicoresPerWorker, row.Overhead, row.BoxMeasured = boxCost(ps, opts, row.MillicoresPerWorker)
				row.FillWorkers = fillWorkers(row.BoxMillicoresPerWorker, hostMillicores)
				own, fromPool, _ := headroomFor(v.CPUHeadroom, headroom)
				row.Ceiling = cpuCeiling(row.FillWorkers, hostMillicores, own)
				row.Headroom, row.HeadroomFromPool = own, fromPool
			}
		}
		if row.Shape != "" {
			row.Limit = "memory"
			if row.CPUBound || (row.Ceiling > 0 && atPlan > row.Ceiling) {
				row.Limit = "cpu"
			}

			host.NeededAtPlan += atPlan * row.BoxMillicoresPerWorker
			host.NeededNow += row.Current * row.BoxMillicoresPerWorker
		}

		out = append(out, row)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })

	return out, host
}
