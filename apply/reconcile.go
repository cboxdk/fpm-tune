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
// The backup directory is written before the pool fragments and emptied after
// the change sticks, so anything found in it at startup means a previous process
// died in between: killed by the OOM reaper, the host rebooted, someone pressed
// ctrl-c during the reload. Until this existed nothing ever read those files
// back. They accumulated silently, and the fragment they were a backup OF stayed
// in the pool directory — possibly one PHP-FPM had already rejected, waiting to
// be adopted by the next reload from any source.
//
// The rule is to trust the validator, not the bookkeeping. If what is on disk
// now passes `php-fpm -t`, the change either stuck or was already undone, and
// the backups are stale; if it does not pass, they are the only route back.
func Reconcile(ctx context.Context, master Master, opts Options, log *slog.Logger) error {
	opts = opts.Defaults()
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	saved, err := staleBackups(opts.BackupDir, master.DropInDir)
	if err != nil || len(saved) == 0 {
		return err
	}

	log.Warn("A previous run did not finish; checking what it left behind",
		"backups", len(saved), "dir", opts.BackupDir)

	if err := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); err == nil {
		log.Info("The configuration on disk is valid; discarding the stale backups")
		for _, s := range saved {
			if rmErr := os.Remove(s); rmErr != nil && !os.IsNotExist(rmErr) {
				log.Warn("Could not remove a stale backup", "path", s, "error", rmErr)
			}
		}

		return nil
	}

	log.Error("The configuration left on disk is invalid; restoring the previous one")

	var failed []string
	for _, s := range saved {
		target, ok := backupTarget(master.DropInDir, s)
		if !ok {
			continue
		}

		content, readErr := os.ReadFile(s)
		if readErr != nil {
			log.Error("Could not read a backup", "path", s, "error", readErr)
			failed = append(failed, target)

			continue
		}
		if writeErr := writeAtomic(target, content); writeErr != nil {
			log.Error("Could not restore", "path", target, "error", writeErr)
			failed = append(failed, target)

			continue
		}
		_ = os.Remove(s)
	}

	if len(failed) > 0 {
		return fmt.Errorf("%w, and it could not be undone: %s",
			ErrUnreconciled, strings.Join(failed, ", "))
	}

	// Validated again rather than assumed: the backups are what was there before
	// the failed run, but "before" is not proof of "valid" — the run may have
	// been correcting something already broken.
	if err := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); err != nil {
		return fmt.Errorf("%w: the previous configuration was restored and is ALSO rejected: %w",
			ErrUnreconciled, err)
	}

	log.Info("Restored the previous configuration; it validates")

	return nil
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

// staleBackups lists this master's saved fragments left by an unfinished run.
func staleBackups(dir, dropInDir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("cannot read the backup directory: %w", err)
	}

	var saved []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".bak") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		// Another master's saved fragments live here too when the default
		// backup directory is shared. Restoring those into this master's pool
		// directory would be inventing configuration.
		if _, ok := backupTarget(dropInDir, full); !ok {
			continue
		}
		saved = append(saved, full)
	}

	return saved, nil
}
