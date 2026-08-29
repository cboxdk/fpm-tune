package serve

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cboxdk/fpm-tune/apply"
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

	loop := applyingLoop(t, t.TempDir())
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

	loop := applyingLoop(t, poolDir)
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
func applyingLoop(t *testing.T, dropInDir string) *Loop {
	t.Helper()

	targets := []phpfpm.Target{{Name: "shop", MaxChildren: 4, ProcessManager: "dynamic"}}

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
