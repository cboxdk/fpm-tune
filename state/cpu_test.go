package state

import (
	"encoding/json"
	"strings"
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

// TestCPUIsNotMeasuredUnlessAskedFor: the feature is opt-in, and opt-in means
// the state file carries nothing about it unless the operator turned it on. A
// histogram that appears on its own is a state file the operator did not ask
// for, and the mark of a measurement nobody has checked yet leaking into the
// baseline of everyone.
func TestCPUIsNotMeasuredUnlessAskedFor(t *testing.T) {
	s := New()
	s.Learn(Observation{
		Pool: "www", At: time.Now(),
		Workers: []WorkerSample{idleWorker(1, 100, 80, 200_000), idleWorker(2, 100, 80, 200_000)},
	}, Options{})

	ps := s.Pools["www"]
	if ps.CPUSamples != 0 || ps.CPUHistogram != nil || ps.CPUSeen != nil {
		t.Errorf("CPU was recorded without MeasureCPU: samples=%d histogram=%v seen=%v",
			ps.CPUSamples, ps.CPUHistogram, ps.CPUSeen)
	}

	body, err := json.Marshal(ps)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cpu_histogram", "cpu_samples", "cpu_seen"} {
		if strings.Contains(string(body), key) {
			t.Errorf("the state file carries %q although CPU was never measured:\n%s", key, body)
		}
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
	opts := Options{MeasureCPU: true}
	at := time.Now()

	// Scrape one: two idle workers, each with a finished request.
	s.Learn(Observation{Pool: "www", At: at, Workers: []WorkerSample{
		idleWorker(1, 10, 80, 200_000),
		idleWorker(2, 10, 20, 200_000),
	}}, opts)
	ps := s.Pools["www"]
	if ps.CPUSamples != 2 {
		t.Fatalf("CPUSamples = %d after the first scrape, want 2", ps.CPUSamples)
	}

	// Scrape two: nothing happened. Same pids, same request counters.
	s.Learn(Observation{Pool: "www", At: at.Add(30 * time.Second), Workers: []WorkerSample{
		idleWorker(1, 10, 80, 200_000),
		idleWorker(2, 10, 20, 200_000),
	}}, opts)
	if ps.CPUSamples != 2 {
		t.Errorf("CPUSamples = %d after an idle scrape, want 2: the same request was counted again", ps.CPUSamples)
	}

	// Scrape three: worker 1 served one more request; worker 2 did not.
	s.Learn(Observation{Pool: "www", At: at.Add(60 * time.Second), Workers: []WorkerSample{
		idleWorker(1, 11, 90, 200_000),
		idleWorker(2, 10, 20, 200_000),
	}}, opts)
	if ps.CPUSamples != 3 {
		t.Errorf("CPUSamples = %d after one new request, want 3", ps.CPUSamples)
	}

	// A worker that is gone falls out of the dedupe map, and a recycled pid
	// whose counter is BELOW what was remembered is a new worker, not an old
	// request.
	s.Learn(Observation{Pool: "www", At: at.Add(90 * time.Second), Workers: []WorkerSample{
		idleWorker(1, 1, 50, 200_000),
	}}, opts)
	if _, still := ps.CPUSeen[2]; still {
		t.Error("a worker that exited is still in the dedupe map")
	}
	if ps.CPUSamples != 4 {
		// pid 1 came back with a counter of 1 against a remembered 11: a
		// recycled worker wearing an old number, whose one request is as new as
		// any. Only an UNCHANGED counter means "the same request".
		t.Errorf("CPUSamples = %d, want 4: a recycled pid's request is a new request", ps.CPUSamples)
	}
}

// TestShortRequestsAreNotBelieved: php-fpm computes the share from a clock
// that ticks at 100Hz, so a two-millisecond request reads as 0% or 500%
// depending on whether it caught a tick. That is the clock's resolution, not
// the request's shape.
func TestShortRequestsAreNotBelieved(t *testing.T) {
	ps := &PoolState{}
	n := ps.observeCPU([]WorkerSample{
		idleWorker(1, 5, 500, 2_000),  // 2ms: caught a tick
		idleWorker(2, 5, 0, 2_000),    // 2ms: missed it
		idleWorker(3, 5, 60, 49_999),  // just under the floor
		idleWorker(4, 5, 60, 50_000),  // on it
		idleWorker(5, 5, 60, 900_000), // well over
	})
	if n != 2 || ps.CPUSamples != 2 {
		t.Errorf("recorded %d readings (samples=%d), want 2: only requests of 50ms or more", n, ps.CPUSamples)
	}
}

// TestOnlyIdleWorkersWithARequestCount: a running worker reports 0 because its
// request is not finished, and a worker that has served nothing has no last
// request. Neither is a measurement.
func TestOnlyIdleWorkersWithARequestCount(t *testing.T) {
	ps := &PoolState{}
	running := idleWorker(1, 5, 0, 200_000)
	running.Idle = false
	fresh := idleWorker(2, 0, 0, 0)
	noPID := idleWorker(0, 5, 70, 200_000)

	if n := ps.observeCPU([]WorkerSample{running, fresh, noPID}); n != 0 {
		t.Errorf("recorded %d readings from workers that had none to give", n)
	}
	// Every worker WITH a pid is remembered, accepted or not, so a rejected
	// reading is not re-examined next scrape; a worker without one cannot be.
	if ps.CPUSeen[1] != 5 || ps.CPUSeen[2] != 0 || len(ps.CPUSeen) != 2 {
		t.Errorf("CPUSeen = %v, want pids 1 and 2 remembered and no entry for pid 0", ps.CPUSeen)
	}
}

// TestCPUPercentilesDescribeWhatWasSeen: the report's numbers are the floor of
// the bucket the fraction lands in, and a reading past 200% is a misread that
// lands in the last bucket rather than anywhere the arithmetic could believe.
func TestCPUPercentilesDescribeWhatWasSeen(t *testing.T) {
	ps := &PoolState{}
	if got := ps.CPUPercentile(0.5); got != 0 {
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
		{1.00, 1.30},
	} {
		if got := ps.CPUPercentile(tc.p); got < tc.want-0.001 || got > tc.want+0.001 {
			t.Errorf("p%.0f = %.2f, want %.2f", tc.p*100, got, tc.want)
		}
	}

	// A misread.
	ps.observeCPU([]WorkerSample{idleWorker(pid, 1, 9_000, 200_000)})
	if got := ps.CPUPercentile(1); got > 2.0 {
		t.Errorf("a 9000%% reading came out as %.2f; it should be clamped into the last bucket", got)
	}
}

// TestCPUHistogramDecays: an all-time record of a pool redeployed six months
// ago describes an application that no longer exists. Halving keeps the shape
// while letting the past fade, as the memory histogram does.
func TestCPUHistogramDecays(t *testing.T) {
	ps := &PoolState{}
	for i := 0; i < cpuDecayAfter+1; i++ {
		ps.observeCPU([]WorkerSample{idleWorker(i+1, 1, 80, 200_000)})
	}
	if ps.CPUSamples > int64(cpuDecayAfter)/2+1 {
		t.Errorf("CPUSamples = %d after %d readings; the histogram did not halve", ps.CPUSamples, cpuDecayAfter+1)
	}
	if got := ps.CPUPercentile(0.5); got < 0.79 || got > 0.81 {
		t.Errorf("p50 = %.2f after decay, want 0.80: halving must keep the shape", got)
	}
}

// TestCPUSurvivesTheStateFile: what was learned round-trips, dedupe map
// included — a daemon restart must not re-count every worker's last request.
func TestCPUSurvivesTheStateFile(t *testing.T) {
	s := New()
	s.Learn(Observation{Pool: "www", At: time.Now(), Workers: []WorkerSample{
		idleWorker(1, 10, 80, 200_000),
	}}, Options{MeasureCPU: true})

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
	}}, Options{MeasureCPU: true})
	if ps.CPUSamples != 1 {
		t.Errorf("CPUSamples = %d after a restart saw the same request again, want 1", ps.CPUSamples)
	}
}
