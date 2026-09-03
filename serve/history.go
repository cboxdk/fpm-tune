package serve

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/plan"
)

// A day of rounds, in memory, for anything that wants to draw a line.
//
// The daemon remembers nothing between rounds: every round is a fresh plan,
// and the only record of the last one is a log line and a Prometheus scrape
// somebody may or may not have taken. That is right for the plan — a plan
// should not depend on what yesterday's plan said — and wrong for a person
// asking "what has this pool been doing", which is a question about a line,
// not a point. So each round leaves one sample in a ring, and each apply
// leaves an event, and /history.json hands them out as JSON. A day at the
// default interval is under three thousand samples of a few numbers per pool;
// the ring costs less than one worker's opcache.
//
// It is a convenience for a dashboard or a terminal UI, not a store: it starts
// empty at every daemon start and is never written to disk. Prometheus is the
// place for anything that must survive a restart.

// HistorySample is one round as the history keeps it.
type HistorySample struct {
	At time.Time `json:"at"`

	// HostBusyRatio is how busy the box's CPU was over the interval that ended
	// at this round, 0 to 1 (above 1 is not possible; a quota is the box).
	// HostBusyKnown is false on the first round and wherever the box could
	// not be read, and the ratio then means nothing.
	HostBusyRatio float64 `json:"host_busy_ratio"`
	HostBusyKnown bool    `json:"host_busy_known"`

	Pools []PoolSample `json:"pools"`
}

// PoolSample is one pool in one round: what was observed, what was planned,
// and what the CPU side made of it.
type PoolSample struct {
	Pool        string `json:"pool"`
	Active      int    `json:"active"`
	Queue       int64  `json:"queue"`
	Configured  int    `json:"configured"`
	Recommended int    `json:"recommended"`
	DemandUnmet bool   `json:"demand_unmet"`
	Unknown     bool   `json:"unknown,omitempty"`

	// WorkerBytes is the per-worker cost the plan sized on.
	WorkerBytes int64 `json:"worker_bytes"`

	// MemoryCeiling is what memory alone would have proposed, before any
	// CPU ceiling: the other bound the plan is the minimum of.
	MemoryCeiling int `json:"memory_ceiling"`

	CPURatioP50 float64 `json:"cpu_ratio_p50"`
	CPUReadings int64   `json:"cpu_readings"`
	CPUFill     int     `json:"cpu_fill_workers"`
	CPUCeiling  int     `json:"cpu_ceiling"`
	CPULimited  bool    `json:"cpu_limited"`
	CPUBound    bool    `json:"cpu_bound"`
}

// HistoryEvent is something the daemon did, or failed to do, to the host.
type HistoryEvent struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Pool string    `json:"pool,omitempty"`
	From int       `json:"from,omitempty"`
	To   int       `json:"to,omitempty"`

	// Detail is the reason, or the error, in the words the log used.
	Detail string `json:"detail,omitempty"`
}

// The event kinds.
const (
	EventResized        = "resized"
	EventApplyFailed    = "apply_failed"
	EventRolledBack     = "rolled_back"
	EventRollbackFailed = "rollback_failed"
	EventRepaired       = "repaired"

	// EventChanged is a pool whose configured ceiling moved between two
	// rounds without this daemon having moved it: a hand edit, a deploy, or
	// an fpm-tune apply run beside the daemon. Recorded so the history shows
	// every change to the host, not only the daemon's own.
	EventChanged = "changed"
)

// historyEvents is how many events the ring keeps. A daemon that reloads
// every five minutes for a day makes under three hundred; a thousand is a
// week of a busy one.
const historyEvents = 1000

// history is the ring itself. Written by the loop, read by the HTTP handler,
// so it locks.
type history struct {
	mu       sync.Mutex
	interval time.Duration
	host     HostInfo

	samples []HistorySample
	head    int // where the next sample goes
	n       int // how many are valid

	events []HistoryEvent
	ehead  int
	en     int
}

func newHistory(rounds int, interval time.Duration) *history {
	if rounds < 2 {
		rounds = 2
	}

	return &history{
		interval: interval,
		samples:  make([]HistorySample, rounds),
		events:   make([]HistoryEvent, historyEvents),
	}
}

// record keeps one round, dropping the oldest when the ring is full.
func (h *history) record(s HistorySample) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.samples[h.head] = s
	h.head = (h.head + 1) % len(h.samples)
	if h.n < len(h.samples) {
		h.n++
	}
}

// event keeps one event, oldest dropped first.
func (h *history) event(e HistoryEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.events[h.ehead] = e
	h.ehead = (h.ehead + 1) % len(h.events)
	if h.en < len(h.events) {
		h.en++
	}
}

// snapshot copies out the newest `last` samples (all when last <= 0), oldest
// first, and every event, oldest first.
func (h *history) snapshot(last int) ([]HistorySample, []HistoryEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	n := h.n
	if last > 0 && last < n {
		n = last
	}
	samples := make([]HistorySample, 0, n)
	start := (h.head - n + len(h.samples)) % len(h.samples)
	for i := 0; i < n; i++ {
		samples = append(samples, h.samples[(start+i)%len(h.samples)])
	}

	events := make([]HistoryEvent, 0, h.en)
	estart := (h.ehead - h.en + len(h.events)) % len(h.events)
	for i := 0; i < h.en; i++ {
		events = append(events, h.events[(estart+i)%len(h.events)])
	}

	return samples, events
}

