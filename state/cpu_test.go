package state

import (
	"testing"
	"time"
)

// idleWorker is a worker whose last request finished: the only kind php-fpm
// reports a CPU figure for.
func idleWorker(pid int, requests int64, cpuPercent float64, micros int64) WorkerSample {
	return WorkerSample{
		RSSBytes: 50 * mb, Requests: requests,
		PID: pid, Idle: true, LastRequestCPU: cpuPercent, LastRequestMicros: micros,
	}
}

// TestCPUIsMeasuredWithoutBeingAskedFor: the number is in every status
// response, so it is recorded on every scrape — a plan has to be able to say
// which of memory and CPU a pool runs out of first without anyone having
// turned a switch a week earlier.
func TestCPUIsMeasuredWithoutBeingAskedFor(t *testing.T) {
	s := New()
	s.Learn(Observation{
		Pool: "www", At: time.Now(),
		Workers: []WorkerSample{idleWorker(1, 100, 80, 200_000), idleWorker(2, 100, 80, 200_000)},
	}, Options{})

	ps := s.Pools["www"]
	if ps.CPUSamples != 2 {
		t.Errorf("CPUSamples = %d, want 2: CPU is measured on every scrape", ps.CPUSamples)
	}
	if ps.CPUShapeKnown(Options{}) {
		t.Error("two readings are not a shape")
	}
}

// TestTheSameRequestIsCountedOnce is the dedupe, and the guard the whole
// measurement stands on. The status page reports each worker's LAST request,
// and a worker that served nothing since the previous scrape is still
// reporting the same one. On a quiet pool that is most workers most of the
// time, so without this a single request is counted every thirty seconds for
// as long as its worker lives, and the distribution describes how idle the
// pool is rather than what its requests look like.
func TestTheSameRequestIsCountedOnce(t *testing.T) {
	s := New()
	at := time.Now()

	// Scrape one: two idle workers, each with a finished request.
	s.Learn(Observation{Pool: "www", At: at, Workers: []WorkerSample{
		idleWorker(1, 10, 80, 200_000),
		idleWorker(2, 10, 20, 200_000),
	}}, Options{})
	ps := s.Pools["www"]
	if ps.CPUSamples != 2 {
		t.Fatalf("CPUSamples = %d after the first scrape, want 2", ps.CPUSamples)
	}

	// Scrape two: nothing happened. Same pids, same request counters.
	s.Learn(Observation{Pool: "www", At: at.Add(30 * time.Second), Workers: []WorkerSample{
		idleWorker(1, 10, 80, 200_000),
		idleWorker(2, 10, 20, 200_000),
	}}, Options{})
	if ps.CPUSamples != 2 {
		t.Errorf("CPUSamples = %d after an idle scrape, want 2: the same request was counted again", ps.CPUSamples)
	}

	// Scrape three: worker 1 served one more request; worker 2 did not.
	s.Learn(Observation{Pool: "www", At: at.Add(60 * time.Second), Workers: []WorkerSample{
		idleWorker(1, 11, 90, 200_000),
		idleWorker(2, 10, 20, 200_000),
	}}, Options{})
	if ps.CPUSamples != 3 {
		t.Errorf("CPUSamples = %d after one new request, want 3", ps.CPUSamples)
	}

	// A worker that is gone falls out of the dedupe map, and a recycled pid
	// whose counter is BELOW what was remembered is a new worker, not an old
	// request.
	s.Learn(Observation{Pool: "www", At: at.Add(90 * time.Second), Workers: []WorkerSample{
		idleWorker(1, 1, 50, 200_000),
	}}, Options{})
	if _, still := ps.CPUSeen[2]; still {
		t.Error("a worker that exited is still in the dedupe map")
	}
	if ps.CPUSamples != 4 {
		t.Errorf("CPUSamples = %d, want 4: a recycled pid's request is a new request", ps.CPUSamples)
	}
}

