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
	At      time.Time
}

// PoolState is what has been learned about one pool.
type PoolState struct {
	Pool string `json:"pool"`

	// TypicalPeakBytes is the sizing number: an exponentially weighted average
	// of the largest mature worker seen in each scrape.
	//
	// Not a mean of all workers, which systematically under-provisions — the
	// tail is what OOMs, and half the workers in any scrape are freshly
	// recycled. Not the all-time maximum either, which never comes down after
	// one unusual request. The typical worst worker is the number that has to
	// fit.
	TypicalPeakBytes int64 `json:"typical_peak_bytes"`

	// HighWaterBytes is the largest worker ever seen. Reported rather than used
	// for sizing: it is the evidence behind a warning that a pool occasionally
	// costs far more than it usually does.
	HighWaterBytes int64 `json:"high_water_bytes"`

	// Samples counts every scrape; BusySamples counts only those that taught us
	// something. The gap between them is how much of the watching was wasted on
	// an idle pool.
	Samples     int `json:"samples"`
	BusySamples int `json:"busy_samples"`

	FirstSeen   time.Time `json:"first_seen"`
	LastUpdated time.Time `json:"last_updated"`

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

	// Alpha is the exponential weight for new observations. Lower is steadier.
	Alpha float64

	// ConfidenceSamples and ConfidenceSpan are what a baseline needs before it
	// is trusted enough to size a pool DOWN.
	ConfidenceSamples int
	ConfidenceSpan    time.Duration
}

// Defaults fills in any unset option.
func (o Options) Defaults() Options {
	if o.MinRequestsPerWorker <= 0 {
		o.MinRequestsPerWorker = 20
	}
	if o.MinMatureWorkers <= 0 {
		o.MinMatureWorkers = 2
	}
	if o.Alpha <= 0 || o.Alpha > 1 {
		o.Alpha = 0.15
	}
	if o.ConfidenceSamples <= 0 {
		o.ConfidenceSamples = 20
	}
	if o.ConfidenceSpan <= 0 {
		o.ConfidenceSpan = 30 * time.Minute
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
	if peak > ps.HighWaterBytes {
		ps.HighWaterBytes = peak
	}

	if ps.TypicalPeakBytes == 0 {
		ps.TypicalPeakBytes = peak
	} else {
		ps.TypicalPeakBytes = int64(opts.Alpha*float64(peak) + (1-opts.Alpha)*float64(ps.TypicalPeakBytes))
	}

	return true
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
	bySpan := float64(ps.LastUpdated.Sub(ps.FirstSeen)) / float64(opts.ConfidenceSpan)

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
