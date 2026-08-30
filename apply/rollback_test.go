package apply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	st.RecordApplied("", "www", 5, time.Now().Add(-time.Hour))

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
	st.RecordApplied("", "www", 5, time.Now().Add(-time.Hour))

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
	st.RecordApplied("", "www", 5, time.Now().Add(-time.Hour))
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

// TestTheDropInIsReplacedByARenameNotARewrite.
//
// "Indivisible" is a claim about the write, not about the layout. php-fpm globs
// the pool directory and reads whatever it finds; a rewrite in place means a
// reload landing mid-write reads half a file, and a crash mid-write leaves one
// permanently. A rename makes the change a single step: the old file or the new
// one, never part of either.
//
// The test that carried this claim asserted that both pools ended up in one
// file, which is a fact about Render. Replacing writeAtomic with a plain
// os.WriteFile left the package green.
//
// Checked by inode, because that is the only observable difference: a rename
// installs a new file over the name, an in-place rewrite keeps the same one.
func TestTheDropInIsReplacedByARenameNotARewrite(t *testing.T) {
	dir := t.TempDir()
	path := DropInPath(dir)

	if err := writeAtomic(path, Render([]allocate.PoolPlan{{Name: "www", MaxChildren: 5}})); err != nil {
		t.Fatal(err)
	}
	before := inodeOf(t, path)

	if err := writeAtomic(path, Render([]allocate.PoolPlan{{Name: "www", MaxChildren: 9}})); err != nil {
		t.Fatal(err)
	}
	if after := inodeOf(t, path); after == before {
		t.Error("the second write kept the same inode: the file was rewritten in place, " +
			"so a reload landing mid-write reads half a configuration and a crash leaves " +
			"one on disk")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "pm.max_children = 9") {
		t.Errorf("the new content did not land:\n%s", got)
	}
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no inode available on this platform")
	}

	return uint64(sys.Ino)
}

// TestDeletingTheFileUndoesEverything.
//
// The README's promise, and the thing anyone reaching for rm expects: "deleting
// that file returns everything to what you configured".
//
// It did not. When a pool was absent from the live file, overrideSet took its
// last applied size out of the state file and wrote it back — so deleting the
// file and running the tool once put every override back. The state was
// consulted for "the window between a successful apply and the file being read
// again", and there is no such window: overrideSet reads the file as it stands,
// and after a successful apply the file contains the pool.
func TestDeletingTheFileUndoesEverything(t *testing.T) {
	dir := t.TempDir()
	configPath := masterConfigAt(t, dir)

	// A previous run sized three pools, and the state remembers all three.
	st := state.New()
	for _, pool := range []string{"shop", "forum", "blog"} {
		st.RecordApplied("", pool, 30, time.Now().Add(-time.Hour))
	}

	// The operator deleted the drop-in. Nothing is ours any more.
	if _, err := os.Stat(DropInPath(dir)); !os.IsNotExist(err) {
		t.Fatal("setup: the drop-in should not exist")
	}

	// One pool now genuinely needs a change.
	_, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{
			{Name: "shop", MaxChildren: 12, Current: 4},
			{Name: "forum", MaxChildren: 8, Current: 8},
			{Name: "blog", MaxChildren: 6, Current: 6},
		},
	}, Master{
		Binary: trueBin(t), ConfigPath: configPath,
		DropInDir: dir, NoMasterExpected: true,
	}, st, Options{BackupDir: filepath.Join(t.TempDir(), "backup")}, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	body, err := os.ReadFile(DropInPath(dir))
	if err != nil {
		t.Fatalf("nothing was written: %v", err)
	}

	// shop changed, so it is written. The other two were not ours any more and
	// are not being changed, so nothing about them belongs in this file.
	if !strings.Contains(string(body), "[shop]") {
		t.Errorf("the pool that changed is missing:\n%s", body)
	}
	for _, resurrected := range []string{"[forum]", "[blog]"} {
		if strings.Contains(string(body), resurrected) {
			t.Errorf("%s came back from the state file after the drop-in was deleted; "+
				"the one documented way to undo everything this tool has done does not "+
				"work:\n%s", resurrected, body)
		}
	}
}

