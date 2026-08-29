// Package state remembers what each pool's workers actually cost.
//
// It is what separates this from a calculator that starts from a table every
// time. A restart should not throw away an hour of observation, and a pool that
// has been watched all week should not be sized by the same guess as one first
// seen a minute ago.
//
// The store is a JSON file written atomically. Deliberately not sqlite or bolt:
// it holds one row per pool, it should be readable with cat while debugging a
// bad allocation, and a corrupt file should be recoverable by deleting it.
package state

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DefaultPath is where state lives on a normal installation.
const DefaultPath = "/var/lib/fpm-tune/state.json"

// formatVersion guards against reading a file written by a future layout.
const formatVersion = 1

// WorkerSample is one worker as seen in one scrape.
type WorkerSample struct {
	RSSBytes int64
	// Requests is how many requests this worker has served since it started.
	// It is the maturity signal — see Learn.
	Requests int64
}

// Observation is one scrape of one pool.
type Observation struct {
	Pool    string
	Workers []WorkerSample

	// ActiveNow is how many workers were busy at this instant, and Accepted is
	// the pool's lifetime connection counter.
	//
	// A smaller reading from a pool doing no work is a quiet pool, not a cheaper
	// one. The counter is the reliable way to tell: how many workers happen to be
	// mid-request when a scrape lands depends on how long a request takes, so a
	// pool answering in two milliseconds reads as idle almost every time it is
	// looked at however much traffic it carries.
	ActiveNow int
	Accepted  int64
	At        time.Time
	// MasterConfig is the php-fpm configuration this pool belongs to. Optional:
	// it exists so a daemon scoped to one master does not forget another's pools
	// out of a shared state file.
	MasterConfig string
}

// PoolState is what has been learned about one pool.
type PoolState struct {
	Pool string `json:"pool"`

	// TypicalPeakBytes is the sizing number: a weighted average of the largest
	// mature worker seen in each scrape, which rises quickly and falls slowly.
	//
	// Not a mean of all workers, which systematically under-provisions — the
	// tail is what OOMs, and half the workers in any scrape are freshly
	// recycled. Not the all-time maximum either, which never comes down after
	// one unusual request and would hold a pool small forever.
	//
	// It tracks the day rather than being pinned to its worst hour: a pool whose
	// workers are genuinely cheaper at three in the morning should be allowed to
	// run more of them. What it will not do is believe that in a single step.
	// See Options.AlphaUp and AlphaDown.
	TypicalPeakBytes int64 `json:"typical_peak_bytes"`

	// HighWaterBytes is the largest worker ever seen. Reported rather than used
	// for sizing: it never comes down, so one pathological request would pin the
	// pool's size forever.
	HighWaterBytes int64 `json:"high_water_bytes"`

	// Samples counts every scrape; BusySamples counts only those that taught us
	// something. The gap between them is how much of the watching was wasted on
	// an idle pool.
	Samples     int `json:"samples"`
	BusySamples int `json:"busy_samples"`

	FirstSeen   time.Time `json:"first_seen"`
	LastUpdated time.Time `json:"last_updated"`

	// FirstBusyAt is when this pool first taught us something, as opposed to
	// when it was first SEEN.
	//
	// Confidence needs both enough samples and enough elapsed time, and measuring
	// the time from first sight let a pool that had been idle for days become
	// trusted after ten minutes of traffic: the clock had run out long before any
	// evidence arrived. The span has to be over the evidence.
	FirstBusyAt time.Time `json:"first_busy_at,omitempty"`

	// LastBusyAt is when this pool last taught us something.
	//
	// The span is measured between the two, so it grows only while evidence is
	// arriving. Measuring to LastUpdated let idle scrapes carry a pool over the
	// line: twenty-five busy samples in ten minutes, then two hours of nothing,
	// and the baseline was fully trusted on the strength of the waiting.
	LastBusyAt time.Time `json:"last_busy_at,omitempty"`

	// LastPeakAt is when LastPeakBytes was taken.
	//
	// Recorded but no longer used to expire the floor: expiring fell back to the
	// smoothed estimate, which after a single new reading has moved only halfway,
	// so a genuine deploy was sized well below what had just been measured. Kept
	// because it says how old the number is, which is worth having when reading a
	// state file by hand.
	LastPeakAt time.Time `json:"last_peak_at,omitempty"`

	// LastPeakBytes is the most recent per-scrape maximum from mature workers.
	//
	// Used as a FLOOR when sizing. The tracked estimate moves halfway towards a
	// new reading per scrape, which is fast but not immediate — and for a memory
	// ceiling, "not immediate" is the wrong direction to be wrong in. A deploy
	// that triples worker cost would otherwise let a pool be grown against a
	// figure the workers no longer resemble.
	LastPeakBytes int64 `json:"last_peak_bytes,omitempty"`

	// LastAccepted is the pool's connection counter as of the last scrape, so
	// the next one can tell whether any work happened in between.
	//
	// A counter that has gone BACKWARDS means php-fpm reset it — a reload, which
	// this tool causes — and that is treated as unknown rather than as negative
	// work.
	LastAccepted int64 `json:"last_accepted,omitempty"`

	// TypicalIntervalSeconds is how often this pool is actually looked at,
	// smoothed. It exists to bound what a HOLE in the observations is allowed to
	// do.
	//
	// The downward weight comes from elapsed time, which is right while the
	// looking is regular and wrong the moment it stops. A daemon restarted for a
	// package upgrade while php-fpm keeps serving comes back to a six-hour gap,
	// and six hours against a thirty-minute half-life is the maximum step the
	// clamp allows: 300MiB a worker to 180MiB on ONE reading of workers that
	// had merely gone quiet. serve plans immediately on start, so that is the
	// first plan after every restart — measured at +68% workers, which clears
	// the growth gate and is written.
	//
	// Elapsed time is only evidence of decay if it was WATCHED. This is how much
	// of it was.
	TypicalIntervalSeconds float64 `json:"typical_interval_seconds,omitempty"`

	// MissedRounds counts consecutive rounds in which this pool was not among
	// the ones discovered, so a transient discovery failure does not delete a
	// week of learning.
	MissedRounds int `json:"missed_rounds,omitempty"`

	// MasterConfig is the php-fpm configuration this pool was learned from.
	//
	// A state file can be shared by two daemons, each scoped to one master, and
	// each sees only its own pools. Without knowing which master a pool belongs
	// to, "not in this round's views" is indistinguishable from "belongs to
	// somebody else" — so a scoped daemon deleted the other master's baselines
	// after five rounds, and the other daemon then sized those pools from a
	// table. Empty means a version that did not record it, and such a pool is
	// never forgotten by a scoped daemon.
	MasterConfig string `json:"master_config,omitempty"`

	// ImmatureRounds counts consecutive rounds in which this pool had workers
	// and none of them had served enough requests to count. It is the evidence
	// for waiving the maturity gate, and it is deliberately not the sample
	// count: an idle pool teaches nothing about how fast its workers recycle.
	ImmatureRounds int `json:"immature_rounds,omitempty"`

	// PeakWorkers is the most workers this pool has had busy at once, as
	// remembered by us rather than by PHP-FPM.
	//
	// It has to be ours, because a reload resets pm.max_active_processes — and
	// this tool reloads. Sizing straight off PHP-FPM's counter therefore
	// ratchets downward: a cut triggers a reload, the reload clears the
	// evidence, the next observation looks quieter still, and the pool is cut
	// again. Observed directly on a live host as 20 -> 6 -> 2 over three rounds
	// while the pool was under load.
	PeakWorkers int       `json:"peak_workers,omitempty"`
	PeakAt      time.Time `json:"peak_at,omitempty"`

	// LastMaxChildrenReached is PHP-FPM's max_children counter as of the last
	// scrape. It is a running total since the master started, so only the delta
	// says whether a pool is hitting its ceiling NOW — a pool that ran out once
	// last Tuesday should not still be growing because of it.
	LastMaxChildrenReached int64 `json:"last_max_children_reached,omitempty"`

	// LastAppliedMaxChildren and LastAppliedAt support hysteresis: a change is
	// only worth a reload if it is big enough and the last one was long enough
	// ago.
	LastAppliedMaxChildren int       `json:"last_applied_max_children,omitempty"`
	LastAppliedAt          time.Time `json:"last_applied_at,omitempty"`
}

