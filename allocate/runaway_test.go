package allocate

import (
	"testing"
)

// TestSaturationGrowthConvergesRatherThanCompounding.
//
// The other half of the 614-worker runaway, and nothing covered it. The existing
// test carrying that story sets HitMaxChildren false, so the growth branch it is
// about never runs — the pool lands at 12 against an assertion of "more than 20",
// which passes whether the bound exists or not. Deleting the bound left allocate
// and plan green.
//
// The branch exists because a saturated pool's observed concurrency is clamped
// by the very ceiling being raised, so demand cannot be read from it. Its base
// is therefore the pool's OWN previous size — which is positive feedback: raise
// the ceiling, the pool fills it, raise it again. On a host with budget to spare
// nothing stops it until the memory runs out.
//
// The bound is one growth step past the headroom the evidence already justifies,
// so the sequence converges instead of compounding. Convergence is the property,
// so the test asserts a value rather than an upper limit: any bound at all
// satisfies "less than some number", including one an order of magnitude too
// generous.
func TestSaturationGrowthConvergesRatherThanCompounding(t *testing.T) {
	opts := Options{}.Defaults()

	// Plenty of budget, so nothing but the bound can stop the growth.
	b := Budget{TotalBytes: 128 * 1024 * mb, CPUs: 512}

	current := 10
	for round := 0; round < 30; round++ {
		plan, err := Compute(b, []Pool{{
			Name: "saturated", WorkerBytes: 64 * mb,
			CurrentMaxChildren: current,
			// Held at 10: the pool cannot be SEEN to need more, because the
			// ceiling is what is stopping it. That is the whole difficulty.
			ObservedPeak:   10,
			HitMaxChildren: true,
			Measured:       true,
			Reducible:      true,
		}}, opts)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		current = plan.Pools[0].MaxChildren
	}

	// 10 workers of evidence, a headroom factor and one growth step: the
	// sequence stops here and stays.
	want := int(float64(10) * opts.HeadroomFactor * opts.GrowthFactor)
	if current != want {
		t.Errorf("a saturated pool with 10 workers of evidence converged to %d, want %d; "+
			"without the bound the ceiling raises itself off its own previous size and "+
			"the only thing that stops it is the host running out of memory", current, want)
	}
}

// TestGrowthStillOutrunsTheHeadroomFactorOnce: the bound must not make the
// growth branch pointless. A saturated pool has to end up with MORE than the
// headroom factor alone would give it, or there was no reason to treat
// saturation differently in the first place.
func TestGrowthStillOutrunsTheHeadroomFactorOnce(t *testing.T) {
	opts := Options{}.Defaults()
	b := Budget{TotalBytes: 128 * 1024 * mb, CPUs: 512}

	pool := Pool{
		Name: "saturated", WorkerBytes: 64 * mb,
		CurrentMaxChildren: 10, ObservedPeak: 10,
		HitMaxChildren: true, Measured: true, Reducible: true,
	}

	saturated, err := Compute(b, []Pool{pool}, opts)
	if err != nil {
		t.Fatal(err)
	}

	pool.HitMaxChildren = false
	calm, err := Compute(b, []Pool{pool}, opts)
	if err != nil {
		t.Fatal(err)
	}

	if saturated.Pools[0].MaxChildren <= calm.Pools[0].MaxChildren {
		t.Errorf("a pool queueing at its ceiling was given %d workers, the same pool not "+
			"queueing %d: the bound has swallowed the branch it bounds",
			saturated.Pools[0].MaxChildren, calm.Pools[0].MaxChildren)
	}
}
