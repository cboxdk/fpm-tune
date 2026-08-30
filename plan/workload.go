package plan

import (
	"strings"

	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
)

// measuredChildPerWorker is the per-worker child memory a pool has actually been
// observed carrying, or zero if none has been. It reads the pool's own record,
// keyed by the master it belongs to so a name shared by two masters cannot cross
// the wires.
func measuredChildPerWorker(st *state.State, view observe.PoolView) int64 {
	if st == nil {
		return 0
	}
	if ps := st.Lookup(view.Target.ConfigPath, view.Name); ps != nil {
		return ps.ChildPerWorkerHighWaterBytes
	}

	return 0
}

// A workload is the shape of what a pool's workers do — specifically, whether
// they spawn other processes, and how many at once.
//
// It exists to solve the cold-start problem. A pool's own worker memory is
// measured within a few scrapes, but whether a request shells out to an ffmpeg
// is invisible until one happens to run while a scrape lands — and on a
// subprocess-heavy host, under-provisioning until then is exactly the window an
// OOM arrives in. Declaring the workload lets the plan account for children from
// the first run, before anything has been observed; measurement then refines it.
//
// The class encodes a concurrency assumption that measurement is slow to
// recover: whether every worker might be transcoding at once (subprocess-heavy),
// or only a few now and then (bursty), or none (web).
type Workload struct {
	Name string

	// ChildBytes is the assumed peak resident memory of what ONE worker spawns.
	// Zero for a pool that shells out to nothing, which is most web and API
	// pools.
	ChildBytes int64

	// ConcurrentFraction is how many of a pool's workers are assumed to be
	// running a child at once, as a fraction: all for subprocess-heavy, a quarter
	// for bursty, none for web. ChildBytes times this fraction is the per-worker
	// child cost the plan adds before it has measured anything — amortised, so a
	// bursty pool is not sized as if every worker had a child.
	ConcurrentFraction float64
}

// The named workloads. The numbers are deliberately round bootstrap guesses,
// not measurements — they are a floor a real observation is expected to replace,
// and their only job is to keep a freshly started subprocess-heavy pool from
// being sized as if its ffmpeg were free.
var (
	// WorkloadWeb is a pool whose workers spawn nothing: a plain web or API pool
	// serving requests in PHP. Its whole cost is its own worker memory, which is
	// what this tool measured before any of this existed. The default.
	WorkloadWeb = Workload{Name: "web", ChildBytes: 0, ConcurrentFraction: 0}

	// WorkloadBursty spawns a child now and then — a pool that occasionally
	// generates a PDF or resizes an upload, not on every request. A few workers
	// are assumed busy with a child at once.
	WorkloadBursty = Workload{Name: "bursty", ChildBytes: 256 << 20, ConcurrentFraction: 0.25}

	// WorkloadSubprocessHeavy shells out on most requests — transcoding, image
	// processing, PDF rendering — so every worker is assumed to have a child.
	// This is the class that under-provisions catastrophically without an
	// up-front reservation.
	WorkloadSubprocessHeavy = Workload{Name: "subprocess-heavy", ChildBytes: 512 << 20, ConcurrentFraction: 1.0}
)

// workloadsByName resolves the canonical names and their aliases. The canonical
// set is web / bursty / subprocess-heavy; the aliases are the words an operator
// might reach for first, kept so `--workload media` finds the right class rather
// than falling through to the default.
var workloadsByName = map[string]Workload{
	"web":              WorkloadWeb,
	"api":              WorkloadWeb,
	"simple":           WorkloadWeb,
	"bursty":           WorkloadBursty,
	"subprocess-heavy": WorkloadSubprocessHeavy,
	"subprocess":       WorkloadSubprocessHeavy,
	"media":            WorkloadSubprocessHeavy,
	"children":         WorkloadSubprocessHeavy,
}

// KnownWorkloads is the canonical set, for help text and warnings.
const KnownWorkloads = "web, bursty, subprocess-heavy"

// WorkloadByName resolves a marker to its class, falling back to the supplied
// default for an empty or unrecognised name. ok reports whether the name was
// recognised, so a caller can warn about a typo rather than silently treating
// "medai" as the default — which, for a per-pool marker that reserves memory,
// is the difference between a warning and a silent OOM.
func WorkloadByName(name string, fallback Workload) (w Workload, ok bool) {
	if name == "" {
		return fallback, true
	}
	if w, ok := workloadsByName[strings.ToLower(strings.TrimSpace(name))]; ok {
		return w, true
	}

	return fallback, false
}

// childCostPerWorker is the memory to add to one worker's own cost to account
// for what it spawns.
//
// It is a PER-WORKER cost, not a host-wide reserve, and that is the whole point
// of the redesign: the allocator already sizes each pool by dividing the budget
// by a bounded per-worker cost, so folding children into that cost means the
// number of workers a pool is given scales with the true cost of a worker, the
// bounds that stop a per-worker cost running away apply to children too, and
// accounting for children can only ever mean "fewer workers", never "no plan at
// all". A host-wide reserve had none of those properties.
//
// The larger of the declared floor and the measured cost wins. The floor is the
// workload's per-worker child guess (its child size amortised over how many
// workers run one at once); the measured cost is what the pool's workers were
// actually observed carrying, already per-worker and already reflecting real
// concurrency. Measurement takes over once it exceeds the guess.
func childCostPerWorker(w Workload, measuredPerWorker int64) int64 {
	bootstrap := int64(float64(w.ChildBytes) * w.ConcurrentFraction)
	if measuredPerWorker > bootstrap {
		return measuredPerWorker
	}

	return bootstrap
}
