package serve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cboxdk/phpfpm"
)

// TestTheRecommendationIsWrittenWhereNothingReadsIt.
//
// What it writes carries this tool's own generated marker, because the point is
// that you can paste it. So a recommendation left in the pool directory is a
// file php-fpm loads and this tool believes it wrote: the pools would be
// configured by a run that was explicitly not applying anything, and the repair
// path would treat it as its own work.
func TestTheRecommendationIsWrittenWhereNothingReadsIt(t *testing.T) {
	tr := poolTree(t, "8.5")
	defer swapDiscovery([]phpfpm.Master{
		{PID: 4242, ConfigPath: tr.configPath, Binary: trueBinary(t)},
	})()

	// Straight into the directory the master includes.
	inside := filepath.Join(tr.poolDir, "recommended.conf")
	loop := applyingLoop(t, tr.poolDir, tr.configPath)
	loop.cfg.Apply = false
	loop.cfg.RecommendPath = inside

	loop.round(context.Background())

	if _, err := os.Stat(inside); err == nil {
		body, _ := os.ReadFile(inside)
		t.Errorf("a recommendation was written where php-fpm will load it:\n%s", body)
	}

	// And somewhere safe it is written.
	outside := filepath.Join(t.TempDir(), "recommended.conf")
	loop.cfg.RecommendPath = outside
	loop.round(context.Background())

	body, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("nothing was written outside the pool directory: %v", err)
	}
	if !strings.Contains(string(body), "NOTHING READS THIS FILE") {
		t.Errorf("the file does not say what it is:\n%s", body)
	}
	if !strings.Contains(string(body), "pm.max_children") {
		t.Errorf("the file carries no configuration to copy:\n%s", body)
	}
}

// TestTheRecommendationIsRewrittenOnlyWhenItChanges.
//
// The file's modification time is then the answer to "when did the
// recommendation last move", which is the question a sidecar exists to answer.
// Rewriting identical bytes every thirty seconds throws that away and leaves
// mtime saying nothing but "the daemon is running", which the metrics say
// better.
func TestTheRecommendationIsRewrittenOnlyWhenItChanges(t *testing.T) {
	tr := poolTree(t, "8.5")
	defer swapDiscovery(nil)()

	path := filepath.Join(t.TempDir(), "recommended.conf")
	loop := applyingLoop(t, tr.poolDir, tr.configPath)
	loop.cfg.Apply = false
	loop.cfg.RecommendPath = path

	loop.round(context.Background())
	first, err := os.Stat(path)
	if err != nil {
		t.Fatalf("nothing was written: %v", err)
	}

	// Far enough apart that a rewrite would be visible.
	time.Sleep(20 * time.Millisecond)
	loop.round(context.Background())

	second, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !second.ModTime().Equal(first.ModTime()) {
		t.Error("an unchanged recommendation was rewritten; the modification time now " +
			"says the daemon is running rather than that its advice moved")
	}
}
