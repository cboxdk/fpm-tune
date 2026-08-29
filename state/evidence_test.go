package state

import (
	"fmt"
	"testing"
	"time"
)

// TestAnOvernightTrickleDoesNotCollapseTheEstimate.
//
// The gate on downward movement was a COUNT of requests between two scrapes,
// with a default of five. On a thirty-second scrape that is 0.17 requests a
// second — cron, uptime checks and crawlers clear it without trying. So a pool
// established at 400MiB a worker during the day spent the night being told by
// its own idle workers that it had got cheaper, and by morning was sized at
// 60MiB.
//
// What that costs is the whole point: 6144MiB divided by 60MiB is 102 workers,
// the morning's traffic makes those workers 400MiB again, and 40800MiB of
// php-fpm arrives on a machine with 6144MiB. The OOM killer decides which site
// survives.
func TestAnOvernightTrickleDoesNotCollapseTheEstimate(t *testing.T) {
	opts := Options{}.Defaults()
	s := New()
	night := time.Date(2026, 3, 1, 23, 0, 0, 0, time.UTC)

	// Established by a working day: 400MiB a worker at real traffic.
	var accepted int64
	for i := 0; i < 40; i++ {
		at := night.Add(time.Duration(i-40) * time.Minute)
		accepted += 60 * 30 // 30 requests a second
		s.Learn(Observation{Pool: "shop", At: at, ActiveNow: 8, Accepted: accepted,
			Workers: []WorkerSample{
				{RSSBytes: 400 * mb, Requests: 500},
				{RSSBytes: 400 * mb, Requests: 500},
			}}, opts)
	}

	established := s.Pools["shop"].SizingBytes()
	if established < 390*mb {
		t.Fatalf("setup: the pool established %s a worker, want ~400MiB",
			humanMiB(established))
	}

	// Eight hours of nothing but bots: 0.2 requests a second, and the surviving
	// workers idle down to 60MiB the way PHP workers do when they return large
	// allocations to the operating system.
	for i := 0; i < 8*120; i++ {
		at := night.Add(time.Duration(i) * 30 * time.Second)
		accepted += 6 // 6 requests per 30s scrape = 0.2/s
		s.Learn(Observation{Pool: "shop", At: at, ActiveNow: 0, Accepted: accepted,
			Workers: []WorkerSample{
				{RSSBytes: 60 * mb, Requests: 600},
				{RSSBytes: 60 * mb, Requests: 600},
			}}, opts)
	}

	if got := s.Pools["shop"].SizingBytes(); got < 390*mb {
		t.Errorf("after a night at 0.2 requests a second the estimate fell from %s to %s; "+
			"the workers got quiet, not cheap, and the morning is sized for workers that "+
			"do not exist", humanMiB(established), humanMiB(got))
	}
}

// TestTheScrapeIntervalDoesNotChangeTheAnswer.
//
// The half-life was moved into TIME precisely so that how often this tool looks
// could not change what it concludes. The gate on downward movement was left as
// a per-scrape count, which put the dependency straight back: the same 0.1
// requests a second held the estimate at a ten-second interval and collapsed it
// at two minutes, because a longer interval accumulates more requests into one
// comparison.
func TestTheScrapeIntervalDoesNotChangeTheAnswer(t *testing.T) {
	opts := Options{}.Defaults()

	run := func(interval time.Duration) int64 {
		s := New()
		base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		var accepted int64

		s.Learn(Observation{Pool: "p", At: base, ActiveNow: 4, Accepted: 0,
			Workers: []WorkerSample{
				{RSSBytes: 400 * mb, Requests: 500}, {RSSBytes: 400 * mb, Requests: 500},
			}}, opts)

		for at := base.Add(interval); at.Before(base.Add(6 * time.Hour)); at = at.Add(interval) {
			// 0.1 requests a second, whatever the interval.
			accepted += int64(interval.Seconds() / 10)
			s.Learn(Observation{Pool: "p", At: at, ActiveNow: 0, Accepted: accepted,
				Workers: []WorkerSample{
					{RSSBytes: 60 * mb, Requests: 600}, {RSSBytes: 60 * mb, Requests: 600},
				}}, opts)
		}

		return s.Pools["p"].SizingBytes()
	}

	for _, interval := range []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute} {
		if got := run(interval); got < 390*mb {
			t.Errorf("at a %s interval the same 0.1 requests a second took the estimate "+
				"to %s; the scrape interval is not supposed to be an input",
				interval, humanMiB(got))
		}
	}
}

