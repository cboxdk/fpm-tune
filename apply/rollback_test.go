package apply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/state"
)

// TestAMasterThatDoesNotComeBackIsPutBack.
//
// The promise this package is built around, and until now nothing executed it.
// Every stub master survived its reload, so the whole branch — restore the
// previous file, reload again to bring the master up on it, report which of the
// two happened — was reachable only in production.
//
// The scenario is the one that matters most: php-fpm accepted the configuration
// at validation and then failed to initialise on it anyway. Validation forks a
// separate process with no sockets to bind and no pools to start, so the two
// answers genuinely differ, and the difference is a master that is gone.
func TestAMasterThatDoesNotComeBackIsPutBack(t *testing.T) {
	dir := t.TempDir()
	configPath := masterConfigAt(t, dir)
	path := DropInPath(dir)

	// A previous run's file, so there is something to put back.
	previous := Render([]allocate.PoolPlan{{Name: "www", MaxChildren: 5}})
	if err := os.WriteFile(path, previous, 0o644); err != nil {
		t.Fatal(err)
	}

	st := state.New()
	st.RecordApplied("www", 5, time.Now().Add(-time.Hour))

	dying := stubMaster(t, configPath, true)

	res, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "www", MaxChildren: 50, Current: 5}},
	}, Master{
		// Accepts everything. The rejection is not what kills this master; the
		// reload is.
		Binary: trueBin(t), ConfigPath: configPath, DropInDir: dir, PID: dying.pid,
	}, st, Options{
		BackupDir:  filepath.Join(t.TempDir(), "backup"),
		SettleTime: 500 * time.Millisecond,
	}, nil)

	if !errors.Is(err, ErrMasterDidNotSurvive) {
		t.Fatalf("err = %v, want ErrMasterDidNotSurvive: a master that is gone was "+
			"reported as a successful apply", err)
	}
	if !res.RolledBack {
		t.Errorf("RolledBack = false with RollbackFailed = %v; the file that killed the "+
			"master is still what the next start will read", res.RollbackFailed)
	}
	if len(res.RollbackFailed) != 0 {
		t.Errorf("RollbackFailed = %v on a writable directory", res.RollbackFailed)
	}

	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("the previous file was not put back: %v", rerr)
	}
	if !strings.Contains(string(got), "pm.max_children = 5") {
		t.Errorf("the 50-worker change is still on disk:\n%s", got)
	}
}

// TestARollbackThatCannotWriteSaysSo.
//
// restore used to log and move on while the caller set RolledBack
// unconditionally, so the CLI printed "the previous configuration has been
// restored" with the configuration php-fpm had just rejected still armed in the
// pool directory. Nothing is broken at that moment — the master was never
// signalled — but the next reload from any source adopts it, and a master that
// fails to initialise does not come back.
//
// The existing test for this ran against a writable directory, so the restore
// succeeded and it asserted the success path. Reverting restore to log-and-move
// on left the entire package green: RollbackFailed drives the loudest message
// the CLI can print, and no test ever put a value in it.
func TestARollbackThatCannotWriteSaysSo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only directory, so the failure cannot be staged")
	}

	dir := t.TempDir()
	configPath := masterConfigAt(t, dir)
	path := DropInPath(dir)

	if err := os.WriteFile(path,
		Render([]allocate.PoolPlan{{Name: "www", MaxChildren: 5}}), 0o644); err != nil {
		t.Fatal(err)
	}
	// Restored whatever happens, so the directory can be removed at the end.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	st := state.New()
	st.RecordApplied("www", 5, time.Now().Add(-time.Hour))

	res, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "www", MaxChildren: 50, Current: 5}},
	}, Master{
		Binary: rejectsAndSeals(t, configPath, dir), ConfigPath: configPath,
		DropInDir: dir, PID: os.Getpid(),
	}, st, Options{BackupDir: filepath.Join(t.TempDir(), "backup")}, nil)

	if !errors.Is(err, ErrValidationFailed) {
		t.Fatalf("err = %v, want ErrValidationFailed", err)
	}
	if len(res.RollbackFailed) == 0 {
		t.Fatalf("RollbackFailed is empty after a restore into a read-only directory; "+
			"the operator is told the previous configuration is back (RolledBack = %v) "+
			"while the rejected one is still what php-fpm will read", res.RolledBack)
	}
	if res.RolledBack {
		t.Error("RolledBack = true when the restore could not write: this is the exact " +
			"claim that made the CLI print a reassurance it had not earned")
	}
	if got := res.RollbackFailed[0]; got != path {
		t.Errorf("RollbackFailed names %q, want the drop-in path %q — an operator has to "+
			"be told which file to deal with", got, path)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the error does not name the file left behind:\n%v", err)
	}
}

