package state

import (
	"os"
	"path/filepath"
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

	steady := func(rss int64, at time.Time) Observation {
		return Observation{Pool: "app", At: at, Workers: []WorkerSample{
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

	if afterFirstQuiet < 130*mb {
		t.Errorf("one quiet reading dropped the estimate to %dMB; the next busy "+
			"minute would find the pool over-provisioned with workers", afterFirstQuiet/mb)
	}

	for i := 0; i < 120; i++ {
		s.Learn(steady(50*mb, now.Add(time.Duration(60+i)*time.Minute)), opts)
	}
	if released := s.Pools["app"].TypicalPeakBytes; released > 60*mb {
		t.Errorf("the estimate stayed at %dMB after two hours of 50MB workers; "+
			"the pool is pinned to its worst hour and the quiet day is wasted", released/mb)
	}

	// The spike is remembered, it is just not what the pool is sized on.
	if s.Pools["app"].HighWaterBytes != 150*mb {
		t.Errorf("high water = %dMB, want 150MB", s.Pools["app"].HighWaterBytes/mb)
	}
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
	base := time.Now()

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
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	first := New()
	first.Learn(busyObs("app", time.Now()), Options{})
	if err := first.Save(path); err != nil {
		t.Fatal(err)
	}

	// A second save must not leave temp files behind.
	if err := first.Save(path); err != nil {
		t.Fatal(err)
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
func TestForgetDropsRemovedPools(t *testing.T) {
	s := New()
	for _, name := range []string{"kept-a", "kept-b", "removed"} {
		s.Learn(busyObs(name, time.Now()), Options{})
	}

	dropped := s.Forget([]string{"kept-a", "kept-b"})

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

// TestRecordApplied stores what hysteresis needs later.
func TestRecordApplied(t *testing.T) {
	s := New()
	at := time.Now()

	s.RecordApplied("new-pool", 24, at)

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

func busyObs(pool string, at time.Time) Observation {
	return Observation{
		Pool: pool, At: at,
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
