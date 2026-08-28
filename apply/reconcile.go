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
// PHP-FPM will not accept, and it could not be undone.
var ErrUnreconciled = errors.New("a previous run left a rejected configuration in place")

// Reconcile cleans up after a run that did not finish.
//
// A transaction record is written before the first live write and removed once
// the change has settled, so finding one at startup means a process died in
// between: the OOM reaper, a reboot, ctrl-c during the reload. Until this
// existed nothing ever looked, and the fragments stayed — possibly ones php-fpm
// had already rejected, waiting for the next reload from any source to adopt
// them and take the master down for good.
//
// Two rules make it safe to run automatically.
//
// It trusts the validator over the bookkeeping. If what is on disk now passes
// `php-fpm -t` the change either stuck or was already undone, and the record is
// stale; undoing a good configuration because a process died after writing it
// would be its own outage.
//
// And it only touches files it can prove are its own, by comparing them against
// the hash the dead run recorded. A configuration can fail to validate for
// reasons that have nothing to do with the unfinished run — an operator editing
// an unrelated pool at the wrong moment — and reverting their work to a state
// this tool happens to hold a copy of is not a repair.
func Reconcile(ctx context.Context, master Master, opts Options, log *slog.Logger) error {
	opts = opts.Defaults()
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	txn, found := readTransaction(opts.BackupDir, master.DropInDir)
	if !found {
		// No record, so nothing was in flight. Any saved copies still lying
		// around belong to a transaction that was closed after they were taken —
		// the record is removed first precisely so this is the harmless order —
		// and nothing will ever read them again.
		sweepOrphanBackups(opts.BackupDir, master.DropInDir, log)

		return nil
	}

	log.Warn("A previous run did not finish; checking what it left behind",
		"files", len(txn.Files), "dir", opts.BackupDir)

	if err := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); err == nil {
		// Valid, so the change is kept — but validation is not the commit point
		// for a RUNNING master. The run may have died after writing the files
		// and before signalling, in which case the master is still serving the
		// old configuration while the files on disk say otherwise. Nothing would
		// ever correct that: the next round reads the configuration from the
		// files, concludes the pool is already where it wants it, and never
		// reloads. So the transaction is finished rather than merely accepted.
		if err := finishReload(ctx, master, opts, log); err != nil {
			return err
		}

		discard(txn, opts.BackupDir, log)

		return nil
	}

	log.Error("The configuration left on disk is invalid; undoing what the run had written")

	var failed, foreign []string
	for _, file := range txn.Files {
		live := hashOf(file.Path)

		switch {
		case live == "" && !file.Existed:
			// Never created, or already removed. Nothing to undo.
			continue

		case live != file.Wrote:
			// Not what the dead run wrote. Someone else owns this file now, and
			// reverting it would be destroying their work rather than ours.
			foreign = append(foreign, file.Path)

			continue
		}

		if err := undo(file, opts.BackupDir); err != nil {
			log.Error("Could not undo", "path", file.Path, "error", err)
			failed = append(failed, file.Path)
		}
	}

	if len(foreign) > 0 {
		log.Warn("Left alone: changed since the unfinished run wrote them, so they are "+
			"no longer ours to undo", "paths", foreign)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%w, and it could not be undone: %s",
			ErrUnreconciled, strings.Join(failed, ", "))
	}

	// Validated again rather than assumed. What the run replaced is not proof of
	// valid — it may have been correcting something already broken — and if
	// files were left alone above, the thing that breaks the config may be one
	// of those.
	if err := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); err != nil {
		return fmt.Errorf("%w: undoing it was not enough and the configuration is still "+
			"rejected: %w", ErrUnreconciled, err)
	}

	discard(txn, opts.BackupDir, log)
	log.Info("Undid the unfinished change; the configuration validates")

	return nil
}

// finishReload completes an unfinished apply by doing what it did not get to.
//
// Idempotent by nature: a master that already adopted these files re-reads the
// same ones. The cost is one graceful reload at startup, and the alternative is
// a host whose configuration files and running master disagree with nothing to
// notice it.
func finishReload(ctx context.Context, master Master, opts Options, log *slog.Logger) error {
	if master.PID <= 0 {
		log.Info("The configuration a previous run left is valid; no master is running to adopt it")

		return nil
	}

	log.Warn("The configuration a previous run left is valid but may never have been "+
		"adopted; reloading to be sure", "pid", master.PID)

	if _, err := phpfpm.ReloadAndWait(ctx, phpfpm.ReloadTarget{
		PID:        master.PID,
		PIDFile:    master.PIDFile,
		ConfigPath: master.ConfigPath,
	}, opts.SettleTime, log); err != nil {
		return fmt.Errorf("%w: the leftover configuration is valid but the master could "+
			"not be reloaded to adopt it: %w", ErrUnreconciled, err)
	}

	phpfpm.InvalidateConfigCache(master.Binary, master.ConfigPath)

	return nil
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

// backupTarget maps a saved fragment back to the file it came from, and reports
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

// undo puts one file back the way the transaction found it.
func undo(file txnFile, backupDir string) error {
	if !file.Existed {
		// There was no fragment before, so undoing means removing ours rather
		// than leaving an empty one behind — an empty [pool] section is still a
		// pool definition.
		if err := os.Remove(file.Path); err != nil && !os.IsNotExist(err) {
			return err
		}

		return nil
	}

	if file.Saved == "" {
		return errors.New("the transaction says a file was replaced but names no backup")
	}

	content, err := os.ReadFile(filepath.Join(backupDir, file.Saved))
	if err != nil {
		return err
	}

	return writeAtomic(file.Path, content)
}

// discard removes a finished transaction and the copies it took.
func discard(txn transaction, backupDir string, log *slog.Logger) {
	for _, file := range txn.Files {
		if file.Saved == "" {
			continue
		}
		if err := os.Remove(filepath.Join(backupDir, file.Saved)); err != nil && !os.IsNotExist(err) {
			log.Debug("Could not remove a backup", "name", file.Saved, "error", err)
		}
	}

	clearTransaction(backupDir, txn.DropInDir)
}
