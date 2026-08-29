package serve

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cboxdk/fpm-tune/apply"
	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/state"
	"github.com/cboxdk/phpfpm"
)

// TestTheDaemonRefusesToApplyWithNoMasterAndSaysSoOnTheEndpoint.
//
// applyPlan is the daemon's write path and no test had ever entered it. That
// includes the three ways an apply can be blocked — and being blocked is the
// state an operator most needs to see, because a tool that is watching and a
// tool that is acting look identical from outside. The log says it once; the
// metric is what an alert can read.
func TestTheDaemonRefusesToApplyWithNoMasterAndSaysSoOnTheEndpoint(t *testing.T) {
	defer swapDiscovery(nil)()

	// A properly scoped daemon: it knows which master it is for, and that master
	// is not running. That is the case an operator hits, and it is the one where
	// a watching process and an acting one look identical from outside.
	tr := poolTree(t, "8.5")
	loop := applyingLoop(t, tr.poolDir, tr.configPath)
	loop.round(context.Background())

	if got := blockedReason(t, loop); got != "no_master" {
		t.Errorf("apply_blocked reason = %q, want no_master: a daemon that cannot act "+
			"publishes nothing to say so, and looks exactly like one that is acting", got)
	}
}

// TestTheDaemonWritesWhenItCan is the other half, so a loop that never applied
// anything at all could not pass the test above.
func TestTheDaemonWritesWhenItCan(t *testing.T) {
	dir := t.TempDir()
	poolDir := filepath.Join(dir, "pool.d")
	if err := os.MkdirAll(poolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "php-fpm.conf")
	if err := os.WriteFile(configPath,
		[]byte("[global]\ninclude = "+filepath.Join(poolDir, "*.conf")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A master that is alive and survives its reload. sh with USR2 trapped to
	// nothing is exactly that, and it keeps this test free of the stub-process
	// machinery the apply package needs for its own reasons.
	defer swapDiscovery([]phpfpm.Master{
		{PID: livingMaster(t, configPath), ConfigPath: configPath, Binary: trueBinary(t)},
	})()

	loop := applyingLoop(t, poolDir, configPath)
	loop.round(context.Background())

	body, err := os.ReadFile(apply.DropInPath(poolDir))
	if err != nil {
		t.Fatalf("the daemon applied nothing: %v", err)
	}
	if !strings.Contains(string(body), "[shop]") {
		t.Errorf("the pool is missing from what was written:\n%s", body)
	}
	if got := blockedReason(t, loop); got != "" {
		t.Errorf("apply_blocked = %q on a round that applied cleanly", got)
	}
}

// applyingLoop is a daemon configured to act, driven by fixed observations.
//
// configPath matters: the loop scopes discovery to the master that includes its
// pool directory, so a target that does not say which master it belongs to is
// correctly filtered out. Empty means "do not scope", for the cases that are
// not about scoping.
func applyingLoop(t *testing.T, dropInDir, configPath string) *Loop {
	t.Helper()

	targets := []phpfpm.Target{{
		Name: "shop", MaxChildren: 4, ProcessManager: "dynamic", ConfigPath: configPath,
	}}

	loop, err := New(Config{
		StatePath:      filepath.Join(t.TempDir(), "state.json"),
		BackupDir:      filepath.Join(t.TempDir(), "backup"),
		DropInDir:      dropInDir,
		MetricsAddr:    "",
		MemoryOverride: 4096 * mb,
		Apply:          true,
		Discover: func(context.Context) ([]phpfpm.Target, error) {
			return targets, nil
		},
		Sample: func(context.Context, []phpfpm.Target) []observe.PoolView {
			return []observe.PoolView{{
				Name: "shop", ProcessManager: "dynamic",
				CurrentMaxChildren: 4, MaxChildrenKnown: true,
				Target:    targets[0],
				ActiveNow: 4, ObservedPeak: 4, Accepted: 100_000,
				Workers: []state.WorkerSample{
					{RSSBytes: 40 * mb, Requests: 500},
					{RSSBytes: 42 * mb, Requests: 500},
				},
			}}
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { loop.Close() })

	return loop
}

// blockedReason reads back which reason, if any, is currently published as
// blocking an apply.
func blockedReason(t *testing.T, l *Loop) string {
	t.Helper()

	families, err := l.Metrics().Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "fpm_tune_apply_blocked" {
			continue
		}
		for _, m := range f.GetMetric() {
			if m.GetGauge().GetValue() != 1 {
				continue
			}
			for _, lab := range m.GetLabel() {
				if lab.GetName() == "reason" {
					return lab.GetValue()
				}
			}
		}
	}

	return ""
}

// livingMaster is a process that both LOOKS like a php-fpm master and survives
// SIGUSR2, because the reload path checks the identity before it signals and
// then watches the process live through it.
//
// A copy of this test binary, re-entered as a helper. sh will not do: it cannot
// set the command line the identity check reads.
func livingMaster(t *testing.T, configPath string) int {
	t.Helper()

	binDir := t.TempDir()
	binary := filepath.Join(binDir, "php-fpm")
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Skipf("cannot read the test binary to build a stand-in master: %v", err)
	}
	if err := os.WriteFile(binary, self, 0o755); err != nil {
		t.Fatal(err)
	}

	ready := filepath.Join(binDir, "ready")
	cmd := exec.Command(binary, "-test.run=TestServeStubMasterHelper",
		"php-fpm: master process ("+configPath+")")
	cmd.Env = append(os.Environ(), "FPM_TUNE_SERVE_STUB=1", "FPM_TUNE_SERVE_STUB_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	waited := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(waited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waited
	})

	// Waited for after the handler is installed: the default action for USR2 is
	// to terminate, so signalling early kills it and reads as a master that did
	// not survive.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return cmd.Process.Pid
		}
		if time.Now().After(deadline) {
			t.Fatal("the stand-in master never signalled that its handler was installed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServeStubMasterHelper is the stand-in master, running inside a copy of
// this test binary. A no-op unless the environment says otherwise.
func TestServeStubMasterHelper(t *testing.T) {
	if os.Getenv("FPM_TUNE_SERVE_STUB") != "1" {
		t.Skip("helper process only")
	}

	got := make(chan os.Signal, 1)
	signal.Notify(got, syscall.SIGUSR2)

	if ready := os.Getenv("FPM_TUNE_SERVE_STUB_READY"); ready != "" {
		if err := os.WriteFile(ready, []byte("ok"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case <-got:
		// Survives it, as a healthy master does, and stays up for the settle
		// window that follows.
		time.Sleep(5 * time.Second)
	case <-time.After(30 * time.Second):
	}
}

func trueBinary(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "php-fpm")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	return path
}

// TestTheLockFollowsTheDirectoryBeingWritten.
//
// A PHP upgrade in place moves the pool directory. Re-keying the resource lock
// lived only inside reconcile, which runs once per process — so the daemon went
// on holding the lock for the OLD directory while applyPlan wrote to the new
// one. A concurrent `fpm-tune apply` found the new key free and both wrote the
// same file, and the daemon never reconciled the new tree, so a record left by
// an unfinished run there would be written over unread.
func TestTheLockFollowsTheDirectoryBeingWritten(t *testing.T) {
	old := poolTree(t, "8.2")
	upgraded := poolTree(t, "8.5")

	current := old
	restore := swapDiscoveryFunc(func() []phpfpm.Master { return []phpfpm.Master{current.master(t)} })
	defer restore()

	loop := applyingLoop(t, "", "")
	loop.round(context.Background())

	if loop.resourceDir != old.poolDir {
		t.Fatalf("setup: the lock is on %q, want the original pool directory %q",
			loop.resourceDir, old.poolDir)
	}

	// The upgrade.
	current = upgraded
	loop.round(context.Background())

	if loop.resourceDir != upgraded.poolDir {
		t.Errorf("after the pool directory moved to %q the lock is still on %q; a "+
			"concurrent apply on the new directory is not excluded, and this process "+
			"has never reconciled it", upgraded.poolDir, loop.resourceDir)
	}
}

type tree struct {
	poolDir, configPath string
}

func poolTree(t *testing.T, version string) tree {
	t.Helper()

	root := filepath.Join(t.TempDir(), version)
	poolDir := filepath.Join(root, "pool.d")
	if err := os.MkdirAll(poolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "php-fpm.conf")
	if err := os.WriteFile(configPath,
		[]byte("[global]\ninclude = "+filepath.Join(poolDir, "*.conf")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	return tree{poolDir: poolDir, configPath: configPath}
}

func (tr tree) master(t *testing.T) phpfpm.Master {
	t.Helper()

	return phpfpm.Master{
		PID: livingMaster(t, tr.configPath), ConfigPath: tr.configPath, Binary: trueBinary(t),
	}
}

// swapDiscoveryFunc is swapDiscovery for a list that changes between rounds.
func swapDiscoveryFunc(fn func() []phpfpm.Master) func() {
	saved := discoverMasters
	discoverMasters = func(*slog.Logger) ([]phpfpm.Master, error) { return fn(), nil }

	return func() { discoverMasters = saved }
}

// TestTheLoopItselfScopesToItsMaster.
//
// ForMaster is tested directly elsewhere; this is about the LOOP calling it.
// The filter existed and was correct, and lived in the CLI — so plan and apply
// scoped and the daemon did not, which is a fault in the wiring rather than in
// the rule, and no test of the rule could catch it.
func TestTheLoopItselfScopesToItsMaster(t *testing.T) {
	dir := t.TempDir()
	mine := writeMasterConfig(t, dir, "8.5")
	theirs := writeMasterConfig(t, dir, "8.2")

	loop, err := New(Config{
		StatePath:      filepath.Join(t.TempDir(), "state.json"),
		MetricsAddr:    "",
		MemoryOverride: 4096 * mb,
		DropInDir:      filepath.Join(dir, "8.5", "pool.d"),
		Discover: func(context.Context) ([]phpfpm.Target, error) {
			return []phpfpm.Target{
				{Name: "shop", ConfigPath: mine, PID: 100, MaxChildren: 8, ProcessManager: "dynamic"},
				{Name: "api", ConfigPath: theirs, PID: 200, MaxChildren: 8, ProcessManager: "dynamic"},
			}, nil
		},
		Sample: func(_ context.Context, targets []phpfpm.Target) []observe.PoolView {
			views := make([]observe.PoolView, 0, len(targets))
			for _, tg := range targets {
				views = append(views, observe.PoolView{
					Name: tg.Name, ProcessManager: "dynamic", Target: tg,
					CurrentMaxChildren: 8, MaxChildrenKnown: true,
					ActiveNow: 4, ObservedPeak: 4, Accepted: 100_000,
					Workers: []state.WorkerSample{
						{RSSBytes: 40 * mb, Requests: 500}, {RSSBytes: 41 * mb, Requests: 500},
					},
				})
			}

			return views
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { loop.Close() })

	loop.round(context.Background())

	if learned := loop.State().Lookup(theirs, "api"); learned != nil {
		t.Error("the daemon learned a pool belonging to a master it was not pointed at; " +
			"its budget is read from one master's cgroup and is now being divided among " +
			"pools that master does not run")
	}
	if learned := loop.State().Lookup(mine, "shop"); learned == nil {
		t.Error("the daemon learned nothing at all; the filter has removed its own pools")
	}
}

// TestNoLearnRecordsNothing.
//
// The flag was registered for `serve` and read by nothing. A daemon started
// with -no-learn wrote a state file with a sample count per pool, while its own
// help said "do not record this scrape" — so someone using it to watch a host
// without disturbing a baseline was disturbing it, silently.
func TestNoLearnRecordsNothing(t *testing.T) {
	tr := poolTree(t, "8.5")
	defer swapDiscovery([]phpfpm.Master{
		{PID: livingMaster(t, tr.configPath), ConfigPath: tr.configPath, Binary: trueBinary(t)},
	})()

	statePath := filepath.Join(t.TempDir(), "state.json")
	loop := applyingLoop(t, tr.poolDir, tr.configPath)
	// Watching, not acting: the two are refused together by the CLI, because
	// applying has to record what it wrote.
	loop.cfg.Apply = false
	loop.cfg.NoLearn = true
	loop.cfg.StatePath = statePath

	loop.round(context.Background())
	loop.save(time.Now(), true)

	if n := len(loop.State().Pools); n != 0 {
		t.Errorf("%d pools were recorded by a run told not to record anything", n)
	}
	if _, err := os.Stat(statePath); err == nil {
		t.Error("a state file was written by a run told not to record anything; whatever " +
			"a previous run learned has just been replaced by a file that learned nothing")
	}
}

// TestNoLearnDoesNotForgetOrSaveOnTheWayOut.
//
// -no-learn skipped LearnFrom and nothing else. Forget still counted absences
// and deleted pools, and the shutdown save went straight to the store rather
// than through the path that honours the flag — so a daemon told to record
// nothing wrote a file on its way out, creating one where there had been none
// or replacing what a previous run had learned.
//
// Forgetting is a change to the store as much as learning is.
func TestNoLearnDoesNotForgetOrSaveOnTheWayOut(t *testing.T) {
	tr := poolTree(t, "8.5")
	defer swapDiscovery([]phpfpm.Master{
		{PID: livingMaster(t, tr.configPath), ConfigPath: tr.configPath, Binary: trueBinary(t)},
	})()

	statePath := filepath.Join(t.TempDir(), "state.json")
	loop := applyingLoop(t, tr.poolDir, tr.configPath)
	loop.cfg.Apply = false
	loop.cfg.NoLearn = true
	loop.cfg.StatePath = statePath

	// A baseline for a pool this round will not see.
	loop.State().Learn(state.Observation{
		Pool: "gone", MasterConfig: tr.configPath, At: time.Now().Add(-time.Hour),
		Workers: []state.WorkerSample{{RSSBytes: 90 * mb, Requests: 400}},
	}, state.Options{})

	for i := 0; i < 10; i++ {
		loop.round(context.Background())
	}
	loop.shutdown(nil)

	if loop.State().Lookup(tr.configPath, "gone") == nil {
		t.Error("a run told to record nothing deleted a baseline; forgetting is a change " +
			"to the store as much as learning is")
	}
	if _, err := os.Stat(statePath); err == nil {
		t.Error("a run told to record nothing wrote a state file on its way out")
	}
}

// TestTheDaemonWillNotWriteFromABudgetItCouldNotConfirm.
//
// The detection falls back to the machine's memory when php-fpm's own limit
// cannot be read, and the two numbers are indistinguishable. A service capped
// at 3GiB on a 32GiB host would be sized against 32GiB and grown into a ceiling
// it never sees — so reading it is right and writing from it is not.
//
// Published as well as logged, because a daemon that has quietly stopped
// applying looks exactly like one with nothing to do. And the pool-directory
// lock goes back: the way out of this state is a one-shot apply with --memory,
// and a daemon that blocks the remedy it recommends is worse than one that
// simply stops.
func TestTheDaemonWillNotWriteFromABudgetItCouldNotConfirm(t *testing.T) {
	tr := poolTree(t, "8.5")
	defer swapDiscovery([]phpfpm.Master{
		{PID: livingMaster(t, tr.configPath), ConfigPath: tr.configPath, Binary: trueBinary(t)},
	})()

	loop := applyingLoop(t, tr.poolDir, tr.configPath)
	loop.cfg.MemoryOverride = 0
	loop.cfg.DetectBudget = func(int) budget.Limits {
		return budget.Limits{
			MemoryBytes: 32 * 1024 * mb, CPUs: 8, Source: budget.SourceMemInfo,
			LookupErr: errors.New("permission denied"),
		}
	}

	loop.round(context.Background())

	if _, err := os.Stat(apply.DropInPath(tr.poolDir)); err == nil {
		t.Error("the daemon wrote pool configuration from the machine's memory after " +
			"failing to read php-fpm's own limit; if php-fpm is capped below it, those " +
			"pools have just been grown into a ceiling they never see")
	}
	if got := blockedReason(t, loop); got != "budget_unconfirmed" {
		t.Errorf("apply_blocked = %q, want budget_unconfirmed: a daemon that has stopped "+
			"applying looks exactly like one with nothing to do", got)
	}
	if loop.resource != nil {
		t.Error("the daemon is still holding the pool-directory lock for work it has " +
			"decided not to do; the way out is a one-shot apply with --memory, and that " +
			"lock refuses it")
	}
}
