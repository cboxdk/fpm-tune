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

// DefaultPath is the lock beside the state file, so one lock covers both the
// configuration writes and the state the next run reads.
func DefaultPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "fpm-tune.lock")
}