// TestALargeReadingIsNeverDiscardedForBeingYoung.
//
// The peak was taken only over workers that had served enough requests to have
// loaded the application. A maximum can only ever be LOWERED by dropping
// candidates, so that filter could not do the job it was there for and could
// only lose large readings — the one direction that ends in an OOM.
//
// The scenario is ordinary: pm.max_requests recycles workers, and the scrape
// catches three fresh ones part-way through an export. The tool sees 2.1GiB of
// live worker memory and moves the estimate DOWN.
func TestALargeReadingIsNeverDiscardedForBeingYoung(t *testing.T) {
	opts := Options{}.Defaults()
	s := New()
	base := time.Now().Add(-time.Hour)

	var accepted int64
	for i := 0; i < 20; i++ {
		accepted += 3000
		s.Learn(Observation{Pool: "app", At: base.Add(time.Duration(i) * time.Minute),
			ActiveNow: 5, Accepted: accepted,
			Workers: []WorkerSample{
				{RSSBytes: 90 * mb, Requests: 400}, {RSSBytes: 91 * mb, Requests: 380},
			}}, opts)
	}

	accepted += 3000
	s.Learn(Observation{Pool: "app", At: base.Add(21 * time.Minute),
		ActiveNow: 5, Accepted: accepted,
		Workers: []WorkerSample{
			{RSSBytes: 700 * mb, Requests: 3},
			{RSSBytes: 690 * mb, Requests: 4},
			{RSSBytes: 705 * mb, Requests: 2},
			{RSSBytes: 90 * mb, Requests: 400},
			{RSSBytes: 88 * mb, Requests: 380},
		}}, opts)

	ps := s.Pools["app"]
	if ps.SizingBytes() < 700*mb {
		t.Errorf("sizing = %s after observing a 705MiB worker; a pool sized at 90MiB a "+
			"worker will fork enough of them to take the host down the next time an "+
			"export runs", humanMiB(ps.SizingBytes()))
	}
	if ps.HighWaterBytes < 700*mb {
		t.Errorf("high water = %s: the largest worker this tool has ever seen is the one "+
			"number an operator asks it for", humanMiB(ps.HighWaterBytes))
	}
}

// TestConfidenceIsNotEarnedByStandingStill.
//
// Confidence is permission to size a pool DOWN — a trusted pool's floor drops
// from its configured ceiling to two workers, which puts it first in the queue
// when a budget has to be cut. It was accruing whenever two mature workers
// existed, which is a fact about the past, not evidence about the workload.
//
// So a pool left with two warm workers from a deploy smoke test reached full
// confidence over thirty minutes of serving nothing at all, and was then cut on
// the strength of sixty-one readings of an idle process.
func TestConfidenceIsNotEarnedByStandingStill(t *testing.T) {
	opts := Options{}.Defaults()
	s := New()
	base := time.Now().Add(-time.Hour)

	for i := 0; i <= 60; i++ {
		s.Learn(Observation{Pool: "static", At: base.Add(time.Duration(i) * 30 * time.Second),
			ActiveNow: 0,
			Accepted:  1000, // frozen: not one request in half an hour
			Workers: []WorkerSample{
				{RSSBytes: 45 * mb, Requests: 400}, {RSSBytes: 45 * mb, Requests: 380},
			}}, opts)
	}

	ps := s.Pools["static"]
	if ps.Trusted(opts) {
		t.Errorf("confidence %.2f from %d samples over thirty minutes of zero traffic; "+
			"this pool is now first to be cut", ps.Confidence(opts), ps.Samples)
	}
}

// TestAClockStepDoesNotEraseConfidence: LastBusyAt was assigned unconditionally,
// so one NTP step backwards made the confidence span negative and took a fully
// trusted pool to zero — where its floor becomes its configured ceiling, which
// is the input shape that used to overcommit the allocator.
func TestAClockStepDoesNotEraseConfidence(t *testing.T) {
	opts := Options{}.Defaults()
	s := New()
	base := time.Now().Add(-2 * time.Hour)

	var accepted int64
	for i := 0; i < 30; i++ {
		accepted += 6000
		s.Learn(busyAt("p", base.Add(time.Duration(i)*2*time.Minute), accepted), opts)
	}
	if !s.Pools["p"].Trusted(opts) {
		t.Fatal("setup: the pool was meant to be trusted")
	}

	accepted += 6000
	s.Learn(busyAt("p", base.Add(-2*time.Hour), accepted), opts)

	if c := s.Pools["p"].Confidence(opts); c <= 0 {
		t.Errorf("confidence %.2f after the clock stepped backwards; an hour of real "+
			"evidence was thrown away by ntpd", c)
	}
}

func busyAt(pool string, at time.Time, accepted int64) Observation {
	return Observation{Pool: pool, At: at, ActiveNow: 6, Accepted: accepted,
		Workers: []WorkerSample{
			{RSSBytes: 64 * mb, Requests: 400}, {RSSBytes: 70 * mb, Requests: 350},
		}}
}

func humanMiB(b int64) string {
	return fmt.Sprintf("%dMiB", b/mb)
}
