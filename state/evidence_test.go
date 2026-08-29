package state

import (
	"fmt"
	"os"
	"path/filepath"
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

// TestAGapDoesNotBuyABiggerStepThanAScrape.
//
// The scenario is a package upgrade. fpm-tune is restarted while php-fpm keeps
// running and keeps serving, so the state file persists, the request counter
// climbs across the gap, and the first scrape back lands six hours later on
// workers that have gone quiet.
//
// Six hours against a thirty-minute half-life is the largest step the clamp
// permits — measured, a pool established at 300MiB a worker fell to 180MiB on
// ONE reading, and every gap from thirty minutes to twelve hours produced that
// identical maximum step. serve plans immediately on start, so it is the first
// plan after every restart: +68% workers, past the growth gate, written and
// reloaded, and at the pool's real cost that configuration needs 13.5GiB on a
// 9GiB budget.
//
// Elapsed time is only evidence of decay if it was WATCHED, and the gap is
// exactly the time that was not.
func TestAGapDoesNotBuyABiggerStepThanAScrape(t *testing.T) {
	opts := Options{}.Defaults()
	base := time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)

	run := func(gap time.Duration) int64 {
		s := New()
		var accepted int64

		// Two hours at 6 requests a second, thirty-second scrapes, workers at
		// 300MiB. Real traffic throughout: the rate gate is not what is being
		// tested here.
		at := base
		for i := 0; i < 240; i++ {
			accepted += 180
			s.Learn(Observation{Pool: "shop", At: at, ActiveNow: 6, Accepted: accepted,
				Workers: []WorkerSample{
					{RSSBytes: 300 * mb, Requests: 500}, {RSSBytes: 300 * mb, Requests: 500},
				}}, opts)
			at = at.Add(30 * time.Second)
		}

		// The gap. php-fpm kept serving at the same rate throughout.
		at = at.Add(gap)
		accepted += int64(gap.Seconds() * 6)

		s.Learn(Observation{Pool: "shop", At: at, ActiveNow: 2, Accepted: accepted,
			Workers: []WorkerSample{
				{RSSBytes: 60 * mb, Requests: 500}, {RSSBytes: 58 * mb, Requests: 500},
			}}, opts)

		return s.Pools["shop"].SizingBytes()
	}

	// A normal scrape is the reference: whatever one reading is allowed to do,
	// a hole may not do more.
	reference := run(30 * time.Second)

	for _, gap := range []time.Duration{
		30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 11*time.Hour + 59*time.Minute,
	} {
		if got := run(gap); got < reference {
			t.Errorf("a %s hole took the estimate to %dMiB, against %dMiB for a single "+
				"ordinary scrape: the time nobody was watching was counted as evidence "+
				"that the pool got cheaper", gap, got/mb, reference/mb)
		}
	}
}

// TestAPoolThatOnlyEverRunsOneWorkerIsStillMeasured.
//
// An ondemand pool serving 0.4 requests a second at 150ms a request runs one
// worker, always. The maturity gate wanted two, so — measured over seven days
// and 20,160 scrapes — such a pool was never learned from once. SizingBytes
// stayed at zero and the plan sized it from a 48MiB profile guess against a
// 90MiB truth, permanently.
//
// On a host with two of them the plan reported 384MiB of headroom while being
// 1056MiB over its budget: half the OS reserve, and the pools that COULD be
// measured had been grown into the difference.
func TestAPoolThatOnlyEverRunsOneWorkerIsStillMeasured(t *testing.T) {
	opts := Options{}.Defaults()
	s := New()
	base := time.Now().Add(-4 * time.Hour)

	var accepted int64
	for i := 0; i < 200; i++ {
		// Busy enough to clear the decay gate, and still one worker: 3 requests
		// a second answered in 20ms is a concurrency of 0.06. That combination
		// isolates the rule — the pool IS working, so nothing but the maturity
		// count can be what stops it being trusted.
		accepted += 90 // 3 req/s across a 30s scrape
		s.Learn(Observation{Pool: "ondemand", At: base.Add(time.Duration(i) * 30 * time.Second),
			ActiveNow: 1, Accepted: accepted,
			Workers: []WorkerSample{{RSSBytes: 90 * mb, Requests: 400}},
		}, opts)
	}

	ps := s.Pools["ondemand"]
	if ps == nil || ps.SizingBytes() < 85*mb {
		t.Fatalf("a pool that has run one 90MiB worker for four hours is sized at %v; the "+
			"host is budgeted against a profile guess", ps)
	}

	// And it must NOT have earned permission to be cut. One worker is a
	// measurement, not a traffic pattern — and a trusted ondemand pool has its
	// floor dropped from its configured ceiling to two.
	if ps.Trusted(opts) {
		t.Errorf("confidence %.2f from a pool that has never run two workers at once: "+
			"reserving what it costs is not the same as earning permission to cut it",
			ps.Confidence(opts))
	}
}

