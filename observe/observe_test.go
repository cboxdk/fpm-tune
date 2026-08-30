package observe

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cboxdk/phpfpm"
)

// TestAFailedScrapeStillReportsWhatThePoolIsConfiguredFor.
//
// This is the plumbing, and it is where the data was actually lost. The
// allocator's reservation logic for an unreachable pool was correct all along —
// it was being fed zero, because the view built from a scrape error carried
// nothing but the name and the error.
//
// The consequence is the outage the tool exists to prevent: a site configured
// for twenty workers restarts, its socket refuses for a few seconds, nothing is
// reserved for it, its memory goes to its neighbours, they are reloaded with
// larger ceilings, and the host is committed past its budget the moment the pool
// comes back and forks.
//
// The plan-level tests supply CurrentMaxChildren by hand, so they prove the
// allocator and not the path that feeds it. This one goes through Sample.
func TestAFailedScrapeStillReportsWhatThePoolIsConfiguredFor(t *testing.T) {
	// A socket nothing is listening on: the scrape fails the way a restarting
	// pool's does.
	targets := []phpfpm.Target{{
		Name:           "restarting",
		Socket:         "127.0.0.1:1",
		StatusPath:     "/status",
		MaxChildren:    20,
		ProcessManager: "dynamic",
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	views := Sample(ctx, targets, nil)

	if len(views) != 1 {
		t.Fatalf("got %d views, want 1: a pool that cannot be reached must not "+
			"vanish, or the allocator hands its memory away", len(views))
	}

	view := views[0]
	if view.Err == nil {
		t.Fatal("the scrape was expected to fail; something is listening on port 1")
	}
	if view.Name != "restarting" {
		t.Errorf("name = %q, want the target's", view.Name)
	}
	if view.CurrentMaxChildren != 20 || !view.MaxChildrenKnown {
		t.Errorf("CurrentMaxChildren = %d (known=%v), want the configured 20: "+
			"without it nothing is reserved for a pool that is merely restarting",
			view.CurrentMaxChildren, view.MaxChildrenKnown)
	}
	if view.ProcessManager != "dynamic" {
		t.Errorf("ProcessManager = %q, want the configured one", view.ProcessManager)
	}
}

// TestAPoolWithNoConfiguredCeilingIsStillMarkedUnknown: the carried value must
// not be mistaken for evidence when there is none. A target discovery could not
// read pm.max_children for reports zero, and zero is not a size.
func TestAPoolWithNoConfiguredCeilingIsStillMarkedUnknown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	views := Sample(ctx, []phpfpm.Target{{
		Name: "unknown", Socket: "127.0.0.1:1", StatusPath: "/status",
	}}, nil)

	if len(views) != 1 {
		t.Fatalf("got %d views, want 1", len(views))
	}
	if views[0].MaxChildrenKnown {
		t.Error("a pool whose configured ceiling was never read was reported as known; " +
			"a resize would then be proposed against a number that does not exist")
	}
}

// TestDiscoveryHonoursTheCallersDeadline.
//
// Discovery forks `php-fpm -tt` once per master. Unbounded, a binary that wedges
// — an NFS-backed include, an operator's wrapper script, a host under memory
// pressure — stops the loop above it for good, and a daemon that has stopped
// observing is worse than one that has stopped: it looks alive.
func TestDiscoveryHonoursTheCallersDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead

	start := time.Now()
	_, err := Discover(ctx, nil)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Errorf("discovery took %s against a cancelled context", elapsed)
	}

	// The error, not just the clock. On any host with no php-fpm master —
	// every runner in the unit-test job — there is nothing to fork, so the
	// elapsed time is near zero however the context is handled: passing
	// context.Background() straight through left this passing.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled: with no master on the host there is "+
			"nothing to fork, so the elapsed time says nothing — and an empty list with "+
			"a nil error tells the operator this host runs no php-fpm, which is not what "+
			"happened", err)
	}
}

