package state

import (
	"fmt"
	"testing"
	"time"
)

// TestThePercentilesDescribeWhatWasSeen.
//
// The sizing number is one number on purpose, and one number cannot answer the
// question an operator asks when deciding by hand: how bad does this pool get?
// A pool whose median worker is 60MiB and whose p99 is 400MiB is a different
// pool from one that sits flat at 90MiB, and the EWMA hides the first while the
// high-water mark is only the second.
func TestThePercentilesDescribeWhatWasSeen(t *testing.T) {
	ps := &PoolState{}

	// Ninety workers at 60MiB, nine at 200MiB, one at 400MiB: a pool with a
	// long tail, which is what an export endpoint or a slow query looks like.
	for i := 0; i < 90; i++ {
		ps.observeRSS(60 * mb)
	}
	for i := 0; i < 9; i++ {
		ps.observeRSS(200 * mb)
	}
	ps.observeRSS(400 * mb)

	for _, tc := range []struct{ p, want float64 }{
		{0.50, 60},
		{0.95, 200},
		{0.99, 200},
		{1.00, 400},
	} {
		got := float64(ps.Percentile(tc.p)) / float64(mb)
		// Buckets are about 19% wide, which is finer than any decision made
		// about worker memory.
		if got < tc.want*0.85 || got > tc.want*1.05 {
			t.Errorf("p%.0f = %.0fMiB, want about %.0fMiB", tc.p*100, got, tc.want)
		}
	}

	if ps.Percentile(0.5) == 0 {
		t.Error("a pool with a hundred readings reports no median")
	}
}

// TestThePercentilesAreEmptyBeforeAnythingIsSeen: zero has to mean "nothing
// recorded", not "zero bytes", or a fresh pool reads as free.
func TestThePercentilesAreEmptyBeforeAnythingIsSeen(t *testing.T) {
	var ps PoolState
	if got := ps.Percentile(0.95); got != 0 {
		t.Errorf("p95 = %d on a pool nothing has been seen from", got)
	}
}

// TestTheDistributionForgetsTheApplicationThatWasReplaced.
//
// An all-time histogram of a pool redeployed six months ago describes an
// application that no longer exists. Halving every bucket once the count grows
// keeps the shape while letting the past fade, costs one pass over 64 integers,
// and needs no timestamps — which matters, because the alternative is another
// thing that goes wrong when a clock steps.
func TestTheDistributionForgetsTheApplicationThatWasReplaced(t *testing.T) {
	ps := &PoolState{}

	for i := 0; i < 20000; i++ {
		ps.observeRSS(50 * mb)
	}
	if got := ps.Percentile(0.5); got > 70*mb {
		t.Fatalf("setup: median %dMiB, want about 50", got/mb)
	}

	// A deploy. The new application costs four times as much.
	for i := 0; i < 20000; i++ {
		ps.observeRSS(200 * mb)
	}

	if got := ps.Percentile(0.5); got < 150*mb {
		t.Errorf("median is %dMiB after twenty thousand readings of a 200MiB "+
			"application; the histogram is still describing the one it replaced",
			got/mb)
	}
	if ps.RSSSamples > decayAfter*2 {
		t.Errorf("the histogram holds %d samples; it is meant to stay bounded",
			ps.RSSSamples)
	}
}

// TestTheDistributionSurvivesARestart, because a percentile that resets on every
// restart describes the last few minutes and calls it a distribution.
func TestTheDistributionSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"

	before := New()
	at := time.Now().Add(-time.Hour)
	var accepted int64
	for i := 0; i < 60; i++ {
		accepted += 6000
		before.Learn(Observation{
			Pool: "shop", At: at.Add(time.Duration(i) * time.Minute),
			ActiveNow: 4, Accepted: accepted,
			Workers: []WorkerSample{
				{RSSBytes: 90 * mb, Requests: 400}, {RSSBytes: 300 * mb, Requests: 400},
			},
		}, Options{})
	}
	want := before.Pools["shop"].Percentile(0.95)
	if want == 0 {
		t.Fatal("setup: nothing was recorded")
	}
	if err := before.Save(path); err != nil {
		t.Fatal(err)
	}

	after, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Pools["shop"].Percentile(0.95); got != want {
		t.Errorf("p95 = %s after a restart, want %s",
			fmt.Sprintf("%dMiB", got/mb), fmt.Sprintf("%dMiB", want/mb))
	}
}
