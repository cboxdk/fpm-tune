package allocate

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"
)

// TestAPlanNeverCommitsMoreThanTheBudget.
//
// The one invariant this package exists to hold, checked the way it was broken:
// a randomised sweep rather than a hand-picked case. A review generated 277,684
// valid inputs and 3.5% of them produced a plan over the budget — every one of
// them with a pool whose configuration could not be read.
//
// The mechanism is worth stating, because the guard that was missing looks like
// the guard that was there. Compute checks that one worker per pool fits. That
// does not imply the last-resort branch fits, because an unwritable pool is held
// at its FLOOR, not at one worker: with the floors of the unwritable pools
// taking nearly the whole budget, scaling the rest and rounding each up to one
// worker leaves a total the trim cannot bring down, and it was returned anyway.
//
// The consequence is not an abstract one. The plan reports a NEGATIVE FreeBytes,
// which reads as a fit everywhere downstream, and the memory it commits past the
// budget comes out of the reserve that keeps the OS and nginx alive.
func TestAPlanNeverCommitsMoreThanTheBudget(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	opts := Options{}.Defaults()

	for round := 0; round < 20000; round++ {
		n := 1 + rng.Intn(12)
		pools := make([]Pool, n)
		for i := range pools {
			pools[i] = Pool{
				Name:               fmt.Sprintf("p%d", i),
				WorkerBytes:        int64(1+rng.Intn(200)) * mb,
				CurrentMaxChildren: rng.Intn(80),
				Floor:              rng.Intn(80),
				Measured:           rng.Intn(2) == 0,
				Reducible:          rng.Intn(2) == 0,
				// The trigger. A pool whose php-fpm -tt could not be read is the
				// ordinary case on a first run, not a corner.
				Unknown: rng.Intn(4) == 0,
				// The demand pass hands a QUEUEING pool its whole shortfall
				// rather than a proportion of it, so the budget invariant has to
				// be checked against that branch too — it is the one that spends
				// without dividing.
				ObservedPeak:   rng.Intn(60),
				HitMaxChildren: rng.Intn(3) == 0,
				QueueDepth:     int64(rng.Intn(50)),
			}
		}

		allocatable := int64(64+rng.Intn(8192)) * mb
		b := Budget{TotalBytes: allocatable + 512*mb, ReserveBytes: 512 * mb, CPUs: 1 + rng.Intn(16)}
		plan, err := Compute(b, pools, opts)
		if err != nil {
			// Refusing is a legitimate answer, and the only other one.
			if !errors.Is(err, ErrCannotFit) {
				t.Fatalf("round %d: %v", round, err)
			}

			continue
		}

		if plan.AllocatedBytes > allocatable {
			t.Fatalf("round %d: the plan commits %s against %s available (free = %s)\n%+v",
				round, humanBytes(plan.AllocatedBytes), humanBytes(allocatable),
				humanBytes(plan.FreeBytes), pools)
		}
		if plan.FreeBytes < 0 {
			t.Fatalf("round %d: FreeBytes = %s", round, humanBytes(plan.FreeBytes))
		}
		for _, pp := range plan.Pools {
			if pp.MaxChildren < 1 {
				t.Fatalf("round %d: pool %q was left with %d workers, which is not a "+
					"configuration php-fpm will accept", round, pp.Name, pp.MaxChildren)
			}
		}
	}
}

// TestTheUnwritableFloorsGuardCountsTheRestToo is the specific shape, kept
// alongside the sweep because a sweep tells you THAT something is wrong and this
// tells you what.
//
// One pool at a floor of 63 workers that cannot be written, forty ordinary sites
// beside it. The refusal has to account for a worker each for the forty; only
// counting the unwritable pool's own need let the plan through at 9951MiB
// against a 6144MiB budget.
func TestTheUnwritableFloorsGuardCountsTheRestToo(t *testing.T) {
	opts := Options{}.Defaults()

	pools := []Pool{{
		Name: "legacy", WorkerBytes: 97 * mb, Floor: 63,
		CurrentMaxChildren: 63, Unknown: true,
	}}
	for i := 0; i < 40; i++ {
		pools = append(pools, Pool{
			Name:               fmt.Sprintf("site%02d", i),
			WorkerBytes:        96 * mb,
			Floor:              4,
			CurrentMaxChildren: 4,
			Measured:           true,
			Reducible:          true,
		})
	}

	plan, err := Compute(Budget{TotalBytes: 6144*mb + 512*mb, ReserveBytes: 512 * mb, CPUs: 16}, pools, opts)
	if err == nil {
		t.Fatalf("a plan was returned committing %s against 6144MiB: nothing can be "+
			"written for the pool holding the memory, so no arrangement of the rest helps",
			humanBytes(plan.AllocatedBytes))
	}
	if !errors.Is(err, ErrCannotFit) {
		t.Fatalf("err = %v, want ErrCannotFit", err)
	}
}

