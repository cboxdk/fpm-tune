package apply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cboxdk/phpfpm"
)

// ErrUnreconciled reports that a previous run left configuration behind that
// could not be resolved.
var ErrUnreconciled = errors.New("a previous run left a change this could not resolve")

// Reconcile finishes or undoes what a previous run did not complete.
//
// A record is written before the single live write and removed only once the
// change has been proven to survive, so finding one at startup means a process
// died in between: the OOM reaper, a reboot, ctrl-c during the reload.
//
// Three rules make it safe to run automatically.
//
// It knows how far the run got, rather than inferring it. The record carries a
// phase, because the one thing that matters — whether the running master ever
// adopted the change — is not observable from the files afterwards. Validation
// passing says the configuration is acceptable, not that anyone has read it.
//
// It only touches a file it can prove is its own, by comparing it against the
// hash the dead run recorded. A configuration can fail to validate for reasons
// that have nothing to do with the unfinished run — an operator editing an
// unrelated pool during an incident, which is exactly when they would be — and
// reverting their work to a state this tool happens to hold a copy of is not a
// repair.
//
// And it never treats being interrupted as an answer. A cancelled validation or
// settle leaves the record in place for the next start, because the alternatives
// are undoing a change that was adopted, or deleting the only way back from one
// that was not.
// Reconcile finishes or undoes what a previous run did not complete, and reports
// whether it had to do anything.
//
// The bool is the interesting half for a caller keeping counters: an ordinary
// start finds nothing and returns false, and a start that had to undo, complete
// or remove something returns true. Counting the ERROR return instead got this
// exactly backwards — successful repairs were invisible and a condition it could
// not fix was counted once per round forever.
func Reconcile(ctx context.Context, master Master, opts Options, log *slog.Logger) (bool, error) {
	opts = opts.Defaults()
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	txn, found, err := readTransaction(opts.BackupDir, master.DropInDir)
	if err != nil {
		// A record that cannot be used is not the same as no record. It exists
		// because a run died with a change in flight; sweeping on the strength
		// of it being unreadable would delete the copy that is the only route
		// back, exactly when the state is worst.
		return false, fmt.Errorf("%w: %w", ErrUnreconciled, err)
	}
	if !found {
		// Nothing was in flight. Any saved copies still lying around belong to a
		// transaction closed after they were taken — the record is removed first
		// precisely so this is the harmless order — and nothing will read them.
		sweepOrphanBackups(opts.BackupDir, master.DropInDir, log)

		// The repair needs a binary and a config path to ask php-fpm anything,
		// and the caller has neither when discovery failed and the state file is
		// missing — which is exactly the host this repair exists for, because
		// php-fpm being DOWN is why discovery failed. The sidecar written beside
		// the backups on every successful apply is the answer: it survives a
		// lost state file, and it is written by the same code that knows the
		// master is real.
		master = master.filledFrom(rememberedMaster(opts.BackupDir, master.DropInDir))

		return repairIfOursIsBroken(ctx, master, opts, log)
	}

	// The record carries the binary and config, so recovery works even when no
	// master can be discovered. That is not hypothetical: if a reload killed the
	// master and this process then died, the next start finds nothing running —
	// and without these the configuration that killed it would sit there
	// unreconciled through every restart attempt.
	master = master.completedFrom(txn)

	log.Warn("A previous run did not finish", "phase", txn.Phase, "file", txn.Path)

	if !txn.landed() && txn.Phase == PhaseWritten {
		// The rename never happened, so the file is untouched and there is
		// nothing to undo. One atomic write is what makes this a clean answer;
		// the per-pool layout had to guess which of N files were ours.
		log.Info("The change never reached disk; nothing to undo")
		discard(txn, opts.BackupDir, log)

		return true, nil
	}

	if !txn.landed() {
		// Signalled, and what is on disk is not what was written. That is not
		// "nothing happened": the master may well have adopted the change and
		// something else — an operator, a config-management run — has edited the
		// file since without reloading. Discarding here threw away the only
		// record saying a master might be running on a configuration nobody can
		// see any more.
		//
		// The file on disk is what the next reload will adopt, so the question
		// worth asking is whether php-fpm accepts it. If it does, the record has
		// done its job and the current file stands; if it does not, this is a
		// broken host and the caller has to know.
		log.Warn("A previous run signalled the master, and the file has been changed since",
			"file", txn.Path)

		if verr := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); verr != nil {
			return false, fmt.Errorf("%w: a previous run signalled the master and the "+
				"configuration has been edited since, and php-fpm will not accept what is "+
				"there now (%s): %w", ErrUnreconciled, txn.Path, verr)
		}

		log.Info("What is on disk now is valid; adopting it and closing the record")
		discard(txn, opts.BackupDir, log)

		return true, nil
	}

	valid := phpfpm.Validate(ctx, master.Binary, master.ConfigPath)
	if ctxErr := ctx.Err(); ctxErr != nil {
		// Interrupted mid-check. Undoing on the strength of a validation that
		// was cancelled rather than failed would revert a change that may
		// already be running.
		return false, fmt.Errorf("%w: interrupted before it could be resolved: %w",
			ErrUnreconciled, ctxErr)
	}

	if valid != nil {
		return true, undoLeftover(ctx, txn, master, opts, valid, log)
	}

	// Valid, so the change is kept — but validation is not the commit point for
	// a RUNNING master. The run may have died before signalling, in which case
	// the master still serves the old configuration while the file says
	// otherwise, and nothing would ever correct it: the next round reads the
	// file, concludes the pool is already where it wants it, and never reloads.
	if err := finishReload(ctx, txn, master, opts, log); err != nil {
		return true, err
	}

	discard(txn, opts.BackupDir, log)

	return true, nil
}

