package serve

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
	"github.com/cboxdk/phpfpm"
)

const untrackedMaster = "/etc/php/8.4/fpm/php-fpm.conf"

// untrackedConfig builds a loop config where the master serves one statused pool
// (www) and one pool with no status page (www-forge), the exact shape that let a
// busy pool go unsized on a real host.
func untrackedConfig(t *testing.T, apply bool) Config {
	t.Helper()

	return Config{
		StatePath:      filepath.Join(t.TempDir(), "state.json"),
		MemoryOverride: 4096 * mb,
		Apply:          apply,
		Discover: func(context.Context) ([]phpfpm.Target, error) {
			return []phpfpm.Target{{
				Name: "www", MaxChildren: 20, ProcessManager: "dynamic", ConfigPath: untrackedMaster,
			}}, nil
		},
		Sample: func(context.Context, []phpfpm.Target) []observe.PoolView {
			return []observe.PoolView{{
				Name: "www", ProcessManager: "dynamic", CurrentMaxChildren: 20, MaxChildrenKnown: true,
				Workers: []state.WorkerSample{{RSSBytes: 40 * mb, Requests: 500}},
			}}
		},
		Unstatused: func(context.Context) ([]phpfpm.Unstatused, error) {
			return []phpfpm.Unstatused{{Name: "www-forge", ConfigPath: untrackedMaster}}, nil
		},
	}
}

// TestUntrackedPoolIsWarnedAboutInAdvisory: the loop must not silently size only the
// pools that happen to have a status page. A pool without one is named in a warning,
// pointing at the command that fixes it. This is the guard that would have made the
// www-forge gap obvious in seconds instead of after a long hunt.
func TestUntrackedPoolIsWarnedAboutInAdvisory(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	loop, err := New(untrackedConfig(t, false), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { loop.Close() })

	loop.round(context.Background())

	out := buf.String()
	if !strings.Contains(out, "www-forge") {
		t.Errorf("advisory did not warn about the untracked pool www-forge:\n%s", out)
	}
	if !strings.Contains(out, "enable-status") {
		t.Errorf("the warning did not point at `fpm-tune enable-status`:\n%s", out)
	}
}

// TestUntrackedPoolResetsReconcileInApplyMode: in apply mode the loop should not just
// warn — it should re-run the reconcile that turns the status page on, so a pool the
// startup pass missed (or one added since) is picked up without a manual restart.
func TestUntrackedPoolResetsReconcileInApplyMode(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	loop, err := New(untrackedConfig(t, true), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { loop.Close() })

	// Pretend the startup reconcile ran and left www-forge unstatused.
	loop.reconciled = true
	loop.round(context.Background())

	if loop.reconciled {
		t.Error("apply mode left reconciled=true; the untracked pool's status page will never be enabled")
	}
}

// TestApplyReEnableIsCapped: a pool that can never be given a status page (an unsafe
// name, a config php-fpm rejects) must not reset the reconcile on every round for the
// life of the process. After the cap the loop stops churning; the warning stands.
func TestApplyReEnableIsCapped(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	loop, err := New(untrackedConfig(t, true), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { loop.Close() })

	targets := []phpfpm.Target{{Name: "www", ConfigPath: untrackedMaster}}

	// Simulate many rounds where the pool stays unstatused (reconcile keeps setting
	// reconciled back to true, the pool never gets a page).
	for range maxStatusRetries + 2 {
		loop.reconciled = true
		loop.noteUntracked(context.Background(), targets)
	}

	loop.reconciled = true
	loop.noteUntracked(context.Background(), targets)
	if !loop.reconciled {
		t.Error("past the retry cap the loop still reset reconcile; it would churn a reconcile every round forever")
	}
}
