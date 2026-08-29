package apply

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/state"
)

// trueBin and falseBin stand in for a php-fpm that accepts or rejects a
// configuration.
//
// Looked up rather than hardcoded, and deliberately so: /bin/true does not exist
// on macOS, and an exec that fails because the binary is missing produces the
// same error as a config that was rejected. The "rejected" tests would then pass
// without validation ever having run.
func trueBin(t *testing.T) string  { return lookup(t, "true") }
func falseBin(t *testing.T) string { return lookup(t, "false") }

func lookup(t *testing.T, name string) string {
	t.Helper()

	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("no %s binary available: %v", name, err)
	}

	return path
}

// TestRenderOverridesOnlyPMSettings: the fragment repeats the pool's section
// header with just the pm.* keys, which PHP-FPM merges over the original
// definition. Writing anything else would silently replace settings the operator
// owns — listen, user, php_admin_value — and that is not this tool's business.
func TestRenderOverridesOnlyPMSettings(t *testing.T) {
	got := string(Render([]allocate.PoolPlan{{
		Name: "shop", MaxChildren: 12,
		StartServers: 3, MinSpare: 2, MaxSpare: 6,
		Reason: "peak 9 workers busy; measured 40MiB/worker",
	}}))

	for _, want := range []string{
		"[shop]",
		"pm.max_children = 12",
		"pm.start_servers = 3",
		"pm.min_spare_servers = 2",
		"pm.max_spare_servers = 6",
		"measured 40MiB/worker", // the rationale travels with the file
		"Do not edit",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered fragment lacks %q:\n%s", want, got)
		}
	}

	// Anything that is not a pm.* setting must not appear.
	for _, unwanted := range []string{"listen", "user =", "group", "php_admin_value", "php_value"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("the fragment writes %q, which belongs to the operator:\n%s", unwanted, got)
		}
	}
}

// TestRenderOmitsSpareSettingsForStaticPools: pm.start_servers and the spare
// settings are only valid for dynamic pools. PHP-FPM refuses a config that sets
// them otherwise, which would turn a routine change into a failed reload.
func TestRenderOmitsSpareSettingsForStaticPools(t *testing.T) {
	got := string(Render([]allocate.PoolPlan{{Name: "worker", MaxChildren: 4}}))

	if strings.Contains(got, "spare") || strings.Contains(got, "start_servers") {
		t.Errorf("a static pool got dynamic settings:\n%s", got)
	}
	if !strings.Contains(got, "pm.max_children = 4") {
		t.Errorf("max_children missing:\n%s", got)
	}
}

// TestHysteresis covers when a change is worth interrupting a pool for. A reload
// is graceful but not free — workers finish their request and are replaced — so
// a host reloaded every thirty seconds spends its time cycling workers instead
// of serving.
func TestHysteresis(t *testing.T) {
	now := time.Now()
	opts := Options{}.Defaults()

	tests := map[string]struct {
		lastApplied int
		lastAt      time.Time
		planned     int
		wantAction  Action
	}{
		"never configured":      {0, time.Time{}, 10, ActionApplied},
		"identical":             {10, now.Add(-time.Hour), 10, ActionUnchanged},
		"big change, long ago":  {10, now.Add(-time.Hour), 20, ActionApplied},
		"big change, just now":  {10, now.Add(-time.Second), 20, ActionTooSoon},
		"tiny change, long ago": {20, now.Add(-time.Hour), 21, ActionTooSmall},
		"big cut, long ago":     {40, now.Add(-time.Hour), 10, ActionApplied},
		"tiny cut":              {20, now.Add(-time.Hour), 19, ActionTooSmall},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			st := state.New()
			if tt.lastApplied > 0 {
				st.RecordApplied("p", tt.lastApplied, tt.lastAt)
			}

			out, worth := decide(allocate.PoolPlan{Name: "p", MaxChildren: tt.planned}, st, opts, now)

			if out.Action != tt.wantAction {
				t.Errorf("action = %q (%s), want %q", out.Action, out.Reason, tt.wantAction)
			}
			if worth != (tt.wantAction == ActionApplied) {
				t.Errorf("worth = %v for action %q", worth, out.Action)
			}
			if out.Reason == "" {
				t.Error("no reason given; the operator cannot see why nothing happened")
			}
		})
	}
}

