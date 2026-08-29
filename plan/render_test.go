package plan

import (
	"strings"
	"testing"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
)

// TestAPoolThatWillNotBeWrittenDoesNotShowAPlan.
//
// The PLAN column is a promise: those are the numbers that will be in the pool
// directory after `apply`. A pool whose current configuration could not be read
// is accounted for in the budget and deliberately not written — setting a
// ceiling means replacing a known one — so printing a number for it in that
// column sends an operator away expecting a change that will not arrive.
func TestAPoolThatWillNotBeWrittenDoesNotShowAPlan(t *testing.T) {
	var b strings.Builder
	err := Result{
		Budget: budget.Limits{MemoryBytes: 4 * gb, Source: budget.SourceMemInfo},
		Plan: allocate.Plan{
			TotalBytes: 4 * gb,
			Pools: []allocate.PoolPlan{
				{Name: "readable", MaxChildren: 12, Current: 8, Bytes: 512 * mb},
				{Name: "unreadable", MaxChildren: 20, Current: 20, Bytes: 900 * mb, Unknown: true},
			},
		},
		Unreachable: []string{"unreadable"},
	}.Render(&b)
	if err != nil {
		t.Fatal(err)
	}

	for _, line := range strings.Split(b.String(), "\n") {
		if !strings.HasPrefix(line, "unreadable") {
			continue
		}
		if strings.Contains(line, "20") && !strings.Contains(line, "—") {
			t.Errorf("a pool that will not be written shows a plan number:\n%s", line)
		}

		return
	}
	t.Fatalf("the pool is missing from the table entirely:\n%s", b.String())
}

// TestTheWorstCaseIsReportedButNotSizedTo.
//
// A pool with a rare 700MiB export endpoint and a 90MiB typical cost is sized at
// 90MiB, deliberately: sizing to the high-water mark pins every host to its
// worst minute. The high-water mark is kept anyway — and nothing anywhere
// consulted it, so a plan could authorise ninety workers whose worst-case
// footprint is ten times the machine, with no line of output saying so.
//
// This is the second use of a number the tool already has. It is advisory, and
// it must not move the sizing.
func TestTheWorstCaseIsReportedButNotSizedTo(t *testing.T) {
	st := state.New()
	st.Pools["shop"] = &state.PoolState{
		Pool: "shop", TypicalPeakBytes: 90 * mb, HighWaterBytes: 700 * mb,
	}

	res, err := Build(Input{
		Limits: budget.Limits{MemoryBytes: 4 * gb, CPUs: 8, Source: budget.SourceMemInfo},
		State:  st,
		Views: []observe.PoolView{{
			Name: "shop", ProcessManager: "dynamic",
			CurrentMaxChildren: 20, MaxChildrenKnown: true, ObservedPeak: 18,
			Workers: []state.WorkerSample{{RSSBytes: 90 * mb, Requests: 400}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The sizing is unmoved.
	if got := res.Plan.Pools[0].WorkerBytes; got > 100*mb {
		t.Errorf("the pool is sized at %dMiB a worker; the high-water mark has leaked "+
			"into the estimate and this host is now pinned to its worst minute", got/mb)
	}

	var b strings.Builder
	if err := res.Render(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "largest worker ever seen") {
		t.Errorf("a plan whose worst case is %s against a %s budget says nothing about "+
			"it:\n%s", budget.HumanBytes(res.WorstCaseBytes),
			budget.HumanBytes(res.Plan.TotalBytes-res.Reserve), b.String())
	}
}