// TestARequestStillRunningAtTheScrapeIsCountedWhenItFinishes.
//
// php-fpm counts a request the moment it STARTS, so a worker seen Running
// with requests=12 shows requests=12 again once it is Idle. Remembering the
// running worker's counter made that finished request read as "already
// counted" — and a request that is still running when the scrape lands is
// exactly the long, CPU-heavy one this measurement exists to see.
func TestARequestStillRunningAtTheScrapeIsCountedWhenItFinishes(t *testing.T) {
	ps := &PoolState{}

	running := idleWorker(7, 12, 0, 0)
	running.Idle = false
	ps.observeCPU([]WorkerSample{running})
	if _, remembered := ps.CPUSeen[7]; remembered {
		t.Fatal("a running worker's counter was remembered; it names the request that is not finished")
	}

	ps.observeCPU([]WorkerSample{idleWorker(7, 12, 95, 20_000_000)})
	if ps.CPUSamples != 1 {
		t.Errorf("CPUSamples = %d, want 1: the request that spanned the scrape was never counted", ps.CPUSamples)
	}

	// And a worker that was counted, then seen running, then idle again with
	// the SAME counter as before it ran, is the old request — the remembered
	// value carries across the running scrape.
	ps.observeCPU([]WorkerSample{func() WorkerSample { w := idleWorker(7, 13, 0, 0); w.Idle = false; return w }()})
	if ps.CPUSeen[7] != 12 {
		t.Errorf("CPUSeen[7] = %d across a running scrape, want the remembered 12", ps.CPUSeen[7])
	}
	ps.observeCPU([]WorkerSample{idleWorker(7, 13, 60, 200_000)})
	if ps.CPUSamples != 2 {
		t.Errorf("CPUSamples = %d, want 2", ps.CPUSamples)
	}
}

// TestOurOwnRequestsAreNotTheSite: every scrape sends the status call and an
// opcache probe through a worker. On a quiet pool they are the only requests
// that move a counter, and a large opcache's probe computes for well over the
// duration floor — so without the exclusion a staging pool reads as cpu-bound
// from being watched. The counter is still remembered, so the same probe is
// not re-examined next scrape.
func TestOurOwnRequestsAreNotTheSite(t *testing.T) {
	ps := &PoolState{}
	probe := idleWorker(1, 5, 100, 300_000)
	probe.OwnRequest = true

	ps.observeCPU([]WorkerSample{probe, idleWorker(2, 5, 30, 300_000)})
	if ps.CPUSamples != 1 {
		t.Errorf("CPUSamples = %d, want 1: the tool's own request was measured as the site's", ps.CPUSamples)
	}
	if ps.CPUSeen[1] != 5 {
		t.Errorf("CPUSeen[1] = %d, want 5: a rejected reading is still remembered", ps.CPUSeen[1])
	}
}

// TestShortRequestsAreNotBelieved: php-fpm computes the share from a clock
// that ticks at 100Hz, so a two-millisecond request reads as 0% or 500%
// depending on whether it caught a tick. That is the clock's resolution, not
// the request's shape.
func TestShortRequestsAreNotBelieved(t *testing.T) {
	ps := &PoolState{}
	ps.observeCPU([]WorkerSample{
		idleWorker(1, 5, 500, 2_000),  // 2ms: caught a tick
		idleWorker(2, 5, 0, 2_000),    // 2ms: missed it
		idleWorker(3, 5, 60, 49_999),  // just under the floor
		idleWorker(4, 5, 60, 50_000),  // on it
		idleWorker(5, 5, 60, 900_000), // well over
	})
	if ps.CPUSamples != 2 {
		t.Errorf("recorded %d readings, want 2: only requests of 50ms or more", ps.CPUSamples)
	}
}

