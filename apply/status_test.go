package apply

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestRenderStatusRoundTrips: what renderStatus writes, parseStatusOurs reads back
// as the same pools. The two are fed into each other on every re-run, so a format
// one writes and the other rejects would disable a pool the moment a second is
// enabled.
func TestRenderStatusRoundTrips(t *testing.T) {
	in := map[string]string{"www": "/status", "api": "/status", "shop": "/fpm-status"}

	got, err := parseStatusOursBytes(t, renderStatus(in))
	if err != nil {
		t.Fatalf("parseStatusOurs of what renderStatus produced: %v", err)
	}
	if len(got) != len(in) {
		t.Fatalf("round-tripped %d pools, want %d", len(got), len(in))
	}
	for name, path := range in {
		if got[name] != path {
			t.Errorf("pool %q round-tripped to %q, want %q", name, got[name], path)
		}
	}
}

// parseStatusOursBytes writes content to a temp file and parses it, so the pure
// round-trip does not need a pool directory.
func parseStatusOursBytes(t *testing.T, content []byte) (map[string]string, error) {
	t.Helper()

	path := t.TempDir() + "/zz-fpm-tune-status.conf"
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	return parseStatusOurs(path)
}

// TestParseStatusOursRefusesAForeignFile: a file under this name without the marker
// is an operator's own, and silently overwriting it is a worse outcome than
// declining and saying so.
func TestParseStatusOursRefusesAForeignFile(t *testing.T) {
	_, err := parseStatusOursBytes(t, []byte("[www]\npm.status_path = /status\n"))
	if !errors.Is(err, ErrForeignDropIn) {
		t.Fatalf("err = %v, want ErrForeignDropIn", err)
	}
}

// TestParseStatusOursRefusesABodyItDidNotWrite: the marker with contents this tool
// would not produce (a sizing key, a hand edit) is refused rather than salvaged —
// its values are fed straight back into the next version of the file.
func TestParseStatusOursRefusesABodyItDidNotWrite(t *testing.T) {
	body := statusGeneratedMarker + "\n[www]\npm.max_children = 10\n"
	_, err := parseStatusOursBytes(t, []byte(body))
	if !errors.Is(err, errNotOurStatusFormat) {
		t.Fatalf("err = %v, want errNotOurStatusFormat", err)
	}
}

