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

// TestDryRunNeverTouchesTheLiveDirectory.
//
// The old dry run wrote the real fragments, validated, and restored them —
// so the one mode whose entire promise is "this changes nothing" placed
// unvalidated configuration in the directory PHP-FPM globs, and left it there
// for as long as the fork took. An unrelated reload in that window adopted it.
// A crash in that window left it permanently.
func TestDryRunNeverTouchesTheLiveDirectory(t *testing.T) {
	dir := t.TempDir()
	existing := DropInPath(dir, "shop")
	original := "[shop]\npm.max_children = 5\n"

	if err := os.WriteFile(existing, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{
			{Name: "shop", MaxChildren: 50, Current: 5},
			{Name: "blog", MaxChildren: 20, Current: 4},
		},
	}, Master{
		Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir),
		DropInDir: dir, PID: os.Getpid(),
	}, state.New(), Options{DryRun: true, BackupDir: filepath.Join(dir, "backup")}, nil); err != nil {
		t.Fatalf("dry run: %v", err)
	}

	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("the dry run removed an existing fragment: %v", err)
	}
	if string(got) != original {
		t.Errorf("the dry run rewrote a live fragment:\n%s", got)
	}

	// Rewriting the same bytes back would pass the check above, so the mtime is
	// checked too: a dry run must not have opened the file for writing at all.
	after, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("the dry run rewrote a live fragment with identical content")
	}

	if _, err := os.Stat(DropInPath(dir, "blog")); !os.IsNotExist(err) {
		t.Error("the dry run created a fragment for a pool that had none")
	}
}