// TestAPoolThatHasRunTwoWorkersStillNeedsTwo: the relaxation applies only while
// a pool has never had two mature workers. A busy pool caught mid-recycle with
// one mature worker must not be measured off that anecdote.
func TestAPoolThatHasRunTwoWorkersStillNeedsTwo(t *testing.T) {
	opts := Options{}.Defaults()
	s := New()
	base := time.Now().Add(-time.Hour)

	var accepted int64
	for i := 0; i < 30; i++ {
		accepted += 3000
		s.Learn(Observation{Pool: "busy", At: base.Add(time.Duration(i) * time.Minute),
			ActiveNow: 8, Accepted: accepted,
			Workers: []WorkerSample{
				{RSSBytes: 200 * mb, Requests: 400}, {RSSBytes: 200 * mb, Requests: 400},
			}}, opts)
	}
	s.Pools["busy"].ObservePeak(8, base.Add(30*time.Minute), opts)

	before := s.Pools["busy"].SizingBytes()

	// One mature worker left, and it is small.
	accepted += 3000
	s.Learn(Observation{Pool: "busy", At: base.Add(31 * time.Minute),
		ActiveNow: 1, Accepted: accepted,
		Workers: []WorkerSample{{RSSBytes: 30 * mb, Requests: 400}},
	}, opts)

	if got := s.Pools["busy"].SizingBytes(); got < before {
		t.Errorf("a pool known to run eight workers was re-measured from the single one "+
			"left mid-recycle: %dMiB to %dMiB", before/mb, got/mb)
	}
}

// TestAPoolThatRecyclesItsWorkersFastIsStillMeasured.
//
// pm.max_requests at or below the maturity threshold means no worker ever
// reaches it. Measured across a full weekday at up to 25 requests a second: at
// pm.max_requests = 20 a fully loaded pool learned from 0 of 2880 scrapes, and
// the same at 15 and 10. It fell back to a 48MiB profile guess against a 120MiB
// truth, permanently — the same blind spot as a pool that never runs two
// workers, reached from the other side.
//
// A young worker is worse evidence than an old one. It is much better evidence
// than a table.
func TestAPoolThatRecyclesItsWorkersFastIsStillMeasured(t *testing.T) {
	opts := Options{}.Defaults()
	s := New()
	base := time.Now().Add(-4 * time.Hour)

	var accepted int64
	for i := 0; i < 100; i++ {
		accepted += 750 // 25 req/s
		s.Learn(Observation{Pool: "churny", At: base.Add(time.Duration(i) * 30 * time.Second),
			ActiveNow: 8, Accepted: accepted,
			Workers: []WorkerSample{
				// pm.max_requests = 10: nothing ever reaches the threshold of 20.
				{RSSBytes: 120 * mb, Requests: 3},
				{RSSBytes: 118 * mb, Requests: 7},
				{RSSBytes: 121 * mb, Requests: 1},
			}}, opts)
	}

	ps := s.Pools["churny"]
	if ps == nil || ps.SizingBytes() < 110*mb {
		t.Fatalf("a fully loaded pool recycling its workers every 10 requests measured "+
			"nothing across four hours: %v", ps)
	}

	// Still not trusted: it has never produced a mature worker, so nothing has
	// established what it costs once warm.
	if ps.Trusted(opts) {
		t.Errorf("confidence %.2f from a pool whose workers never warm up", ps.Confidence(opts))
	}
}