// repairIfOursIsBroken puts the master back on its feet when this tool's own
// file is what is stopping it.
//
// A transaction only covers a run that died mid-change. The configuration can
// become invalid long afterwards, through no crash at all: an operator removes a
// site, this file still declares that pool, and a pool defined only here has no
// listen and no user — so php-fpm refuses to start. Observed on a VM, where the
// master then stayed down through six systemd restart attempts with fpm-tune
// running alongside it, doing nothing, having caused it.
//
// The file is removed whole rather than edited, because a running master on its
// configured defaults beats a dead one on tuned settings, and the next round
// rewrites it correctly within one interval. Nothing is touched unless removing
// it demonstrably fixes the problem: if the configuration is broken for some
// other reason, deleting this tool's work would achieve nothing except deleting
// this tool's work.
func repairIfOursIsBroken(ctx context.Context, master Master, opts Options, log *slog.Logger) (bool, error) {
	if master.Binary == "" || master.ConfigPath == "" {
		return false, nil
	}
	if err := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); err == nil {
		return false, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// Interrupted, not answered. Returning nil here marked the repair as
		// done for the life of the process, so a `php-fpm -t` that timed out
		// once under load left the host down until someone restarted the daemon.
		return false, fmt.Errorf("%w: could not determine whether this tool's file is "+
			"the problem: %w", ErrUnreconciled, ctxErr)
	}

	path := DropInPath(master.DropInDir)
	body, err := os.ReadFile(path)
	if err != nil {
		// Nothing of ours on disk, so the breakage is not ours to fix.
		return false, nil
	}

	// Proved ours before it is deleted, not assumed from the name. A file called
	// zz-something.conf is a natural thing for an operator to have written
	// themselves — last in the include order is exactly where you put your own
	// overrides — and removing theirs to fix a problem would be its own outage.
	if !isOurs(body) {
		log.Error("The configuration is rejected and a file with this tool's name is "+
			"present, but it was not written by this tool; leaving it alone", "path", path)

		return false, nil
	}

	// Rehearsed in a sandbox before the file is touched.
	//
	// The live re-check below, with its put-back, already guarantees the final
	// bytes — so what this buys is the WINDOW. Removing first and checking after
	// leaves the pool directory without the file for as long as a `php-fpm -t`
	// takes, and anything reloading php-fpm in that window adopts a
	// configuration nobody chose. That is the whole reason it is here.
	if err := validateReplacement(ctx, master, path, nil); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, fmt.Errorf("%w: could not determine whether removing this "+
				"tool's file would help: %w", ErrUnreconciled, ctxErr)
		}

		// Two things reach here and they need different words. php-fpm may have
		// rejected the configuration for a reason this file is not the whole
		// cause of — removing it does not FIX it, which is not the same as it
		// not being involved — or php-fpm may not have run at all, because the
		// binary recorded for it no longer exists after an upgrade.
		if !binaryRuns(master.Binary) {
			log.Error("The configuration cannot be checked: the php-fpm binary recorded "+
				"for this master is not there. It is not safe to remove anything on that "+
				"basis.", "binary", master.Binary)

			return false, nil
		}

		log.Error("The configuration is rejected, and removing this tool's file does not " +
			"fix it; leaving it alone. Something else in the configuration is broken too.")

		return false, nil
	}

	log.Error("php-fpm will not accept its configuration, and removing this tool's "+
		"file fixes it — most likely a pool it still overrides has been removed. "+
		"Taking the file out; the next round writes it again for the pools that "+
		"remain. If php-fpm is DOWN it will not come back on its own: systemd gives "+
		"up after a few rapid restarts, long before this repair lands, so start it "+
		"once you are satisfied with what happened here.", "path", path)

	if err := os.Remove(path); err != nil {
		return false, fmt.Errorf("%w: this tool's file is stopping php-fpm from starting "+
			"and could not be removed: %w", ErrUnreconciled, err)
	}
	_ = syncDir(filepath.Dir(path))

	if err := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); err != nil {
		// Put it back: removing it did not help after all, and leaving the host
		// both broken AND untuned is worse than broken alone.
		// The put-back, and its own failure, which used to be thrown away. It is
		// the worse of the two outcomes: this tool has removed its file, php-fpm
		// still will not start, and now nothing says the file is gone.
		if werr := writeAtomic(path, body); werr != nil {
			return true, fmt.Errorf("%w: removing this tool's file did not make the "+
				"configuration valid (%w), AND it could not be put back — %s no longer "+
				"exists and those pools are at whatever their own files say: %w",
				ErrUnreconciled, err, path, werr)
		}

		return true, fmt.Errorf("%w: removing this tool's file did not make the "+
			"configuration valid: %w", ErrUnreconciled, err)
	}

	return true, nil
}

