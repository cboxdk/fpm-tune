package state

// How CPU-bound a pool's requests are, kept beside the memory it measures.
//
// Memory is the wrong dimension for a whole class of pool. A request that
// computes for most of its wall time gets SLOWER for every worker that runs
// beside it once the cores are full: the requests take longer, so the workers
// stay busy longer, so the queue stops draining — and the memory budget, which
// said there was room for eighty workers, saw none of it. Uncached WordPress is
// the common case, and the people who tune it by hand land on one and a half
// to two workers per core, not the fifty per core the allocator's coarse bound
// allows.
//
// This file measures that dimension, always: the number is in every status
// response the scrape already fetches, so measuring costs nothing and a plan
// can say which of the two — memory or CPU — is the one a pool runs out of
// first. Whether the measurement is allowed to CAP a pool is the operator's
// call (plan.Input.CPUCeiling); a report and a ceiling are different levels of
// trust and are switched separately.
//
// The source is php-fpm's own `last request cpu` from the full status page:
// the share of the request's wall time spent on CPU (user plus system, children
// included), as a percentage. A share of 70% is 700 millicores while the
// worker is busy — the same unit a container quota is written in.

const (
	// cpuBuckets covers 0% to 200% in 5% steps. Above 100% is possible — a
	// request that spawned a child which computed in parallel — and above the
	// top is a misread rather than a workload, so it lands in the last bucket.
	cpuBuckets     = 40
	cpuBucketWidth = 5.0
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
func (ps *PoolState) observeCPU(workers []WorkerSample) {
	seen := make(map[int]int64, len(workers))

	for _, w := range workers {
		if w.PID <= 0 || w.Requests <= 0 {
			// No pid: nothing to remember it by. No requests: no last request,
			// and nothing worth a map entry until there is one.
			continue
		}

		if !w.Idle {
			// php-fpm counts a request the moment it STARTS, so a running
			// worker's counter already names the request that is not finished.
			// Remembering that value would make the request read as "already
			// counted" the moment it completes — and a request that is still
			// running when the scrape lands is exactly the long, CPU-heavy one
			// this measurement exists to see. Keep whatever was remembered
			// before it, so the finished request is new next time.
			if prev, ok := ps.CPUSeen[w.PID]; ok {
				seen[w.PID] = prev
			}

			continue
		}

		// Remembered whether or not the reading is accepted below, so a
		// request rejected for being short, or for being our own, is not
		// re-examined on the next scrape either.
		seen[w.PID] = w.Requests

		if prev, ok := ps.CPUSeen[w.PID]; ok && w.Requests == prev {
			// The same request as last time. A counter that went DOWN is a
			// recycled pid — a new worker wearing an old number — and its
			// request is as new as one from a pid never seen.
			continue
		}
		if w.OwnRequest {
			// The status call and the opcache probe this tool sends every
			// scrape. On a quiet pool they are the only requests that move a
			// counter, and a large opcache's probe computes for well over the
			// floor below — so without this a staging pool reads as cpu-bound
			// from being watched.
			continue
		}
		if w.LastRequestMicros < minCPURequestMicros || w.LastRequestCPU < 0 {
			continue
		}

		if ps.CPUHistogram == nil {
			ps.CPUHistogram = make([]uint32, cpuBuckets)
		}
		histogramAdd(ps.CPUHistogram, &ps.CPUSamples, cpuBucketOf(w.LastRequestCPU))
	}

	ps.CPUSeen = seen
	if len(ps.CPUSeen) == 0 {
		ps.CPUSeen = nil
	}
}

// cpuBucketOf maps a percentage to its bucket, clamped at both ends.
func cpuBucketOf(percent float64) int {
	i := int(percent / cpuBucketWidth)
	if i < 0 {
		return 0
	}
	if i >= cpuBuckets {
		return cpuBuckets - 1
	}

	return i
}

// CPUShare is the CPU share of a request at the given fraction of the
// distribution, as a fraction of wall time (0.70 for 70%), and 0 when nothing
// has been recorded.
//
// Like Percentile it reports a bucket's FLOOR: every number this package
// produces is read as "at least this much", and the workers-per-core
// arithmetic built on it should err toward calling a pool less CPU-bound, not
// more.
func (ps *PoolState) CPUShare(p float64) float64 {
	i, ok := histogramBucketAt(ps.CPUHistogram, p)
	if !ok {
		return 0
	}

	return float64(i) * cpuBucketWidth / 100
}

// CPUShapeKnown reports whether enough requests have been read to say what
// shape this pool's requests have. Twenty is a few minutes on a busy pool and
// an honest "not yet" on a quiet one.
func (ps *PoolState) CPUShapeKnown(opts Options) bool {
	opts = opts.Defaults()

	return ps.CPUSamples >= int64(opts.MinCPUReadings)
}
