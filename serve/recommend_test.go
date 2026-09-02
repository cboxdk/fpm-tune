package serve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/plan"
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

// TestRecommendInsideThePoolDirectoryIsRefusedAtStartup.
//
// A recommendation path inside the pool directory is a file php-fpm would load
// and this tool would believe it wrote — and the running daemon would refuse it
// every interval, forever, while looking healthy and producing nothing. The
// deterministic case is caught at startup, with the fix in the message.
func TestRecommendInsideThePoolDirectoryIsRefusedAtStartup(t *testing.T) {
	dir := t.TempDir()

	_, err := New(Config{
		StatePath:     filepath.Join(t.TempDir(), "state.json"),
		MetricsAddr:   "",
		DropInDir:     dir,
		RecommendPath: filepath.Join(dir, "recommended.conf"),
	}, nil)
	if err == nil {
		t.Fatal("a recommendation path inside the pool directory was accepted; php-fpm " +
			"would load it and the daemon would refuse it every interval forever")
	}
	if !strings.Contains(err.Error(), "outside the pool directory") {
		t.Errorf("the error does not tell the operator the fix:\n%v", err)
	}

	// A path outside it is fine.
	loop, err := New(Config{
		StatePath:     filepath.Join(t.TempDir(), "state.json"),
		MetricsAddr:   "",
		DropInDir:     dir,
		RecommendPath: filepath.Join(t.TempDir(), "recommended.conf"),
	}, nil)
	if err != nil {
		t.Fatalf("a recommendation path outside the pool directory was refused: %v", err)
	}
	loop.Close()
}

// TestRecommendationShowsChildrenWhenAPoolSpawnsThem: the whole point of the
// subtree measurement is that a media pool's ffmpeg shows up somewhere a person
// reading the recommendation can see it. A plain pool that spawns nothing gets
// no children line, so the file is not cluttered with zeroes.
func TestRecommendationShowsChildrenWhenAPoolSpawnsThem(t *testing.T) {
	const mb = 1 << 20

	result := plan.Result{
		Plan: allocate.Plan{
			Pools: []allocate.PoolPlan{
				{Name: "media", MaxChildren: 4, WorkerBytes: 90 * mb, Reason: "measured 90MiB"},
				{Name: "web", MaxChildren: 8, WorkerBytes: 60 * mb, Reason: "measured 60MiB"},
			},
			TotalBytes:     8192 * mb,
			AllocatedBytes: 840 * mb,
		},
		Reserve:       512 * mb,
		ReserveReason: "the operating system",
		Distribution: []plan.PoolDistribution{
			{
				Name: "media", P50: 60 * mb, P95: 90 * mb, P99: 95 * mb, WorstSeen: 95 * mb,
				Samples: 100, SubtreeHighWater: 690 * mb, ChildPerWorker: 150 * mb, // children folded in
			},
			{
				Name: "web", P50: 55 * mb, P95: 60 * mb, P99: 62 * mb, WorstSeen: 62 * mb,
				Samples: 100, SubtreeHighWater: 60 * mb, ChildPerWorker: 0, // nothing spawned
			},
		},
		CPU: []plan.PoolCPU{
			{Name: "media", P50: 0.7, P90: 0.75, Samples: 50, Shape: "cpu-bound", MillicoresPerWorker: 700, FillWorkers: 6, Limit: "cpu"},
			{Name: "web", Samples: 2}, // too few readings: no line
		},
		HostCPU: plan.HostCPU{Millicores: 4000, Known: 1},
	}

	file, _ := renderRecommendation(result, time.Unix(1_700_000_000, 0))

	// The CPU dimension, in the same words the plan uses, and only for a pool
	// whose shape is known.
	if !strings.Contains(file, "cpu per request: median 70%, p90 75% (50 readings); 700m per busy worker, ~6 fill 4 core(s); limit: cpu") {
		t.Errorf("the media pool's CPU shape is not in the recommendation:\n%s", file)
	}
	if n := strings.Count(file, "cpu per request"); n != 1 {
		t.Errorf("cpu line rendered %d times, want exactly 1 (web has too few readings):\n%s", n, file)
	}

	if !strings.Contains(file, "children per worker") {
		t.Errorf("the media pool's children are not shown; an operator sizing by hand "+
			"cannot see the ffmpeg:\n%s", file)
	}
	// The web pool spawns nothing, so only the media pool carries a children line.
	if n := strings.Count(file, "children per worker"); n != 1 {
		t.Errorf("children line rendered %d times, want exactly 1 (only the media pool):\n%s", n, file)
	}
}

// TestSettingsOfIgnoresCommentary: the recommendation file is rewritten (and "The
// recommendation changed" logged) only when the pm.* settings move — not when the
// commentary around them moves, which it does every round (the reading count climbs,
// a percentile shifts a bucket, the "measured Nmib" in a reason drifts). settingsOf
// is what draws that line, so it must reduce a file to its settings alone.
func TestSettingsOfIgnoresCommentary(t *testing.T) {
	withReason := func(measured, readings string) string {
		return "; header line that changes\n;\n" +
			"; www-forge: raised to 6, measured " + measured + "/worker\n" +
			";   measured per worker: median 32.0MiB (" + readings + " readings)\n" +
			"[www-forge]\npm.max_children = 6\npm.start_servers = 2\n"
	}

	a := withReason("41.9MiB", "748")
	b := withReason("42.3MiB", "812") // same pm.* values, only the commentary moved

	if settingsOf(a) != settingsOf(b) {
		t.Errorf("settingsOf differs when only the commentary changed:\n%q\nvs\n%q",
			settingsOf(a), settingsOf(b))
	}

	c := "[www-forge]\npm.max_children = 7\npm.start_servers = 2\n" // a real change
	if settingsOf(a) == settingsOf(c) {
		t.Error("settingsOf did not notice pm.max_children moving from 6 to 7")
	}
}