// TestARecordThatCannotBeWrittenStopsTheReload.
//
// The recovery record's whole job is to describe what MIGHT have happened, and
// it is read by the next start to choose between finishing a change and undoing
// it. Its write was attempted and its error thrown away — so a full disk left
// the record saying "written, not signalled" while the master was signalled a
// line later. A crash in the settle window would then be recovered as "nobody
// was told", and the configuration php-fpm had already adopted discarded as
// unfinished work.
//
// Not signalling is the safe half of the trade: nothing has been reloaded, the
// previous file goes back, and the operator gets a disk-full error instead of a
// host recovered against a record that was never true.
func TestARecordThatCannotBeWrittenStopsTheReload(t *testing.T) {
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

	// The backup directory is sealed BETWEEN the two writes that go into it.
	//
	// Sealing it from the start fails at the backup instead, which is a
	// different and already-safe path — nothing has been written, so there is
	// nothing to undo. What has to be staged is a backup that succeeded and a
	// phase update that cannot. Validation of the real tree runs between them.
	backupDir := filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(backupDir, 0o755) })

	st := state.New()
	st.RecordApplied("", "www", 5, time.Now().Add(-time.Hour))
	stubbed := fakeMasterWithLog(t, configPath)

	res, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "www", MaxChildren: 50, Current: 5}},
	}, Master{
		Binary: sealsThenAccepts(t, configPath, backupDir), ConfigPath: configPath,
		DropInDir: dir, PID: stubbed.pid,
	}, st, Options{BackupDir: backupDir, SettleTime: 200 * time.Millisecond}, nil)

	if err == nil {
		t.Fatal("the reload went ahead with no durable record of it")
	}
	if n := stubbed.signalsSeen(t); n != 0 {
		t.Errorf("the master was signalled %d times with no record that it would be; a "+
			"crash in the settle window is then recovered as if nothing had happened", n)
	}
	if !res.RolledBack {
		t.Errorf("the change was left on disk: RolledBack = false, RollbackFailed = %v",
			res.RollbackFailed)
	}

	body, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("the previous file is gone: %v", rerr)
	}
	if !strings.Contains(string(body), "pm.max_children = 5") {
		t.Errorf("the previous configuration was not put back:\n%s", body)
	}
}

// TestAnUnreachablePoolKeepsTheCeilingWeAlreadySetForIt.
//
// Unknown means "do not change this pool". It used to mean "delete the ceiling
// this tool already set for it": overrideSet skipped unknown pools BEFORE
// checking whether they were in the file, so a pool whose scrape failed while a
// neighbour was being resized had its section dropped from the rewritten file.
//
// The reload then returns it to whatever its own config says — which is the
// number this tool was lowering it from. The plan reserved six workers and the
// host got fifty, none of them budgeted.
func TestAnUnreachablePoolKeepsTheCeilingWeAlreadySetForIt(t *testing.T) {
	dir := t.TempDir()
	configPath := masterConfigAt(t, dir)

	// A previous run set both.
	if err := os.WriteFile(DropInPath(dir), Render([]allocate.PoolPlan{
		{Name: "shop", MaxChildren: 6},
		{Name: "api", MaxChildren: 8},
	}), 0o644); err != nil {
		t.Fatal(err)
	}

	st := state.New()
	st.RecordApplied("", "shop", 6, time.Now().Add(-time.Hour))
	st.RecordApplied("", "api", 8, time.Now().Add(-time.Hour))

	// This round shop could not be read, and api is being resized.
	_, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{
			{Name: "shop", MaxChildren: 6, Current: 6, Unknown: true},
			{Name: "api", MaxChildren: 12, Current: 8},
		},
	}, Master{
		Binary: trueBin(t), ConfigPath: configPath,
		DropInDir: dir, NoMasterExpected: true,
	}, st, Options{BackupDir: filepath.Join(t.TempDir(), "backup")}, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	body, rerr := os.ReadFile(DropInPath(dir))
	if rerr != nil {
		t.Fatalf("nothing was written: %v", rerr)
	}
	if !strings.Contains(string(body), "[shop]") {
		t.Errorf("the unreachable pool's existing override was dropped while a neighbour "+
			"was resized; the next reload returns it to its own config and those workers "+
			"were never budgeted:\n%s", body)
	}
	if !strings.Contains(string(body), "pm.max_children = 6") {
		t.Errorf("the unreachable pool's ceiling was changed rather than kept:\n%s", body)
	}
}