// binaryRuns reports whether the recorded php-fpm can be executed at all.
//
// A validation that fails because the binary is missing says nothing about the
// configuration, and this package removes files on the strength of validations.
// After a PHP upgrade plus a stale state file, that is a real distinction.
func binaryRuns(binary string) bool {
	if binary == "" {
		return false
	}
	info, err := os.Stat(binary)

	return err == nil && !info.IsDir()
}

// removeOursIfThatFixesIt takes this tool's file out when doing so makes the
// configuration valid, and puts it back when it does not.
//
// The same trade the repair path makes, reached from the other direction: a
// rejected leftover with no copy to restore. Rehearsed first, so the file is
// never absent from the pool directory on the strength of a guess, and checked
// again in place afterwards, because the sandbox is a faithful copy and not the
// thing itself.
func removeOursIfThatFixesIt(ctx context.Context, master Master, path string, log *slog.Logger) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", path, err)
	}
	if !isOurs(body) {
		return fmt.Errorf("%s was not written by this tool", path)
	}

	if err := validateReplacement(ctx, master, path, nil); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		return fmt.Errorf("removing it would not make the configuration valid: %w", err)
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("cannot remove %s: %w", path, err)
	}
	_ = syncDir(filepath.Dir(path))

	if err := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Cancelled, not rejected. Putting the file back is still right —
			// it was removed on the strength of a check that did not finish —
			// but the reason has to say so, or an operator reads it as php-fpm
			// having refused the configuration without its file.
			err = fmt.Errorf("the check was interrupted: %w", ctxErr)
		}

		// The put-back's own failure was thrown away, and it is the worse of
		// the two: this tool has removed its file, php-fpm still will not start,
		// and now nothing anywhere says the file is gone.
		if werr := writeAtomic(path, body); werr != nil {
			return fmt.Errorf("removing it did not make the configuration valid (%w), AND "+
				"it could not be put back: %s no longer exists and the pools are at "+
				"whatever their own files say: %w", err, path, werr)
		}
		if log != nil {
			log.Error("Removing this tool's file did not help after all; put back", "path", path)
		}

		return fmt.Errorf("removing it did not make the configuration valid: %w", err)
	}

	return nil
}

