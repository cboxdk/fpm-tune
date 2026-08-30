package state

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const mb = 1024 * 1024

// TestIdlePoolTeachesNothing is the guard the whole package turns on.
//
// A worker that has served three requests has not loaded most of the
// application. An idle pool is made entirely of such workers, so learning from
// one produces a per-worker cost that is far too small — and it fails at exactly
// the moment traffic arrives and the number starts to matter.
func TestIdlePoolTeachesNothing(t *testing.T) {
	s := New()

	learned := s.Learn(Observation{
		Pool: "quiet",
		At:   time.Now(),
		Workers: []WorkerSample{
			{RSSBytes: 12 * mb, Requests: 1},
			{RSSBytes: 11 * mb, Requests: 0},
			{RSSBytes: 13 * mb, Requests: 3},
		},
	}, Options{})

	if learned {
		t.Error("an idle pool was learned from")
	}

	ps := s.Pools["quiet"]
	if ps.TypicalPeakBytes != 0 {
		t.Errorf("baseline moved to %d from workers that had barely run", ps.TypicalPeakBytes)
	}
	if ps.Samples != 1 {
		t.Errorf("Samples = %d, want 1: the scrape happened even though it taught nothing", ps.Samples)
	}
	if ps.BusySamples != 0 {
		t.Errorf("BusySamples = %d, want 0", ps.BusySamples)
	}
	if ps.Confidence(Options{}) != 0 {
		t.Error("an idle pool produced non-zero confidence")
	}
}

// TestSubtreeHighWaterCapturesChildren: a worker that spawned an ffmpeg has a
// subtree far larger than its own RSS, and the high-water of that subtree is
// what tells an operator the pool costs more than its workers show. It must
// never come out below the worker high-water — "children" is their difference.
func TestSubtreeHighWaterCapturesChildren(t *testing.T) {
	s := New()

	s.Learn(Observation{
		Pool: "media",
		At:   time.Now(),
		Workers: []WorkerSample{
			// A worker at 90MiB that shelled out to a 600MiB ffmpeg.
			{RSSBytes: 90 * mb, SubtreeRSSBytes: 690 * mb, Requests: 500},
			{RSSBytes: 85 * mb, SubtreeRSSBytes: 85 * mb, Requests: 620}, // no child
		},
	}, Options{})

	ps := s.Pools["media"]
	if ps.HighWaterBytes != 90*mb {
		t.Errorf("worker high-water = %d, want 90MiB", ps.HighWaterBytes)
	}
	if ps.SubtreeHighWaterBytes != 690*mb {
		t.Errorf("subtree high-water = %d, want 690MiB — the ffmpeg's memory is being lost",
			ps.SubtreeHighWaterBytes)
	}
	if ps.SubtreeHighWaterBytes < ps.HighWaterBytes {
		t.Error("subtree high-water fell below worker high-water; children would read negative")
	}
}

// TestSubtreeHighWaterNeverBelowWorker: an older scrape with no subtree readings
// must not drag the subtree high-water below the worker one.
func TestSubtreeHighWaterNeverBelowWorker(t *testing.T) {
	s := New()

	s.Learn(Observation{
		Pool: "app",
		At:   time.Now(),
		Workers: []WorkerSample{
			{RSSBytes: 120 * mb, SubtreeRSSBytes: 0, Requests: 500}, // no subtree measured
		},
	}, Options{})

	ps := s.Pools["app"]
	if ps.SubtreeHighWaterBytes < ps.HighWaterBytes {
		t.Errorf("subtree high-water %d below worker high-water %d with no subtree reading",
			ps.SubtreeHighWaterBytes, ps.HighWaterBytes)
	}
}

// TestMatureWorkersAreLearnedFrom is the other half: once workers have done real
// work, their memory is the number to size against.
func TestMatureWorkersAreLearnedFrom(t *testing.T) {
	s := New()

	if !s.Learn(Observation{
		Pool: "busy",
		At:   time.Now(),
		Workers: []WorkerSample{
			{RSSBytes: 80 * mb, Requests: 500},
			{RSSBytes: 95 * mb, Requests: 620},
			{RSSBytes: 12 * mb, Requests: 2}, // just recycled, ignored
		},
	}, Options{}) {
		t.Fatal("a busy pool taught nothing")
	}

	ps := s.Pools["busy"]
	if ps.TypicalPeakBytes != 95*mb {
		t.Errorf("baseline = %d, want the 95MB peak among mature workers", ps.TypicalPeakBytes/mb)
	}
	if ps.HighWaterBytes != 95*mb {
		t.Errorf("high water = %dMB, want 95MB", ps.HighWaterBytes/mb)
	}
}

// TestSizesOnThePeakNotTheMean: PHP-FPM recycles workers at pm.max_requests, so
// memory climbs and resets — at any instant half the workers are small. Sizing
// on the mean of that sawtooth systematically under-provisions.
func TestSizesOnThePeakNotTheMean(t *testing.T) {
	s := New()

	// A realistic sawtooth: freshly recycled workers next to mature ones.
	obs := Observation{
		Pool: "shop",
		At:   time.Now(),
		Workers: []WorkerSample{
			{RSSBytes: 30 * mb, Requests: 40},
			{RSSBytes: 45 * mb, Requests: 200},
			{RSSBytes: 60 * mb, Requests: 400},
			{RSSBytes: 120 * mb, Requests: 900},
		},
	}
	s.Learn(obs, Options{})

	ps := s.Pools["shop"]
	mean := int64((30 + 45 + 60 + 120) / 4 * mb)

	if ps.TypicalPeakBytes <= mean {
		t.Errorf("baseline %dMB is at or below the mean %dMB; the tail is what OOMs",
			ps.TypicalPeakBytes/mb, mean/mb)
	}
	if ps.TypicalPeakBytes != 120*mb {
		t.Errorf("baseline = %dMB, want the 120MB peak", ps.TypicalPeakBytes/mb)
	}
}

// TestEstimateRisesFastAndFallsSlow is the asymmetry, and it is a safety
// property rather than a tuning preference.
//
// A worker costing more than expected puts the whole budget at risk, so it is
// believed within a scrape or two. A worker costing less is only an opportunity,
// so it is taken up gradually — the estimate follows the day rather than being
// pinned to its worst hour, but it never drops to one quiet reading in a single
// step and then meets the morning under-provisioned.
func TestEstimateRisesFastAndFallsSlow(t *testing.T) {
	s := New()
	opts := Options{}
	now := time.Now()

	// The pool is WORKING, and says so with the counter php-fpm always reports.
	// A smaller reading from an idle pool is deliberately not believed — that is
	// a lull, not a cheaper application — so a test about the estimate following
	// the workload has to supply the traffic.
	steady := func(rss int64, at time.Time) Observation {
		return Observation{Pool: "app", At: at, ActiveNow: 8,
			Accepted: acceptedAt(at),
			Workers: []WorkerSample{
				{RSSBytes: rss, Requests: 300},
				{RSSBytes: rss, Requests: 300},
			}}
	}

	for i := 0; i < 40; i++ {
		s.Learn(steady(50*mb, now.Add(time.Duration(i)*time.Minute)), opts)
	}
	settled := s.Pools["app"].TypicalPeakBytes
	if settled < 49*mb || settled > 51*mb {
		t.Fatalf("baseline settled at %dMB after 40 steady samples of 50MB", settled/mb)
	}

	// Workers get bigger: a deploy, or the day starting. This must be believed
	// quickly, because the budget is already committed on the old number.
	afterOne := 0
	for i := 0; i < 3; i++ {
		s.Learn(steady(150*mb, now.Add(time.Duration(41+i)*time.Minute)), opts)
		if i == 0 {
			afterOne = int(s.Pools["app"].TypicalPeakBytes / mb)
		}
	}
	risen := s.Pools["app"].TypicalPeakBytes

	if afterOne <= 60 {
		t.Errorf("after one 150MB observation the estimate was still %dMB; "+
			"the budget is committed on the old number while workers are already bigger", afterOne)
	}
	if risen < 120*mb {
		t.Errorf("three 150MB observations only reached %dMB", risen/mb)
	}
	// Fast, but still smoothed — a single reading does not become the estimate.
	if afterOne >= 150 {
		t.Errorf("one observation became the estimate outright (%dMB)", afterOne)
	}

	// Workers get smaller again. This must be taken up, or the pool is pinned to
	// its worst hour and the quiet part of the day is wasted — but gradually.
	afterFirstQuiet := int64(0)
	for i := 0; i < 3; i++ {
		s.Learn(steady(50*mb, now.Add(time.Duration(50+i)*time.Minute)), opts)
		if i == 0 {
			afterFirstQuiet = s.Pools["app"].TypicalPeakBytes
		}
	}

	// Smoothed, not instant: one reading is one reading, whatever it says. What
	// stops a LULL pulling the estimate down is no longer the length of the
	// half-life but the concurrency guard — a smaller reading from an idle pool
	// is not believed at all — so this only has to check that a single busy
	// observation does not become the answer outright.
	if afterFirstQuiet <= 50*mb {
		t.Errorf("one smaller reading became the estimate outright (%dMB)", afterFirstQuiet/mb)
	}

	// Over a full day of 50MB workers it comes all the way back down, so the
	// pool is not pinned to its worst hour and the quiet part of the day is
	// usable.
	for i := 0; i < 24*60; i++ {
		s.Learn(steady(50*mb, now.Add(time.Duration(60+i)*time.Minute)), opts)
	}
	if released := s.Pools["app"].TypicalPeakBytes; released > 60*mb {
		t.Errorf("the estimate stayed at %dMB after a day of 50MB workers; "+
			"the pool is pinned to its worst hour", released/mb)
	}

	// The spike is remembered, it is just not what the pool is sized on.
	if s.Pools["app"].HighWaterBytes != 150*mb {
		t.Errorf("high water = %dMB, want 150MB", s.Pools["app"].HighWaterBytes/mb)
	}
}