// TestWorkersWithNothingToReportAreSkipped: a worker that has served nothing
// has no last request, and one without a pid cannot be remembered.
func TestWorkersWithNothingToReportAreSkipped(t *testing.T) {
	ps := &PoolState{}
	fresh := idleWorker(2, 0, 0, 0)
	noPID := idleWorker(0, 5, 70, 200_000)

	ps.observeCPU([]WorkerSample{fresh, noPID})
	if ps.CPUSamples != 0 {
		t.Errorf("recorded %d readings from workers that had none to give", ps.CPUSamples)
	}
	if ps.CPUSeen != nil {
		t.Errorf("CPUSeen = %v, want nil: nothing here is worth a map entry", ps.CPUSeen)
	}
}

// TestCPUShareDescribesWhatWasSeen: the report's numbers are the floor of the
// bucket the fraction lands in, a share above 100% is a request whose children
// computed alongside it, and a number no host can produce lands in the last
// bucket rather than anywhere the arithmetic could believe.
func TestCPUShareDescribesWhatWasSeen(t *testing.T) {
	ps := &PoolState{}
	if got := ps.CPUShare(0.5); got != 0 {
		t.Errorf("an empty distribution reports %v, want 0", got)
	}

	// Ninety requests at 12%, nine at 72%, one at 130% (a child computed in
	// parallel): an API pool with a heavy export endpoint.
	var workers []WorkerSample
	pid := 1
	add := func(n int, cpu float64) {
		for i := 0; i < n; i++ {
			workers = append(workers, idleWorker(pid, 1, cpu, 200_000))
			pid++
		}
	}
	add(90, 12)
	add(9, 72)
	add(1, 130)
	ps.observeCPU(workers)

	for _, tc := range []struct{ p, want float64 }{
		{0.50, 0.10}, // 12% lands in the 10-15% bucket, reported as its floor
		{0.90, 0.10},
		{0.95, 0.70},
		{1.00, 1.25}, // above 100% the steps are 25% wide: 130% reads as 125%
	} {
		if got := ps.CPUShare(tc.p); got < tc.want-0.001 || got > tc.want+0.001 {
			t.Errorf("p%.0f = %.2f, want %.2f", tc.p*100, got, tc.want)
		}
	}

	// A request that ran ffmpeg on eight cores: 800%, and it has to read as
	// such — a top of 200% would tell the plan one busy worker costs a fifth
	// of what it costs, in the direction that fills a host.
	pid++
	ps.observeCPU([]WorkerSample{idleWorker(pid, 1, 830, 5_000_000)})
	if got := ps.CPUShare(1); got < 7.99 || got > 8.01 {
		t.Errorf("an 830%% reading came out as %.2f, want 8.00 (the 800%% bucket's floor)", got)
	}

	// And a number no host can produce lands in the last bucket.
	pid++
	ps.observeCPU([]WorkerSample{idleWorker(pid, 1, 90_000, 200_000)})
	if got := ps.CPUShare(1); got != 32.0 {
		t.Errorf("a 90000%% reading came out as %.2f; it should be clamped into the last bucket", got)
	}
}

// TestTheCPUBucketLayout pins the three ranges and their boundaries, so an
// edit to the floors table (a `<=` that adds 100 twice, say) fails here and
// not in a state file.
func TestTheCPUBucketLayout(t *testing.T) {
	if len(cpuBucketFloors) != cpuBuckets {
		t.Fatalf("len(cpuBucketFloors) = %d, cpuBuckets = %d", len(cpuBucketFloors), cpuBuckets)
	}
	for _, tc := range []struct{ percent, floor float64 }{
		{-1, 0}, {0, 0}, {4.9, 0}, {5, 5}, {99, 95}, {100, 100}, {124, 100}, {125, 125},
		{399, 375}, {400, 400}, {499, 400}, {3199, 3100}, {3200, 3200}, {90_000, 3200},
	} {
		if got := cpuBucketFloors[cpuBucketOf(tc.percent)]; got != tc.floor {
			t.Errorf("%.1f%% filed under floor %.0f, want %.0f", tc.percent, got, tc.floor)
		}
	}
	if cpuBucketOf(90_000) != cpuBuckets-1 {
		t.Error("a share past the top is not in the last bucket")
	}
}

