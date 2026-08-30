package plan

import (
	"testing"

	"github.com/cboxdk/fpm-tune/budget"
)

// TestGoodNeighborReserveAddsWhatNeighborsUse: on a shared host the reserve is the
// percentage headroom PLUS the memory other services and the OS are using, so the
// host as a whole stays at the target — not just php-fpm's own share of it.
func TestGoodNeighborReserveAddsWhatNeighborsUse(t *testing.T) {
	total := int64(8) << 30
	neighbors := int64(3) << 30

	res, err := Build(Input{Limits: budget.Limits{
		MemoryBytes: total, NeighborBytes: neighbors, Source: budget.SourceMemInfo, CPUs: 4,
	}})
	if err != nil {
		t.Fatal(err)
	}

	fractional := int64(float64(total) * DefaultProfile.ReserveFraction) // 15%
	if res.Reserve != fractional+neighbors {
		t.Errorf("reserve = %s, want the 15%% headroom (%s) plus what neighbours use (%s)",
			budget.HumanBytes(res.Reserve), budget.HumanBytes(fractional), budget.HumanBytes(neighbors))
	}
	// And what the allocator is given to divide is the rest.
	if avail := res.Plan.TotalBytes - res.Reserve; avail != total-fractional-neighbors {
		t.Errorf("available to workers = %s, want the host minus headroom and neighbours",
			budget.HumanBytes(avail))
	}
}

// TestExplicitReserveSkipsNeighbors: --reserve is the operator's own total, so the
// good-neighbour reserve is not added on top — or the flag would not mean what it
// says.
func TestExplicitReserveSkipsNeighbors(t *testing.T) {
	res, err := Build(Input{
		Limits:       budget.Limits{MemoryBytes: 8 << 30, NeighborBytes: 3 << 30, Source: budget.SourceMemInfo, CPUs: 4},
		ReserveBytes: 1 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reserve != 1<<30 {
		t.Errorf("reserve = %s, want the explicit 1GiB, not 1GiB + neighbours", budget.HumanBytes(res.Reserve))
	}
}

// TestReserveFractionOverrideRetargetsUtilisation: --reserve 20% keeps a fifth back
// (80% utilisation) without a fixed byte amount.
func TestReserveFractionOverrideRetargetsUtilisation(t *testing.T) {
	total := int64(10) << 30

	res, err := Build(Input{Limits: budget.Limits{MemoryBytes: total, CPUs: 4}, ReserveFraction: 0.20})
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(float64(total) * 0.20); res.Reserve != want {
		t.Errorf("reserve = %s, want 20%% of the host (%s)", budget.HumanBytes(res.Reserve), budget.HumanBytes(want))
	}
}
