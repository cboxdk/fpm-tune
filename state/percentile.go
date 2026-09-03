package state

import "math"

// The distribution of worker memory, kept alongside the estimate that sizes.
//
// The sizing number is one number on purpose: an allocator needs a single cost
// per worker, and the asymmetric EWMA in SizingBytes is what makes that number
// safe to divide a budget by. But one number cannot answer the question an
// operator actually asks when they are deciding by hand — "how bad does this
// pool get?" — and a tool run in advisory mode exists to answer exactly that.
//
// A pool whose median worker is 60MiB and whose p99 is 400MiB is a different
// pool from one that sits flat at 90MiB, and they want different decisions.
// Neither the EWMA nor the high-water mark distinguishes them: the first hides
// the tail, the second is only the tail.

// rssBuckets covers 1MiB to 64GiB at four buckets per doubling — about 19% wide,
// which is finer than the decisions anyone makes about worker memory and cheap
// enough to keep per pool and write to disk.
const (
	rssBuckets     = 64
	bucketsPerOct  = 4
	firstBucketLog = 20 // 1MiB
)

// decayAfter is how many samples a pool accumulates before every bucket is
// halved.
//
// Without it the histogram is an all-time record, and an all-time record of a
// pool that was redeployed six months ago describes an application that no
// longer exists. Halving keeps the shape while letting the past fade, costs one
// pass over 64 integers, and needs no timestamps — which matters because the
// alternative is another thing that goes wrong when a clock steps.
const decayAfter = 4096

// observeRSS records one worker's memory.
func (ps *PoolState) observeRSS(bytes int64) {
	if bytes <= 0 {
		return
	}
	if ps.RSSHistogram == nil {
		ps.RSSHistogram = make([]uint32, rssBuckets)
	}

	histogramAdd(ps.RSSHistogram, &ps.RSSSamples, bucketOf(bytes))
}

// histogramAdd counts one reading in a bucket and keeps the sample count
// beside it, halving every bucket once the count passes decayAfter. Shared by
// the memory and CPU histograms, so the decay policy is one policy.
func histogramAdd(hist []uint32, samples *int64, bucket int) {
	hist[bucket]++
	*samples++

	if *samples > decayAfter {
		for i := range hist {
			hist[i] /= 2
		}
		*samples = histogramTotal(hist)
	}
}

func histogramTotal(hist []uint32) int64 {
	var n int64
	for _, c := range hist {
		n += int64(c)
	}

	return n
}

// histogramBucketAt is the first bucket whose cumulative count reaches the
// given fraction of the readings, and false when there are none. The caller
// turns the index into a value; the walk is the same for every histogram.
func histogramBucketAt(hist []uint32, p float64) (int, bool) {
	total := histogramTotal(hist)
	if total == 0 {
		return 0, false
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
	for i, c := range hist {
		seen += int64(c)
		if seen >= want {
			return i, true
		}
	}

	return len(hist) - 1, true
}

// bucketOf maps a size to its bucket, clamped at both ends. Anything below 1MiB
// is a worker that has not loaded anything; anything above 64GiB is a misread.
func bucketOf(bytes int64) int {
	octaves := math.Log2(float64(bytes)) - firstBucketLog
	i := int(octaves * bucketsPerOct)

	if i < 0 {
		return 0
	}
	if i >= rssBuckets {
		return rssBuckets - 1
	}

	return i
}

// bucketFloor is the smallest size a bucket holds, which is what a percentile
// reports.
//
// The FLOOR rather than the midpoint, deliberately: every number this package
// produces about memory is read as "at least this much", and a percentile that
// rounds up is a percentile that quietly recommends more than the evidence.
func bucketFloor(i int) int64 {
	return int64(math.Exp2(firstBucketLog + float64(i)/bucketsPerOct))
}

// Percentile is the worker memory at the given fraction of the distribution,
// 0 when nothing has been recorded.
//
// Reporting only. Sizing goes through SizingBytes, and the two answer different
// questions: this one describes what has been seen, that one decides what to
// reserve.
func (ps *PoolState) Percentile(p float64) int64 {
	i, ok := histogramBucketAt(ps.RSSHistogram, p)
	if !ok {
		return 0
	}

	return bucketFloor(i)
}