// HostInfo is what a client needs to label the history: which box, how big,
// and what the daemon is allowed to do to it.
type HostInfo struct {
	Hostname    string  `json:"hostname"`
	Version     string  `json:"version"`
	Apply       bool    `json:"apply"`
	CPUCeiling  bool    `json:"cpu_ceiling"`
	CPUHeadroom float64 `json:"cpu_headroom"`

	MemoryBytes   int64  `json:"memory_bytes"`
	CPUMillicores int    `json:"cpu_millicores"`
	Source        string `json:"memory_source"`
}

// HistoryResponse is the body of /history.json.
type HistoryResponse struct {
	// IntervalSeconds is how far apart the rounds are, and Capacity how many
	// the ring holds: together, how far back the history can reach.
	IntervalSeconds float64 `json:"interval_seconds"`
	Capacity        int     `json:"capacity"`

	Host HostInfo `json:"host"`

	Rounds []HistorySample `json:"rounds"`
	Events []HistoryEvent  `json:"events"`
}

// setHost records what the daemon knows about the box, refreshed each round
// because the budget can change under a running daemon.
func (h *history) setHost(info HostInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.host = info
}

// ServeHTTP answers GET /history.json. ?last=N limits the rounds to the newest
// N; the events always come whole.
func (h *history) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "GET only", http.StatusMethodNotAllowed)

		return
	}
	last := 0
	if v := r.URL.Query().Get("last"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "last must be a non-negative integer", http.StatusBadRequest)

			return
		}
		last = n
	}

	rounds, events := h.snapshot(last)
	h.mu.Lock()
	host := h.host
	h.mu.Unlock()
	body := HistoryResponse{
		IntervalSeconds: h.interval.Seconds(),
		Capacity:        len(h.samples),
		Host:            host,
		Rounds:          rounds,
		Events:          events,
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

// noteExternalChanges compares each pool's configured ceiling with the
// previous round's and records the ones that moved without this daemon
// moving them. The daemon's own resizes are expected on the following round
// and are not events twice.
func (l *Loop) noteExternalChanges(views []observe.PoolView, now time.Time) {
	if l.lastConfigured == nil {
		l.lastConfigured = map[string]int{}
	}
	if l.expected == nil {
		l.expected = map[string]int{}
	}
	for _, v := range views {
		if v.Err != nil || !v.MaxChildrenKnown {
			continue
		}
		prev, seen := l.lastConfigured[v.Name]
		l.lastConfigured[v.Name] = v.CurrentMaxChildren
		if !seen || prev == v.CurrentMaxChildren {
			continue
		}
		if want, ours := l.expected[v.Name]; ours && want == v.CurrentMaxChildren {
			delete(l.expected, v.Name)

			continue
		}
		l.history.event(HistoryEvent{
			At: now, Kind: EventChanged, Pool: v.Name, From: prev, To: v.CurrentMaxChildren,
			Detail: "configured outside this daemon: a hand edit, a deploy, or fpm-tune apply",
		})
	}
}

// hostBusyRatio turns two consecutive box CPU readings into how busy the box
// was in between, as a fraction of the CPU it has. Unknown on the first round,
// where the box could not be read, across a hole longer than five minutes, and
// when the counter went backwards.
func (l *Loop) hostBusyRatio(now budget.HostCPU, ok bool, millicores int) (float64, bool) {
	prev, had := l.lastHostCPU, l.hasHostCPU
	l.lastHostCPU, l.hasHostCPU = now, ok
	if !ok || !had || millicores <= 0 {
		return 0, false
	}
	wall := now.At.Sub(prev.At)
	busy := now.BusyMicros - prev.BusyMicros
	if wall < 5*time.Second || wall > 5*time.Minute || busy < 0 {
		return 0, false
	}
	ratio := float64(busy) / float64(wall.Microseconds()) / (float64(millicores) / 1000)
	if ratio > 1 {
		ratio = 1
	}

	return ratio, true
}

// observedPool is the part of a round the history keeps from the scrape.
type observedPool struct {
	active     int
	queue      int64
	configured int
}

// historySampleOf flattens a round into what the history keeps of it.
func historySampleOf(result plan.Result, at time.Time, hostBusy float64, hostBusyKnown bool) HistorySample {
	cpu := make(map[string]plan.PoolCPU, len(result.CPU))
	for _, c := range result.CPU {
		cpu[c.Name] = c
	}
	observed := make(map[string]observedPool, len(result.Views))
	for _, v := range result.Views {
		observed[v.Name] = observedPool{v.ActiveNow, v.QueueDepth, v.CurrentMaxChildren}
	}

	sample := HistorySample{At: at, HostBusyRatio: hostBusy, HostBusyKnown: hostBusyKnown}
	for _, p := range result.Plan.Pools {
		o := observed[p.Name]
		c := cpu[p.Name]
		sample.Pools = append(sample.Pools, PoolSample{
			Pool:          p.Name,
			Active:        o.active,
			Queue:         o.queue,
			Configured:    o.configured,
			Recommended:   p.MaxChildren,
			DemandUnmet:   p.DemandUnmet,
			Unknown:       p.Unknown,
			WorkerBytes:   p.WorkerBytes,
			MemoryCeiling: p.MemoryWant,
			CPURatioP50:   c.P50,
			CPUReadings:   c.Samples,
			CPUFill:       c.FillWorkers,
			CPUCeiling:    c.Ceiling,
			CPULimited:    c.Limit == "cpu",
			CPUBound:      p.CPUBound,
		})
	}

	return sample
}