// TestShrinksGoWithTheGrowthTheyPayFor.
//
// The allocator divides ONE budget, so a pool being cut and a pool being grown
// in the same plan are two halves of one decision. Filtering them independently
// broke that: the growth cleared the hysteresis threshold, the matching cut was
// a few percent and did not, and what reached the host was the half that spends
// memory without the half that frees it. A plan that fit the budget was applied
// as one that does not.
func TestShrinksGoWithTheGrowthTheyPayFor(t *testing.T) {
	dir := t.TempDir()

	for _, pool := range []string{"busy", "quiet"} {
		if err := os.WriteFile(DropInPath(dir, pool), []byte("["+pool+"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	st := state.New()
	st.RecordApplied("busy", 10, time.Now().Add(-time.Hour))
	st.RecordApplied("quiet", 40, time.Now().Add(-time.Hour))

	const worker = 100 << 20 // 100MiB

	// The plan fits: 20 + 30 workers at 100MiB is 5000MiB against a 5120MiB
	// budget. The damped subset does not: 20 + 40 is 6000MiB, because the growth
	// cleared the threshold and the cut did not.
	res, err := Apply(context.Background(), allocate.Plan{
		TotalBytes: 5120 << 20,
		Pools: []allocate.PoolPlan{
			// Doubling: well over the threshold on its own.
			{Name: "busy", MaxChildren: 20, Current: 10, WorkerBytes: worker},
			// The 10 workers that pays for, as a 25% cut of a much larger pool —
			// under the 30% shrink threshold, and skipped before this existed.
			{Name: "quiet", MaxChildren: 30, Current: 40, WorkerBytes: worker},
		},
	}, Master{
		Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir), DropInDir: dir,
		PID: fakeMaster(t),
	}, st, Options{BackupDir: filepath.Join(dir, "backup")}, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	applied := map[string]int{}
	for _, o := range res.Changed() {
		applied[o.Pool] = o.To
	}

	if applied["busy"] != 20 {
		t.Fatalf("the growth was not applied: %+v", res.Outcomes)
	}
	if applied["quiet"] != 30 {
		t.Errorf("the growth was applied without the cut that pays for it; "+
			"the host now runs %d workers where the plan allocated %d: %+v",
			applied["busy"]+40, applied["busy"]+applied["quiet"], res.Outcomes)
	}

	body, err := os.ReadFile(DropInPath(dir, "quiet"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "pm.max_children = 30") {
		t.Errorf("the cut was reported as applied but not written:\n%s", body)
	}
}

// TestAShrinkTheBudgetDoesNotNeedIsStillDamped is the other half of the
// coupling, and the reason it is arithmetic rather than a rule.
//
// Forcing every reduction through whenever anything grew would be simpler and
// would quietly undo the damping next door: on a host with several pools there
// is nearly always something growing, so every shrink would fire the moment it
// was proposed — the exact oscillation the thresholds exist to stop.
func TestAShrinkTheBudgetDoesNotNeedIsStillDamped(t *testing.T) {
	dir := t.TempDir()

	st := state.New()
	st.RecordApplied("busy", 10, time.Now().Add(-time.Hour))
	st.RecordApplied("quiet", 40, time.Now().Add(-time.Hour))

	const worker = 100 << 20

	// Same plan, but on a host with room to spare: 20 + 40 workers is 6000MiB
	// against 32GiB, so nothing has to give.
	res, err := Apply(context.Background(), allocate.Plan{
		TotalBytes: 32 << 30,
		Pools: []allocate.PoolPlan{
			{Name: "busy", MaxChildren: 20, Current: 10, WorkerBytes: worker},
			{Name: "quiet", MaxChildren: 30, Current: 40, WorkerBytes: worker},
		},
	}, Master{
		Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir), DropInDir: dir,
		PID: fakeMaster(t),
	}, st, Options{BackupDir: filepath.Join(dir, "backup")}, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for _, o := range res.Outcomes {
		if o.Pool == "quiet" && o.Action == ActionApplied {
			t.Errorf("a 25%% shrink the budget did not need was applied anyway (%s); "+
				"any growth anywhere would then unlock every shrink on the host", o.Reason)
		}
		if o.Pool == "busy" && o.Action != ActionApplied {
			t.Errorf("the growth was not applied: %s (%s)", o.Action, o.Reason)
		}
	}
}

// TestShrinkingIsDampedHarderThanGrowing is the anti-flap property.
//
// A symmetric threshold lets a pool cross it in both directions on adjacent
// rounds — demand rises and it grows, the peak decays and it shrinks back,
// demand rises again — a host reloading every pool every few minutes, each
// reload individually justified. The asymmetry breaks the cycle in the safe
// direction: growing too eagerly costs unused memory, shrinking too eagerly
// costs queued requests.
func TestShrinkingIsDampedHarderThanGrowing(t *testing.T) {
	opts := Options{MinInterval: time.Minute, MinChange: 0.15}.Defaults()
	now := time.Now()

	st := state.New()
	st.RecordApplied("p", 20, now.Add(-time.Hour))

	// A 20% move: over the 15% growth threshold, under the 30% shrink threshold.
	up, worthUp := decide(allocate.PoolPlan{Name: "p", MaxChildren: 24, Current: 20}, st, opts, now)
	down, worthDown := decide(allocate.PoolPlan{Name: "p", MaxChildren: 16, Current: 20}, st, opts, now)

	if !worthUp || up.Action != ActionApplied {
		t.Errorf("a 20%% growth was not applied: %s (%s)", up.Action, up.Reason)
	}
	if worthDown || down.Action != ActionTooSmall {
		t.Errorf("a 20%% shrink was applied; the same move in both directions on "+
			"adjacent rounds is the flap: %s (%s)", down.Action, down.Reason)
	}

	// And the same asymmetry in time: a shrink waits four intervals.
	st.RecordApplied("p", 20, now.Add(-2*time.Minute))
	if _, worth := decide(allocate.PoolPlan{Name: "p", MaxChildren: 40, Current: 20}, st, opts, now); !worth {
		t.Error("a growth was refused two intervals after the last change")
	}
	if out, worth := decide(allocate.PoolPlan{Name: "p", MaxChildren: 4, Current: 20}, st, opts, now); worth {
		t.Errorf("a shrink was allowed two intervals after the last change: %s", out.Reason)
	}
}

// TestDecideComparesAgainstTheRunningSystem.
//
// decide compared against LastAppliedMaxChildren — what this tool remembered
// setting — rather than what the pool is actually configured for. The two
// diverge whenever anything else touches the pool: a hand edit, a deploy that
// replaces the fragment, someone deleting the drop-in to undo a change. In every
// one of those cases the memory said "already at 60" and nothing was done, so
// the undone change stayed undone.
func TestDecideComparesAgainstTheRunningSystem(t *testing.T) {
	opts := Options{}.Defaults()
	now := time.Now()

	st := state.New()
	st.RecordApplied("p", 60, now.Add(-time.Hour))

	// The drop-in was deleted, so the pool is back to its own configured 20.
	out, worth := decide(allocate.PoolPlan{Name: "p", MaxChildren: 60, Current: 20}, st, opts, now)

	if !worth || out.Action != ActionApplied {
		t.Errorf("a pool whose configuration was reverted behind the tool's back was "+
			"left alone: %s (%s)", out.Action, out.Reason)
	}
	if out.From != 20 {
		t.Errorf("From = %d, want the observed 20 rather than the remembered 60", out.From)
	}
}

// crashAfterWriting does what a run does right up to the point it dies: takes
// the backups, records the transaction, writes the live fragments. It then
// returns without validating, reloading or cleaning up — which is exactly the
// state an OOM kill or a reboot leaves behind.
func crashAfterWriting(t *testing.T, master Master, backupDir string, changes ...allocate.PoolPlan) {
	t.Helper()

	if _, err := writeDropIns(master, changes, Options{BackupDir: backupDir}.Defaults(), nil); err != nil {
		t.Fatalf("setting up the crashed state: %v", err)
	}
}

// TestReconcileUndoesWhatACrashLeftBehind.
//
// A transaction record is written before the first live write and removed once
// the change settles, so finding one at startup means a process died in
// between. Until this existed nothing ever looked: the fragments stayed —
// possibly ones php-fpm had already rejected — waiting for the next reload from
// any source to adopt them and take the master down for good.
func TestReconcileUndoesWhatACrashLeftBehind(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backup")

	live := DropInPath(dir, "www")
	original := "[www]\npm.max_children = 10\n"
	if err := os.WriteFile(live, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	configPath := masterConfigAt(t, dir)
	master := Master{
		// Rejects the real tree every time, so the configuration the crashed run
		// left is invalid — the case where the backups are the only way back.
		Binary: rejectsOnly(t, configPath), ConfigPath: configPath, DropInDir: dir,
	}

	crashAfterWriting(t, master, backupDir, allocate.PoolPlan{Name: "www", MaxChildren: 999})

	err := Reconcile(context.Background(), master, Options{BackupDir: backupDir}, nil)
	if !errors.Is(err, ErrUnreconciled) {
		t.Fatalf("err = %v, want ErrUnreconciled: the restore is also rejected here, "+
			"and that must be reported rather than assumed to have worked", err)
	}

	got, readErr := os.ReadFile(live)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != original {
		t.Errorf("the fragment a dead run left behind was not put back:\n%s", got)
	}
}

// TestReconcileRemovesAFragmentThatDidNotExistBefore is the case the backup
// files alone could never cover.
//
// A fragment that did not exist has nothing to back up, so a run that died after
// creating one left it behind with no trace that anyone had been there. Nothing
// could remove it, because nothing knew it was ours.
func TestReconcileRemovesAFragmentThatDidNotExistBefore(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backup")

	configPath := masterConfigAt(t, dir)
	rejecting := Master{Binary: rejectsOnly(t, configPath), ConfigPath: configPath, DropInDir: dir}

	crashAfterWriting(t, rejecting, backupDir, allocate.PoolPlan{Name: "brand-new", MaxChildren: 8})

	created := DropInPath(dir, "brand-new")
	if _, err := os.Stat(created); err != nil {
		t.Fatalf("setting up: the fragment was not created: %v", err)
	}

	_ = Reconcile(context.Background(), rejecting, Options{BackupDir: backupDir}, nil)

	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Error("a fragment created by a run that died was left behind; nothing else " +
			"will ever remove it, and the next reload adopts it")
	}
}

// TestReconcileLeavesAFileSomeoneElseChanged.
//
// A configuration can fail to validate for reasons that have nothing to do with
// the unfinished run — an operator editing an unrelated pool at the wrong
// moment. Reverting their work to a state this tool happens to hold a copy of is
// not a repair, and a backup on its own is not evidence that restoring it is
// right. The hash the dead run recorded is.
func TestReconcileLeavesAFileSomeoneElseChanged(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backup")

	live := DropInPath(dir, "www")
	if err := os.WriteFile(live, []byte("[www]\npm.max_children = 10\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	configPath := masterConfigAt(t, dir)
	master := Master{Binary: rejectsOnly(t, configPath), ConfigPath: configPath, DropInDir: dir}

	crashAfterWriting(t, master, backupDir, allocate.PoolPlan{Name: "www", MaxChildren: 40})

	// The operator has since edited it themselves.
	theirs := "[www]\n; hand-tuned during the incident\npm.max_children = 25\n"
	if err := os.WriteFile(live, []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}

	_ = Reconcile(context.Background(), master, Options{BackupDir: backupDir}, nil)

	got, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != theirs {
		t.Errorf("an operator's own edit was reverted to a copy this tool happened to "+
			"hold:\n%s", got)
	}
}

// TestReconcileLeavesAValidConfigurationAlone: the common case. The run died
// after the change stuck, so undoing it would be its own outage.
func TestReconcileLeavesAValidConfigurationAlone(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backup")

	live := DropInPath(dir, "www")
	if err := os.WriteFile(live, []byte("[www]\npm.max_children = 10\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	master := Master{Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir), DropInDir: dir}
	crashAfterWriting(t, master, backupDir, allocate.PoolPlan{Name: "www", MaxChildren: 40})

	if err := Reconcile(context.Background(), master, Options{BackupDir: backupDir}, nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got, _ := os.ReadFile(live)
	if !strings.Contains(string(got), "pm.max_children = 40") {
		t.Errorf("a valid configuration was reverted:\n%s", got)
	}

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%d file(s) left in the backup directory, so the next start "+
			"reconciles all over again", len(entries))
	}
}

// TestReconcileIgnoresAnotherMastersTransaction.
//
// MasterOnHost refuses a host with more than one master and tells the operator
// to run once per master with the drop-in directory set — but nothing stops both
// runs sharing the default backup directory. Acting on the other master's record
// would mean restoring configuration into a pool directory it was never taken
// from: invented out of another server's history, presented as a rollback.
func TestReconcileIgnoresAnotherMastersTransaction(t *testing.T) {
	ours := t.TempDir()
	theirs := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backup")

	theirConfig := masterConfigAt(t, theirs)
	crashAfterWriting(t,
		Master{Binary: trueBin(t), ConfigPath: theirConfig, DropInDir: theirs},
		backupDir, allocate.PoolPlan{Name: "elsewhere", MaxChildren: 99})

	ourConfig := masterConfigAt(t, ours)
	master := Master{
		// Rejects everything, so if Reconcile took those files for its own it
		// would go on to act on them.
		Binary: rejectsOnly(t, ourConfig), ConfigPath: ourConfig, DropInDir: ours,
	}

	if err := Reconcile(context.Background(), master, Options{BackupDir: backupDir}, nil); err != nil {
		t.Fatalf("Reconcile acted on another master's transaction: %v", err)
	}
	if _, err := os.Stat(DropInPath(ours, "elsewhere")); !os.IsNotExist(err) {
		t.Error("a fragment belonging to another master was written into this one's " +
			"pool directory")
	}
	if _, err := os.Stat(DropInPath(theirs, "elsewhere")); err != nil {
		t.Error("the other master's own fragment was removed by a run that does not " +
			"manage it")
	}
}

// TestSandboxDoesNotPullInTheRealPoolDirectory: the sandbox is only worth
// anything if it is genuinely separate. A master config that lives inside the
// directory it globs would be copied in as a fragment and its own include line
// would reach back out to production.
func TestSandboxDoesNotPullInTheRealPoolDirectory(t *testing.T) {
	dir := t.TempDir()

	// The master config placed inside the pool directory, the awkward case.
	configPath := filepath.Join(dir, "php-fpm.conf")
	if err := os.WriteFile(configPath,
		[]byte("[global]\ninclude = "+filepath.Join(dir, "*.conf")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sandboxConfig, cleanup, err := sandbox(
		Master{ConfigPath: configPath, DropInDir: dir},
		[]allocate.PoolPlan{{Name: "www", MaxChildren: 12}},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	body, err := os.ReadFile(sandboxConfig)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), filepath.Join(dir, "*.conf")) {
		t.Errorf("the sandbox config still includes the real pool directory:\n%s", body)
	}

	staged, err := os.ReadDir(filepath.Dir(sandboxConfig) + "/pool.d")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range staged {
		if e.Name() == "php-fpm.conf" {
			t.Error("the master config was copied into the sandbox as a pool fragment; " +
				"its include line points back at production")
		}
	}
}

// fakeMaster starts a process that a reload will accept and survive.
//
// phpfpm.VerifyMaster reads the process title immediately before signalling —
// so a test master has to look like one. The no-op string at the front puts
// php-fpm's title into the shell's command line, which is what the check reads.
//
// Faking the check out instead would leave the only thing standing between this
// package and SIGUSR2 to an arbitrary process untested.
func fakeMaster(t *testing.T) int {
	t.Helper()

	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command("/bin/sh", "-c",
		`: "php-fpm: master process (/etc/php-fpm.conf)"; trap ':' USR2; touch `+ready+"; sleep 30 & wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Waited for AFTER the trap is installed. The process exists the instant it
	// starts, and the default action for USR2 is to terminate — so signalling
	// too early kills it and looks like a master that did not survive.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return cmd.Process.Pid
		}
		if time.Now().After(deadline) {
			t.Fatal("the stub master never signalled that its trap was installed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestApplyReloadsARealMasterAndSurvives.
//
// The only end-to-end coverage of the reload path in this package. Every other
// successful apply runs in provisioning mode, where there is no master and
// nothing is signalled — so until this existed, the sequence that actually
// changes a running host (write, validate, signal, watch it settle, record) was
// never executed by a test.
func TestApplyReloadsARealMasterAndSurvives(t *testing.T) {
	dir := t.TempDir()
	st := state.New()
	pid := fakeMaster(t)

	res, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "shop", MaxChildren: 12, Current: 4}},
	}, Master{
		Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir), DropInDir: dir, PID: pid,
	}, st, Options{
		BackupDir: filepath.Join(t.TempDir(), "backup"), SettleTime: 200 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !res.Reloaded {
		t.Error("the master was not reloaded")
	}
	if res.RolledBack {
		t.Error("a successful apply was rolled back")
	}

	body, err := os.ReadFile(DropInPath(dir, "shop"))
	if err != nil {
		t.Fatalf("nothing was written: %v", err)
	}
	if !strings.Contains(string(body), "pm.max_children = 12") {
		t.Errorf("the fragment does not carry the new value:\n%s", body)
	}
	if ps := st.Pools["shop"]; ps == nil || ps.LastAppliedMaxChildren != 12 {
		t.Errorf("the change was not recorded, so the next round has no baseline: %+v", ps)
	}
}

// TestAFragmentTheMasterDoesNotIncludeIsRefused.
//
// The quietest possible failure. A master that includes [a-y]*.conf — or names
// its pool files one by one, which plenty of hand-built configurations do —
// never reads zz-fpm-tune-www.conf. Everything then succeeds: `php-fpm -t`
// passes, the reload goes through, the pools are recorded as applied, the
// operator is told the host was retuned. And the running configuration is
// exactly what it was before, forever, with the tool reporting success on every
// subsequent run because it believes it already made the change.
func TestAFragmentTheMasterDoesNotIncludeIsRefused(t *testing.T) {
	dir := t.TempDir()

	_, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "www", MaxChildren: 12, Current: 4}},
	}, Master{
		Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir), DropInDir: dir,
		PID: fakeMaster(t),
		// Everything from a to y. Our fragments start with zz.
		IncludePattern: filepath.Join(dir, "[a-y]*.conf"),
	}, state.New(), Options{BackupDir: filepath.Join(dir, "backup")}, nil)

	if !errors.Is(err, ErrNotIncluded) {
		t.Fatalf("err = %v, want ErrNotIncluded", err)
	}
	if _, statErr := os.Stat(DropInPath(dir, "www")); !os.IsNotExist(statErr) {
		t.Error("a fragment the master will never read was written anyway")
	}
}

// TestTheOrdinaryIncludeStillWorks: the check above must not refuse the layout
// every distribution actually ships.
func TestTheOrdinaryIncludeStillWorks(t *testing.T) {
	dir := t.TempDir()

	res, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "www", MaxChildren: 12, Current: 4}},
	}, Master{
		Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir), DropInDir: dir,
		PID:            fakeMaster(t),
		IncludePattern: filepath.Join(dir, "*.conf"),
	}, state.New(), Options{
		BackupDir: filepath.Join(t.TempDir(), "backup"), SettleTime: 100 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("the standard /etc/php-fpm.d/*.conf layout was refused: %v", err)
	}
	if len(res.Changed()) != 1 {
		t.Errorf("changed = %v, want the one pool", res.Changed())
	}
}