// TestASignalledChangeEditedSinceIsNotTreatedAsNeverHavingHappened.
//
// A record whose hash does not match the file on disk used to be read as "the
// rename never happened, nothing to undo" — for both phases. After a SIGNAL
// that is the wrong reading. The master may well have adopted the change, and a
// mismatch then means something else edited the file afterwards without
// reloading: an operator, a config-management run, another tool.
//
// Discarding there threw away the only record saying a master might be running
// on a configuration nobody can see any more. What is on disk is what the next
// reload adopts, so the question worth asking is whether php-fpm accepts it.
func TestASignalledChangeEditedSinceIsNotTreatedAsNeverHavingHappened(t *testing.T) {
	stage := func(t *testing.T, binary string) (string, error) {
		t.Helper()

		dir := t.TempDir()
		backupDir := filepath.Join(t.TempDir(), "backup")
		configPath := masterConfigAt(t, dir)

		if err := os.WriteFile(DropInPath(dir),
			Render([]allocate.PoolPlan{{Name: "www", MaxChildren: 10}}), 0o644); err != nil {
			t.Fatal(err)
		}

		master := Master{Binary: binary, ConfigPath: configPath, DropInDir: dir}
		crashAfterWriting(t, master, backupDir, allocate.PoolPlan{Name: "www", MaxChildren: 40})
		b := backup{
			path: DropInPath(dir), existed: true,
			saved: filepath.Join(backupDir, backupName(dir, DropInPath(dir))),
		}
		if err := markSignalled(backupDir, master, b,
			Render([]allocate.PoolPlan{{Name: "www", MaxChildren: 40}})); err != nil {
			t.Fatal(err)
		}

		// Someone edits the file afterwards and does not reload. The hash in the
		// record now matches nothing.
		if err := os.WriteFile(DropInPath(dir),
			Render([]allocate.PoolPlan{{Name: "www", MaxChildren: 12}}), 0o644); err != nil {
			t.Fatal(err)
		}

		// A master is running, so this is not the provisioning path.
		master.PID = fakeMaster(t, configPath)

		_, err := Reconcile(context.Background(), master, Options{BackupDir: backupDir}, nil)

		body, rerr := os.ReadFile(DropInPath(dir))
		if rerr != nil {
			t.Fatalf("the file is gone: %v", rerr)
		}

		return string(body), err
	}

	t.Run("what is there now is valid", func(t *testing.T) {
		body, err := stage(t, trueBin(t))
		if err != nil {
			t.Errorf("Reconcile: %v", err)
		}
		if !strings.Contains(body, "pm.max_children = 12") {
			t.Errorf("the edit was overwritten by a backup taken before it:\n%s", body)
		}
	})

	t.Run("what is there now is rejected", func(t *testing.T) {
		_, err := stage(t, alwaysRejects(t))
		if !errors.Is(err, ErrUnreconciled) {
			t.Errorf("err = %v, want ErrUnreconciled: the master was signalled, the file "+
				"has been changed since, and php-fpm will not accept what is there — the "+
				"one state where saying nothing is worst", err)
		}
	})
}