// TestAMasterGoneAndAFileThatCannotBeRemovedIsSaidPlainly.
//
// The worst state this tool can leave a host in, and the one an operator most
// needs named: the master is gone, and the configuration that killed it is
// still what the next start will read. Nothing but the error text and
// RollbackFailed distinguishes it from the ordinary rollback, where the host is
// one systemctl start away from fine.
//
// Reverting that branch to log-and-move-on left the package green, because the
// only test touching RollbackFailed asserted it was EMPTY.
func TestAMasterGoneAndAFileThatCannotBeRemovedIsSaidPlainly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only directory, so the failure cannot be staged")
	}

	dir := t.TempDir()
	configPath := masterConfigAt(t, dir)
	path := DropInPath(dir)

	if err := os.WriteFile(path,
		Render([]allocate.PoolPlan{{Name: "www", MaxChildren: 5}}), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	st := state.New()
	st.RecordApplied("www", 5, time.Now().Add(-time.Hour))
	dying := stubMaster(t, configPath, true)

	res, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "www", MaxChildren: 50, Current: 5}},
	}, Master{
		// Accepts everything, and seals the directory on its way past — so the
		// write lands, the validation passes, the master is signalled and dies,
		// and the restore that follows cannot write.
		Binary: sealsThenAccepts(t, configPath, dir), ConfigPath: configPath,
		DropInDir: dir, PID: dying.pid,
	}, st, Options{
		BackupDir:  filepath.Join(t.TempDir(), "backup"),
		SettleTime: 500 * time.Millisecond,
	}, nil)

	if !errors.Is(err, ErrMasterDidNotSurvive) {
		t.Fatalf("err = %v, want ErrMasterDidNotSurvive", err)
	}
	if len(res.RollbackFailed) == 0 {
		t.Fatalf("RollbackFailed is empty: the master is gone and the file that killed "+
			"it is still armed, and the only thing saying so is RolledBack = %v",
			res.RolledBack)
	}
	if res.RolledBack {
		t.Error("RolledBack = true when the restore could not write")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the error does not name the file an operator has to remove by hand:\n%v", err)
	}
}

// sealsThenAccepts makes the pool directory read-only when it is asked to
// validate the real tree, and accepts anyway — so the failure arrives later, at
// the reload, with the restore already unable to write.
func sealsThenAccepts(t *testing.T, configPath, sealDir string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "php-fpm-stub")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  [ \"$a\" = \"" + configPath + "\" ] && chmod 0555 \"" + sealDir + "\"\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	return path
}

// rejectsAndSeals accepts the sandbox, then makes the pool directory read-only
// before rejecting the real tree — so the fragments are written, the validation
// fails, and the restore that follows cannot write.
//
// The order matters and is the only reason this is stageable: validation of the
// real tree runs after the live write and before the restore.
func rejectsAndSeals(t *testing.T, configPath, sealDir string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "php-fpm-stub")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$a\" = \"" + configPath + "\" ]; then\n" +
		"    chmod 0555 \"" + sealDir + "\"\n" +
		"    exit 78\n" +
		"  fi\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	return path
}
