package apply

import (
	"context"
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
		return nil
	}

	log.Warn("A previous run did not finish; checking what it left behind",
		"files", len(txn.Files), "dir", opts.BackupDir)

	if err := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); err == nil {
		log.Info("The configuration on disk is valid; the unfinished change stands")
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
