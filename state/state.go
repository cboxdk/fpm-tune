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

	// ActiveNow is how many workers were busy at this instant. A smaller reading
	// from a pool with nobody working is a quiet pool, not a cheaper one.
	ActiveNow int
	At        time.Time
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

	// LastPeakBytes is the most recent per-scrape maximum from mature workers.
	//
	// Used as a FLOOR when sizing. The tracked estimate moves halfway towards a
	// new reading per scrape, which is fast but not immediate — and for a memory
	// ceiling, "not immediate" is the wrong direction to be wrong in. A deploy
	// that triples worker cost would otherwise let a pool be grown against a
	// figure the workers no longer resemble.
	LastPeakBytes int64 `json:"last_peak_bytes,omitempty"`

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

	// MinActiveToDecay is how many workers must be busy for a smaller reading to
	// count as evidence.
	//
	// This is the distinction the half-life alone could not make. A pool that got
	// CHEAPER and a pool that got QUIETER both show smaller workers — PHP returns
	// large allocations to the operating system, so an idle survivor genuinely
	// shrinks — and only one of them says anything about what the workload costs.
	// Concurrency tells them apart.
	MinActiveToDecay int

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
	if o.MinActiveToDecay <= 0 {
		o.MinActiveToDecay = 2
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

	return &s, nil
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

	ps.Samples++
	ps.LastUpdated = at

	// Only workers that have done enough work to have loaded the application.
	var peak int64
	mature := 0
	for _, w := range obs.Workers {
		if w.Requests < opts.MinRequestsPerWorker || w.RSSBytes <= 0 {
			continue
		}
		mature++
		if w.RSSBytes > peak {
			peak = w.RSSBytes
		}
	}

	if mature < opts.MinMatureWorkers || peak <= 0 {
		return false
	}

	ps.BusySamples++
	if ps.FirstBusyAt.IsZero() {
		ps.FirstBusyAt = at
	}
	ps.LastPeakBytes = peak
	if peak > ps.HighWaterBytes {
		ps.HighWaterBytes = peak
	}

	if ps.TypicalPeakBytes == 0 {
		ps.TypicalPeakBytes = peak
	} else {
		alpha := decayAlpha(since, opts.HalfLifeDown)
		switch {
		case peak > ps.TypicalPeakBytes:
			alpha = opts.AlphaUp
		case obs.ActiveNow < opts.MinActiveToDecay:
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
	// Measured from the first BUSY sample, not from first sight. A pool idle for
	// days had its clock run out long before any evidence arrived, so twenty
	// busy samples over ten minutes made it fully trusted — and the span exists
	// precisely to insist that a baseline has been watched through a real traffic
	// pattern rather than a lunchtime.
	since := ps.FirstBusyAt
	if since.IsZero() {
		since = ps.FirstSeen
	}
	bySpan := float64(ps.LastUpdated.Sub(since)) / float64(opts.ConfidenceSpan)

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
func (s *State) Forget(current []string) []string {
	keep := make(map[string]bool, len(current))
	for _, name := range current {
		keep[name] = true
	}

	var dropped []string
	for name := range s.Pools {
		if !keep[name] {
			dropped = append(dropped, name)
			delete(s.Pools, name)
		}
	}
	sort.Strings(dropped)

	return dropped
}

// Names returns the pools in the store, sorted, so output is stable.
func (s *State) Names() []string {
	names := make([]string, 0, len(s.Pools))
	for name := range s.Pools {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}