// TestInvalidConfigNeverReachesTheMaster is the guard the package is built
// around. PHP-FPM does not fail gracefully on a bad reload: the master refuses
// to come back and every pool it served goes down.
func TestInvalidConfigNeverReachesTheMaster(t *testing.T) {
	dir := t.TempDir()
	master := Master{
		Binary:     falseBin(t), // stands in for php-fpm rejecting the config
		ConfigPath: masterConfigAt(t, dir),
		DropInDir:  dir,
		PID:        os.Getpid(), // would be signalled if the guard failed
	}

	existing := DropInPath(dir)
	original := Render([]allocate.PoolPlan{{Name: "shop", MaxChildren: 5}})
	if err := os.WriteFile(existing, original, 0o644); err != nil {
		t.Fatal(err)
	}

	st := state.New()
	st.RecordApplied("shop", 5, time.Now().Add(-time.Hour))

	res, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "shop", MaxChildren: 50}},
	}, master, st, Options{BackupDir: filepath.Join(dir, "backup")}, nil)

	if !errors.Is(err, ErrValidationFailed) {
		t.Fatalf("err = %v, want ErrValidationFailed", err)
	}
	if res.Reloaded {
		t.Error("the master was reloaded with a configuration php-fpm had rejected")
	}

	// Nothing to roll back, because nothing was written. Validation happens
	// against a sandbox copy first, so a rejected change set never reaches the
	// directory PHP-FPM globs — not even for the length of one fork, which is
	// what the old order left open for anything else to reload into.
	if res.RolledBack {
		t.Error("a rollback happened, so the rejected fragment had been written live")
	}

	// The previous fragment must be untouched, byte for byte.
	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("the previous fragment is gone: %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("the live fragment was modified by a rejected change:\n%s", got)
	}

	// And nothing else was left in the pool directory either.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(existing) && e.Name() != "backup" {
			t.Errorf("a rejected change left %q behind in the pool directory", e.Name())
		}
	}
}

// TestRollbackRemovesAFragmentThatDidNotExist: undoing a first write means
// deleting the file, not leaving an empty one — an empty [pool] section is still
// a pool definition.
func TestRollbackRemovesAFragmentThatDidNotExist(t *testing.T) {
	dir := t.TempDir()
	// Accepts the sandbox and rejects the real tree, so the fragment IS written
	// and then has to be taken back. With a stub that rejects everything the
	// sandbox stops it first, and the test proves "never written" — true, and
	// not what its name claims.
	configPath := masterConfigAt(t, dir)
	master := Master{
		Binary:     rejectsOnly(t, configPath),
		ConfigPath: configPath,
		DropInDir:  dir,
		// Provisioning: php-fpm is not up yet, which is exactly when a pool has
		// no fragment to begin with.
		NoMasterExpected: true,
	}

	path := DropInPath(dir)

	_, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "new-pool", MaxChildren: 8}},
	}, master, state.New(), Options{BackupDir: filepath.Join(dir, "backup")}, nil)

	if !errors.Is(err, ErrValidationFailed) {
		t.Fatalf("err = %v, want ErrValidationFailed", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a rejected first write left %s behind", path)
	}
}

// TestNothingWorthChangingDoesNotReload: the common steady-state outcome. A tool
// that reloads on every run to write identical numbers is worse than no tool.
func TestNothingWorthChangingDoesNotReload(t *testing.T) {
	dir := t.TempDir()
	st := state.New()
	st.RecordApplied("a", 10, time.Now().Add(-time.Hour))
	st.RecordApplied("b", 20, time.Now().Add(-time.Hour))

	res, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{
			{Name: "a", MaxChildren: 10},
			{Name: "b", MaxChildren: 21}, // 5%, below the threshold
		},
	}, Master{
		// A binary that would fail if it were ever run: nothing should be
		// validated, because nothing should be written.
		Binary:    "/nonexistent/php-fpm",
		DropInDir: dir,
		PID:       os.Getpid(),
	}, st, Options{BackupDir: filepath.Join(dir, "backup")}, nil)

	if err != nil {
		t.Fatalf("a no-op run returned an error: %v", err)
	}
	if res.Reloaded {
		t.Error("the master was reloaded with nothing to change")
	}
	if len(res.Changed()) != 0 {
		t.Errorf("changed = %v, want none", res.Changed())
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("a no-op run wrote %d file(s)", len(entries))
	}
}