// TestTheViewIsAboutThePoolItIsLabelledWith.
//
// The mapping used to take whatever entry the range reached first and break.
// There is one pool per scrape today, so the map has one entry and the two are
// the same thing — but that rests on an invariant of another module, and map
// iteration order is random. Driven with three pools, the view labelled `alpha`
// carried another pool's peak 31% of the time: one site's workers measured
// under another site's name, silently and differently on each run.
func TestTheViewIsAboutThePoolItIsLabelledWith(t *testing.T) {
	outcome := phpfpm.PoolOutcome{
		Name: "alpha",
		Result: &phpfpm.Result{Pools: map[string]phpfpm.Pool{
			"alpha": {Name: "alpha", MaxActiveProcesses: 11},
			"beta":  {Name: "beta", MaxActiveProcesses: 22},
			"gamma": {Name: "gamma", MaxActiveProcesses: 33},
		}},
	}

	// Repeated, because the failure is a coin toss and one run proves nothing.
	for i := 0; i < 50; i++ {
		view := viewFromOutcome(outcome, phpfpm.Target{Name: "alpha"})
		if view.ObservedPeak != 11 {
			t.Fatalf("the view for alpha carried a peak of %d, which belongs to another "+
				"pool; its workers are about to be measured as alpha's", view.ObservedPeak)
		}
	}
}

// TestAPoolWithNoResultStillCarriesItsCeiling.
//
// Sample's own failure branch carries the configured ceiling forward and this
// one did not, so a pool that came back with no result at all was accounted for
// as having no known ceiling — and a pool with no known ceiling is reserved at
// the default floor while it actually runs forty workers. The difference goes
// to a neighbour, and the neighbour is written.
func TestAPoolWithNoResultStillCarriesItsCeiling(t *testing.T) {
	view := viewFromOutcome(
		phpfpm.PoolOutcome{Name: "www"},
		phpfpm.Target{Name: "www", MaxChildren: 40, ProcessManager: "dynamic"},
	)

	if view.Err == nil {
		t.Error("a pool with no result was not marked as failed")
	}
	if view.CurrentMaxChildren != 40 || !view.MaxChildrenKnown {
		t.Errorf("CurrentMaxChildren = %d (known=%v), want the configured 40: without it "+
			"nothing is reserved for a pool that is merely restarting",
			view.CurrentMaxChildren, view.MaxChildrenKnown)
	}
}

// TestAPoolIsNotTwoPoolsBecauseTwoProcessesServeIt.
//
// Discovery scans the process table, and a host can carry more than one master
// for the same configuration: an old one still holding a wedged worker after a
// restart, or the moment during a daemonized reload when the re-execed master
// and its predecessor are both up. Each reports the same pools from the same
// file.
//
// Counted twice, every one of those pools is planned twice and the budget is
// divided among twice as many entries. Measured on a five-pool host with a
// lingering master: ten rows, every pool cut to half the workers it should have
// had, and an `allocated` figure that agreed with itself all the way down.
func TestAPoolIsNotTwoPoolsBecauseTwoProcessesServeIt(t *testing.T) {
	same := []phpfpm.Target{
		{Name: "shop", ConfigPath: "/etc/php-fpm.conf", PID: 100},
		{Name: "www", ConfigPath: "/etc/php-fpm.conf", PID: 100},
		// The same two pools, from a master that has not exited yet.
		{Name: "shop", ConfigPath: "/etc/php-fpm.conf", PID: 200},
		{Name: "www", ConfigPath: "/etc/php-fpm.conf", PID: 200},
		// A spelling of the same path, which is the same file.
		{Name: "shop", ConfigPath: "/etc/./php-fpm.conf", PID: 300},
	}

	got := Dedupe(same)
	if len(got) != 2 {
		names := make([]string, 0, len(got))
		for _, g := range got {
			names = append(names, g.Name+"@"+g.ConfigPath)
		}
		t.Errorf("two pools served by three processes came back as %d targets (%v); each "+
			"duplicate takes a share of the budget, so every pool is planned at a "+
			"fraction of what it should have", len(got), names)
	}

	// Two masters with DIFFERENT configurations are different pools, and merging
	// them would be the opposite mistake.
	different := Dedupe([]phpfpm.Target{
		{Name: "www", ConfigPath: "/etc/php/8.2/php-fpm.conf"},
		{Name: "www", ConfigPath: "/etc/php/8.3/php-fpm.conf"},
	})
	if len(different) != 2 {
		t.Errorf("two masters' pools of the same name were merged into %d; they are "+
			"different sites with different applications", len(different))
	}
}