// PendingRepair reports whether a previous run left something unfinished,
// without touching anything.
//
// For a dry run, which must not repair — it removes files, rewrites them from
// backups and signals the master, and the whole promise of a dry run is that it
// does none of that. Telling the operator one is waiting is the useful half.
func PendingRepair(backupDir, dropInDir string) (path string, found bool, err error) {
	txn, ok, err := readTransaction(backupDir, dropInDir)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}

	return txn.Path, true, nil
}

// completedFrom fills in what the caller could not discover.
func (m Master) completedFrom(txn transaction) Master {
	if m.Binary == "" {
		m.Binary = txn.Binary
	}
	if m.ConfigPath == "" {
		m.ConfigPath = txn.ConfigPath
	}
	if m.DropInDir == "" {
		m.DropInDir = txn.DropInDir
	}

	return m
}

// undoLeftover takes back a change php-fpm will not accept.
//
// The rollback is rehearsed in a sandbox first. A configuration can be invalid
// for reasons that have nothing to do with the unfinished run, and in that case
// reverting this tool's file achieves nothing except undoing a change that may
// already be running — so if the rollback would not actually make the
// configuration valid, the live file is left alone and the record is kept.
func undoLeftover(
	ctx context.Context,
	txn transaction,
	master Master,
	opts Options,
	why error,
	log *slog.Logger,
) error {
	log.Error("The configuration left on disk is rejected", "error", why)

	previous, err := previousContent(txn, opts.BackupDir)
	if err != nil {
		// No copy to put back — the backup directory was cleaned by a tmpfiles
		// rule, or an operator tidying /var/lib/fpm-tune.
		//
		// This used to be the end of it, and it is the worst place to stop: the
		// host is down BECAUSE of this tool's file, and the repair that would
		// take it out and get php-fpm started runs only when there is no record
		// at all. `apply` exited 1 before it could plan, `serve` published
		// apply_blocked="unrepaired" and never applied again, and nothing but a
		// person cleared it.
		//
		// Removing the file is the same trade the repair path already makes,
		// and it is rehearsed the same way: a running master on the settings the
		// operator configured beats a master that is not running.
		log.Error("The configuration left on disk is rejected and there is no copy to put "+
			"back; trying to remove this tool's file instead", "file", txn.Path, "error", err)

		if rerr := removeOursIfThatFixesIt(ctx, master, txn.Path, log); rerr != nil {
			return fmt.Errorf("%w: it is rejected, the previous configuration is gone, "+
				"and removing this tool's file does not fix it either: %w",
				ErrUnreconciled, rerr)
		}

		clearTransaction(opts.BackupDir, txn.DropInDir)
		log.Warn("Removed this tool's file to get php-fpm startable again; the pools are " +
			"back at what they are configured for until the next run")

		return nil
	}

	if err := validateReplacement(ctx, master, txn.Path, previous); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: interrupted before it could be resolved: %w",
				ErrUnreconciled, ctxErr)
		}

		// Undoing would not help, so something else is broken. Reverting anyway
		// would destroy a change that may be running and still leave the host
		// unable to reload.
		return fmt.Errorf("%w: the configuration is rejected, and undoing this tool's "+
			"change does not fix it — something else in the configuration is broken: %w",
			ErrUnreconciled, why)
	}

	if err := applyPrevious(txn, previous); err != nil {
		return fmt.Errorf("%w, and it could not be undone: %w", ErrUnreconciled, err)
	}

	// Checked in place as well as in rehearsal. The sandbox is a faithful copy
	// but it is still a copy, and this is the one path where being wrong means
	// leaving a host that cannot reload.
	if err := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); err != nil {
		return fmt.Errorf("%w: the change was taken back out and the configuration is "+
			"STILL rejected: %w", ErrUnreconciled, err)
	}

	log.Info("Undid the unfinished change; the configuration validates")
	discard(txn, opts.BackupDir, log)

	return nil
}