// TestDecayRateIsIndependentOfTheScrapeInterval is the property that makes the
// asymmetry mean anything.
//
// The downward weight used to be per SAMPLE, which the scrape rate then silently
// multiplied: 0.05 per sample at a thirty-second interval is a twelve-minute
// half-life. A quiet night collapsed the cost estimate in twenty minutes while
// the concurrency peak was deliberately held for a day, so the pool was sized
// for many cheap workers and the morning made them expensive again — measured at
// 147 workers of 100MiB configured on an 8GiB host, which is the OOM this whole
// tool exists to prevent, manufactured by its own learner.
//
// Expressed as a half-life in time, the sampling rate cannot change it.
func TestDecayRateIsIndependentOfTheScrapeInterval(t *testing.T) {
	opts := Options{}.Defaults()

	fellBelow := func(interval time.Duration) time.Duration {
		s := New()
		base := time.Now()
		s.Learn(steadyAt("p", 100*mb, base), opts)

		for i := 1; i <= 100000; i++ {
			at := base.Add(time.Duration(i) * interval)
			s.Learn(steadyAt("p", 30*mb, at), opts)
			if s.Pools["p"].TypicalPeakBytes <= 65*mb {
				return time.Duration(i) * interval
			}
		}

		return 0
	}

	fast := fellBelow(5 * time.Second)
	slow := fellBelow(5 * time.Minute)

	t.Logf("halfway after %s when sampled every 5s, %s when sampled every 5m", fast, slow)

	if fast == 0 || slow == 0 {
		t.Fatal("the estimate never fell halfway")
	}

	// Within one coarse sample of each other.
	diff := fast - slow
	if diff < 0 {
		diff = -diff
	}
	if diff > 10*time.Minute {
		t.Errorf("sampling every 5s took %s but every 5m took %s; the scrape rate "+
			"is changing how fast the estimate falls", fast, slow)
	}
	// The length is deliberately no longer asked to outlast a quiet night. That
	// job moved to the concurrency guard, which refuses a smaller reading from an
	// idle pool however long the half-life is — and leaving the length to do it
	// cost the tool most of a working day at twice the memory a pool needed,
	// measured on a VM. What matters here is that the rate is a property of TIME
	// and not of how often anyone happens to look.
	if fast < 5*time.Minute {
		t.Errorf("halfway in %s is fast enough for one odd reading to matter", fast)
	}
}

// steadyAt is a pool under steady load. The traffic matters: a smaller reading
// from an idle pool is not evidence that the application got cheaper, so a pool
// that is meant to be busy has to carry a rising request counter.
func steadyAt(pool string, rss int64, at time.Time) Observation {
	return Observation{Pool: pool, At: at, ActiveNow: 8,
		Accepted: acceptedAt(at),
		Workers: []WorkerSample{
			{RSSBytes: rss, Requests: 300},
			{RSSBytes: rss, Requests: 300},
		}}
}

// TestConfidenceNeedsBothSamplesAndTime: samples alone would let a tight polling
// loop claim confidence in thirty seconds, which measures the scrape interval
// rather than the workload — no daily traffic pattern has been seen yet.
func TestConfidenceNeedsBothSamplesAndTime(t *testing.T) {
	opts := Options{}.Defaults()
	base := time.Now()

	t.Run("many samples in no time", func(t *testing.T) {
		s := New()
		for i := 0; i < 200; i++ {
			s.Learn(busyObs("fast", base.Add(time.Duration(i)*time.Millisecond)), opts)
		}
		if c := s.Pools["fast"].Confidence(opts); c >= 1 {
			t.Errorf("confidence %.2f after 200 samples in 200ms", c)
		}
	})

	t.Run("long span, few samples", func(t *testing.T) {
		s := New()
		for i := 0; i < 3; i++ {
			s.Learn(busyObs("slow", base.Add(time.Duration(i)*time.Hour)), opts)
		}
		if c := s.Pools["slow"].Confidence(opts); c >= 1 {
			t.Errorf("confidence %.2f after 3 samples over 2 hours", c)
		}
	})

	t.Run("both satisfied", func(t *testing.T) {
		s := New()
		for i := 0; i < 25; i++ {
			s.Learn(busyObs("real", base.Add(time.Duration(i)*2*time.Minute)), opts)
		}
		ps := s.Pools["real"]
		if !ps.Trusted(opts) {
			t.Errorf("confidence %.2f after 25 samples over 50 minutes; want trusted",
				ps.Confidence(opts))
		}
	})
}

// TestConfidenceIgnoresIdleSamples: an hour of watching a quiet pool is not an
// hour of evidence.
func TestConfidenceIgnoresIdleSamples(t *testing.T) {
	s := New()
	opts := Options{}.Defaults()
	base := time.Now()

	for i := 0; i < 100; i++ {
		s.Learn(Observation{
			Pool: "quiet", At: base.Add(time.Duration(i) * time.Minute),
			Workers: []WorkerSample{{RSSBytes: 20 * mb, Requests: 1}},
		}, opts)
	}

	if c := s.Pools["quiet"].Confidence(opts); c != 0 {
		t.Errorf("confidence %.2f from 100 idle samples over 100 minutes", c)
	}
}

// TestSurvivesRestart is the reason the file exists: an hour of observation must
// not be thrown away by a restart, and the pool must come back trusted.
func TestSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	opts := Options{}.Defaults()
	// Dated in the PAST, as real observations are. Load discards timestamps that
	// have not happened yet, because a state file claiming the future gives a
	// pool permanent confidence no live observation can shorten — and a fixture
	// that learns fifty minutes ahead of the clock is not a restart, it is that
	// bug wearing a test's name.
	base := time.Now().Add(-time.Hour)

	before := New()
	for i := 0; i < 25; i++ {
		before.Learn(busyObs("app", base.Add(time.Duration(i)*2*time.Minute)), opts)
	}
	if !before.Pools["app"].Trusted(opts) {
		t.Fatal("test setup: the pool was not trusted before saving")
	}
	if err := before.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	after, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got, want := after.Pools["app"], before.Pools["app"]
	if got == nil {
		t.Fatal("the pool did not survive the round trip")
	}
	if got.TypicalPeakBytes != want.TypicalPeakBytes {
		t.Errorf("baseline = %d, want %d", got.TypicalPeakBytes, want.TypicalPeakBytes)
	}
	if got.BusySamples != want.BusySamples {
		t.Errorf("BusySamples = %d, want %d", got.BusySamples, want.BusySamples)
	}
	if !got.Trusted(opts) {
		t.Error("a trusted pool came back untrusted; the restart discarded its history")
	}
}

