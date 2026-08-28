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
		if !errors.Is(err, ErrHeld) && err == nil {
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
