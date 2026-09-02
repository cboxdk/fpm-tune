package state

import "math"

// How CPU-bound a pool's requests are, kept beside the memory it measures.
//
// Memory is what this tool sizes on, and memory is the wrong dimension for a
// whole class of pool. A request that computes for most of its wall time gets
// SLOWER for every worker that runs beside it once the cores are full: the
// requests take longer, so the workers stay busy longer, so the queue stops
// draining — and the memory budget, which said there was room for eighty
// workers, saw none of it. Uncached WordPress is the common case, and the
// operators who tune it by hand land on one and a half to two workers per core,
// not the fifty per core the memory arithmetic allows.
//
// This file measures that dimension. It does not act on it: a pool's CPU share
// is reported for the person deciding by hand, and sizing stays on memory until
// the readings have been checked against real hosts. Opt-in (Options.MeasureCPU)
// for the same reason.
//
// The source is php-fpm's own `last request cpu` from the full status page,
// which the scrape already fetches: the share of the request's wall time spent
// on CPU (user plus system, children included), as a percentage. It costs no
// extra read and no extra permission, which is what makes it the right first
// step.

const (
	// cpuBuckets covers 0% to 200% in 5% steps. Above 100% is possible — a
	// request that spawned a child which computed in parallel — and above 200%
	// is a misread rather than a workload, so it is clamped into the last bucket.
	cpuBuckets      = 40
	cpuBucketWidth  = 5.0
	cpuMaxPercent   = 200.0
	cpuDecayAfter   = decayAfter
	cpuBucketToFrac = cpuBucketWidth / 100
)

// minCPURequestMicros is the shortest request whose CPU share is believed.
//
// php-fpm computes the share from the process clock, which ticks at 100Hz on
// most kernels. A request that took two milliseconds either caught a tick or
// did not, and reads as 0% or 500% accordingly — that is the clock's
// resolution, not the request's shape. Fifty milliseconds is five ticks, which
// bounds the error at about a fifth; short requests are also the ones a
// worker-count decision matters least for.
const minCPURequestMicros = 50_000

// observeCPU records the CPU share of each worker's most recent request, once.
//
// The status page reports the LAST request of each worker, and a worker that
// has served nothing since the previous scrape is still reporting the same
// one. On a quiet pool that is most of them, most of the time — so without
// the dedupe a single request is counted every thirty seconds for as long as
// the worker lives, and the distribution describes how idle the pool is
// rather than what its requests look like. The request counter is what tells
// the two apart: it moved, or it did not.
//
// Returns how many readings were taken.
func (ps *PoolState) observeCPU(workers []WorkerSample) int {
	// Every live worker's counter is remembered, whether or not its reading
	// was accepted, so a request rejected for being short is not re-examined
	// on the next scrape either. Workers that are gone fall out of the map here.
	seen := make(map[int]int64, len(workers))
	recorded := 0

	for _, w := range workers {
		if w.PID <= 0 {
			continue
		}
		seen[w.PID] = w.Requests

		if !w.Idle || w.Requests <= 0 {
			// Running: the figure is zero because the request is not finished.
			// No requests: there is no last request.
			continue
		}
		if prev, ok := ps.CPUSeen[w.PID]; ok && w.Requests == prev {
			// The same request as last time. A counter that went DOWN is a
			// recycled pid — a new worker wearing an old number — and its
			// request is as new as one from a pid never seen.
			continue
		}
		if w.LastRequestMicros < minCPURequestMicros || w.LastRequestCPU < 0 {
			continue
		}

		if ps.CPUHistogram == nil {
			ps.CPUHistogram = make([]uint32, cpuBuckets)
		}
		ps.CPUHistogram[cpuBucketOf(w.LastRequestCPU)]++
		ps.CPUSamples++
		recorded++

		if ps.CPUSamples > cpuDecayAfter {
			for i := range ps.CPUHistogram {
				ps.CPUHistogram[i] /= 2
			}
			ps.CPUSamples = ps.cpuTotal()
		}
	}

	ps.CPUSeen = seen
	if len(ps.CPUSeen) == 0 {
		ps.CPUSeen = nil
	}

	return recorded
}

func (ps *PoolState) cpuTotal() int64 {
	var n int64
	for _, c := range ps.CPUHistogram {
		n += int64(c)
	}

	return n
}

// cpuBucketOf maps a percentage to its bucket, clamped at both ends.
func cpuBucketOf(percent float64) int {
	if percent >= cpuMaxPercent {
		return cpuBuckets - 1
	}
	i := int(percent / cpuBucketWidth)
	if i < 0 {
		return 0
	}
	if i >= cpuBuckets {
		return cpuBuckets - 1
	}

	return i
}

// CPUPercentile is the CPU share at the given fraction of the distribution, as
// a fraction of wall time (0.72 for 72%), and 0 when nothing has been recorded.
//
// Like Percentile it reports a bucket's FLOOR: every number this package
// produces is read as "at least this much", and the cores-per-worker arithmetic
// built on it should err toward calling a pool less CPU-bound, not more.
func (ps *PoolState) CPUPercentile(p float64) float64 {
	total := ps.cpuTotal()
	if total == 0 {
		return 0
	}
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}

	want := int64(math.Ceil(p * float64(total)))
	if want < 1 {
		want = 1
	}

	var seen int64
	for i, c := range ps.CPUHistogram {
		seen += int64(c)
		if seen >= want {
			return float64(i) * cpuBucketToFrac
		}
	}

	return float64(cpuBuckets-1) * cpuBucketToFrac
}