// TestLoadMissingFileIsAFirstRun, not an error.
func TestLoadMissingFileIsAFirstRun(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("a missing state file was an error: %v", err)
	}
	if s == nil || s.Pools == nil {
		t.Fatal("Load returned an unusable store")
	}
	if len(s.Pools) != 0 {
		t.Errorf("a first run started with %d pools", len(s.Pools))
	}
}

// TestCorruptStateIsReportedNotReset: quietly starting over would discard
// everything learned and re-tune the host from bootstrap estimates — which looks
// exactly like the tool working normally. The caller decides.
func TestCorruptStateIsReportedNotReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Error("a corrupt state file was silently accepted")
	}
}

// TestFutureFormatIsRefused: a file written by a newer build may mean something
// different by the same field names.
func TestFutureFormatIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version": 99, "pools": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Error("a state file from a newer format version was accepted")
	}
}

// TestSaveIsAtomic: a crash mid-write must leave the previous state, not a
// truncated file. This runs on a schedule for as long as the host is up, which
// is enough occasions for the unlucky one to happen.
//
// Checked by the inode, because that is the only observable difference between
// the two implementations. A rename installs a NEW inode over the name; an
// in-place rewrite keeps it and is the thing that can be caught half-written.
// Asserting only that no temp files were left behind passed against a plain
// os.WriteFile, which is exactly what must not be there.
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	first := New()
	first.Learn(busyObs("app", time.Now()), Options{})
	if err := first.Save(path); err != nil {
		t.Fatal(err)
	}

	before, err := inodeOf(t, path)
	if err != nil {
		t.Fatal(err)
	}

	// A second save must not leave temp files behind.
	if err := first.Save(path); err != nil {
		t.Fatal(err)
	}

	after, err := inodeOf(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Error("the second save kept the same inode: the file was rewritten in place " +
			"rather than renamed over, so a crash part-way through leaves a truncated " +
			"state file where the previous one used to be")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %04o, want 0644: the temp file's mode carries across the rename",
			perm)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("Save left %s behind", e.Name())
		}
	}
}

// TestSaveCreatesItsDirectory: /var/lib/fpm-tune does not exist on a fresh host.
func TestSaveCreatesItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "state.json")

	if err := New().Save(path); err != nil {
		t.Fatalf("Save into a missing directory: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("state was not written: %v", err)
	}
}

// TestForgetDropsRemovedPools: a host that has had sites come and go for years
// should not carry every one of them forever.
//
// After several consecutive rounds, not the first. Discovery skips a master
// whose configuration it cannot parse rather than failing the round, so one
// transient `php-fpm -tt` error on a host running two PHP versions used to make
// every pool of one of them disappear — and a week of learning with it.
func TestForgetDropsRemovedPools(t *testing.T) {
	s := New()
	for _, name := range []string{"kept-a", "kept-b", "removed"} {
		s.Learn(busyObs(name, time.Now()), Options{})
	}

	var dropped []string
	for i := 0; i < forgetAfterMissedRounds; i++ {
		if _, still := s.Pools["removed"]; !still {
			t.Fatalf("the pool was forgotten after %d rounds of absence; a discovery "+
				"failure lasting one round costs a week of learning", i)
		}
		dropped = s.Forget([]string{"kept-a", "kept-b"}, "")
	}

	if len(dropped) != 1 || dropped[0] != "removed" {
		t.Errorf("dropped = %v, want [removed]", dropped)
	}
	if _, still := s.Pools["removed"]; still {
		t.Error("the removed pool is still in the store")
	}
	if len(s.Pools) != 2 {
		t.Errorf("%d pools remain, want 2", len(s.Pools))
	}
}

// TestATransientDiscoveryFailureDoesNotForget: a pool that comes back has its
// absence counter cleared, so an intermittent failure never accumulates its way
// to a deletion.
func TestATransientDiscoveryFailureDoesNotForget(t *testing.T) {
	s := New()
	for _, name := range []string{"a", "b"} {
		s.Learn(busyObs(name, time.Now()), Options{})
	}

	for i := 0; i < 20; i++ {
		// b vanishes every other round, the way a master that intermittently
		// fails to parse does.
		if i%2 == 0 {
			s.Forget([]string{"a"}, "")

			continue
		}
		s.Forget([]string{"a", "b"}, "")
	}

	if _, still := s.Pools["b"]; !still {
		t.Error("a pool that was present in half the rounds was forgotten; an " +
			"intermittent discovery failure should not add up to a deletion")
	}
}

// TestRecordApplied stores what hysteresis needs later.
func TestRecordApplied(t *testing.T) {
	s := New()
	at := time.Now()

	s.RecordApplied("", "new-pool", 24, at)

	ps := s.Pools["new-pool"]
	if ps == nil {
		t.Fatal("RecordApplied did not create the pool")
	}
	if ps.LastAppliedMaxChildren != 24 {
		t.Errorf("LastAppliedMaxChildren = %d, want 24", ps.LastAppliedMaxChildren)
	}
	if !ps.LastAppliedAt.Equal(at) {
		t.Errorf("LastAppliedAt = %v, want %v", ps.LastAppliedAt, at)
	}
}

// TestNamesAreSorted keeps output stable between runs.
func TestNamesAreSorted(t *testing.T) {
	s := New()
	for _, name := range []string{"zulu", "alpha", "mike"} {
		s.Learn(busyObs(name, time.Now()), Options{})
	}

	got := s.Names()
	want := []string{"alpha", "mike", "zulu"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
}

// busyObs is a pool under load. The request counter is not decoration: what
// makes a reading evidence is that the pool was WORKING when it was taken, and
// a helper called busy that serves nothing is the same confusion the code had.
func busyObs(pool string, at time.Time) Observation {
	return Observation{
		Pool: pool, At: at, ActiveNow: 4,
		Accepted: acceptedAt(at),
		Workers: []WorkerSample{
			{RSSBytes: 64 * mb, Requests: 400},
			{RSSBytes: 70 * mb, Requests: 350},
		},
	}
}

// TestPeakSurvivesAReload is the ratchet this exists to stop.
//
// PHP-FPM resets pm.max_active_processes on reload, and this tool reloads. Sizing
// straight off that counter is a downward spiral: a cut triggers a reload, the
// reload clears the evidence, the next observation looks quieter still, and the
// pool is cut again. Observed on a live host as 20 -> 6 -> 2 over three rounds
// while the pool was under load.
func TestPeakSurvivesAReload(t *testing.T) {
	ps := &PoolState{Pool: "shop"}
	opts := Options{}.Defaults()
	now := time.Now()

	if got := ps.ObservePeak(12, now, opts); got != 12 {
		t.Fatalf("first observation gave %d, want 12", got)
	}

	// A reload happens; PHP-FPM now reports almost nothing.
	if got := ps.ObservePeak(1, now.Add(time.Minute), opts); got != 12 {
		t.Errorf("after a reload the peak collapsed to %d; the pool would be cut "+
			"on evidence the reload destroyed", got)
	}
	// And again, which is where the spiral used to accelerate.
	if got := ps.ObservePeak(1, now.Add(2*time.Minute), opts); got != 12 {
		t.Errorf("the peak eroded to %d on a second quiet scrape", got)
	}
}

// TestPeakRisesImmediately: a pool that suddenly needs more must not wait out a
// decay window to be believed.
func TestPeakRisesImmediately(t *testing.T) {
	ps := &PoolState{Pool: "shop"}
	opts := Options{}.Defaults()
	now := time.Now()

	ps.ObservePeak(4, now, opts)
	if got := ps.ObservePeak(30, now.Add(time.Second), opts); got != 30 {
		t.Errorf("a jump to 30 concurrent workers was recorded as %d", got)
	}
}

// TestStalePeakDecays: without any decay a single spike would pin a pool's size
// forever, and a site that has genuinely quietened down could never give its
// headroom back to a neighbour.
func TestStalePeakDecays(t *testing.T) {
	ps := &PoolState{Pool: "shop"}
	opts := Options{PeakWindow: time.Hour}.Defaults()
	base := time.Now()

	ps.ObservePeak(40, base, opts)

	// Inside the window, nothing moves.
	if got := ps.ObservePeak(2, base.Add(30*time.Minute), opts); got != 40 {
		t.Errorf("the peak decayed inside its window: %d", got)
	}

	// Past it, it comes down — but gradually, not in one step.
	first := ps.ObservePeak(2, base.Add(2*time.Hour), opts)
	if first >= 40 {
		t.Errorf("a stale peak did not decay: %d", first)
	}
	if first <= 2 {
		t.Errorf("a stale peak collapsed straight to the current value (%d); "+
			"one quiet scrape should not undo a day of evidence", first)
	}

	// Repeated quiet observations eventually reach the current level.
	got := first
	for i := 0; i < 20; i++ {
		got = ps.ObservePeak(2, base.Add(time.Duration(3+i)*time.Hour), opts)
	}
	if got != 2 {
		t.Errorf("the peak settled at %d after twenty quiet hours, want 2", got)
	}
}

// TestPeakIsPersisted, or a restart puts the pool straight back into the ratchet.
func TestPeakIsPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	before := New()
	before.Learn(busyObs("shop", time.Now()), Options{})
	before.Pools["shop"].ObservePeak(18, time.Now(), Options{})

	if err := before.Save(path); err != nil {
		t.Fatal(err)
	}

	after, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Pools["shop"].PeakWorkers; got != 18 {
		t.Errorf("PeakWorkers = %d after a restart, want 18", got)
	}
}