// TestAWorkerCountThatCannotBeRealIsRefused: granted × WorkerBytes is int64, and
// a count in the millions wraps it — to exactly zero in the case demonstrated,
// which reads as a free pool and passes every budget check downstream. The bound
// lives in the package that does the multiplying, because this package is meant
// to be embedded and an embedder does not inherit the observation boundary's
// validation.
func TestAWorkerCountThatCannotBeRealIsRefused(t *testing.T) {
	_, err := Compute(Budget{TotalBytes: 1024 * mb, CPUs: 4}, []Pool{
		{Name: "wrapped", WorkerBytes: 1 << 40, Floor: 1 << 24, CurrentMaxChildren: 1 << 24},
	}, Options{}.Defaults())

	if err == nil {
		t.Fatal("a floor of 16.7 million workers was accepted; the product wraps to zero " +
			"and the plan reports the pool as costing nothing")
	}
}

// TestAPerWorkerCostThatCannotBeRealIsRefused: bounding the worker COUNT is not
// enough, because the product wraps if EITHER factor is absurd. A per-worker
// cost near MaxInt64 makes floor × cost negative with a floor of two — and a
// negative total passes a fit precondition and a budget assertion alike, both of
// which are written to catch a number that is too large.
func TestAPerWorkerCostThatCannotBeRealIsRefused(t *testing.T) {
	_, err := Compute(Budget{TotalBytes: 1024 * mb, CPUs: 4}, []Pool{
		{Name: "wrapped", WorkerBytes: math.MaxInt64/2 + 1, Floor: 2, CurrentMaxChildren: 2},
	}, Options{}.Defaults())

	if err == nil {
		t.Fatal("a per-worker cost of 4 exabytes was accepted; two workers of it wrap to " +
			"a negative total, which reads as a pool that costs less than nothing")
	}
}

// TestARefusalNamesThePoolThatMadeItImpossible.
//
// When one worker each does not fit, no arrangement helps and refusing is
// right. But the total alone does not say WHICH pool grew out of proportion,
// and on a host with many tenants that is the only actionable thing in the
// message — the operator is otherwise left to work it out from a table, at
// exactly the moment they are least able to.
//
// Measured: a tenant whose workers reach 4GiB each stops the tool planning the
// whole host. That is honest — nothing fits — and it is one pool's doing.
func TestARefusalNamesThePoolThatMadeItImpossible(t *testing.T) {
	opts := Options{}.Defaults()

	pools := []Pool{
		{Name: "quiet-site", WorkerBytes: 48 * mb, Floor: 12, CurrentMaxChildren: 12},
		{Name: "runaway", WorkerBytes: 4000 * mb, Floor: 12, CurrentMaxChildren: 12},
		{Name: "another-site", WorkerBytes: 48 * mb, Floor: 12, CurrentMaxChildren: 12},
	}

	_, err := Compute(Budget{TotalBytes: 4096 * mb, ReserveBytes: 512 * mb, CPUs: 8},
		pools, opts)
	if err == nil {
		t.Fatal("a host where one worker each does not fit produced a plan")
	}
	if !strings.Contains(err.Error(), "runaway") {
		t.Errorf("the refusal does not name the pool that caused it:\n%v", err)
	}
	if !strings.Contains(err.Error(), "3.9GiB") && !strings.Contains(err.Error(), "4.0GiB") {
		t.Errorf("the refusal does not say what that pool costs:\n%v", err)
	}
}