// TestTheRecordIsDurableOrTheReloadDoesNotHappen.
//
// The record's whole value is that the NEXT start can read it. A rename this
// process believes happened and the kernel has not committed leaves recovery
// deciding on a record that is not there — so the directory flush is strict for
// the record, where it is best-effort for everything else.
//
// Staged with a directory that can be written but not read, which is what a
// failing directory sync looks like from here.
func TestTheRecordIsDurableOrTheReloadDoesNotHappen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory without read permission")
	}

	backupDir := filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write and traverse, but not read: the temp file lands and the rename
	// succeeds, and flushing the directory entry cannot be confirmed.
	if err := os.Chmod(backupDir, 0o333); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(backupDir, 0o755) })

	err := writeTransaction(backupDir, transaction{
		DropInDir: "/etc/php-fpm.d", Path: "/etc/php-fpm.d/zz-fpm-tune.conf",
		Phase: PhaseSignalled, Wrote: "abc",
	})
	if err == nil {
		t.Error("a record whose directory entry could not be flushed was reported as " +
			"written; the reload then goes ahead on a promise the kernel has not made")
	}
}

// TestRecoveryDoesNotUndoTheFirstApplyOnAHostWithNoMaster.
//
// The measured case. On a host where this was the FIRST apply there is no
// previous version of the file, so "restoring the previous configuration" meant
// removing the drop-in outright — and every pool went back to its own
// pm.max_children, which is the overcommit this tool exists to prevent,
// performed by its own recovery.
//
// It needs no crash to reach. A SIGTERM inside the settle window leaves the
// record open on purpose, and a reboot that starts fpm-tune before php-fpm does
// the rest.
func TestRecoveryDoesNotUndoTheFirstApplyOnAHostWithNoMaster(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backup")
	configPath := masterConfigAt(t, dir)
	master := Master{Binary: trueBin(t), ConfigPath: configPath, DropInDir: dir}

	// Nothing there before: the first apply this host ever had.
	if _, err := os.Stat(DropInPath(dir)); !os.IsNotExist(err) {
		t.Fatal("setup: the drop-in should not exist yet")
	}

	crashAfterWriting(t, master, backupDir, allocate.PoolPlan{Name: "www", MaxChildren: 40})
	if err := markSignalled(backupDir, master,
		backup{path: DropInPath(dir), existed: false},
		Render([]allocate.PoolPlan{{Name: "www", MaxChildren: 40}})); err != nil {
		t.Fatal(err)
	}

	_, err := Reconcile(context.Background(), master, Options{BackupDir: backupDir}, nil)
	if !errors.Is(err, ErrUnreconciled) {
		t.Fatalf("err = %v, want ErrUnreconciled", err)
	}

	if _, serr := os.Stat(DropInPath(dir)); serr != nil {
		t.Fatalf("recovery removed the whole override file, so every pool is back at its "+
			"own ceiling and nothing on this host is budgeted: %v", serr)
	}
}