// TestSizingNeverTrailsTheLatestReading.
//
// The tracked estimate moves halfway towards a new observation per scrape. Fast
// is not immediate, and for a memory ceiling "not immediate" is the wrong
// direction to be wrong in: a deploy that triples what a worker costs leaves the
// estimate at half the truth for a scrape or two, and a pool grown against that
// figure is grown against workers that no longer exist.
//
// Being slow to believe memory got cheaper costs some unused headroom. Being
// slow to believe it got more expensive costs the host.
func TestSizingNeverTrailsTheLatestReading(t *testing.T) {
	st := New()
	opts := Options{}.Defaults()
	now := time.Now()

	// Settled on cheap workers.
	for i := range 10 {
		st.Learn(Observation{Pool: "shop", At: now.Add(time.Duration(i) * time.Minute), Workers: []WorkerSample{
			{RSSBytes: 40 << 20, Requests: 500},
			{RSSBytes: 40 << 20, Requests: 500},
		}}, opts)
	}

	settled := st.Pools["shop"].SizingBytes()
	if settled < 38<<20 || settled > 42<<20 {
		t.Fatalf("settled at %d bytes, want about 40MiB", settled)
	}

	// A deploy: the very next scrape shows workers three times the size.
	st.Learn(Observation{Pool: "shop", At: now.Add(11 * time.Minute), Workers: []WorkerSample{
		{RSSBytes: 120 << 20, Requests: 500},
		{RSSBytes: 120 << 20, Requests: 500},
	}}, opts)

	if got := st.Pools["shop"].SizingBytes(); got < 120<<20 {
		t.Errorf("SizingBytes = %d after workers tripled to 120MiB; sizing on %d "+
			"would allocate against workers that no longer exist", got, got)
	}
}

// TestConfidenceIsMeasuredOverEvidence: a pool idle for days had its confidence
// clock run out long before any evidence arrived, so twenty busy samples over
// ten minutes made it fully trusted. The span exists to insist a baseline has
// been watched through a real traffic pattern, not a lunchtime.
func TestConfidenceIsMeasuredOverEvidence(t *testing.T) {
	st := New()
	opts := Options{ConfidenceSamples: 20, ConfidenceSpan: 2 * time.Hour}.Defaults()

	start := time.Now().Add(-72 * time.Hour)

	// Three days of nothing: seen every interval, teaching nothing.
	for i := range 40 {
		st.Learn(Observation{Pool: "quiet", At: start.Add(time.Duration(i) * time.Hour)}, opts)
	}

	// Then ten minutes of real traffic.
	busy := start.Add(70 * time.Hour)
	for i := range 25 {
		st.Learn(Observation{Pool: "quiet", At: busy.Add(time.Duration(i) * 24 * time.Second),
			Workers: []WorkerSample{
				{RSSBytes: 60 << 20, Requests: 400},
				{RSSBytes: 60 << 20, Requests: 400},
			}}, opts)
	}

	if c := st.Pools["quiet"].Confidence(opts); c >= 1 {
		t.Errorf("confidence = %.2f after ten minutes of evidence against a two-hour "+
			"span; the pool's age was standing in for having been watched", c)
	}
}

// TestASustainedReductionIsBelievedWithinTheHour.
//
// The downward half-life was six hours, to stop a quiet period pulling the
// estimate down. That reasoning is answered twice over already:
// MinRequestsPerWorker excludes workers that have not loaded the application,
// and PHP does not return memory to the OS — so a quiet pool produces readings
// at or near its previous peak, or none at all, not small ones.
//
// So a reading below the estimate means the workload changed. Measured on a VM,
// six hours of disbelieving it left the tool reserving 214MiB per worker while
// its workers had measured 93MiB for forty minutes and 595 samples. On a fully
// committed host that is every other pool going short.
func TestASustainedReductionIsBelievedWithinTheHour(t *testing.T) {
	st := New()
	opts := Options{}.Defaults()
	now := time.Now()

	// Settled expensive.
	for i := range 20 {
		st.Learn(Observation{Pool: "shop", At: now.Add(time.Duration(i) * time.Minute), ActiveNow: 8,
			Accepted: acceptedAt(now.Add(time.Duration(i) * time.Minute)),
			Workers: []WorkerSample{
				{RSSBytes: 240 << 20, Requests: 500},
				{RSSBytes: 240 << 20, Requests: 500},
			}}, opts)
	}
	if got := st.Pools["shop"].SizingBytes(); got < 230<<20 {
		t.Fatalf("did not settle expensive: %d", got)
	}

	// A deploy makes it cheap, and it STAYS cheap for an hour.
	cheap := now.Add(30 * time.Minute)
	for i := range 60 {
		st.Learn(Observation{Pool: "shop", At: cheap.Add(time.Duration(i) * time.Minute), ActiveNow: 8,
			Accepted: acceptedAt(cheap.Add(time.Duration(i) * time.Minute)),
			Workers: []WorkerSample{
				{RSSBytes: 90 << 20, Requests: 500},
				{RSSBytes: 90 << 20, Requests: 500},
			}}, opts)
	}

	// An hour is two half-lives, so a quarter of the original distance is left:
	// 90 + (240-90)/4 = 127.5MiB. That is the model working, not the model
	// lagging.
	//
	// This used to assert 120MiB, which the model alone does not reach — it was
	// met by the ten-minute jump between the two loops above being taken as ten
	// minutes of decay in one step. It is not: nothing was watched across it, and
	// a hole no longer buys a larger step than an ordinary scrape. The threshold
	// was calibrated against that.
	got := st.Pools["shop"].SizingBytes()
	if got > 130<<20 {
		t.Errorf("after an hour of measuring 90MiB the estimate is still %dMiB; the pool "+
			"is reserving %.1fx what it needs, and on a committed host that is taken "+
			"from its neighbours", got>>20, float64(got)/float64(90<<20))
	}
	if got < 120<<20 {
		t.Errorf("the estimate fell to %dMiB in an hour, past the 127MiB two half-lives "+
			"allow: something is decaying faster than the documented rate", got>>20)
	}
}