// previousContent is what the file held before the unfinished run, or nil when
// there was no file.
func previousContent(txn transaction, backupDir string) ([]byte, error) {
	if !txn.Existed {
		return nil, nil
	}
	if txn.Saved == "" {
		return nil, errors.New("the record says a file was replaced but names no backup")
	}

	return os.ReadFile(filepath.Join(backupDir, txn.Saved))
}

// applyPrevious puts the file back the way the transaction found it.
func applyPrevious(txn transaction, previous []byte) error {
	if previous == nil {
		// There was no file before, so undoing means removing ours rather than
		// leaving an empty one behind.
		if err := os.Remove(txn.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		_ = syncDir(filepath.Dir(txn.Path))

		return nil
	}

	return writeAtomic(txn.Path, previous)
}

// validateReplacement checks what the configuration would be after a rollback,
// without performing it.
func validateReplacement(ctx context.Context, master Master, path string, content []byte) error {
	configPath, cleanup, err := sandboxReplacing(master, path, content)
	if err != nil {
		return err
	}
	defer cleanup()

	return phpfpm.Validate(ctx, master.Binary, configPath)
}

// finishReload completes an unfinished apply by doing what it did not get to.
//
// Idempotent: a master that already adopted the file re-reads the same one. The
// cost is one graceful reload at startup, and the alternative is a host whose
// configuration and running master disagree with nothing to notice.
func finishReload(
	ctx context.Context,
	txn transaction,
	master Master,
	opts Options,
	log *slog.Logger,
) error {
	if master.PID <= 0 {
		if txn.Phase == PhaseSignalled {
			// Signalled, and nothing is running. Reported, and the record kept —
			// but the file is LEFT ALONE.
			//
			// Reporting it as "nothing to adopt it" let the caller discard the
			// record, deleting the only copy of the previous configuration, and
			// the host then sat with php-fpm down and the daemon logging "no
			// pools found" every round for ever. So the error stays.
			//
			// Undoing the change was the other half, and it was wrong. This
			// branch is only reached AFTER php-fpm has been asked and has said
			// it accepts the file. This tool writes nothing but pm.* keys, so a
			// file php-fpm validates does not stop a master initialising on it —
			// and restoring cannot help a master that is down for any other
			// reason, which after a reboot is most of them.
			//
			// What it did instead, measured: on a host where this was the FIRST
			// apply there is no previous version, so "restoring" removed the
			// drop-in entirely and returned every pool to its own
			// pm.max_children. That is the overcommit this tool exists to
			// prevent, performed by its own recovery. And it needs no crash to
			// reach — a SIGTERM inside the settle window leaves the record open
			// deliberately, and a reboot that starts fpm-tune before php-fpm
			// does the rest.
			//
			// If the master comes up, the next Reconcile finds a pid, finishes
			// the reload and closes the record. If it never comes up, the
			// operator gets this every start, and the saved copy is still there.
			saved := "(none: this was the first apply)"
			if txn.Saved != "" {
				saved = filepath.Join(opts.BackupDir, txn.Saved)
			}
			log.Error("A previous run signalled the master and no master is running. "+
				"php-fpm accepts what is on disk, so it is left in place; start php-fpm "+
				"and the next run will finish the change.",
				"file", txn.Path, "previous", saved)

			return fmt.Errorf("%w: the master was signalled by a previous run and is not "+
				"running. php-fpm accepts the configuration on disk, so it has been left "+
				"there; the previous version is at %s if it needs putting back by hand",
				ErrUnreconciled, saved)
		}

		// Written but never signalled: the ordinary provisioning case, where the
		// files are in place for whenever php-fpm starts.
		log.Info("The configuration a previous run left is valid; no master is running to adopt it")

		return nil
	}
	if err := ctx.Err(); err != nil {
		// Shutting down. Starting a reload here would signal a master on the way
		// out, for a change nobody is waiting on.
		return fmt.Errorf("%w: interrupted before it could be resolved: %w", ErrUnreconciled, err)
	}

	log.Warn("The configuration a previous run left is valid but may never have been "+
		"adopted; reloading to be sure", "pid", master.PID, "phase", txn.Phase)

	_, err := phpfpm.ReloadAndWait(ctx, phpfpm.ReloadTarget{
		PID:        master.PID,
		PIDFile:    master.PIDFile,
		ConfigPath: master.ConfigPath,
	}, opts.SettleTime, log)
	if err == nil {
		phpfpm.InvalidateConfigCache(master.Binary, master.ConfigPath)

		return nil
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		// The signal went out and the settle window never finished. Delivery is
		// not survival, so this is neither success nor failure — the record
		// stays, and the next start settles it.
		return fmt.Errorf("%w: the completing reload was interrupted before the master "+
			"was seen to survive it: %w", ErrUnreconciled, ctxErr)
	}

	// The completing reload killed the master. Doing nothing would have left the
	// old one alive, so this is recovery having made things worse — and the
	// configuration that did it must not be left in place.
	log.Error("The completing reload did not survive; taking the change back out",
		"pid", master.PID, "error", err)

	previous, perr := previousContent(txn, opts.BackupDir)
	if perr != nil {
		return fmt.Errorf("%w: the completing reload killed the master and the previous "+
			"configuration is gone: %w", ErrUnreconciled, perr)
	}
	if perr := applyPrevious(txn, previous); perr != nil {
		return fmt.Errorf("%w: the completing reload killed the master and the change "+
			"could not be taken back out: %w", ErrUnreconciled, perr)
	}

	return fmt.Errorf("%w: the completing reload killed the master; the previous "+
		"configuration has been restored: %w", ErrUnreconciled, err)
}

// sweepOrphanBackups removes saved copies with no transaction naming them.
func sweepOrphanBackups(backupDir, dropInDir string, log *slog.Logger) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".bak") {
			continue
		}
		if _, ok := backupTarget(dropInDir, filepath.Join(backupDir, e.Name())); !ok {
			continue
		}
		if err := os.Remove(filepath.Join(backupDir, e.Name())); err != nil {
			log.Debug("Could not remove an orphaned backup", "name", e.Name(), "error", err)
		}
	}
}

// backupTarget maps a saved copy back to the file it came from, and reports
// whether it belongs to this master at all.
func backupTarget(dropInDir, saved string) (string, bool) {
	name := filepath.Base(saved)

	prefix, rest, found := strings.Cut(name, "-")
	if !found {
		return "", false
	}
	sum := sha256.Sum256([]byte(filepath.Clean(dropInDir)))
	if prefix != hex.EncodeToString(sum[:4]) {
		return "", false
	}

	return filepath.Join(dropInDir, strings.TrimSuffix(rest, ".bak")), true
}

// discard closes a finished transaction: the record first, then the copy.
func discard(txn transaction, backupDir string, log *slog.Logger) {
	clearTransaction(backupDir, txn.DropInDir)

	if txn.Saved == "" {
		return
	}
	if err := os.Remove(filepath.Join(backupDir, txn.Saved)); err != nil && !os.IsNotExist(err) {
		log.Debug("Could not remove a backup", "name", txn.Saved, "error", err)
	}
}