// TestARejectedLeftoverWithNoBackupIsStillRepaired.
//
// The dead end. A rejected leftover whose saved copy has gone — cleaned by a
// tmpfiles rule, or an operator tidying /var/lib/fpm-tune — used to end the
// story: the host is down BECAUSE of this tool's file, and the repair that
// would take it out runs only when there is no record at all. `apply` exited
// before it could plan, `serve` published apply_blocked and never applied
// again, and nothing but a person cleared it.
//
// Removing the file is the same trade the repair path already makes, rehearsed
// the same way: a running master on the settings the operator configured beats
// a master that is not running.
func TestARejectedLeftoverWithNoBackupIsStillRepaired(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backup")
	configPath := masterConfigAt(t, dir)

	// Rejects while our file is present, accepts once it is gone: the shape of
	// a pool that exists only as an override, which is what a removed site
	// leaves behind.
	master := Master{Binary: rejectsOurFile(t), ConfigPath: configPath, DropInDir: dir}

	// A previous version has to EXIST, or "the previous configuration is gone"
	// is not the state under test: with nothing there before, the ordinary undo
	// simply removes the file and never reaches the dead end.
	if err := os.WriteFile(DropInPath(dir),
		Render([]allocate.PoolPlan{{Name: "shop", MaxChildren: 6}}), 0o644); err != nil {
		t.Fatal(err)
	}

	crashAfterWriting(t, master, backupDir, allocate.PoolPlan{Name: "gone", MaxChildren: 4})

	// The saved copy is removed, which is the whole point of the case.
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".bak") {
			if rerr := os.Remove(filepath.Join(backupDir, e.Name())); rerr != nil {
				t.Fatal(rerr)
			}
		}
	}

	acted, rerr := Reconcile(context.Background(), master, Options{BackupDir: backupDir}, nil)
	if rerr != nil {
		t.Fatalf("Reconcile: %v", rerr)
	}
	if !acted {
		t.Error("Reconcile reported doing nothing while removing a file")
	}

	if _, serr := os.Stat(DropInPath(dir)); !os.IsNotExist(serr) {
		t.Errorf("the rejected file is still there, so php-fpm still will not start "+
			"and nothing will ever clear it: %v", serr)
	}
	if _, found, terr := readTransaction(backupDir, dir); found || terr != nil {
		t.Errorf("the record was left behind (found=%v err=%v), so every future run "+
			"reconciles it again", found, terr)
	}
}

// TestTheRepairWorksWithNothingButTheBackupDirectory.
//
// The host this repair exists for is one where php-fpm will not start because
// of this tool's own file. On that host discovery finds nothing — there is no
// master to discover — so the caller has no binary and no config path, and
// repairIfOursIsBroken returned immediately without doing anything. If the
// state file was also missing or from an older version, nothing anywhere knew
// where php-fpm lived and the repair silently no-opped.
//
// The sidecar written beside the backups on every successful apply closes that:
// it is written by the code that has just proved the master is real, and it
// survives the state file being deleted to reset the baselines.
func TestTheRepairWorksWithNothingButTheBackupDirectory(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backup")
	configPath := masterConfigAt(t, dir)
	binary := rejectsOurFile(t)

	// A successful apply on a host where the tool's file is fine, which is what
	// leaves the sidecar. trueBin accepts everything.
	if _, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "shop", MaxChildren: 6}},
	}, Master{
		Binary: trueBin(t), ConfigPath: configPath, DropInDir: dir,
		NoMasterExpected: true,
	}, state.New(), Options{BackupDir: backupDir}, nil); err != nil {
		t.Fatalf("setting up: %v", err)
	}

	// Then a site is removed, php-fpm will no longer start, and nothing can be
	// discovered. This is the Master the CLI builds in that situation: a
	// directory and nothing else.
	blind := Master{DropInDir: dir}

	// The sidecar names a binary that accepts everything; point it at the one
	// that rejects while our file is present, which is the removed-site shape.
	rememberMaster(backupDir, Master{Binary: binary, ConfigPath: configPath, DropInDir: dir})

	acted, err := Reconcile(context.Background(), blind, Options{BackupDir: backupDir}, nil)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !acted {
		t.Fatal("the repair did nothing at all: with no binary and no config path it " +
			"cannot ask php-fpm anything, so a host that is down because of this tool's " +
			"file stays down and nothing but a person clears it")
	}
	if _, serr := os.Stat(DropInPath(dir)); !os.IsNotExist(serr) {
		t.Errorf("this tool's file is still there: %v", serr)
	}
}