// TestAnUnidentifiedMasterIsRefusedBeforeAnythingIsWritten.
//
// This was checked AFTER the fragments were on disk, so a caller that could not
// identify the master left new configuration in the pool directory and then had
// to take it back. If the restore failed, or the process died, or anything
// reloaded in between, the change was adopted with no master known to have
// accepted it.
func TestAnUnidentifiedMasterIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backup")

	_, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "www", MaxChildren: 12, Current: 4}},
	}, Master{
		Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir), DropInDir: dir,
		PID: 0, NoMasterExpected: false,
	}, state.New(), Options{BackupDir: backupDir}, nil)

	if !errors.Is(err, ErrMasterUnknown) {
		t.Fatalf("err = %v, want ErrMasterUnknown", err)
	}
	if _, statErr := os.Stat(DropInPath(dir, "www")); !os.IsNotExist(statErr) {
		t.Error("a fragment was written for a master that could not be identified")
	}
	if entries, _ := os.ReadDir(backupDir); len(entries) > 0 {
		t.Errorf("%d backup(s) were taken before the refusal", len(entries))
	}
}

// TestADryRunNeedsNoMaster: it reloads nothing, so rehearsing a change set on a
// host where php-fpm is not up yet is a legitimate thing to want.
func TestADryRunNeedsNoMaster(t *testing.T) {
	dir := t.TempDir()

	if _, err := Apply(context.Background(), allocate.Plan{
		Pools: []allocate.PoolPlan{{Name: "www", MaxChildren: 12, Current: 4}},
	}, Master{
		Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir), DropInDir: dir,
	}, state.New(), Options{
		DryRun: true, BackupDir: filepath.Join(t.TempDir(), "backup"),
	}, nil); err != nil {
		t.Errorf("a dry run was refused for want of a master it does not use: %v", err)
	}
}

