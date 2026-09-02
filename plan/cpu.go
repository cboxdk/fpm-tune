package plan

import (
	"math"
	"sort"

	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
)

// PoolCPU is how CPU-bound one pool's requests have measured, for the report.
//
// Reporting only, and opt-in. Sizing is on memory, and a pool's CPU share is
// the dimension memory cannot see: past the point where a pool's busy workers
// fill the cores, more workers make every request slower rather than serving
// more of them. The number here is for the operator deciding whether the
// memory-sized ceiling is one this pool can actually use.
type PoolCPU struct {
	Name string

	// P50 and P90 are the share of a request's wall time spent on CPU, as
	// fractions: 0.72 is a request that computed for 72% of its duration. P50
	// is what the shape and the worker arithmetic are built on; P90 says how
	// much heavier the heavy requests are.
	P50, P90 float64
	Samples  int64

	// Shape classifies the pool, or is empty when there are too few readings
	// to call it: "cpu-bound", "mixed", "i/o-bound".
	Shape string

	// SaturatingWorkers is roughly how many of this pool's workers, all busy at
	// once, keep the host's cores fully occupied: cores divided by P50, rounded
	// up. Past it, concurrency stops buying throughput. Zero when the core
	// count or the share is unknown.
	SaturatingWorkers int

	// Allowed is the pm.max_children this plan gives the pool, so the report
	// can put the two numbers side by side. Zero when the pool is not written.
	Allowed int
}

// The shape thresholds, on the median share. Coarse on purpose: the point is
// to separate a pool whose workers compute from one whose workers wait, and
// finer distinctions than that are not what anyone sizes by hand on.
const (
	cpuBoundAbove = 0.50
	mixedAbove    = 0.20

	// minCPUReadings is how many requests a pool needs before it is called
	// anything. Twenty is a handful of minutes on a busy pool and an honest
	// "not yet" on a quiet one.
	minCPUReadings = 20
)

// cpuShape classifies a pool from its median share and reading count, and
// works out how many busy workers saturate the cores. A pure function of
// numbers, so it can be table-tested.
func cpuShape(p50 float64, samples int64, cores int) (shape string, saturating int) {
	if samples < minCPUReadings {
		return "", 0
	}

	switch {
	case p50 >= cpuBoundAbove:
		shape = "cpu-bound"
	case p50 >= mixedAbove:
		shape = "mixed"
	default:
		shape = "i/o-bound"
	}

	if cores > 0 && p50 > 0 {
		saturating = int(math.Ceil(float64(cores) / p50))
	}

	return shape, saturating
}

// cpuOf collects the CPU report for every pool that has a record, whether or
// not it has enough readings yet — a pool listed with "too few readings" tells
// the operator the measurement is running, where an absent row would not.
func cpuOf(views []observe.PoolView, st *state.State, cores int, allowed map[string]int) []PoolCPU {
	if st == nil {
		return nil
	}

	var out []PoolCPU
	for _, v := range views {
		if v.Err != nil {
			continue
		}
		ps := st.Lookup(v.Target.ConfigPath, v.Name)
		if ps == nil {
			continue
		}

		p50 := ps.CPUPercentile(0.50)
		shape, saturating := cpuShape(p50, ps.CPUSamples, cores)
		out = append(out, PoolCPU{
			Name:              v.Name,
			P50:               p50,
			P90:               ps.CPUPercentile(0.90),
			Samples:           ps.CPUSamples,
			Shape:             shape,
			SaturatingWorkers: saturating,
			Allowed:           allowed[v.Name],
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })

	return out
}