// State is the whole store.
type State struct {
	Version int                   `json:"version"`
	Pools   map[string]*PoolState `json:"pools"`

	// Master is how to find the php-fpm installation again when nothing is
	// running.
	//
	// Discovery scans the process table, which answers only while there is a
	// process to find. That is the wrong moment: if this tool's own file is what
	// stops php-fpm from starting — a pool it still overrides having been
	// removed, say — then there is no master to discover, so the repair can
	// never run, and the master stays down through every restart attempt with
	// fpm-tune sitting alongside it having caused it. Observed exactly that on a
	// VM.
	Master MasterRef `json:"master,omitempty"`
}

// MasterRef is the identity of a php-fpm installation, remembered across runs.
type MasterRef struct {
	Binary     string `json:"binary,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
	DropInDir  string `json:"drop_in_dir,omitempty"`
}

// Known reports whether there is enough here to act on.
func (m MasterRef) Known() bool {
	return m.Binary != "" && m.ConfigPath != "" && m.DropInDir != ""
}

// RememberMaster records where php-fpm lives, so a later run can find it when
// it is not running.
func (s *State) RememberMaster(binary, configPath, dropInDir string) {
	if binary == "" || configPath == "" || dropInDir == "" {
		return
	}
	s.Master = MasterRef{Binary: binary, ConfigPath: configPath, DropInDir: dropInDir}
}

// Options tunes learning. The zero value is usable.
type Options struct {
	// MinRequestsPerWorker is how many requests a worker must have served
	// before its memory is believed.
	//
	// This is the single most important guard in the package. A worker that has
	// served three requests has not yet loaded most of the application; one that
	// has served five hundred has. Learning from the former produces a number
	// that fails the moment real traffic arrives, and an idle pool is made
	// entirely of the former.
	MinRequestsPerWorker int64

	// MinMatureWorkers is how many such workers a scrape needs before it counts.
	// One mature worker is an anecdote.
	MinMatureWorkers int

	// AlphaUp is the weight given to an observation LARGER than the current
	// estimate. Per sample, and deliberately high: a worker costing more than
	// expected puts the whole budget at risk, and the budget is already
	// committed on the old number.
	AlphaUp float64

	// HalfLifeDown is how long it takes the estimate to fall halfway toward a
	// smaller observation.
	//
	// Expressed in TIME rather than as a per-sample weight, and that distinction
	// is the whole point. A per-sample weight is silently multiplied by the
	// scrape rate: 0.05 per sample at a 30-second interval is a twelve-minute
	// half-life, so a quiet night collapsed the estimate in twenty minutes while
	// the concurrency peak was deliberately held for a day. The pool was then
	// sized for many cheap workers, and the morning made them expensive again —
	// measured at 147 workers × 100MiB configured on an 8GiB host.
	//
	// In time, the sampling rate cannot change the behaviour.
	//
	// Six hours was chosen so a quiet night could not pull the estimate down,
	// and it did that at a price: measured on a VM, the tool sat at 214MiB per
	// worker while its workers had measured 93MiB for forty minutes and 595
	// consecutive samples, reserving well over twice what the pool needed. On a
	// fully committed host that is not caution — it is every other pool going
	// short, for most of a working day.
	//
	// Shortening it alone would have reintroduced the failure it was guarding
	// against, so the guard moved instead: the estimate only falls while the pool
	// is actually BUSY (see Observation.ActiveNow). A quiet pool no longer
	// teaches anything downward whatever the half-life says, which is what makes
	// a shorter one safe.
	HalfLifeDown time.Duration

	// MinRequestsPerSecondToDecay is how hard a pool must be working, since the
	// last scrape for a smaller reading to count as evidence.
	//
	// This is the distinction the half-life alone could not make. A pool that got
	// CHEAPER and a pool that got QUIETER both show smaller workers — PHP returns
	// large allocations to the operating system, so an idle survivor genuinely
	// shrinks — and only one of them says anything about what the workload costs.
	//
	// Counted in REQUESTS rather than in workers caught mid-flight, because the
	// latter measures request duration as much as load: a pool answering in two
	// milliseconds has nobody busy at almost any instant, and gating on that
	// would pin its estimate to whatever peak it once reached, for ever. Which is
	// the same fault as the one this gate exists to prevent, arrived at from the
	// other side.
	//
	// A RATE, in requests per second, and not a running total. Accumulating would
	// let a pool serving one request every five minutes reach the threshold
	// overnight and take its estimate down with it — and that pool's small
	// workers are idle workers, not cheap ones. Holding its estimate costs
	// nothing while nothing is asking anything of it, and the alternative is the
	// morning finding it sized for workers that do not exist.
	//
	// It was a count of requests between two scrapes, which is a rate only if
	// the scrape interval is fixed — so the same workload decayed or did not
	// depending on how often it was looked at, which is precisely the fault the
	// half-life was moved into TIME to remove. Five requests per thirty-second
	// scrape is 0.17 requests a second: cron, uptime checks and crawlers clear
	// it, so a night of nothing but bots pulled a pool from 400MiB a worker to
	// 60MiB, and the morning sized it at 102 workers of 400MiB against a 6GiB
	// budget.
	MinRequestsPerSecondToDecay float64

	// SamplesBeforeMaturityIsWaived is how long a pool may teach this tool
	// nothing before its immature workers are read anyway. A configuration that
	// recycles workers faster than they can mature is otherwise a permanent
	// blind spot, and a guess is worse evidence than a young worker.
	SamplesBeforeMaturityIsWaived int

	// MaxDecayGap is how long a hole in the observations may be before a smaller
	// reading is refused as evidence. A gap is missing information, not proof a
	// pool got cheaper, and the rate above averaged across it cannot tell a busy
	// hour followed by silence from steady work.
	MaxDecayGap time.Duration

	// ConfidenceSamples and ConfidenceSpan are what a baseline needs before it
	// is trusted enough to size a pool DOWN.
	ConfidenceSamples int
	ConfidenceSpan    time.Duration

	// PeakWindow is how long a remembered concurrency peak stands before it is
	// allowed to fall toward what is currently observed.
	//
	// Long enough to span a daily cycle, because the number that matters is what
	// the pool needs at its busiest hour, not at three in the morning. Without
	// any decay a single spike would pin a pool's size forever; with too little,
	// the peak is forgotten between busy periods and the pool is cut just before
	// it needs the workers.
	PeakWindow time.Duration
}

// Defaults fills in any unset option.
func (o Options) Defaults() Options {
	if o.MinRequestsPerWorker <= 0 {
		o.MinRequestsPerWorker = 20
	}
	if o.MinRequestsPerSecondToDecay <= 0 {
		// One request a second. Below this a pool is not being exercised: its
		// workers are between requests rather than cheap, and what they measure
		// is idle memory. High enough to exclude monitoring and crawler traffic,
		// low enough that a genuinely small site still teaches this anything.
		o.MinRequestsPerSecondToDecay = 1
	}
	if o.SamplesBeforeMaturityIsWaived <= 0 {
		o.SamplesBeforeMaturityIsWaived = 20
	}
	if o.MaxDecayGap <= 0 {
		o.MaxDecayGap = 12 * time.Hour
	}
	if o.MinMatureWorkers <= 0 {
		o.MinMatureWorkers = 2
	}
	if o.AlphaUp <= 0 || o.AlphaUp > 1 {
		o.AlphaUp = 0.5
	}
	if o.HalfLifeDown <= 0 {
		o.HalfLifeDown = 30 * time.Minute
	}
	if o.ConfidenceSamples <= 0 {
		o.ConfidenceSamples = 20
	}
	if o.ConfidenceSpan <= 0 {
		o.ConfidenceSpan = 30 * time.Minute
	}
	if o.PeakWindow <= 0 {
		o.PeakWindow = 24 * time.Hour
	}

	return o
}

// New returns an empty store.
func New() *State {
	return &State{Version: formatVersion, Pools: map[string]*PoolState{}}
}

// Load reads the store. A missing file is not an error — it is a first run.
//
// A file that cannot be parsed IS an error, and deliberately not a silent reset:
// starting over quietly would throw away everything learned and re-tune the host
// from bootstrap estimates, which looks exactly like the tool working. The
// caller decides whether to delete it.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return New(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read state %s: %w", path, err)
	}

	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("state file %s is not readable JSON (delete it to start over): %w", path, err)
	}
	if s.Version > formatVersion {
		return nil, fmt.Errorf("state file %s was written by a newer version (format %d, this build understands %d)",
			path, s.Version, formatVersion)
	}
	if s.Pools == nil {
		s.Pools = map[string]*PoolState{}
	}
	s.Version = formatVersion

	now := time.Now()
	for _, ps := range s.Pools {
		ps.inferCadence()
		ps.forgetTheFuture(now)
	}

	return &s, nil
}

// forgetTheFuture drops timestamps that have not happened yet.
//
// Confidence is measured between FirstBusyAt and LastBusyAt, and LastBusyAt can
// only move FORWARD — so a state file carrying 2099 gives a pool full
// confidence for ever, and no live observation can ever shorten the span. Full
// confidence is permission to CUT, so the pool is permanently eligible to be
// trimmed on evidence that never existed.
//
// One NTP step, one restore from a mis-clocked host, one container with a dead
// RTC. The tolerance is generous because a few seconds of clock skew between
// writing and reading is ordinary and means nothing.
func (ps *PoolState) forgetTheFuture(now time.Time) {
	const skew = time.Minute

	horizon := now.Add(skew)
	for _, t := range []*time.Time{
		&ps.FirstSeen, &ps.LastUpdated, &ps.FirstBusyAt, &ps.LastBusyAt,
		&ps.LastPeakAt, &ps.PeakAt,
	} {
		if t.After(horizon) {
			*t = time.Time{}
		}
	}

	// LastAppliedAt is CLAMPED, not cleared, and the difference matters.
	//
	// It is a brake: hysteresis reads it to refuse a reload within five minutes
	// of the last one. Zeroing it says "nothing has ever been applied", which
	// releases the brake — so an NTP correction of five minutes backwards, after
	// an apply, let the next round reload a pool it had just reloaded. Clamping
	// keeps the brake on for the full interval from now, which is the safe
	// direction for a value whose only job is to make the tool wait.
	if ps.LastAppliedAt.After(horizon) {
		ps.LastAppliedAt = now
	}
}

// inferCadence fills in the observation cadence for state written before it was
// recorded.
//
// Without it, the first scrape after an upgrade is a pool with no cadence and
// hours of elapsed time, which is exactly the uncapped step the cadence exists
// to prevent: 300MiB a worker to 180MiB on the first reading after the deploy
// that shipped the fix. The average interval over the pool's whole life is a
// good enough starting point, and one real scrape replaces it.
func (ps *PoolState) inferCadence() {
	if ps.TypicalIntervalSeconds > 0 || ps.Samples < 2 {
		return
	}
	if ps.FirstSeen.IsZero() || !ps.LastUpdated.After(ps.FirstSeen) {
		return
	}

	span := ps.LastUpdated.Sub(ps.FirstSeen).Seconds()
	ps.TypicalIntervalSeconds = span / float64(ps.Samples-1)
}

// Save writes the store atomically.
//
// Temp file plus rename, so a crash mid-write leaves the previous state rather
// than a truncated file. This runs on a schedule for as long as the host is up,
// which is enough occasions for the unlucky one to happen.
func (s *State) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("cannot create state directory: %w", err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode state: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.json")
	if err != nil {
		return fmt.Errorf("cannot create temporary state file: %w", err)
	}
	tmpName := tmp.Name()

	defer func() {
		// Only fires on the failure paths; the rename below has already moved
		// the file out from under it on success.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("cannot write state: %w", err)
	}
	// Flush to disk before the rename, so a power loss cannot leave a renamed
	// but empty file where the previous good state used to be.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("cannot flush state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot close state: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("cannot set state permissions: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("cannot install state: %w", err)
	}

	return nil
}

// Learn folds one observation into the store.
//
// It returns whether the observation taught anything. A scrape of an idle pool
// counts as a sample but does not move the baseline — see
// Options.MinRequestsPerWorker for why that distinction is the difference
// between a number that holds under load and one that does not.
func (s *State) Learn(obs Observation, opts Options) bool {
	opts = opts.Defaults()

	at := obs.At
	if at.IsZero() {
		at = time.Now()
	}

	ps := s.Pools[obs.Pool]
	if ps == nil {
		ps = &PoolState{Pool: obs.Pool, FirstSeen: at}
		s.Pools[obs.Pool] = ps
	}

	// Captured before LastUpdated moves: the downward weight is derived from how
	// much time has passed, not from how many times we happened to look.
	since := at.Sub(ps.LastUpdated)

	// Decided and recorded on EVERY scrape, before anything can return early.
	//
	// The counter used to advance only when a scrape produced mature workers, so
	// the delta was measured against the last MATURE scrape rather than the last
	// one — and a pool serving a request per scrape overnight with immature
	// workers accumulated the whole night into one comparison. That is the
	// running total this deliberately is not: it would let a quiet stretch buy
	// itself permission to pull the estimate down.
	// A threshold, and it stays a threshold. The ratio is kept because the
	// reasoning below needs the number.
	//
	// A ramp was tried — decay at a speed proportional to the traffic — because
	// the threshold is a cliff, and a measured one: 0.9 requests a second holds
	// a pool at 300MiB a worker for a week while 1.0 takes it to 90MiB in six
	// hours. A 10% difference in traffic, a 3.3x difference in reserved memory.
	//
	// The ramp does not work, and the reason is arithmetic rather than taste.
	// Decaying at a fifth of the speed still arrives: replayed, a pool at 0.2
	// requests a second — cron, uptime checks, crawlers — fell from 400MiB a
	// worker to 97MiB across one night, which is the whole failure the gate
	// exists to prevent, reached more slowly. Any speed above zero gets there
	// given a night, and nights are long.
	//
	// So the cliff is kept, on the side that costs capacity rather than the host.
	// A pool at 0.9 requests a second with 150ms requests has a concurrency of
	// 0.14: its workers are idle almost all the time, and their memory is idle
	// memory whatever the clock says. Every threshold has a cliff; this one is
	// placed where the wrong answer wastes memory instead of losing it.
	effort := workRatio(ps, obs, opts, since)
	worked := effort >= 1

	// Did it serve ANYTHING, which is a different and much lower bar than
	// "was it working hard enough for a smaller reading to be believed".
	servedSomething := effort > 0
	ps.LastAccepted = obs.Accepted

	// Learned before it is used, so the cadence tracks a changing interval and a
	// first observation contributes nothing.
	effective := cappedSince(ps, since, opts)
	ps.observeInterval(since, opts)

	ps.Samples++
	ps.LastUpdated = at
	if obs.MasterConfig != "" {
		ps.MasterConfig = obs.MasterConfig
	}

	// The peak is taken over EVERY worker with a reading; maturity decides only
	// whether the scrape counts at all.
	//
	// The maturity test used to filter the peak as well, and a maximum can only
	// ever be lowered by dropping candidates — which is the one direction that
	// ends in an OOM. A pool steady at 90MiB, scraped while three freshly
	// recycled workers were part-way through an export at 700MiB each, recorded
	// 90MiB and moved DOWN: 2.1GiB of live worker memory observed and discarded
	// for being young. The reason for the filter is that a young worker has not
	// loaded the application and reads small — and a small reading cannot move a
	// maximum, so the filter was never doing that job here.
	var peak int64
	mature := 0
	for _, w := range obs.Workers {
		if w.RSSBytes <= 0 {
			continue
		}
		if w.Requests >= opts.MinRequestsPerWorker {
			mature++
		}
		if w.RSSBytes > peak {
			peak = w.RSSBytes
		}
	}

	if peak <= 0 {
		return false
	}

	// Two mature workers, unless the pool has never HAD two.
	//
	// An ondemand pool serving 0.4 requests a second at 150ms a request runs one
	// worker, always. Measured over seven days and 20,160 scrapes, such a pool
	// was never learned from once: the plan sized it from a 48MiB profile guess
	// against a 90MiB truth, for ever. On a host with two of them, the plan
	// believed it had 384MiB of headroom while being 1056MiB over its budget —
	// half the OS reserve, and the measured pools had been grown into it.
	//
	// The relaxation is to the READING only. BusySamples stays where it is
	// (below), so confidence stays at zero, the pool is never Reducible, and its
	// floor stays at what it is configured for. Reserving what it costs is not
	// the same as earning permission to cut it, and letting one worker do the
	// second job would size an ondemand pool at two.
	sole := mature == 1 && ps.PeakWorkers <= 1

	// And a pool whose workers are recycled before they can ever mature.
	//
	// pm.max_requests at or below the maturity threshold means no worker ever
	// reaches it. Measured across a full weekday at up to 25 requests a second:
	// at pm.max_requests = 20 a fully loaded pool learned from 0 of 2880
	// scrapes, and at 15 and 10 the same. It fell back to the 48MiB profile
	// against a 120MiB truth, permanently — the same blind spot as the
	// single-worker pool, arrived at from the other side.
	//
	// Waived only after a long stretch of learning nothing at all, so an
	// ordinary pool's first few scrapes are still held to the real gate.
	// Counted on rounds where the pool HAD workers and none of them matured,
	// which is what "recycled before they warm up" looks like. Total samples
	// counts idle rounds too, and an ondemand pool sitting visible-but-empty for
	// twenty scrapes earned the waiver without ever having been seen to recycle
	// anything — so the first burst of four one-request workers at 80MiB was
	// taken as the pool's cost against a 200MiB truth.
	// Counted on any round where the pool SERVED something and produced no
	// mature worker, which is the recycling signature.
	//
	// It used to require `worked` — the decay threshold, one request a second —
	// and that is the wrong bar here. A pool at half a request a second with
	// pm.max_requests=10 recycles just as thoroughly and was never measured at
	// all, so it kept the profile's guess for ever, which is the exact
	// under-measurement the waiver exists to fix. The rate gate is about whether
	// a SMALLER reading is believable, and a waived reading can only ever raise
	// the estimate — so gating it on the rate buys nothing and costs every
	// low-traffic pool.
	if mature == 0 && len(obs.Workers) > 0 && servedSomething {
		ps.ImmatureRounds++
	} else if mature > 0 {
		ps.ImmatureRounds = 0
	}

	// Once the pattern is EARNED it keeps applying, for as long as it holds.
	//
	// It used to require TypicalPeakBytes == 0, which made the waiver one-shot:
	// the first waived reading set the estimate and every later reading was
	// refused again, so a pool with pm.max_requests=10 went on being costed at
	// whatever it happened to cost the day it was first seen. A deploy that made
	// every worker 220MiB was invisible.
	//
	// But that condition was also doing real work, and dropping it alone opened
	// the hole it was closing: a QUIET pool's workers have low request counts
	// too, so the waiver started reading idle memory and a quiet night pulled an
	// established estimate from 240MiB to 50MiB.
	//
	// `worked` is what separates them. A pool recycling its workers every ten
	// requests is a pool serving twenty-five requests a second; a pool whose
	// workers are young because nothing is asking anything of them is not. The
	// counter is advanced only on rounds where the pool was busy AND produced no
	// mature worker, which is the recycling signature and nothing else, and it
	// resets the moment a mature worker appears.
	recycled := mature == 0 && servedSomething &&
		ps.ImmatureRounds >= opts.SamplesBeforeMaturityIsWaived

	if mature < opts.MinMatureWorkers && !sole && !recycled {
		return false
	}

	// A reading taken from workers that never matured may RAISE the estimate and
	// never lower it.
	//
	// The waiver exists because a pool recycling every ten requests has no
	// mature workers to offer and would otherwise be sized from a table. But a
	// worker on its first or second request has not loaded the application yet,
	// and at 100 requests a second that is most of what a scrape catches — so
	// the readings are a mixture of warm workers and cold ones, and the cold
	// ones are not evidence that the pool got cheaper. Measured: an established
	// 240MiB pool fell to 50MiB across a night of exactly that.
	//
	// Upward is different. 2.75x the memory is real memory, whatever the worker
	// has served, and a deploy that makes every worker more expensive is the
	// case the waiver was added for.
	if recycled && mature == 0 && peak <= ps.SizingBytes() {
		return false
	}

	// Confidence is permission to size a pool DOWN, and the only thing that earns
	// it is having watched the pool under load.
	//
	// This block used to run whenever two mature workers existed, so a pool left
	// with two workers from a deploy smoke test reached full confidence over
	// thirty minutes of zero traffic — and full confidence drops its floor from
	// its configured ceiling to two, putting it first in the queue to be cut on
	// the strength of sixty-one readings of nothing. The memory readings below
	// are recorded either way; it is the confidence accounting that needs the
	// traffic, and `worked` is the signal the decay branch already trusts for it.
	if worked && mature >= opts.MinMatureWorkers {
		ps.BusySamples++
		if ps.FirstBusyAt.IsZero() {
			ps.FirstBusyAt = at
		}
		// LastBusyAt - FirstBusyAt is the confidence span, and a clock stepping
		// backwards used to make it negative: confidence 1.00 to 0.00 on one
		// sample, held there until wall time caught up. The rate gate in didWork
		// now refuses a non-positive interval, which makes this unreachable —
		// at > LastUpdated >= FirstBusyAt. Kept because the two live in
		// different functions and the span must not be able to invert.
		if at.After(ps.LastBusyAt) {
			ps.LastBusyAt = at
		}
	}
	ps.LastPeakBytes = peak
	ps.LastPeakAt = at
	if peak > ps.HighWaterBytes {
		ps.HighWaterBytes = peak
	}

	if ps.TypicalPeakBytes == 0 {
		ps.TypicalPeakBytes = peak
	} else {
		alpha := decayAlpha(effective, opts.HalfLifeDown)
		switch {
		case peak > ps.TypicalPeakBytes:
			alpha = opts.AlphaUp
		case !worked:
			// Smaller, from a pool that is not working. That is what a quiet
			// stretch looks like, and believing it is how a quiet night leaves
			// the morning sized for workers that do not exist. The high-water
			// mark and the sample count above still stand — only the estimate
			// holds.
			alpha = 0
		}
		ps.TypicalPeakBytes = int64(alpha*float64(peak) + (1-alpha)*float64(ps.TypicalPeakBytes))
	}

	return true
}

// ObservePeak records the concurrency high-water mark this tool has seen.
//
// Returns the peak to size against. A new high replaces the old one outright; a
// lower observation is ignored until the remembered peak is older than
// PeakWindow, after which it decays toward what is actually being seen so a pool
// that has genuinely quietened down can eventually give its headroom back.
func (ps *PoolState) ObservePeak(current int, at time.Time, opts Options) int {
	opts = opts.Defaults()

	if current > ps.PeakWorkers {
		ps.PeakWorkers = current
		ps.PeakAt = at

		return ps.PeakWorkers
	}

	if ps.PeakAt.IsZero() {
		ps.PeakAt = at
	}

	if at.Sub(ps.PeakAt) > opts.PeakWindow {
		// Halve the distance to what is being seen now, rather than dropping
		// straight to it: one quiet scrape after a stale peak should not undo a
		// day of evidence in a single step.
		ps.PeakWorkers -= (ps.PeakWorkers - current + 1) / 2
		if ps.PeakWorkers < current {
			ps.PeakWorkers = current
		}
		ps.PeakAt = at
	}

	return ps.PeakWorkers
}

// decayAlpha converts elapsed time into an exponential weight with the given
// half-life, so the estimate falls at the same rate whether it is sampled every
// five seconds or every five minutes.
//
// A gap longer than the half-life is clamped: a daemon that was stopped for a
// week should not treat its first observation on return as the whole truth.
// observeInterval keeps a smoothed record of how often this pool is looked at.
//
// Only intervals that look like observation rather than absence, so a hole does
// not teach the cadence that holes are normal.
func (ps *PoolState) observeInterval(since time.Duration, opts Options) {
	if since <= 0 || since > opts.MaxDecayGap {
		return
	}
	if ps.TypicalIntervalSeconds <= 0 {
		ps.TypicalIntervalSeconds = since.Seconds()

		return
	}
	if since.Seconds() > gapMultiple*ps.TypicalIntervalSeconds {
		// A gap, not a change of cadence. Learning from it would let a long
		// enough absence normalise itself and unlock the step it should have
		// been refused.
		return
	}

	const alpha = 0.25
	ps.TypicalIntervalSeconds = (1-alpha)*ps.TypicalIntervalSeconds + alpha*since.Seconds()
}

// gapMultiple is how far an interval may stray from the learned cadence and
// still be taken as a change of cadence rather than a hole. A daemon whose
// interval is reconfigured settles onto the new one within a few rounds; an
// absence never teaches it that absences are normal.
const gapMultiple = 3

// cappedSince bounds the elapsed time that reaches the decay, so a hole can
// never produce a larger step than a normal scrape.
//
// A one-shot run from cron has a cadence of an hour, and an hour of decay is
// exactly right for it; a daemon on a fifteen-second loop that has been away for
// six hours gets fifteen seconds' worth. The difference between them is what
// this tool was doing during the time, which is the only thing that makes
// elapsed time evidence.
func cappedSince(ps *PoolState, since time.Duration, opts Options) time.Duration {
	_ = opts
	if ps.TypicalIntervalSeconds <= 0 {
		return since
	}

	// The cadence itself, not a multiple of it. Whatever one ordinary scrape is
	// allowed to move the estimate, a hole may not move it more — that is the
	// whole claim, and a multiple would leave a long absence worth several
	// scrapes it did not perform. Under a slightly irregular cadence this
	// under-decays by a few percent, which is the safe direction.
	limit := time.Duration(ps.TypicalIntervalSeconds * float64(time.Second))
	if since > limit {
		return limit
	}

	return since
}

func decayAlpha(since, halfLife time.Duration) float64 {
	if since <= 0 || halfLife <= 0 {
		return 0
	}

	alpha := 1 - math.Exp2(-since.Seconds()/halfLife.Seconds())
	if alpha > 0.5 {
		alpha = 0.5
	}

	return alpha
}

// SizingBytes is the per-worker cost a pool should be sized against.
//
// The tracked estimate, which follows the workload up quickly and down slowly —
// but never below the most recent reading.
//
// The floor is the important half. The estimate moves halfway towards a new
// observation per scrape, which is fast, and fast is not the same as immediate:
// a deploy that triples what a worker costs leaves the estimate at half the
// truth for a scrape or two, and a pool grown against that figure is grown
// against workers that no longer exist. Being slow to believe memory got CHEAPER
// costs some unused headroom; being slow to believe it got more EXPENSIVE costs
// the host.
func (ps *PoolState) SizingBytes() int64 {
	if ps == nil {
		return 0
	}

	size := ps.TypicalPeakBytes
	if size == 0 {
		size = ps.HighWaterBytes
	}
	// The floor does not expire on a clock, and an earlier version of it did.
	//
	// The worry was that one anomalous scrape holds the sizing high on a pool
	// that then goes quiet. It does — and the alternative turned out to be worse.
	// Expiring falls back to the smoothed estimate, which after a single new
	// reading has moved only halfway: a genuine deploy from 40MiB to 120MiB would
	// be sized at 80MiB fifteen minutes later, having seen nothing at all to
	// contradict the 120. Under-sizing is the failure that ends in an OOM kill;
	// holding high costs unused headroom on one pool.
	//
	// So the floor is simply the most recent mature measurement, replaced — up or
	// down — by the next one.
	if ps.LastPeakBytes > size {
		size = ps.LastPeakBytes
	}

	return size
}

// Confidence is how far a pool's baseline can be trusted, from 0 to 1.
//
// Derived rather than stored: a stored confidence goes stale the moment anything
// else changes, and this is cheap.
//
// Both a sample count and a time span are required, and the lower of the two
// wins. Samples alone would let a tight polling loop declare confidence in
// thirty seconds, which measures the scrape interval rather than the workload —
// no daily traffic pattern has been seen yet.
func (ps *PoolState) Confidence(opts Options) float64 {
	if ps == nil {
		return 0
	}
	opts = opts.Defaults()

	if ps.BusySamples == 0 || ps.TypicalPeakBytes <= 0 {
		return 0
	}

	bySamples := float64(ps.BusySamples) / float64(opts.ConfidenceSamples)
	// Measured across the EVIDENCE: first busy sample to last busy sample. Not
	// from first sight, which let a pool idle for days arrive pre-trusted, and
	// not to LastUpdated, which let idle scrapes carry it over the line — twenty
	// busy samples in ten minutes, then two hours of nothing, and the baseline
	// counted as watched through a real traffic pattern.
	//
	// State written before these fields existed has neither, and is treated as
	// having no span rather than as having satisfied it: inheriting the old
	// overconfidence on upgrade would be the worst of both.
	if ps.FirstBusyAt.IsZero() || ps.LastBusyAt.IsZero() {
		return 0
	}
	bySpan := float64(ps.LastBusyAt.Sub(ps.FirstBusyAt)) / float64(opts.ConfidenceSpan)

	c := bySamples
	if bySpan < c {
		c = bySpan
	}
	if c > 1 {
		c = 1
	}
	if c < 0 {
		c = 0
	}

	return c
}

// Trusted reports whether a baseline may be used instead of a bootstrap
// estimate.
func (ps *PoolState) Trusted(opts Options) bool {
	return ps.Confidence(opts) >= 1
}

// RecordApplied notes that a pool was reconfigured, for hysteresis.
func (s *State) RecordApplied(pool string, maxChildren int, at time.Time) {
	ps := s.Pools[pool]
	if ps == nil {
		ps = &PoolState{Pool: pool, FirstSeen: at}
		s.Pools[pool] = ps
	}
	ps.LastAppliedMaxChildren = maxChildren
	ps.LastAppliedAt = at
}

// Forget drops pools that are no longer configured, so a host that has had
// sites removed over the years does not carry them forever.
//
// Called with the pools that currently exist; anything else goes.
func (s *State) Forget(current []string, scope string) []string {
	keep := make(map[string]bool, len(current))
	for _, name := range current {
		keep[name] = true
	}

	var dropped []string
	for name, ps := range s.Pools {
		if keep[name] {
			ps.MissedRounds = 0

			continue
		}

		// Not mine to forget.
		//
		// A caller scoped to one master sees only that master's pools, so a pool
		// it cannot see may simply belong to the other daemon sharing this file.
		// Deleting it took a week of that daemon's learning, and the pool then
		// came back sized from a profile.
		if scope != "" && ps.MasterConfig != scope {
			continue
		}

		// Missing from ONE round is not the same as gone.
		//
		// Discovery skips a master whose configuration it cannot parse rather
		// than failing the round, so a single transient `php-fpm -tt` error on a
		// host running two PHP versions made every pool of one of them
		// disappear — and a week of learning went with it. The direction was
		// safe, since a forgotten pool reverts to its configured ceiling and the
		// profile guess, but it is a week of learning either way.
		//
		// A site that really is removed is still forgotten, a few rounds later.
		ps.MissedRounds++
		if ps.MissedRounds < forgetAfterMissedRounds {
			continue
		}

		dropped = append(dropped, name)
		delete(s.Pools, name)
	}
	sort.Strings(dropped)

	return dropped
}

// forgetAfterMissedRounds is how many consecutive rounds a pool may be absent
// before its baseline is discarded.
const forgetAfterMissedRounds = 5

// Names returns the pools in the store, sorted, so output is stable.
func (s *State) Names() []string {
	names := make([]string, 0, len(s.Pools))
	for name := range s.Pools {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}

// didWork reports whether the pool served anything since the last scrape.
//
// The counter first, because it is the honest measure of load: it rises with
// throughput whatever a request costs in time. The instantaneous busy count is a
// fallback for a scrape that did not report the counter, and on its own it
// measures request duration as much as traffic.
//
// A counter that has gone backwards means php-fpm reset it, which happens on
// every reload — including the ones this tool performs. Unknown, not negative:
// the reading is neither believed nor used to block, so the pool keeps whatever
// estimate it had until the next scrape can compare properly.
func workRatio(ps *PoolState, obs Observation, opts Options, since time.Duration) float64 {
	if obs.Accepted <= 0 || ps.LastAccepted <= 0 {
		// No counter to compare against. Refused rather than guessed: the
		// instantaneous busy count measures request duration as much as load, and
		// a pool answering quickly reads idle whatever it is carrying. Declining
		// to decay leaves the estimate where it is, which costs headroom on one
		// pool — the alternative costs the host.
		return 0
	}

	if since > opts.MaxDecayGap {
		// Nothing was watched across the hole, and the average rate over it says
		// nothing about whether the pool was working when this reading was taken.
		return 0
	}

	served := obs.Accepted - ps.LastAccepted
	if served < 0 {
		// A reload zeroed the counter, so the difference means nothing. Unknown,
		// not negative work.
		//
		// A review asked for this to be detected by the master's start time
		// instead, on the grounds that a busy pool's fresh count can overtake the
		// old reading and hide the reset. Worked through, the start time changes
		// no answer: after a reset obs.Accepted is at least as large as the
		// difference, so wherever the difference clears the threshold the fresh
		// count does too. Where they differ is a fresh count above the threshold
		// with a difference below it — a pool that has served a good deal since
		// the master came up and very little since the last scrape, which is a
		// SLOW pool, and slow pools are exactly what this must not let decay.
		// Detecting the reset there would have made it worse. The cost of not
		// detecting it is one skipped decay sample, because LastAccepted now
		// advances on every scrape and the readings realign immediately.
		return 0
	}

	if since <= 0 {
		// The first observation of a pool, or a clock that stepped backwards.
		// Neither is a measured interval, so there is no rate to compare.
		return 0
	}

	// Clamped at 1: working harder than the threshold does not decay FASTER
	// than the half-life says. The threshold is where a pool counts as fully
	// exercised, not a scale.
	ratio := (float64(served) / since.Seconds()) / opts.MinRequestsPerSecondToDecay
	if ratio > 1 {
		ratio = 1
	}

	return ratio
}
