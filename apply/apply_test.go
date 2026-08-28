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
	got := string(Render(allocate.PoolPlan{
		Name: "shop", MaxChildren: 12,
		StartServers: 3, MinSpare: 2, MaxSpare: 6,
		Reason: "peak 9 workers busy; measured 40MiB/worker",
	}))

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
	got := string(Render(allocate.PoolPlan{Name: "worker", MaxChildren: 4}))

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
		ConfigPath: filepath.Join(dir, "php-fpm.conf"),
		DropInDir:  dir,
		PID:        os.Getpid(), // would be signalled if the guard failed
	}

	existing := DropInPath(dir, "shop")
	if err := os.WriteFile(existing, []byte("[shop]\npm.max_children = 5\n"), 0o644); err != nil {
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
	if !res.RolledBack {
		t.Error("the change was not rolled back")
	}

	// The previous fragment must be exactly as it was.
	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("the previous fragment is gone: %v", err)
	}
	if !strings.Contains(string(got), "pm.max_children = 5") {
		t.Errorf("the previous fragment was not restored:\n%s", got)
	}
}

// TestRollbackRemovesAFragmentThatDidNotExist: undoing a first write means
// deleting the file, not leaving an empty one — an empty [pool] section is still
// a pool definition.
func TestRollbackRemovesAFragmentThatDidNotExist(t *testing.T) {
	dir := t.TempDir()
	master := Master{
		Binary:     falseBin(t),
		ConfigPath: filepath.Join(dir, "php-fpm.conf"),
		DropInDir:  dir,
	}

	path := DropInPath(dir, "new-pool")

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
	path := DropInPath(dir, "shop")

	_, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "shop", MaxChildren: 12}},
	}, Master{
		Binary: trueBin(t), ConfigPath: filepath.Join(dir, "php-fpm.conf"), DropInDir: dir,
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
		Binary: falseBin(t), ConfigPath: filepath.Join(dir, "php-fpm.conf"), DropInDir: dir,
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
		Binary: trueBin(t), ConfigPath: filepath.Join(dir, "php-fpm.conf"), DropInDir: dir,
		PID: 0,
	}, st, Options{BackupDir: filepath.Join(dir, "backup")}, nil)

	if err != nil {
		t.Fatalf("provisioning run: %v", err)
	}
	if res.Reloaded {
		t.Error("something was signalled with no master running")
	}

	content, err := os.ReadFile(DropInPath(dir, "shop"))
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

	existing := DropInPath(dir, "shop")
	if err := os.WriteFile(existing, []byte("[shop]\npm.max_children = 5\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st := state.New()
	st.RecordApplied("shop", 5, time.Now().Add(-time.Hour))

	_, _ = Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "shop", MaxChildren: 50}},
	}, Master{
		Binary: falseBin(t), ConfigPath: filepath.Join(dir, "php-fpm.conf"), DropInDir: dir,
	}, st, Options{BackupDir: backupDir}, nil)

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
		Binary: trueBin(t), ConfigPath: filepath.Join(dir, "php-fpm.conf"), DropInDir: dir,
	}, st, Options{BackupDir: backupDir}, nil)

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Changed()) != 1 {
		t.Fatalf("changed = %v, want one pool", res.Changed())
	}

	if entries, err := os.ReadDir(backupDir); err == nil && len(entries) > 0 {
		t.Errorf("%d backup(s) left behind after a successful apply", len(entries))
	}
	if ps := st.Pools["shop"]; ps == nil || ps.LastAppliedMaxChildren != 12 {
		t.Errorf("the change was not recorded: %+v", ps)
	}
}