// TestAQuietSpellDoesNotPullTheEstimateDown is the other half: the shorter
// half-life must not reintroduce what the long one was for. A pool that goes
// quiet teaches nothing — its workers keep their memory and its readings stop —
// so the estimate should hold where it was.
func TestAQuietSpellDoesNotPullTheEstimateDown(t *testing.T) {
	st := New()
	opts := Options{}.Defaults()
	now := time.Now()

	for i := range 20 {
		st.Learn(Observation{Pool: "shop", At: now.Add(time.Duration(i) * time.Minute), ActiveNow: 8,
			Accepted: acceptedAt(now.Add(time.Duration(i) * time.Minute)),
			Workers: []WorkerSample{
				{RSSBytes: 240 << 20, Requests: 500},
				{RSSBytes: 240 << 20, Requests: 500},
			}}, opts)
	}
	settled := st.Pools["shop"].SizingBytes()

	// Eight quiet hours: workers exist but have served almost nothing, so they
	// are excluded — and a fresh worker's small RSS must not be mistaken for the
	// pool having become cheap.
	quiet := now.Add(30 * time.Minute)
	for i := range 96 {
		st.Learn(Observation{Pool: "shop", At: quiet.Add(time.Duration(i) * 5 * time.Minute), ActiveNow: 8,
			Accepted: acceptedAt(quiet.Add(time.Duration(i) * 5 * time.Minute)),
			Workers: []WorkerSample{
				{RSSBytes: 12 << 20, Requests: 2},
				{RSSBytes: 11 << 20, Requests: 1},
			}}, opts)
	}

	if got := st.Pools["shop"].SizingBytes(); got < settled/2 {
		t.Errorf("a quiet night pulled the estimate from %dMiB to %dMiB; the morning "+
			"would find the pool sized for workers that do not exist",
			settled>>20, got>>20)
	}
}

// TestConfidenceDoesNotAdvanceOnWaiting.
//
// The span exists to insist a baseline has been watched through a real traffic
// pattern rather than a lunchtime. Measuring it to LastUpdated let idle scrapes
// carry a pool over the line instead: twenty-five busy samples in ten minutes,
// then two hours of nothing at all, and the baseline counted as watched — on the
// strength of the waiting.
func TestConfidenceDoesNotAdvanceOnWaiting(t *testing.T) {
	st := New()
	opts := Options{ConfidenceSamples: 20, ConfidenceSpan: 2 * time.Hour}.Defaults()
	now := time.Now()

	// Ten minutes of real evidence.
	for i := range 25 {
		st.Learn(Observation{Pool: "shop", At: now.Add(time.Duration(i) * 24 * time.Second),
			ActiveNow: 8, Workers: []WorkerSample{
				{RSSBytes: 60 << 20, Requests: 400},
				{RSSBytes: 60 << 20, Requests: 400},
			}}, opts)
	}

	// Then two hours of being looked at and teaching nothing.
	idle := now.Add(15 * time.Minute)
	for i := range 240 {
		st.Learn(Observation{Pool: "shop", At: idle.Add(time.Duration(i) * 30 * time.Second)}, opts)
	}

	if c := st.Pools["shop"].Confidence(opts); c >= 1 {
		t.Errorf("confidence = %.2f: two hours of idle scrapes carried a ten-minute "+
			"baseline over a two-hour span", c)
	}
}

// TestStatePredatingTheBusyFieldsIsNotTrustedOnSight: an upgrade must not
// inherit the overconfidence the new fields exist to remove. A pool whose
// recorded span was its own age starts again rather than arriving trusted.
func TestStatePredatingTheBusyFieldsIsNotTrustedOnSight(t *testing.T) {
	opts := Options{ConfidenceSamples: 20, ConfidenceSpan: 30 * time.Minute}.Defaults()

	old := &PoolState{
		Pool:             "legacy",
		BusySamples:      500,
		FirstSeen:        time.Now().Add(-72 * time.Hour),
		LastUpdated:      time.Now(),
		TypicalPeakBytes: 90 << 20,
		// FirstBusyAt and LastBusyAt absent, as an older build left them.
	}

	if c := old.Confidence(opts); c != 0 {
		t.Errorf("confidence = %.2f for state with no record of when the evidence "+
			"arrived; the upgrade inherits exactly the overconfidence the fields "+
			"were added to remove", c)
	}
}

// TestTheSizingFloorIsTheLatestMeasurementAndDoesNotExpire.
//
// This replaces a test asserting the opposite, and the reversal is the point.
//
// A previous round made the floor expire after fifteen minutes, so that one
// anomalous scrape could not hold a quiet pool's sizing high forever. The worry
// was real; the remedy was the wrong way round. Expiring falls back to the
// smoothed estimate, which after a single new reading has moved only halfway —
// so a genuine deploy from 40MiB to 120MiB would be sized at 80MiB a quarter of
// an hour later, having seen nothing whatever to contradict the 120.
//
// Under-sizing ends in an OOM kill. Holding high costs unused headroom on one
// pool until the next mature scrape replaces the reading, which is the direction
// to be wrong in.
func TestTheSizingFloorIsTheLatestMeasurementAndDoesNotExpire(t *testing.T) {
	st := New()
	opts := Options{}.Defaults()
	now := time.Now()

	accepted := int64(0)
	for i := range 10 {
		accepted += 100
		st.Learn(Observation{Pool: "shop", At: now.Add(time.Duration(i) * time.Minute),
			ActiveNow: 6, Accepted: accepted,
			Workers: []WorkerSample{{RSSBytes: 40 << 20, Requests: 500}, {RSSBytes: 40 << 20, Requests: 500}},
		}, opts)
	}

	// A deploy: one scrape sees 120MiB. The smoothed estimate only reaches ~80.
	accepted += 100
	st.Learn(Observation{Pool: "shop", At: now.Add(11 * time.Minute),
		ActiveNow: 6, Accepted: accepted,
		Workers: []WorkerSample{{RSSBytes: 120 << 20, Requests: 500}, {RSSBytes: 120 << 20, Requests: 500}},
	}, opts)

	// An hour passes with the pool seen but teaching nothing.
	for i := range 60 {
		st.Learn(Observation{Pool: "shop", At: now.Add(time.Duration(12+i) * time.Minute)}, opts)
	}

	if got := st.Pools["shop"].SizingBytes(); got < 120<<20 {
		t.Errorf("sizing fell to %dMiB an hour after measuring 120MiB, with nothing "+
			"seen to contradict it; the pool is sized for workers smaller than the "+
			"ones it has", got>>20)
	}

	// And a genuinely smaller mature reading replaces it, which is what stops a
	// high reading holding forever.
	accepted += 100
	st.Learn(Observation{Pool: "shop", At: now.Add(80 * time.Minute),
		ActiveNow: 6, Accepted: accepted,
		Workers: []WorkerSample{{RSSBytes: 50 << 20, Requests: 500}, {RSSBytes: 50 << 20, Requests: 500}},
	}, opts)

	if got := st.Pools["shop"].SizingBytes(); got >= 120<<20 {
		t.Errorf("a newer, smaller measurement did not replace the floor (%dMiB)", got>>20)
	}
}