// TestDryRunValidatesButKeepsNothing: rehearsing the part that can take the host
// down is the point of a dry run.
func TestDryRunValidatesButKeepsNothing(t *testing.T) {
	dir := t.TempDir()
	path := DropInPath(dir)

	_, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "shop", MaxChildren: 12}},
	}, Master{
		Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir), DropInDir: dir,
		PID: os.Getpid(),
	}, state.New(), Options{DryRun: true, BackupDir: filepath.Join(dir, "backup")}, nil)

	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a dry run left %s behind", path)
	}
}

// TestDryRunStillReportsARejectedConfig — otherwise it rehearses nothing.
func TestDryRunStillReportsARejectedConfig(t *testing.T) {
	dir := t.TempDir()

	_, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "shop", MaxChildren: 12}},
	}, Master{
		Binary: falseBin(t), ConfigPath: masterConfigAt(t, dir), DropInDir: dir,
	}, state.New(), Options{DryRun: true, BackupDir: filepath.Join(dir, "backup")}, nil)

	if !errors.Is(err, ErrValidationFailed) {
		t.Errorf("a dry run against a rejected config returned %v", err)
	}
}

// TestNoRunningMasterWritesWithoutReloading: provisioning a host before PHP-FPM
// has started is a legitimate use, and there is nothing to signal.
func TestNoRunningMasterWritesWithoutReloading(t *testing.T) {
	dir := t.TempDir()
	st := state.New()

	res, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "shop", MaxChildren: 12}},
	}, Master{
		Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir), DropInDir: dir,
		PID: 0, NoMasterExpected: true,
	}, st, Options{BackupDir: filepath.Join(dir, "backup")}, nil)

	if err != nil {
		t.Fatalf("provisioning run: %v", err)
	}
	if res.Reloaded {
		t.Error("something was signalled with no master running")
	}

	content, err := os.ReadFile(DropInPath(dir))
	if err != nil {
		t.Fatalf("nothing was written: %v", err)
	}
	if !strings.Contains(string(content), "pm.max_children = 12") {
		t.Errorf("wrong content:\n%s", content)
	}

	// And it is recorded, so the next run's hysteresis has something to compare
	// against rather than treating it as a first configuration again.
	if ps := st.Pools["shop"]; ps == nil || ps.LastAppliedMaxChildren != 12 {
		t.Errorf("the write was not recorded: %+v", ps)
	}
}

// TestBackupsAreKeptOutOfTheConfigDirectory: PHP-FPM includes the pool directory
// by glob. A backup that matched — now, or after someone widens the pattern —
// would be loaded as configuration, defining every pool twice.
func TestBackupsAreKeptOutOfTheConfigDirectory(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backup")

	existing := DropInPath(dir)
	original := Render([]allocate.PoolPlan{{Name: "shop", MaxChildren: 5}})
	if err := os.WriteFile(existing, original, 0o644); err != nil {
		t.Fatal(err)
	}

	// Stopped at the point where the backups exist. A completed Apply removes
	// them, so asserting afterwards would find an empty directory and pass
	// whatever the code did with them in between.
	crashAfterWriting(t,
		Master{Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir), DropInDir: dir},
		backupDir, allocate.PoolPlan{Name: "shop", MaxChildren: 50})

	saved, err := os.ReadDir(backupDir)
	if err != nil || len(saved) == 0 {
		t.Fatalf("no backup was taken, so this proves nothing about where they go (err = %v)", err)
	}

	// PHP-FPM includes the pool directory by glob. A backup that landed there —
	// now, or after someone widened the pattern — would be loaded as
	// configuration, which is why the default backup directory is somewhere else
	// entirely.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(existing) {
			t.Errorf("%s appeared in the pool config directory", e.Name())
		}
	}
}

