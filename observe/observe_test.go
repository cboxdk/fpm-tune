package observe

import (
	"context"
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

	// Whether it errors depends on the host — there may be no masters at all —
	// but it must not sit there.
	_ = err
	if elapsed > 5*time.Second {
		t.Errorf("discovery took %s against a cancelled context", elapsed)
	}
}
