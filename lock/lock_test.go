package lock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSecondAcquireIsRefused is the property the package exists for: two
// fpm-tune processes writing the same pool fragments each take the other's
// half-applied state as "the previous configuration" to roll back to.
func TestSecondAcquireIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fpm-tune.lock")

	release, err := Acquire(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// A second attempt from THIS process would succeed on some platforms —
	// flock is per open file description, not per process — so the contending
	// acquire runs in a child.
	if held := acquiredInChild(t, path); held {
		t.Error("a second process took the lock while the first held it")
	}

	release()

	if held := acquiredInChild(t, path); !held {
		t.Error("the lock was not released")
	}
}

// TestAcquireDoesNotBlock: an operator running `fpm-tune apply` against a host
// with a daemon already running must get an answer, not a terminal that has
// stopped printing.
func TestAcquireDoesNotBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fpm-tune.lock")

	release, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	done := make(chan error, 1)
	go func() {
		_, err := runContender(path)
		done <- err
	}()

	select {
	case err := <-done:
		// `&& err == nil` was here, which made the whole condition false
		// whenever err was non-nil — so a contended acquire failing with the
		// WRONG error, or for the wrong reason entirely, passed silently. The
		// type is the assertion: callers branch on ErrHeld to print "another
		// fpm-tune is already running" rather than a filesystem error.
		if !errors.Is(err, ErrHeld) {
			t.Errorf("a contended acquire returned %v, want ErrHeld", err)
		}
	case <-t.Context().Done():
		t.Fatal("a contended acquire blocked instead of failing")
	}
}

// acquiredInChild reports whether a separate process could take the lock.
func acquiredInChild(t *testing.T, path string) bool {
	t.Helper()

	if os.Getenv("FPM_TUNE_LOCK_CHILD") == "1" {
		return false
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestLockChildHelper")
	cmd.Env = append(os.Environ(), "FPM_TUNE_LOCK_CHILD=1", "FPM_TUNE_LOCK_PATH="+path)

	return cmd.Run() == nil
}

func runContender(path string) (Release, error) {
	return Acquire(path)
}

// TestLockChildHelper is the child half of acquiredInChild. It exits non-zero
// when the lock is held, which is what the parent measures.
func TestLockChildHelper(t *testing.T) {
	if os.Getenv("FPM_TUNE_LOCK_CHILD") != "1" {
		t.Skip("helper process only")
	}

	release, err := Acquire(os.Getenv("FPM_TUNE_LOCK_PATH"))
	if err != nil {
		// A non-zero exit is the signal; t.Fatal produces one.
		t.Fatalf("held: %v", err)
	}
	release()
}

// TestAcquireOnAnUnwritableDirectoryIsAPermissionError pins the contract `plan`
// relies on. `plan` is read-only apart from recording a baseline, so when it
// cannot create the state directory — the ordinary first run, installed and run
// by a user who does not own /var/lib — it branches on os.ErrPermission to report
// without recording, rather than fail on a filesystem error before it has shown
// anything. The type is the assertion, as with ErrHeld above: if Acquire stops
// wrapping the mkdir failure so errors.Is can see it, that branch goes dead and
// the read-only promise the quickstart makes breaks on the very first command.
func TestAcquireOnAnUnwritableDirectoryIsAPermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can create a directory under a mode-0555 parent, so this cannot be provoked as root")
	}

	parent := t.TempDir()
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	// So t.TempDir's own cleanup can remove it again.
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	_, err := Acquire(filepath.Join(parent, "sub", "fpm-tune.lock"))
	if err == nil {
		t.Fatal("acquire under an unwritable directory unexpectedly succeeded")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("acquire under an unwritable directory returned %v, which is not an "+
			"os.ErrPermission; plan branches on that to keep its read-only promise", err)
	}
}

// TestTheResourceLockDoesNotFollowTMPDIR.
//
// The lock that stops two processes writing the same pool files has to be at a
// path both of them compute the same way. os.TempDir() reads $TMPDIR, which
// every process chooses for itself — so two runs against the same pool
// directory took two different lock files and both proceeded. Verified against
// the real binary: an apply under a different TMPDIR ran concurrently with a
// `serve --apply` daemon on the same directory.
func TestTheResourceLockDoesNotFollowTMPDIR(t *testing.T) {
	const dropInDir = "/etc/php-fpm.d"

	t.Setenv("TMPDIR", t.TempDir())
	first := ResourcePath(dropInDir)

	t.Setenv("TMPDIR", t.TempDir())
	second := ResourcePath(dropInDir)

	if first != second {
		t.Errorf("the same pool directory locks at %q under one TMPDIR and %q under "+
			"another; either process can take its own and both will write", first, second)
	}
}