// TestAGrowthWaitsRatherThanForceAReloadTooSoon.
//
// The coupling may override the SIZE threshold — a reduction too small to be
// worth a reload on its own goes through when a neighbour needs the memory.
// It must not override the TIME brake. A pool reloaded a minute ago must not be
// reloaded again because something else wants to grow; the reload is the cost
// being managed, and doing it on demand from a neighbour is the flap wearing a
// different hat.
//
// So the growth waits instead. Deferring it costs some queueing on one pool.
// Taking the memory anyway would overcommit the host.
func TestAGrowthWaitsRatherThanForceAReloadTooSoon(t *testing.T) {
	dir := t.TempDir()
	const worker = 100 << 20

	st := state.New()
	st.RecordApplied("busy", 10, time.Now().Add(-time.Hour))
	// Reloaded seconds ago: inside its shrink interval whatever the size.
	st.RecordApplied("quiet", 40, time.Now().Add(-time.Second))

	res, err := Apply(context.Background(), allocate.Plan{
		TotalBytes: 5120 << 20, // the damped subset would need 6000MiB
		Pools: []allocate.PoolPlan{
			{Name: "busy", MaxChildren: 20, Current: 10, WorkerBytes: worker},
			{Name: "quiet", MaxChildren: 30, Current: 40, WorkerBytes: worker},
		},
	}, Master{
		Binary: trueBin(t), ConfigPath: masterConfigAt(t, dir), DropInDir: dir,
		PID: fakeMaster(t),
	}, st, Options{BackupDir: filepath.Join(t.TempDir(), "backup")}, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for _, o := range res.Outcomes {
		switch o.Pool {
		case "quiet":
			if o.Action == ActionApplied {
				t.Errorf("a pool reloaded a second ago was reloaded again to fund a "+
					"neighbour's growth: %s", o.Reason)
			}
		case "busy":
			if o.Action == ActionApplied {
				t.Errorf("a growth was applied with no memory to pay for it; the host "+
					"would be committed beyond its budget: %s", o.Reason)
			}
			if o.Action != ActionHeldBack {
				t.Errorf("busy = %q, want %q so the operator can see why", o.Action, ActionHeldBack)
			}
		}
	}

	if _, err := os.Stat(DropInPath(dir, "busy")); !os.IsNotExist(err) {
		t.Error("the held-back growth was written anyway")
	}
}