// TestAFastPoolIsNotPinnedHighForever.
//
// The mirror of the fault the decay gate exists to prevent, reached from the
// other side. Gating on how many workers are busy at the instant of a scrape
// measures request DURATION as much as load: a pool answering in two
// milliseconds has nobody mid-request at almost any moment, however much traffic
// it carries. Its estimate would then never be allowed to fall — pinned to
// whatever peak it once reached, for ever, reserving memory nothing needs.
func TestAFastPoolIsNotPinnedHighForever(t *testing.T) {
	st := New()
	opts := Options{}.Defaults()
	now := time.Now()

	accepted := int64(0)
	// Expensive to begin with, and busy — 400 requests between scrapes.
	for i := range 15 {
		accepted += 400
		st.Learn(Observation{Pool: "api", At: now.Add(time.Duration(i) * 30 * time.Second),
			ActiveNow: 0, // nothing is ever caught mid-request: the work is fast
			Accepted:  accepted,
			Workers: []WorkerSample{
				{RSSBytes: 200 << 20, Requests: 5000},
				{RSSBytes: 200 << 20, Requests: 5000},
			}}, opts)
	}

	// A deploy halves the cost. Still fast, still busy, still never caught.
	cheap := now.Add(10 * time.Minute)
	for i := range 90 {
		accepted += 400
		st.Learn(Observation{Pool: "api", At: cheap.Add(time.Duration(i) * 30 * time.Second),
			ActiveNow: 0,
			Accepted:  accepted,
			Workers: []WorkerSample{
				{RSSBytes: 100 << 20, Requests: 5000},
				{RSSBytes: 100 << 20, Requests: 5000},
			}}, opts)
	}

	// 45 minutes is a minute and a half of half-lives, so the model predicts
	// about 135MiB on the way from 200 to 100. What is being checked is that it
	// is MOVING — a pool gated on instantaneous concurrency would still read 200,
	// pinned to its worst hour for as long as its requests stay short.
	if got := st.Pools["api"].SizingBytes(); got > 145<<20 {
		t.Errorf("after 45 minutes of measuring 100MiB the estimate is %dMiB; a pool "+
			"whose requests are too short to be caught mid-flight is pinned to its "+
			"worst hour", got>>20)
	}
}

// TestAQuietPoolStillHoldsItsEstimate: the counter must not let a lull through
// either. A pool serving almost nothing teaches nothing downward, however small
// its idle survivors read.
func TestAQuietPoolStillHoldsItsEstimate(t *testing.T) {
	st := New()
	opts := Options{}.Defaults()
	now := time.Now()

	accepted := int64(0)
	for i := range 15 {
		accepted += 400
		st.Learn(Observation{Pool: "shop", At: now.Add(time.Duration(i) * 30 * time.Second),
			ActiveNow: 6, Accepted: accepted,
			Workers: []WorkerSample{
				{RSSBytes: 200 << 20, Requests: 5000},
				{RSSBytes: 200 << 20, Requests: 5000},
			}}, opts)
	}
	settled := st.Pools["shop"].SizingBytes()

	// A quiet night: one request every few minutes, and the survivors have given
	// their memory back.
	quiet := now.Add(10 * time.Minute)
	for i := range 96 {
		accepted++
		st.Learn(Observation{Pool: "shop", At: quiet.Add(time.Duration(i) * 5 * time.Minute),
			ActiveNow: 0, Accepted: accepted,
			Workers: []WorkerSample{
				{RSSBytes: 15 << 20, Requests: 5000},
				{RSSBytes: 14 << 20, Requests: 5000},
			}}, opts)
	}

	if got := st.Pools["shop"].SizingBytes(); got < settled/2 {
		t.Errorf("a quiet night pulled the estimate from %dMiB to %dMiB; the morning "+
			"would find the pool sized for workers that do not exist",
			settled>>20, got>>20)
	}
}

// TestAReloadResettingTheCounterIsNotReadAsWork: php-fpm resets its counters on
// reload, and this tool causes reloads. A counter that has gone backwards is
// unknown, not evidence either way.
func TestAReloadResettingTheCounterIsNotReadAsWork(t *testing.T) {
	st := New()
	opts := Options{}.Defaults()
	now := time.Now()

	accepted := int64(0)
	for i := range 15 {
		accepted += 400
		st.Learn(Observation{Pool: "shop", At: now.Add(time.Duration(i) * 30 * time.Second),
			ActiveNow: 6, Accepted: accepted,
			Workers: []WorkerSample{
				{RSSBytes: 200 << 20, Requests: 5000},
				{RSSBytes: 200 << 20, Requests: 5000},
			}}, opts)
	}
	settled := st.Pools["shop"].SizingBytes()

	// A reload: the counter starts again, and the fresh survivors read small
	// while nothing is being served.
	after := now.Add(10 * time.Minute)
	for i := range 20 {
		st.Learn(Observation{Pool: "shop", At: after.Add(time.Duration(i) * 30 * time.Second),
			ActiveNow: 0, Accepted: int64(i),
			Workers: []WorkerSample{
				{RSSBytes: 18 << 20, Requests: 5000},
				{RSSBytes: 17 << 20, Requests: 5000},
			}}, opts)
	}

	if got := st.Pools["shop"].SizingBytes(); got < settled/2 {
		t.Errorf("a reload reset the counter and the estimate collapsed from %dMiB to "+
			"%dMiB; this tool causes those reloads", settled>>20, got>>20)
	}
}

// TestASlowPoolHoldsItsEstimateRatherThanDrifting.
//
// A review asked for this to go the other way: a pool that deploys a cheaper
// application but serves one request every thirty seconds never clears a
// per-scrape threshold, so its estimate never comes down. On examination that is
// the behaviour to want.
//
// Such a pool's workers are idle almost all the time, and PHP returns their
// large allocations to the operating system — so a small reading from them is
// evidence about idleness, not about what the application costs. Holding the old
// estimate costs nothing while nothing is asking anything of the pool, and
// letting it drift down is how the morning finds it sized for workers that do
// not exist. Accumulating the requests instead of requiring a rate reintroduced
// exactly that: a night at one request per five minutes pulled a 200MiB estimate
// to 35MiB.
func TestASlowPoolHoldsItsEstimateRatherThanDrifting(t *testing.T) {
	st := New()
	opts := Options{}.Defaults()
	now := time.Now()

	accepted := int64(0)
	for i := range 15 {
		accepted += 400
		st.Learn(Observation{Pool: "shop", At: now.Add(time.Duration(i) * 30 * time.Second),
			ActiveNow: 6, Accepted: accepted,
			Workers: []WorkerSample{{RSSBytes: 200 << 20, Requests: 5000}, {RSSBytes: 200 << 20, Requests: 5000}},
		}, opts)
	}
	settled := st.Pools["shop"].SizingBytes()

	// Twelve hours at one request per scrape, workers reading small because they
	// spend their lives waiting.
	slow := now.Add(10 * time.Minute)
	for i := range 1440 {
		accepted++
		st.Learn(Observation{Pool: "shop", At: slow.Add(time.Duration(i) * 30 * time.Second),
			ActiveNow: 0, Accepted: accepted,
			Workers: []WorkerSample{{RSSBytes: 18 << 20, Requests: 5000}, {RSSBytes: 17 << 20, Requests: 5000}},
		}, opts)
	}

	if got := st.Pools["shop"].SizingBytes(); got < settled/2 {
		t.Errorf("twelve hours at one request per scrape took the estimate from %dMiB "+
			"to %dMiB; those workers are idle, not cheap, and the morning would find "+
			"the pool sized for workers that do not exist", settled>>20, got>>20)
	}
}

// TestTheRequestCounterAdvancesOnEveryScrape.
//
// The gate is meant to ask "did this pool serve anything since the LAST scrape".
// The counter advanced only on scrapes that produced mature workers, so it
// actually asked "since the last scrape that taught us something" — and a pool
// with young workers overnight banked the whole night into one comparison.
//
// The consequence is smaller than it sounds, since one decay step against a
// thirty-minute half-life moves the estimate about a percent, and the very next
// scrape blocks again. It is tested as the invariant rather than dressed up as a
// disaster: the question is per-scrape, so the bookkeeping has to be per-scrape.
func TestTheRequestCounterAdvancesOnEveryScrape(t *testing.T) {
	st := New()
	opts := Options{}.Defaults()
	now := time.Now()

	// One scrape that teaches something, so the pool exists.
	st.Learn(Observation{Pool: "shop", At: now, ActiveNow: 4, Accepted: 100,
		Workers: []WorkerSample{{RSSBytes: 100 << 20, Requests: 500}, {RSSBytes: 100 << 20, Requests: 500}},
	}, opts)

	// Then scrapes with workers too young to learn from, while requests are
	// served. Each one must still move the counter.
	for i := range 5 {
		st.Learn(Observation{Pool: "shop", At: now.Add(time.Duration(i+1) * 30 * time.Second),
			ActiveNow: 1, Accepted: int64(200 + i*100),
			Workers: []WorkerSample{{RSSBytes: 20 << 20, Requests: 1}},
		}, opts)
	}

	if got := st.Pools["shop"].LastAccepted; got != 600 {
		t.Errorf("LastAccepted = %d after five scrapes reporting up to 600; the next "+
			"comparison is against a reading five scrapes old, so a quiet stretch banks "+
			"itself into permission to decay", got)
	}
}