// TestARepairDoesNotUseAnotherMastersNote.
//
// The note recording where php-fpm lives was one unkeyed file per backup
// directory — and the backup directory has a default, so two masters share one.
// The last successful apply overwrote it, and a repair for the OTHER master
// then filled in that one's binary and config: it validated a tree it was not
// about to touch, found it fine, and returned without repairing the host that
// was actually down.
func TestARepairDoesNotUseAnotherMastersNote(t *testing.T) {
	backupDir := filepath.Join(t.TempDir(), "backup")

	healthy := t.TempDir()
	broken := t.TempDir()

	// The healthy master applied last, so under the old scheme its note is the
	// only one there.
	rememberMaster(backupDir, Master{
		Binary: "/usr/sbin/php-fpm8.3", ConfigPath: "/etc/php/8.3/php-fpm.conf",
		DropInDir: healthy,
	})

	if ref := rememberedMaster(backupDir, broken); ref.Binary != "" {
		t.Errorf("a repair for %s was handed %s's php-fpm (%q); it would validate a tree "+
			"it is not about to touch, find it fine, and leave the broken host alone",
			broken, healthy, ref.Binary)
	}

	// Its own note still comes back.
	rememberMaster(backupDir, Master{
		Binary: "/usr/sbin/php-fpm8.2", ConfigPath: "/etc/php/8.2/php-fpm.conf",
		DropInDir: broken,
	})
	if ref := rememberedMaster(backupDir, broken); ref.Binary != "/usr/sbin/php-fpm8.2" {
		t.Errorf("the master's own note did not come back: %+v", ref)
	}
	// And the other's is untouched, which the single-file scheme could not do.
	if ref := rememberedMaster(backupDir, healthy); ref.Binary != "/usr/sbin/php-fpm8.3" {
		t.Errorf("writing one master's note overwrote another's: %+v", ref)
	}
}

// TestADaemonWithNoDirectoryOnlyGuessesWhenThereIsOneAnswer.
//
// A daemon started with no --drop-in-dir has nothing to reconcile without a
// note, and the note is the only thing between the host and being repaired. But
// with several masters remembered there is no way to tell which one is down,
// and guessing points a repair at a master that is fine.
func TestADaemonWithNoDirectoryOnlyGuessesWhenThereIsOneAnswer(t *testing.T) {
	backupDir := filepath.Join(t.TempDir(), "backup")
	only := t.TempDir()

	rememberMaster(backupDir, Master{
		Binary: "/usr/sbin/php-fpm", ConfigPath: "/etc/php-fpm.conf", DropInDir: only,
	})
	if got := RememberedMaster(backupDir, "").DropInDir; got != only {
		t.Errorf("with exactly one master remembered, the daemon found %q, want %q", got, only)
	}

	rememberMaster(backupDir, Master{
		Binary: "/usr/sbin/php-fpm8.2", ConfigPath: "/etc/php/8.2/php-fpm.conf",
		DropInDir: t.TempDir(),
	})
	if got := RememberedMaster(backupDir, "").DropInDir; got != "" {
		t.Errorf("with two masters remembered the daemon picked %q; there is no way to "+
			"tell which one is down, and repairing the wrong one is its own outage", got)
	}
}

// TestAnUnreadableExistingFileIsNotTreatedAsNoFile.
//
// A file that EXISTS and cannot be read is not the same as no file, and
// treating them alike is how a rollback DELETES a configuration instead of
// putting it back: `existed` stays false, so the undo path removes the file
// rather than restoring its contents.
//
// What this test proves is that such a run is REFUSED — by parseOurs, which
// reaches the file first. The same reasoning is repeated at the backup step for
// the window between the two, and nothing can stage that race from a test; the
// comment there says so rather than leaving it looking covered.
func TestAnUnreadableExistingFileIsNotTreatedAsNoFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file with mode 000")
	}

	dir := t.TempDir()
	configPath := masterConfigAt(t, dir)
	path := DropInPath(dir)

	if err := os.WriteFile(path,
		Render([]allocate.PoolPlan{{Name: "www", MaxChildren: 5}}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	_, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "www", MaxChildren: 50, Current: 5}},
	}, Master{
		Binary: trueBin(t), ConfigPath: configPath, DropInDir: dir,
		NoMasterExpected: true,
	}, state.New(), Options{BackupDir: filepath.Join(t.TempDir(), "backup")}, nil)

	if err == nil {
		t.Fatal("a file that could not be read was written over as though it were not " +
			"there; a rollback would now delete the configuration rather than restore it")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the error does not name the file it could not read:\n%v", err)
	}
}
