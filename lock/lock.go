// Package lock serialises the processes that may write PHP-FPM configuration.
//
// Two fpm-tune runs against one host is not a hypothetical: a `serve --apply`
// daemon and an operator running `fpm-tune apply` by hand is the ordinary way
// this goes wrong, and cron plus a slow scrape is the other. Both write the same
// fragments and both keep a backup of "the previous configuration" — so the
// second run backs up the first run's half-applied state and, on rollback,
// restores that instead of what was there originally.
//
// The state file has the same problem from the other end: it is read at start
// and written whole, so two processes silently discard each other's learning.
package lock

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrHeld reports that another fpm-tune process holds the lock.
var ErrHeld = errors.New("another fpm-tune process is already running")

// Release gives the lock up.
type Release func()

// Acquire takes an exclusive lock, without blocking.
//
// Non-blocking deliberately. A blocking acquire would queue an operator's
// interactive `apply` behind a daemon that holds the lock every interval, and
// leave them looking at a terminal that has not printed anything. Failing
// immediately with a clear message lets them stop the daemon and try again.
func Acquire(path string) (Release, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("cannot create the lock directory: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cannot open the lock file %s: %w", path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()

		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (lock: %s)", ErrHeld, path)
		}

		return nil, fmt.Errorf("cannot lock %s: %w", path, err)
	}

	// The pid is written for the operator who finds the lock held and wants to
	// know by what. It is not read back: flock is what holds the lock, and a pid
	// file used as the lock is exactly the race this avoids.
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())

	return func() {
		// Closing releases the flock. The file is left in place: removing it
		// would let a process that opened it before the unlink lock a path
		// nothing else resolves to any more.
		_ = f.Close()
	}, nil
}

// DefaultPath is the lock beside the state file. It covers the state, which is
// read whole and written whole, so two processes sharing one file would discard
// each other's learning.
func DefaultPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "fpm-tune.lock")
}

// ResourcePath is the lock for a pool directory.
//
// Separate from the state lock because they protect different things and are
// not interchangeable. Two runs given different --state paths took different
// state locks and then wrote the SAME pool fragments and the same backups, each
// taking the other's half-applied state as "the previous configuration" — the
// exact interleaving the locking was added to prevent, reachable by passing a
// flag. This one is keyed on what is actually being written.
//
// It lives in a fixed directory, NOT in the backup directory. Keying it there
// made the lock defeatable by a flag: a daemon running with the defaults and an
// operator running `apply --backup-dir /tmp/b` computed different paths, both
// acquired cleanly, and both wrote the same pool file — the exact interleaving
// this exists to prevent, one flag over from the one it fixed.
//
// Not the pool directory either: php-fpm includes that by glob, and a lock file
// that happened to match the pattern would be read as configuration.
func ResourcePath(dropInDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(dropInDir)))

	return filepath.Join(resourceLockDir(), hex.EncodeToString(sum[:4])+"-apply.lock")
}

// resourceLockDir prefers /run, which is a tmpfs cleared on boot and the
// conventional home for this. It falls back to the temporary directory so an
// unprivileged run — a test, an operator trying something — still serialises
// against another unprivileged run.
func resourceLockDir() string {
	for _, dir := range []string{"/run", "/var/run"} {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			locks := filepath.Join(dir, "fpm-tune")
			if err := os.MkdirAll(locks, 0o755); err == nil {
				return locks
			}
		}
	}

	return filepath.Join(os.TempDir(), "fpm-tune")
}