// TestSuccessfulApplyCleansUpAndRecords.
func TestSuccessfulApplyCleansUpAndRecords(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backup")
	st := state.New()

	res, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "shop", MaxChildren: 12}},
	}, Master{
		Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir), DropInDir: dir,
		NoMasterExpected: true,
	}, st, Options{BackupDir: backupDir}, nil)

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Changed()) != 1 {
		t.Fatalf("changed = %v, want one pool", res.Changed())
	}

	// Nothing transient left behind. The master sidecar is not transient: it
	// records where php-fpm lives so recovery can ask it something on a host
	// where discovery cannot — which is the host where php-fpm is down because
	// of this tool's own file.
	if entries, err := os.ReadDir(backupDir); err == nil {
		for _, e := range entries {
			if e.Name() == "master.json" {
				continue
			}
			t.Errorf("%s was left behind after a successful apply", e.Name())
		}
	}

	if ref := rememberedMaster(backupDir); ref.Binary == "" || ref.ConfigPath == "" {
		t.Errorf("nothing records where php-fpm lives: %+v — and recovery on a host whose "+
			"master will not start has no other way to find out", ref)
	}
	if ps := st.Pools["shop"]; ps == nil || ps.LastAppliedMaxChildren != 12 {
		t.Errorf("the change was not recorded: %+v", ps)
	}
}

// TestUnidentifiedMasterIsRefusedNotProvisioned is the silent total failure this
// guard exists for.
//
// PID == 0 used to mean "provisioning: write the files, there is nothing to
// reload", which is correct before PHP-FPM starts and catastrophic afterwards.
// The official php:8.3-fpm image ships `pid` commented out, so there is no pid
// file to read — the master was never identified, the files were written, no
// reload happened, and the pools were recorded as applied. The next run then
// reported "unchanged" forever. Files on disk, master untouched, no retry, and
// nothing above Info in the log.
func TestUnidentifiedMasterIsRefusedNotProvisioned(t *testing.T) {
	dir := t.TempDir()
	st := state.New()

	res, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "www", MaxChildren: 40}},
	}, Master{
		Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir), DropInDir: dir,
		// A master IS running; we simply could not find it.
		PID: 0, NoMasterExpected: false,
	}, st, Options{BackupDir: filepath.Join(dir, "backup")}, nil)

	if !errors.Is(err, ErrMasterUnknown) {
		t.Fatalf("err = %v, want ErrMasterUnknown", err)
	}
	if res.Reloaded {
		t.Error("something was reloaded with no master identified")
	}

	// Nothing may be left behind, and nothing may be recorded — a recorded
	// no-op is what made this permanent.
	if _, err := os.Stat(DropInPath(dir)); !os.IsNotExist(err) {
		t.Error("configuration was written for a master that was never reloaded")
	}
	if ps := st.Pools["www"]; ps != nil && ps.LastAppliedMaxChildren != 0 {
		t.Errorf("the no-op was recorded as applied (%d); the next run reports "+
			"'unchanged' and never retries", ps.LastAppliedMaxChildren)
	}
}

