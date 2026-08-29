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

		return repairIfOursIsBroken(ctx, master, opts, log)
	}

	// The record carries the binary and config, so recovery works even when no
	// master can be discovered. That is not hypothetical: if a reload killed the
	// master and this process then died, the next start finds nothing running —
	// and without these the configuration that killed it would sit there
	// unreconciled through every restart attempt.
	master = master.completedFrom(txn)

	log.Warn("A previous run did not finish", "phase", txn.Phase, "file", txn.Path)

	if !txn.landed() {
		// The rename never happened, so the file is untouched and there is
		// nothing to undo. One atomic write is what makes this a clean answer;
		// the per-pool layout had to guess which of N files were ours.
		log.Info("The change never reached disk; nothing to undo")
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

	if err := validateReplacement(ctx, master, path, nil); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, fmt.Errorf("%w: could not determine whether removing this "+
				"tool's file would help: %w", ErrUnreconciled, ctxErr)
		}

		log.Error("The configuration is rejected, and it is not this tool's file that " +
			"is doing it; leaving it alone")

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
		_ = writeAtomic(path, body)

		return true, fmt.Errorf("%w: removing this tool's file did not make the "+
			"configuration valid: %w", ErrUnreconciled, err)
	}

	return true, nil
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
		return fmt.Errorf("%w: it is rejected and the previous configuration is gone: %w",
			ErrUnreconciled, err)
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
			// Signalled, and nothing is running. That combination has exactly one
			// meaning: a master was told to reload and never came back.
			//
			// Reporting it as "nothing to adopt it" let the caller discard the
			// record — deleting the only copy of the configuration that worked —
			// and the host then sat with php-fpm down, the state file saying the
			// change had been applied, and the daemon logging "no pools found"
			// every round for ever. `php-fpm -t` passing says the file is
			// acceptable, not that a master survived re-reading it.
			log.Error("A previous run signalled the master and no master is running; "+
				"putting the previous configuration back", "file", txn.Path)

			previous, perr := previousContent(txn, opts.BackupDir)
			if perr != nil {
				return fmt.Errorf("%w: the master was signalled and is gone, and the "+
					"previous configuration with it: %w", ErrUnreconciled, perr)
			}
			if perr := applyPrevious(txn, previous); perr != nil {
				return fmt.Errorf("%w: the master was signalled and is gone, and the "+
					"change could not be taken back out: %w", ErrUnreconciled, perr)
			}

			return fmt.Errorf("%w: the master was signalled by a previous run and is not "+
				"running; its previous configuration has been restored, and it needs "+
				"starting", ErrUnreconciled)
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