// acceptedAt is a plausible lifetime request counter for a pool under steady
// load: monotonic in time, and generous enough that any two scrapes are well
// clear of the decay threshold.
func acceptedAt(at time.Time) int64 {
	return at.Unix() * 100
}

// TestAReloadDoesNotPullTheEstimateDown.
//
// php-fpm zeroes its counters on reload, and this tool causes reloads, so the
// difference between two readings can be meaningless. A review asked for this to
// be detected by the master's start time rather than by the count going
// backwards — worked through, that changes no answer, and in the one case where
// it would it makes things worse. The reasoning is recorded at didWork; what
// matters to a caller is this.
func TestAReloadDoesNotPullTheEstimateDown(t *testing.T) {
	st := New()
	opts := Options{}.Defaults()
	now := time.Now()

	// A pool that started recently, so its lifetime counter is still small —
	// which is the ordinary state of affairs after this tool has just reloaded
	// it once.
	for i := range 15 {
		st.Learn(Observation{Pool: "shop", At: now.Add(time.Duration(i) * 30 * time.Second),
			ActiveNow: 6, Accepted: int64(20 + i*20),
			Workers: []WorkerSample{{RSSBytes: 200 << 20, Requests: 5000}, {RSSBytes: 200 << 20, Requests: 5000}},
		}, opts)
	}
	settled := st.Pools["shop"].SizingBytes()

	// A reload. The counter restarts — and because the pool is busier afterwards
	// than it was before, the fresh count OVERTAKES the old one within a scrape.
	// The difference is positive and looks exactly like ordinary traffic; nothing
	// about the number says a reset happened. Only the start time does.
	after := now.Add(10 * time.Minute)
	for i := range 10 {
		st.Learn(Observation{Pool: "shop", At: after.Add(time.Duration(i) * 30 * time.Second),
			ActiveNow: 0, Accepted: int64(500 + i*500),
			Workers: []WorkerSample{{RSSBytes: 20 << 20, Requests: 5000}, {RSSBytes: 19 << 20, Requests: 5000}},
		}, opts)
	}

	if got := st.Pools["shop"].SizingBytes(); got < settled/2 {
		t.Errorf("a reload took the estimate from %dMiB to %dMiB; the counter restarting "+
			"looked like ordinary traffic", settled>>20, got>>20)
	}
}

// TestNoCounterMeansNoDecay: without the counter there is nothing that
// distinguishes a busy pool from a quiet one — the instantaneous count measures
// request duration as much as load. Refusing to decay costs headroom on one
// pool; guessing costs the host.
func TestNoCounterMeansNoDecay(t *testing.T) {
	st := New()
	opts := Options{}.Defaults()
	now := time.Now()

	for i := range 10 {
		st.Learn(Observation{Pool: "shop", At: now.Add(time.Duration(i) * 30 * time.Second),
			ActiveNow: 6, Accepted: int64(1000 + i*400),
			Workers: []WorkerSample{{RSSBytes: 200 << 20, Requests: 5000}, {RSSBytes: 200 << 20, Requests: 5000}},
		}, opts)
	}
	settled := st.Pools["shop"].SizingBytes()

	// The same pool, scraped without a counter, reading small.
	quiet := now.Add(10 * time.Minute)
	for i := range 60 {
		st.Learn(Observation{Pool: "shop", At: quiet.Add(time.Duration(i) * time.Minute),
			ActiveNow: 4,
			Workers:   []WorkerSample{{RSSBytes: 20 << 20, Requests: 5000}, {RSSBytes: 19 << 20, Requests: 5000}},
		}, opts)
	}

	if got := st.Pools["shop"].SizingBytes(); got < settled/2 {
		t.Errorf("the estimate fell from %dMiB to %dMiB on readings with nothing to say "+
			"whether the pool was working", settled>>20, got>>20)
	}
}

// inodeOf identifies the file behind a name, which is what tells a rename from
// an in-place rewrite.
func inodeOf(t *testing.T, path string) (uint64, error) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no inode available on this platform")
	}

	return uint64(sys.Ino), nil
}

// TestAScopedDaemonDoesNotForgetAnotherMastersPools.
//
// A state file can be shared by two daemons, each scoped to one master, and
// each sees only its own pools. Without knowing which master a pool belongs to,
// "not in this round's views" is indistinguishable from "belongs to somebody
// else" — so a scoped daemon deleted the other master's baselines after five
// rounds, and that daemon then sized those pools from a table.
func TestAScopedDaemonDoesNotForgetAnotherMastersPools(t *testing.T) {
	s := New()
	opts := Options{}.Defaults()
	base := time.Now().Add(-time.Hour)

	learn := func(pool, master string) {
		obs := busyObs(pool, base)
		obs.MasterConfig = master
		s.Learn(obs, opts)
	}
	learn("shop", "/etc/php/8.3/php-fpm.conf")
	learn("api", "/etc/php/8.2/php-fpm.conf")

	// Ten rounds of a daemon that only ever sees 8.3's pools.
	for i := 0; i < 10; i++ {
		s.Forget([]string{"shop"}, "/etc/php/8.3/php-fpm.conf")
	}

	if still := s.Lookup("/etc/php/8.2/php-fpm.conf", "api"); still == nil {
		t.Error("a daemon scoped to one master deleted another master's baseline out of " +
			"a shared state file; that pool is now sized from a profile guess, and a " +
			"week of the other daemon's learning is gone")
	}

	// Its own pool that really has gone is still forgotten.
	for i := 0; i < forgetAfterMissedRounds; i++ {
		s.Forget([]string{}, "/etc/php/8.3/php-fpm.conf")
	}
	if still := s.Lookup("/etc/php/8.3/php-fpm.conf", "shop"); still != nil {
		t.Error("a pool of this daemon's own master was never forgotten; a host that has " +
			"had sites come and go for years carries every one of them")
	}
}

// TestTwoMastersWithAPoolOfTheSameNameKeepSeparateBaselines.
//
// `www` is the default pool name in every distribution's package, so a host
// running PHP 8.2 and 8.3 side by side — which is what an upgrade looks like
// for as long as it takes — has two different pools called `www`, with
// different applications, different traffic and different worker costs.
//
// Keying state by name alone shared one record between them. Measured
// consequence: 8.3's trusted 40MiB/worker was used to reserve for 8.2's
// unreachable `www` that actually costs 220MiB, and the 5.4GiB difference went
// to a neighbour that does get written.
func TestTwoMastersWithAPoolOfTheSameNameKeepSeparateBaselines(t *testing.T) {
	s := New()
	opts := Options{}.Defaults()
	at := time.Now().Add(-time.Hour)

	const eight2 = "/etc/php/8.2/fpm/php-fpm.conf"
	const eight3 = "/etc/php/8.3/fpm/php-fpm.conf"

	learn := func(master string, rss int64) {
		var accepted int64
		for i := 0; i < 30; i++ {
			accepted += 6000
			s.Learn(Observation{
				Pool: "www", MasterConfig: master,
				At: at.Add(time.Duration(i) * 2 * time.Minute), ActiveNow: 6, Accepted: accepted,
				Workers: []WorkerSample{
					{RSSBytes: rss, Requests: 500}, {RSSBytes: rss, Requests: 500},
				},
			}, opts)
		}
	}
	learn(eight3, 40*mb)
	learn(eight2, 220*mb)

	cheap := s.Lookup(eight3, "www")
	dear := s.Lookup(eight2, "www")
	if cheap == nil || dear == nil {
		t.Fatalf("one of the two pools is missing: 8.3=%v 8.2=%v", cheap, dear)
	}
	if cheap.SizingBytes() > 60*mb {
		t.Errorf("8.3's www is costed at %dMiB; it has taken 8.2's measurements",
			cheap.SizingBytes()/mb)
	}
	if dear.SizingBytes() < 200*mb {
		t.Errorf("8.2's www is costed at %dMiB against a real 220MiB; the difference is "+
			"about to be offered to a neighbour that does get written",
			dear.SizingBytes()/mb)
	}
}