// TestAHistogramFromAnotherLayoutIsDroppedNotIndexed: the buckets are
// addressed by index, so a state file written by a build with a different
// bucket count would be read under the wrong floors and then indexed past its
// end. A description rebuilds itself within a day; a crashed daemon does not.
func TestAHistogramFromAnotherLayoutIsDroppedNotIndexed(t *testing.T) {
	s := New()
	s.Pools["www"] = &PoolState{Pool: "www", CPUHistogram: make([]uint32, 40), CPUSamples: 12, RSSHistogram: make([]uint32, 3), RSSSamples: 3}
	path := t.TempDir() + "/state.json"
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ps := loaded.Pools["www"]
	if ps.CPUHistogram != nil || ps.CPUSamples != 0 || ps.RSSHistogram != nil || ps.RSSSamples != 0 {
		t.Errorf("misshapen histograms survived the load: %+v", ps)
	}
	// And the load says what it did, so the journal explains a pool that
	// flipped to "too few readings" after an upgrade.
	if len(loaded.Notices) != 2 {
		t.Errorf("Notices = %v, want one per dropped histogram", loaded.Notices)
	}
	// And a scrape after that must not panic.
	ps.observeCPU([]WorkerSample{idleWorker(1, 5, 3_100, 1_000_000)})
	if ps.CPUSamples != 1 {
		t.Errorf("CPUSamples = %d after a reading into a rebuilt histogram", ps.CPUSamples)
	}
}

// TestCPUHistogramDecays: an all-time record of a pool redeployed six months
// ago describes an application that no longer exists. Halving keeps the shape
// while letting the past fade, as the memory histogram does — the same code.
func TestCPUHistogramDecays(t *testing.T) {
	ps := &PoolState{}
	for i := 0; i < decayAfter+1; i++ {
		ps.observeCPU([]WorkerSample{idleWorker(i+1, 1, 80, 200_000)})
	}
	if ps.CPUSamples > int64(decayAfter)/2+1 {
		t.Errorf("CPUSamples = %d after %d readings; the histogram did not halve", ps.CPUSamples, decayAfter+1)
	}
	if got := ps.CPUShare(0.5); got < 0.79 || got > 0.81 {
		t.Errorf("p50 = %.2f after decay, want 0.80: halving must keep the shape", got)
	}
}

// TestCPUSurvivesTheStateFile: what was learned round-trips, dedupe map
// included — a daemon restart must not re-count every worker's last request.
func TestCPUSurvivesTheStateFile(t *testing.T) {
	s := New()
	s.Learn(Observation{Pool: "www", At: time.Now(), Workers: []WorkerSample{
		idleWorker(1, 10, 80, 200_000),
	}}, Options{})

	path := t.TempDir() + "/state.json"
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ps := loaded.Pools["www"]
	if ps.CPUSamples != 1 || ps.CPUSeen[1] != 10 {
		t.Errorf("after a round-trip: samples=%d seen=%v", ps.CPUSamples, ps.CPUSeen)
	}

	loaded.Learn(Observation{Pool: "www", At: time.Now(), Workers: []WorkerSample{
		idleWorker(1, 10, 80, 200_000),
	}}, Options{})
	if ps.CPUSamples != 1 {
		t.Errorf("CPUSamples = %d after a restart saw the same request again, want 1", ps.CPUSamples)
	}
}

// TestTheShapeNeedsTwentyReadings: the threshold is an Option like the memory
// confidence it mirrors, and defaults to twenty.
func TestTheShapeNeedsTwentyReadings(t *testing.T) {
	ps := &PoolState{CPUSamples: 19}
	if ps.CPUShapeKnown(Options{}) {
		t.Error("19 readings called a shape")
	}
	ps.CPUSamples = 20
	if !ps.CPUShapeKnown(Options{}) {
		t.Error("20 readings did not")
	}
	if ps.CPUShapeKnown(Options{MinCPUReadings: 50}) {
		t.Error("the option was not honoured")
	}
}