// TestTheMaturityGateStillHoldsAtFirst: the waiver is for a pool that has proved
// over time that it cannot produce a mature worker, not for the first few
// scrapes of an ordinary one starting up.
func TestTheMaturityGateStillHoldsAtFirst(t *testing.T) {
	opts := Options{}.Defaults()
	s := New()
	base := time.Now().Add(-time.Hour)

	for i := 0; i < 5; i++ {
		s.Learn(Observation{Pool: "starting", At: base.Add(time.Duration(i) * 30 * time.Second),
			ActiveNow: 2, Accepted: int64(100 * (i + 1)),
			Workers: []WorkerSample{
				{RSSBytes: 20 * mb, Requests: 2}, {RSSBytes: 22 * mb, Requests: 1},
			}}, opts)
	}

	if got := s.Pools["starting"].SizingBytes(); got > 0 {
		t.Errorf("a pool five scrapes old was sized at %dMiB from workers that have "+
			"served two requests; those workers have not loaded the application yet",
			got/mb)
	}
}

// TestStateWrittenBeforeTheCadenceWasRecordedIsSafeOnItsFirstScrape.
//
// The cadence bound is what stops a hole in the observations buying a full
// decay step. State written by the version that shipped before it has no
// cadence — so the very first scrape after the upgrade that fixed the problem
// is a pool with no cadence and hours of elapsed time, which is the uncapped
// step in its original form.
//
// The upgrade is itself the gap, which makes this the likeliest moment for it.
func TestStateWrittenBeforeTheCadenceWasRecordedIsSafeOnItsFirstScrape(t *testing.T) {
	opts := Options{}.Defaults()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// Round-12 shape: a pool learned at thirty-second scrapes over two hours,
	// with no typical_interval_seconds field at all.
	night := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)
	legacy := `{"version":1,"pools":{"shop":{"pool":"shop","typical_peak_bytes":314572800,` +
		`"last_peak_bytes":314572800,"high_water_bytes":314572800,"samples":240,` +
		`"busy_samples":240,"last_accepted":100000,` +
		`"first_seen":"` + night.Add(-2*time.Hour).Format(time.RFC3339) + `",` +
		`"last_updated":"` + night.Format(time.RFC3339) + `",` +
		`"first_busy_at":"` + night.Add(-2*time.Hour).Format(time.RFC3339) + `",` +
		`"last_busy_at":"` + night.Format(time.RFC3339) + `"}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	before := s.Pools["shop"].SizingBytes()

	// Six hours later — the upgrade — php-fpm has kept serving throughout and
	// the surviving workers have gone quiet.
	s.Learn(Observation{Pool: "shop", At: night.Add(6 * time.Hour), ActiveNow: 2,
		Accepted: 100000 + 6*3600*6,
		Workers: []WorkerSample{
			{RSSBytes: 60 * mb, Requests: 500}, {RSSBytes: 58 * mb, Requests: 500},
		}}, opts)

	got := s.Pools["shop"].SizingBytes()
	if got < before*9/10 {
		t.Errorf("the first scrape after the upgrade took the estimate from %dMiB to "+
			"%dMiB; the state carried no cadence, so the six hours nobody was watching "+
			"were counted in full — which is the fault the cadence was added to fix, on "+
			"the one run where it matters most", before/mb, got/mb)
	}
}

// TestTheRecycledWaiverIsEarnedByRecyclingNotByIdling.
//
// The waiver reads a pool's immature workers once it has proved it cannot
// produce a mature one. Counting total samples let an ondemand pool sitting
// visible-but-empty for twenty scrapes earn it without having been seen to
// recycle anything — and its first burst of four one-request workers at 80MiB
// was then taken as the pool's cost, against a 200MiB truth.
func TestTheRecycledWaiverIsEarnedByRecyclingNotByIdling(t *testing.T) {
	opts := Options{}.Defaults()
	s := New()
	base := time.Now().Add(-time.Hour)

	// Visible and empty: no workers at all.
	for i := 0; i < 25; i++ {
		s.Learn(Observation{Pool: "ondemand", At: base.Add(time.Duration(i) * 30 * time.Second),
			ActiveNow: 0, Accepted: 0,
		}, opts)
	}

	// First traffic: four workers, none of them warm yet.
	s.Learn(Observation{Pool: "ondemand", At: base.Add(13 * time.Minute),
		ActiveNow: 4, Accepted: 400,
		Workers: []WorkerSample{
			{RSSBytes: 80 * mb, Requests: 1}, {RSSBytes: 80 * mb, Requests: 2},
			{RSSBytes: 78 * mb, Requests: 3}, {RSSBytes: 81 * mb, Requests: 1},
		}}, opts)

	if got := s.Pools["ondemand"].SizingBytes(); got > 0 {
		t.Errorf("a pool that had been idle, not recycling, was sized at %dMiB from four "+
			"workers that have served one request each; they have not loaded the "+
			"application yet, and its warm cost is more than twice that", got/mb)
	}
}

// TestARecyclingPoolKeepsLearningAfterADeploy.
//
// The waiver that lets an always-recycling pool be measured required that
// nothing had been measured yet, which made it one-shot: the first waived
// reading set the estimate and every later reading was refused again. So a pool
// with pm.max_requests=10 learned what it cost the day it was first seen, and
// went on being costed at that for ever.
//
// A deploy is the case that matters. Workers that go from 80MiB to 220MiB are
// 2.75x the memory, and the plan kept dividing the budget as though nothing had
// changed — the exact under-measurement the waiver was added to prevent, one
// deploy later.
func TestARecyclingPoolKeepsLearningAfterADeploy(t *testing.T) {
	opts := Options{}.Defaults()
	s := New()
	base := time.Now().Add(-4 * time.Hour)

	var accepted int64
	at := base
	scrape := func(rss int64) {
		accepted += 750 // 25 req/s across a 30s scrape
		s.Learn(Observation{Pool: "churny", At: at, ActiveNow: 8, Accepted: accepted,
			Workers: []WorkerSample{
				// pm.max_requests = 10: nothing reaches the threshold of 20.
				{RSSBytes: rss, Requests: 3},
				{RSSBytes: rss - mb, Requests: 7},
				{RSSBytes: rss, Requests: 1},
			}}, opts)
		at = at.Add(30 * time.Second)
	}

	for i := 0; i < 60; i++ {
		scrape(80 * mb)
	}
	if got := s.Pools["churny"].SizingBytes(); got < 75*mb {
		t.Fatalf("setup: the pool measured %dMiB, want about 80", got/mb)
	}

	// The deploy.
	for i := 0; i < 60; i++ {
		scrape(220 * mb)
	}

	if got := s.Pools["churny"].SizingBytes(); got < 200*mb {
		t.Errorf("after a deploy made every worker 220MiB the pool is still costed at "+
			"%dMiB; the estimate is frozen at whatever it cost the day it was first "+
			"seen, and the budget is being divided as though nothing changed", got/mb)
	}
	// And still never trusted: nothing has established what it costs once warm.
	if s.Pools["churny"].Trusted(opts) {
		t.Error("a pool whose workers never mature was granted permission to be cut")
	}
}

// TestAColdWorkerIsNotEvidenceThatAPoolGotCheaper.
//
// The waiver that measures an always-recycling pool reads workers that never
// matured, and a worker on its first request has not loaded the application. At
// twenty-five requests a second most of what a scrape catches is exactly that,
// so the readings are a mixture of warm workers and cold ones — and the cold
// ones are not evidence of anything except that a fork is cheap.
//
// Measured before this: an established 240MiB pool fell to 50MiB across a night
// of nothing but recycled workers.
func TestAColdWorkerIsNotEvidenceThatAPoolGotCheaper(t *testing.T) {
	opts := Options{}.Defaults()
	s := New()
	at := time.Now().Add(-8 * time.Hour)

	var accepted int64
	learn := func(rss int64, requests int64, n int) {
		for i := 0; i < n; i++ {
			accepted += 3000
			s.Learn(Observation{Pool: "churny", At: at, ActiveNow: 8, Accepted: accepted,
				Workers: []WorkerSample{
					{RSSBytes: rss, Requests: requests},
					{RSSBytes: rss - mb, Requests: requests},
				}}, opts)
			at = at.Add(time.Minute)
		}
	}

	// Established properly, from warm workers.
	learn(240*mb, 500, 20)
	settled := s.Pools["churny"].SizingBytes()
	if settled < 230*mb {
		t.Fatalf("setup: settled at %dMiB", settled/mb)
	}

	// Then hours of nothing but freshly forked workers.
	learn(12*mb, 1, 200)

	if got := s.Pools["churny"].SizingBytes(); got < settled/2 {
		t.Errorf("readings from workers that had served one request took the estimate "+
			"from %dMiB to %dMiB; those workers had not loaded the application, and the "+
			"morning finds the pool sized for workers that do not exist",
			settled/mb, got/mb)
	}

	// And a real rise is still believed, from workers just as young.
	learn(700*mb, 2, 3)
	if got := s.Pools["churny"].SizingBytes(); got < 700*mb {
		t.Errorf("a 700MiB worker was ignored for being young: sizing = %dMiB. Upward is "+
			"real memory whatever the worker has served, and refusing it is how a host "+
			"is committed past what it has", got/mb)
	}
}

// TestAStateFileFromTheFutureGrantsNothing.
//
// Confidence is measured between FirstBusyAt and LastBusyAt, and LastBusyAt can
// only move FORWARD — so a state file carrying 2099 gives a pool full
// confidence for ever, and no live observation can ever shorten the span.
//
// Full confidence is permission to CUT. The pool is then permanently eligible
// to be trimmed on evidence that never existed, and nothing short of deleting
// the file recovers it. One NTP step, one restore from a mis-clocked host, one
// container with a dead RTC.
func TestAStateFileFromTheFutureGrantsNothing(t *testing.T) {
	opts := Options{}.Defaults()
	path := filepath.Join(t.TempDir(), "state.json")

	future := time.Now().AddDate(70, 0, 0)
	body := `{"version":1,"pools":{"shop":{"pool":"shop","typical_peak_bytes":104857600,` +
		`"last_peak_bytes":104857600,"samples":500,"busy_samples":500,` +
		`"first_seen":"` + future.Format(time.RFC3339) + `",` +
		`"last_updated":"` + future.Format(time.RFC3339) + `",` +
		`"first_busy_at":"` + future.Format(time.RFC3339) + `",` +
		`"last_busy_at":"` + future.AddDate(0, 5, 0).Format(time.RFC3339) + `"}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	ps := s.Pools["shop"]
	if ps.Trusted(opts) {
		t.Errorf("confidence %.2f from five months of evidence dated seventy years from "+
			"now; the pool is permanently eligible to be cut and no observation can "+
			"take it back", ps.Confidence(opts))
	}

	// What it MEASURED is kept. The timestamps are wrong; 100MiB a worker is
	// still the only thing anyone knows about this pool's cost, and throwing it
	// away would size the host from a table.
	if ps.SizingBytes() < 100*mb {
		t.Errorf("the measured cost was discarded along with the clock: %d", ps.SizingBytes())
	}
}

// TestAClockCorrectionDoesNotReleaseTheReloadBrake.
//
// LastAppliedAt is a brake: hysteresis reads it to refuse a reload within five
// minutes of the last one. Dropping it along with the other future timestamps
// says "nothing has ever been applied", which releases the brake — so an NTP
// correction of five minutes backwards, just after an apply, let the next round
// reload a pool it had reloaded a moment earlier.
//
// Clamped rather than cleared. For a value whose only job is to make the tool
// wait, keeping it is the safe direction.
func TestAClockCorrectionDoesNotReleaseTheReloadBrake(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	// Written by a host whose clock was five minutes fast.
	ahead := time.Now().Add(5 * time.Minute)
	body := `{"version":1,"pools":{"shop":{"pool":"shop","typical_peak_bytes":104857600,` +
		`"last_applied_max_children":12,` +
		`"last_applied_at":"` + ahead.Format(time.RFC3339) + `"}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	ps := s.Pools["shop"]
	if ps.LastAppliedAt.IsZero() {
		t.Error("the record of when this pool was last applied was thrown away with the " +
			"clock; hysteresis now believes nothing has ever been applied, and the next " +
			"round may reload a pool that was reloaded a minute ago")
	}
	if ps.LastAppliedAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("LastAppliedAt is still in the future: %v", ps.LastAppliedAt)
	}
}
