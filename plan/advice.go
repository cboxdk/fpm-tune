package plan

import (
	"fmt"

	"github.com/cboxdk/fpm-tune/budget"
)

// ModeAdvice is a suggestion that a pool's process-manager mode (pm =
// static|dynamic|ondemand) may fit its workload better as something else.
//
// It is advisory and nothing more. fpm-tune sizes pm.max_children inside
// whatever mode the operator chose; it never writes pm itself, because the right
// mode depends on latency targets and memory preferences this tool does not
// measure. The suggestion is here to start that conversation, not to end it.
type ModeAdvice struct {
	Pool string
	From string // the mode the pool is in now
	To   string // the mode its measured shape points toward
	Why  string // one line, in plain terms, safe to print after the plan
}

// staticReclaimFloor is how much idle resident memory a static pool must be
// holding before it is worth suggesting anything. Below it, the nag is not worth
// the operator's attention: a static pool that keeps two spare workers warm for
// latency is a reasonable choice, not a mistake.
const staticReclaimFloor = 256 << 20 // 256 MiB

// adviceInput is the measured shape of one pool, pulled together from the view,
// the learned state, and the plan so the decision below is a pure function of
// numbers and can be table-tested exhaustively.
type adviceInput struct {
	Pool        string
	Mode        string // static | dynamic | ondemand
	Current     int    // configured pm.max_children
	Peak        int    // busiest concurrency we know of (FPM since-start, or our remembered peak — whichever is larger)
	MaxKnown    bool   // the configured ceiling was actually read, not guessed
	Measured    bool   // the per-worker cost is measured, not a bootstrap estimate
	Queue       int64  // requests waiting right now
	DemandUnmet bool   // the pool wanted more workers than it was given this round
	WorkerBytes int64  // what one worker costs (own + child)
}

// adviseMode returns a mode suggestion for one pool, or false when its shape does
// not clearly point anywhere. Two signals, both conservative:
//
//   - A static pool whose busiest moment leaves a lot of workers idle is paying
//     for resident memory it never uses — static runs every allowed worker at all
//     times. dynamic or ondemand would hand that memory back between requests.
//   - An ondemand pool that is queueing is paying cold-start latency on every
//     burst: ondemand spawns each worker on demand and lets idle ones die. A warm
//     floor (dynamic's start_servers/min_spare) absorbs the burst instead.
//
// It deliberately does NOT push a busy dynamic pool toward static: telling the two
// apart needs a sustained-saturation signal this tool does not keep, and dynamic
// is a safe default, so a guess there would be noise.
func adviseMode(in adviceInput) (ModeAdvice, bool) {
	switch in.Mode {
	case "static":
		// Needs a real ceiling and a real cost, or the arithmetic below is built
		// on a guess; and a peak of zero means we have never seen it work.
		if !in.MaxKnown || !in.Measured || in.Peak < 1 || in.WorkerBytes <= 0 {
			return ModeAdvice{}, false
		}
		idle := in.Current - in.Peak
		if idle < 2 {
			return ModeAdvice{}, false
		}
		reclaim := int64(idle) * in.WorkerBytes
		if reclaim < staticReclaimFloor {
			return ModeAdvice{}, false
		}

		return ModeAdvice{
			Pool: in.Pool,
			From: "static",
			To:   "dynamic",
			Why: fmt.Sprintf(
				"static keeps all %d workers resident; the busiest moment used %d, so ~%d sit idle — about %s you could hand back between requests. Keep static only if you want them always warm for latency.",
				in.Current, in.Peak, idle, budget.HumanBytes(reclaim)),
		}, true

	case "ondemand":
		// Requests waiting, or a pool the allocator could not fully satisfy: on
		// ondemand either one was served by cold-starting a worker.
		if in.Queue <= 0 && !in.DemandUnmet {
			return ModeAdvice{}, false
		}
		why := "this ondemand pool hit demand it could not serve at once; ondemand spawns each worker on demand and lets idle ones die, so bursts wait on a cold start. dynamic keeps a warm floor so the next burst finds workers already up."
		if in.Queue > 0 {
			why = fmt.Sprintf(
				"requests are queuing here (%d waiting); ondemand spawns each worker on demand and lets idle ones die, so a burst waits on cold starts. dynamic keeps a warm floor (start_servers/min_spare) so it doesn't.",
				in.Queue)
		}

		return ModeAdvice{Pool: in.Pool, From: "ondemand", To: "dynamic", Why: why}, true
	}

	return ModeAdvice{}, false
}