// TestALegacyRecordIsAdoptedRatherThanDiscarded: state written before pools
// carried a master has no master. Refusing it would throw away every baseline
// on the first run after an upgrade, so the first master to observe the pool
// adopts the record under its own key — one round of confusion on one upgrade,
// against sharing a record for ever.
func TestALegacyRecordIsAdoptedRatherThanDiscarded(t *testing.T) {
	s := New()
	opts := Options{}.Defaults()
	at := time.Now().Add(-time.Hour)

	// A record with no master, as an older version wrote it.
	s.Learn(busyObs("www", at), opts)
	legacy := s.Pools["www"]
	if legacy == nil {
		t.Fatal("setup: the unscoped record was not created")
	}
	legacy.TypicalPeakBytes = 123 * mb

	// It is found before any master has claimed it.
	if got := s.Lookup("/etc/php-fpm.conf", "www"); got == nil {
		t.Fatal("a baseline written before pools carried a master was thrown away")
	}

	// And the first master to observe it takes it over.
	obs := busyObs("www", at.Add(time.Minute))
	obs.MasterConfig = "/etc/php-fpm.conf"
	s.Learn(obs, opts)

	if _, still := s.Pools["www"]; still {
		t.Error("the unscoped record was left behind as well, so the pool now has two")
	}
	adopted := s.Lookup("/etc/php-fpm.conf", "www")
	if adopted == nil || adopted.TypicalPeakBytes == 0 {
		t.Errorf("the history was not carried over: %+v", adopted)
	}
}

// TestAScopedRoundDoesNotKeepAnotherMastersRemovedPoolAlive.
//
// The scope check sat after the keep check, which is the same fault as sharing
// a record, one line later. A scoped 8.3 round reset the absence counter of an
// 8.2 pool that happened to share the name — `www` in both, which is the case
// the scoping exists for — so a pool genuinely removed from 8.2 was never
// forgotten, and its stale baseline sat waiting for the next pool given that
// name.
func TestAScopedRoundDoesNotKeepAnotherMastersRemovedPoolAlive(t *testing.T) {
	s := New()
	opts := Options{}.Defaults()
	base := time.Now().Add(-time.Hour)

	const eight2 = "/etc/php/8.2/php-fpm.conf"
	const eight3 = "/etc/php/8.3/php-fpm.conf"

	for _, m := range []string{eight2, eight3} {
		obs := busyObs("www", base)
		obs.MasterConfig = m
		s.Learn(obs, opts)
	}

	// 8.2's www is removed, and 8.2's own daemon notices it four times.
	for i := 0; i < forgetAfterMissedRounds-1; i++ {
		s.Forget(nil, eight2)
	}

	// Meanwhile 8.3's daemon runs, and it sees a pool of the same name every
	// round. Its rounds must not touch 8.2's record at all — not to delete it,
	// which is the scoping working, and not to RESET its progress either.
	for i := 0; i < 20; i++ {
		s.Forget([]string{"www"}, eight3)
	}

	if s.Lookup(eight3, "www") == nil {
		t.Error("the running daemon's own pool was forgotten while it was being seen")
	}

	// One more round from 8.2's own daemon finishes the job.
	s.Forget(nil, eight2)
	if s.Lookup(eight2, "www") != nil {
		t.Error("a pool removed from 8.2 four rounds ago was not forgotten on the fifth; " +
			"8.3's rounds reset its counter by matching the NAME before the scope was " +
			"checked, so it can never be forgotten at all and its stale baseline waits " +
			"for whatever pool is next given that name")
	}
}

// TestSaveKeepsThePermissionsTheFileAlreadyHad.
//
// A rename installs the temporary file's permissions over the name, and this
// forced 0644 on every save — so an operator who deliberately chmodded the
// state file to 0600 on a shared host had it widened again within five minutes,
// silently, for as long as the daemon ran.
//
// What is in the file is pool names and memory numbers rather than secrets,
// which is why 0644 is a fine default and a bad thing to insist on.
func TestSaveKeepsThePermissionsTheFileAlreadyHad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New()
	s.Learn(busyObs("app", time.Now().Add(-time.Hour)), Options{})

	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o644 {
		t.Fatalf("a new state file is %04o, want 0644", info.Mode().Perm())
	}

	// The operator's choice.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %04o after a save, want the 0600 it was set to: the daemon "+
			"widens it again every five minutes for as long as it runs",
			info.Mode().Perm())
	}
}

// TestChildPerWorkerAmortisesOverThePeak is the fix for the review's P1: the
// per-worker child cost is measured as a scrape's total child memory over the
// pool's CONCURRENCY PEAK, not the few workers that happened to be alive when a
// scrape landed. Without it, a scrape catching a low-worker ondemand pool with
// both its workers transcoding would pin a whole worker's child as the PER-worker
// cost and, sized back up, reserve many times the child memory that ever existed.
func TestChildPerWorkerAmortisesOverThePeak(t *testing.T) {
	st := New()
	base := time.Now()
	st.Learn(Observation{
		Pool: "app", At: base, ActiveNow: 4, Accepted: base.Unix() * 100,
		Workers: []WorkerSample{{RSSBytes: 90 * mb, Requests: 500}, {RSSBytes: 90 * mb, Requests: 500}},
	}, Options{})
	// The pool has run 40 workers at its busiest.
	st.Pools["app"].PeakWorkers = 40

	// A scrape catches only two live workers, both mid-transcode (600MiB child).
	st.Learn(Observation{
		Pool: "app", At: base.Add(2 * time.Minute), ActiveNow: 2, Accepted: base.Unix()*100 + 1000,
		Workers: []WorkerSample{
			{RSSBytes: 90 * mb, SubtreeRSSBytes: 690 * mb, Requests: 500},
			{RSSBytes: 90 * mb, SubtreeRSSBytes: 690 * mb, Requests: 500},
		},
	}, Options{})

	// 1200MiB of children over the 40-worker peak = 30MiB per worker.
	if got := st.Pools["app"].ChildPerWorkerHighWaterBytes; got != 30*mb {
		t.Errorf("child per worker = %d bytes, want 30MiB (1200MiB over the 40-worker peak); a "+
			"low-worker scrape pinned a whole worker's child as the per-worker cost", got)
	}
}

// TestChildPerWorkerKeepsAGenuineSingleWorkerPool: the amortisation must not hide
// a real one-worker pool's child — there the peak IS one, so the full child is
// its per-worker cost.
func TestChildPerWorkerKeepsAGenuineSingleWorkerPool(t *testing.T) {
	st := New()
	base := time.Now()
	st.Learn(Observation{
		Pool: "solo", At: base, ActiveNow: 1, Accepted: base.Unix() * 100,
		Workers: []WorkerSample{{RSSBytes: 90 * mb, SubtreeRSSBytes: 690 * mb, Requests: 500}},
	}, Options{})
	st.Pools["solo"].PeakWorkers = 1
	st.Learn(Observation{
		Pool: "solo", At: base.Add(2 * time.Minute), ActiveNow: 1, Accepted: base.Unix()*100 + 500,
		Workers: []WorkerSample{{RSSBytes: 90 * mb, SubtreeRSSBytes: 690 * mb, Requests: 500}},
	}, Options{})

	if got := st.Pools["solo"].ChildPerWorkerHighWaterBytes; got != 600*mb {
		t.Errorf("child per worker = %d bytes, want the full 600MiB for a genuine one-worker pool", got)
	}
}