func TestSafeStatusPathRejectsUnsafeValues(t *testing.T) {
	for _, tc := range []struct {
		path string
		ok   bool
	}{
		{"/status", true},
		{"/fpm-status", true},
		{"", false},
		{"status", false},                 // no leading slash
		{"/status\nlisten = /pwn", false}, // an injected directive
		{"/status ; evil", false},
		{"/a[b]", false},
	} {
		err := safeStatusPath(tc.path)
		if tc.ok && err != nil {
			t.Errorf("safeStatusPath(%q) = %v, want nil", tc.path, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("safeStatusPath(%q) = nil, want an error", tc.path)
		}
	}
}

// TestEnableStatusTurnsOnAPoolAndReloads is the happy path: a pool with no status
// page gets one, the file is written where the master reads it, and the master is
// reloaded exactly once.
func TestEnableStatusTurnsOnAPoolAndReloads(t *testing.T) {
	dir := t.TempDir()
	master, st := newMasterWithStub(t, dir)

	res, err := EnableStatus(context.Background(), master,
		[]string{"www"}, map[string]bool{"www": true}, "/status",
		Options{BackupDir: t.TempDir(), SettleTime: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatalf("EnableStatus: %v", err)
	}
	if !res.Reloaded {
		t.Error("the master was not reloaded")
	}
	if len(res.Enabled) != 1 || res.Enabled[0] != "www" {
		t.Errorf("Enabled = %v, want [www]", res.Enabled)
	}
	if n := st.signalsSeen(t); n != 1 {
		t.Errorf("the master received %d reloads, want exactly 1", n)
	}

	owned, err := parseStatusOurs(StatusDropInPath(dir))
	if err != nil {
		t.Fatalf("the written status file does not parse: %v", err)
	}
	if owned["www"] != "/status" {
		t.Errorf("the status file did not turn www's page on: %v", owned)
	}
}

// TestEnableStatusIsIdempotent: run twice, and the second run writes nothing and
// reloads nothing. A reload cycles workers, and doing it to write bytes already on
// disk is the churn the tool is careful to avoid.
func TestEnableStatusIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	master, st := newMasterWithStub(t, dir)
	opts := Options{BackupDir: t.TempDir(), SettleTime: 100 * time.Millisecond}

	if _, err := EnableStatus(context.Background(), master,
		[]string{"www"}, map[string]bool{"www": true}, "/status", opts, nil); err != nil {
		t.Fatalf("first EnableStatus: %v", err)
	}

	res, err := EnableStatus(context.Background(), master,
		[]string{"www"}, map[string]bool{"www": true}, "/status", opts, nil)
	if err != nil {
		t.Fatalf("second EnableStatus: %v", err)
	}
	if res.Reloaded {
		t.Error("the second run reloaded the master for a file that was already correct")
	}
	if n := st.signalsSeen(t); n != 1 {
		t.Errorf("the master received %d reloads across two runs, want 1", n)
	}
}

// TestEnableStatusDropsARemovedPool: a section this tool enabled for a pool whose
// own configuration has since been removed must not be carried forward — a [pool]
// with a status path and no listen is a pool definition php-fpm will not start on.
func TestEnableStatusDropsARemovedPool(t *testing.T) {
	dir := t.TempDir()
	master, _ := newMasterWithStub(t, dir)

	// A status file this tool wrote earlier, enabling two pools.
	seed := statusGeneratedMarker + "\n[www]\npm.status_path = /status\n[gone]\npm.status_path = /status\n"
	if err := os.WriteFile(StatusDropInPath(dir), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	// `gone` no longer exists on the host; `api` is new and needs enabling.
	_, err := EnableStatus(context.Background(), master,
		[]string{"api"}, map[string]bool{"www": true, "api": true}, "/status",
		Options{BackupDir: t.TempDir(), SettleTime: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatalf("EnableStatus: %v", err)
	}

	owned, err := parseStatusOurs(StatusDropInPath(dir))
	if err != nil {
		t.Fatalf("parseStatusOurs: %v", err)
	}
	if _, kept := owned["gone"]; kept {
		t.Error("a section for a pool that no longer exists was carried forward; php-fpm " +
			"would refuse to start on a pool with a status path and no listen")
	}
	if owned["www"] != "/status" || owned["api"] != "/status" {
		t.Errorf("the pools that still exist were not both kept: %v", owned)
	}
}

// TestEnableStatusRefusesAForeignStatusFile: a file under this name it did not
// write is not its to replace.
func TestEnableStatusRefusesAForeignStatusFile(t *testing.T) {
	dir := t.TempDir()
	master, st := newMasterWithStub(t, dir)

	foreign := []byte("[www]\npm.status_path = /operators-own\n")
	if err := os.WriteFile(StatusDropInPath(dir), foreign, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := EnableStatus(context.Background(), master,
		[]string{"www"}, map[string]bool{"www": true}, "/status",
		Options{BackupDir: t.TempDir()}, nil)
	if !errors.Is(err, ErrForeignDropIn) {
		t.Fatalf("err = %v, want ErrForeignDropIn", err)
	}
	if n := st.signalsSeen(t); n != 0 {
		t.Errorf("the master was reloaded %d times over a foreign file", n)
	}
	if got, _ := os.ReadFile(StatusDropInPath(dir)); string(got) != string(foreign) {
		t.Error("the operator's own file was modified")
	}
}

// TestEnableStatusRejectedConfigNeverReachesTheMaster: a tree php-fpm rejects is
// caught against the sandbox copy, so nothing is written live and the master is
// never signalled — the sizing path's guarantee, kept for status too.
func TestEnableStatusRejectedConfigNeverReachesTheMaster(t *testing.T) {
	dir := t.TempDir()
	master := Master{
		Binary:     falseBin(t), // stands in for php-fpm rejecting the config
		ConfigPath: masterConfigAt(t, dir),
		DropInDir:  dir,
		PID:        os.Getpid(), // would be signalled if the guard failed
	}

	res, err := EnableStatus(context.Background(), master,
		[]string{"www"}, map[string]bool{"www": true}, "/status",
		Options{BackupDir: t.TempDir()}, nil)
	if !errors.Is(err, ErrValidationFailed) {
		t.Fatalf("err = %v, want ErrValidationFailed", err)
	}
	if res.Reloaded {
		t.Error("the master was reloaded with a configuration php-fpm had rejected")
	}
	// Not rolled back, because nothing was written: validation happens against a
	// sandbox copy first. A rollback here would mean the rejected file had reached
	// the live directory — which is what the sandbox check exists to prevent, and
	// asserting it is what holds that guard against the mutation sweep.
	if res.RolledBack {
		t.Error("a rollback happened, so the rejected file had been written live")
	}
	if _, statErr := os.Stat(StatusDropInPath(dir)); !os.IsNotExist(statErr) {
		t.Error("a rejected status file was written to the directory php-fpm globs")
	}
}

// TestEnableStatusRollsBackWhenTheMasterDies: validation passes but the master does
// not come back from the reload, so the file it did not have before is taken back
// out. This is the case that separates a status page nobody asked to break the host
// for from one that did.
func TestEnableStatusRollsBackWhenTheMasterDies(t *testing.T) {
	dir := t.TempDir()
	config := masterConfigAt(t, dir)
	dying := stubMaster(t, config, true)
	master := Master{Binary: trueBin(t), ConfigPath: config, DropInDir: dir, PID: dying.pid}

	res, err := EnableStatus(context.Background(), master,
		[]string{"www"}, map[string]bool{"www": true}, "/status",
		Options{BackupDir: t.TempDir(), SettleTime: 500 * time.Millisecond}, nil)
	if !errors.Is(err, ErrMasterDidNotSurvive) {
		t.Fatalf("err = %v, want ErrMasterDidNotSurvive", err)
	}
	if !res.RolledBack {
		t.Error("the master died and the change was not rolled back")
	}
	if _, statErr := os.Stat(StatusDropInPath(dir)); !os.IsNotExist(statErr) {
		t.Error("the status file was left behind after a rollback; the next reload adopts it")
	}
}