// TestUnsafePoolNamesAreRefused: pool names come from section headers in
// root-owned configuration, so this is hardening rather than a live hole — but
// php-fpm genuinely accepts a section called [a/../../tmp/pwn] and reports it
// verbatim from -tt, so the name reaching this package is not guaranteed to be a
// bare word. Joining it into a path wrote files outside the pool directory.
func TestUnsafePoolNamesAreRefused(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{
		"foo/../../etc/cron.d/pwn",
		"../../root/.ssh/authorized_keys",
		"a/b",
		"..",
		"",
		// Now that the layout is ONE file, a control character matters more
		// than a path separator does. Render writes the name into [%s], so a
		// newline ends the section header and everything after it is read as
		// directives in the file php-fpm actually loads.
		"shop\nlisten = 0.0.0.0:9000",
		"shop\rpm.max_children = 9999",
		"a\x00b",
	} {
		_, err := Apply(context.Background(), allocate.Plan{
			Pools: []allocate.PoolPlan{{Name: name, MaxChildren: 8}},
		}, Master{
			Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir),
			DropInDir: dir, NoMasterExpected: true,
		}, state.New(), Options{BackupDir: filepath.Join(dir, "backup")}, nil)

		if !errors.Is(err, ErrUnsafePoolName) {
			t.Errorf("pool name %q was accepted (err = %v)", name, err)
		}
	}

	// An ordinary name still works.
	if _, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "www-data_8.2", MaxChildren: 8}},
	}, Master{
		Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir),
		DropInDir: dir, NoMasterExpected: true,
	}, state.New(), Options{BackupDir: filepath.Join(dir, "backup")}, nil); err != nil {
		t.Errorf("an ordinary pool name was refused: %v", err)
	}
}

// TestRollbackFailureIsNotReportedAsSuccess.
//
// restore used to log and move on while the caller set RolledBack
// unconditionally, so the CLI printed "the previous configuration has been
// restored" with the configuration php-fpm had just rejected still armed in the
// pool directory. Nothing is broken at that moment — the master was never
// signalled — but the next reload from any source adopts it, and a master that
// fails to initialise does not come back.
func TestRollbackFailureIsNotReportedAsSuccess(t *testing.T) {
	dir := t.TempDir()
	path := DropInPath(dir)

	// Written as this tool writes it: the fixture stands for a previous run's
	// output, and a file without the generated header is deliberately refused.
	if err := os.WriteFile(path,
		Render([]allocate.PoolPlan{{Name: "www", MaxChildren: 5}}), 0o644); err != nil {
		t.Fatal(err)
	}

	st := state.New()
	st.RecordApplied("www", 5, time.Now().Add(-time.Hour))

	configPath := masterConfigAt(t, dir)

	res, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "www", MaxChildren: 50, Current: 5}},
	}, Master{
		// Accepts the sandbox and rejects the real tree, so the fragments are
		// written and then have to be taken back.
		Binary: rejectsOnly(t, configPath), ConfigPath: configPath,
		DropInDir: dir, PID: os.Getpid(),
	}, st, Options{BackupDir: filepath.Join(dir, "backup")}, nil)

	if !errors.Is(err, ErrValidationFailed) {
		t.Fatalf("err = %v, want ErrValidationFailed", err)
	}
	// This directory is writable, so the rollback should have worked — and then
	// RolledBack is the truth rather than an assumption.
	if !res.RolledBack {
		t.Errorf("RolledBack = false with RollbackFailed = %v", res.RollbackFailed)
	}
	if len(res.RollbackFailed) != 0 {
		t.Errorf("RollbackFailed = %v on a writable directory", res.RollbackFailed)
	}

	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "pm.max_children = 5") {
		t.Errorf("the previous fragment was not restored:\n%s", got)
	}
}

// masterConfigAt writes a master config that includes dir, the way a real one
// does. The sandbox reads it to build its copy, so a test that points at a path
// with no file behind it is not exercising the real path.
func masterConfigAt(t *testing.T, dir string) string {
	t.Helper()

	// Written OUTSIDE the pool directory, as on a real host: /etc/php-fpm.conf
	// includes /etc/php-fpm.d/*.conf. A master config sitting inside the
	// directory it globs would include itself.
	path := filepath.Join(t.TempDir(), "php-fpm.conf")
	body := "[global]\ninclude = " + filepath.Join(dir, "*.conf") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

// rejectsOnly is a php-fpm stand-in that accepts every config EXCEPT one.
//
// Needed because validation now happens twice against two different paths: once
// against a sandbox copy, and once against the real tree after the fragments are
// written. A stub that rejects everything never gets past the sandbox, so it can
// no longer reach — or test — the rollback path.
func rejectsOnly(t *testing.T, configPath string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "php-fpm-stub")
	script := "#!/bin/sh\nfor a in \"$@\"; do\n  [ \"$a\" = \"" + configPath + "\" ] && exit 78\ndone\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	return path
}
