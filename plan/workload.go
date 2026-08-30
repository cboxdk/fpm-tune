package plan

import (
	"math"
	"strings"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
)

// A workload is the shape of what a pool's workers do — specifically, whether
// they spawn other processes, and how many at once.
//
// It exists to solve the cold-start problem. A pool's own worker memory is
// measured within a few scrapes, but whether a request shells out to an ffmpeg
// is invisible until one happens to run while a scrape lands — and on a
// subprocess-heavy host, under-provisioning until then is exactly the window an
// OOM arrives in. Declaring the workload lets the plan reserve for children from
// the first run, before anything has been observed; measurement then refines it.
//
// The class encodes a concurrency assumption that measurement alone cannot
// recover from per-worker RSS: whether every worker might be transcoding at once
// (subprocess-heavy), or only a few now and then (bursty), or none (web). A
// cgroup high-water would eventually reveal it, but only after the host has
// already reached that peak once.
type Workload struct {
	Name string

	// ChildBytes is the assumed peak resident memory of what ONE worker spawns.
	// Zero for a pool that shells out to nothing, which is most web and API
	// pools.
	ChildBytes int64

	// ConcurrentFraction is how many of a pool's workers are assumed to be
	// running a child at once, as a fraction of its worker count: all for media,
	// a quarter for bursty, none for simple.
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

// WorkloadByName resolves a marker to its class, falling back to the supplied
// default for an empty or unrecognised name. ok reports whether the name was
// recognised, so a caller can warn about a typo rather than silently treating
// "medai" as the default.
func WorkloadByName(name string, fallback Workload) (w Workload, ok bool) {
	if name == "" {
		return fallback, true
	}
	if w, ok := workloadsByName[strings.ToLower(strings.TrimSpace(name))]; ok {
		return w, true
	}

	return fallback, false
}

// concurrentChildren is how many children a pool of workerCount workers is
// assumed to run at once under this workload.
//
// At least one whenever the workload spawns anything and the pool has workers,
// so a media pool with three workers is not rounded down to reserving nothing —
// the floor is what makes the declaration mean something on a small pool.
func (w Workload) concurrentChildren(workerCount int) int {
	if w.ChildBytes <= 0 || workerCount <= 0 {
		return 0
	}

	n := int(math.Ceil(float64(workerCount) * w.ConcurrentFraction))
	if n < 1 {
		n = 1
	}
	if n > workerCount {
		n = workerCount
	}

	return n
}

// childReserveFor is the memory to keep for the processes pool workers spawn,
// held back from the budget before the workers themselves are sized.
//
// It is ADDITIVE to the system reserve, and so can only lower what workers are
// given, never raise it: accounting for children makes this tool run fewer
// workers, never overcommit a host it was not overcommitting before. That is the
// property that makes it safe to turn on.
//
// The larger of two estimates wins, because each is blind to what the other
// sees:
//
//   - The per-pool estimate takes each pool's per-worker child memory — the
//     larger of the workload's up-front guess and the measured subtree — times a
//     concurrency the workload class declares (all workers for subprocess-heavy,
//     a few for bursty). It is all there is on a host with no cgroup, and it is
//     what lets a subprocess-heavy pool reserve from its first run.
//
//   - The cgroup high-water is the ground truth where there is a cgroup: it
//     counts a child that lived and died between two scrapes, and it is the
//     number the OOM killer enforces against. But it is one figure for the whole
//     master, and absent on a bare VM.
func childReserveFor(views []observe.PoolView, st *state.State, pools []allocate.Pool, def Workload, usage budget.CgroupUsage, hasCgroup bool) (int64, string) {
	ownBytesOf := map[string]int64{}
	for _, p := range pools {
		ownBytesOf[p.Name] = p.WorkerBytes
	}

	var perPool, workerOwnTotal int64
	for _, v := range views {
		workers := v.CurrentMaxChildren
		if workers <= 0 {
			workers = v.ObservedPeak
		}
		if workers <= 0 {
			// No countable workers means no countable children, and no basis for
			// the cgroup subtraction either.
			continue
		}

		workerOwnTotal += ownBytesOf[v.Name] * int64(workers)

		w, _ := WorkloadByName(v.Workload, def)

		var measuredChild int64
		if st != nil {
			if ps := st.Lookup(v.Target.ConfigPath, v.Name); ps != nil &&
				ps.SubtreeHighWaterBytes > ps.HighWaterBytes {
				measuredChild = ps.SubtreeHighWaterBytes - ps.HighWaterBytes
			}
		}

		perWorker := w.ChildBytes
		if measuredChild > perWorker {
			perWorker = measuredChild
		}
		if perWorker <= 0 {
			continue
		}

		// The class's concurrency, but never fewer than one child when there is
		// any child memory at all — so a pool marked "web" that is nonetheless
		// observed spawning something is still reserved for, the measurement
		// beating the declaration.
		n := w.concurrentChildren(workers)
		if n < 1 {
			n = 1
		}
		if n > workers {
			n = workers
		}

		perPool += perWorker * int64(n)
	}

	var cgroupBased int64
	if hasCgroup && usage.PeakBytes > workerOwnTotal {
		cgroupBased = usage.PeakBytes - workerOwnTotal
	}

	if cgroupBased > perPool {
		return cgroupBased, "the cgroup used this much beyond its workers at its peak"
	}
	if perPool > 0 {
		return perPool, "declared and measured per-pool child memory"
	}

	return 0, ""
}
