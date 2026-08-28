package apply

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Phase is how far a change had got when the process stopped.
//
// Recorded rather than inferred. The scheme this replaced reconstructed it from
// file hashes and `php-fpm -t`, which cannot see the thing that matters:
// whether the running master ever adopted the change. Validation passing says
// the files are acceptable, not that anyone has read them.
type Phase string

const (
	// PhaseWritten: the file is on disk. It may or may not have been validated,
	// and it certainly has not been signalled.
	PhaseWritten Phase = "written"

	// PhaseSignalled: SIGUSR2 was delivered. The master may have adopted it,
	// died, or still be settling — delivery is not survival.
	PhaseSignalled Phase = "signalled"
)

// transaction records what a run is about to do, before it does any of it.
//
// One file, one record, one rename. The scheme this replaced wrote a fragment
// per pool, so a run could die between them and leave half a plan on disk — and
// half a plan validates perfectly: the growth without the reduction that funds
// it passes `php-fpm -t` and commits the host past its budget. Recovery then had
// to reconstruct which of N files had landed, from hashes, without ever being
// able to see what the master had actually adopted.
//
// A single atomic rename removes that class entirely. The file holds either the
// old bytes or the new bytes, never a mixture, so there is nothing to
// reconstruct.
type transaction struct {
	DropInDir string `json:"drop_in_dir"`

	// Binary and ConfigPath are carried so recovery can run when no master can
	// be discovered. That is not hypothetical: if a reload killed the master and
	// then this process died, the next start finds nothing running — and the
	// configuration that killed it is still on disk, unreconciled, for every
	// restart attempt after that.
	Binary     string `json:"binary"`
	ConfigPath string `json:"config_path"`

	Path string `json:"path"`

	// Existed distinguishes a file that was replaced from one that was created.
	// Undoing them differs: the first is rewritten from Saved, the second is
	// deleted.
	Existed bool   `json:"existed"`
	Saved   string `json:"saved,omitempty"`

	// Wrote is the SHA-256 of the content this run put at Path.
	Wrote string `json:"wrote"`

	Phase Phase `json:"phase"`
}

// transactionPath is where a master's in-flight record lives.
//
// Scoped by drop-in directory because nothing stops two masters sharing the
// default backup directory, and acting on another master's record would mean
// restoring configuration into a pool directory it was never taken from.
func transactionPath(backupDir, dropInDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(dropInDir)))

	return filepath.Join(backupDir, hex.EncodeToString(sum[:4])+"-transaction.json")
}

func writeTransaction(backupDir string, txn transaction) error {
	data, err := json.MarshalIndent(txn, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot record the pending change: %w", err)
	}

	// Atomic, and fsynced by writeAtomic: a torn record is worse than none,
	// because recovery would act on half of it.
	return writeAtomic(transactionPath(backupDir, txn.DropInDir), append(data, '\n'))
}

// readTransaction distinguishes three states, and the distinction is the point.
//
//	(found=false, err=nil)  — no record: nothing was in flight.
//	(found=false, err!=nil) — a record that cannot be used: truncated, hand-edited,
//	                          written by an older build, naming another directory.
//	(found=true)            — a record to act on.
//
// Collapsing the middle case into the first is the opposite of failing closed. A
// record exists precisely because a run died with a change in flight, so the one
// situation where the saved copy is the only route back is the one in which it
// would have been swept away.
func readTransaction(backupDir, dropInDir string) (transaction, bool, error) {
	path := transactionPath(backupDir, dropInDir)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return transaction{}, false, nil
		}

		return transaction{}, false, fmt.Errorf("cannot read %s: %w", path, err)
	}

	var txn transaction
	if err := json.Unmarshal(data, &txn); err != nil {
		return transaction{}, false, fmt.Errorf("%w: %s is not readable JSON: %w",
			errBadTransaction, path, err)
	}
	if err := txn.valid(dropInDir); err != nil {
		return transaction{}, false, err
	}

	return txn, true, nil
}

// valid checks a record before anything acts on it.
//
// Recovery deletes and overwrites the file named in here. The record is written
// by this tool into a root-owned directory, so this is hardening rather than a
// live hole — but "the file said so" is not a reason to remove a path, and a
// truncated or hand-edited record must fail closed rather than half-apply.
func (t transaction) valid(dropInDir string) error {
	dir := filepath.Clean(dropInDir)

	// A mismatch is an UNUSABLE record, not an absent one. Treating it as
	// absence swept the backups of a run that had genuinely left something
	// behind.
	if filepath.Clean(t.DropInDir) != dir {
		return fmt.Errorf("%w: record names %s, not %s", errBadTransaction, t.DropInDir, dir)
	}
	if filepath.Clean(t.Path) != DropInPath(dir) {
		return fmt.Errorf("%w: %s is not this master's drop-in", errBadTransaction, t.Path)
	}
	if t.Existed != (t.Saved != "") {
		return fmt.Errorf("%w: existed=%v with saved=%q", errBadTransaction, t.Existed, t.Saved)
	}
	// A bare name: Saved is joined onto the backup directory, and "../.." would
	// reach out of it.
	if t.Saved != "" && (t.Saved != filepath.Base(t.Saved) || t.Saved == "." || t.Saved == "..") {
		return fmt.Errorf("%w: backup %q is not a bare filename", errBadTransaction, t.Saved)
	}
	if len(t.Wrote) != sha256.Size*2 {
		return fmt.Errorf("%w: malformed hash", errBadTransaction)
	}
	if _, err := hex.DecodeString(t.Wrote); err != nil {
		return fmt.Errorf("%w: malformed hash", errBadTransaction)
	}
	if t.Phase != PhaseWritten && t.Phase != PhaseSignalled {
		return fmt.Errorf("%w: unknown phase %q", errBadTransaction, t.Phase)
	}

	return nil
}

// landed reports whether the file this record describes is on disk with the
// contents it says were written.
//
// With one atomic rename this is a clean yes or no: the file holds either the
// old bytes or the new ones. No is not a failure — it means the rename never
// happened, so there is nothing to undo.
func (t transaction) landed() bool {
	return hashOf(t.Path) == t.Wrote
}

var errBadTransaction = errors.New("malformed transaction record")

func clearTransaction(backupDir, dropInDir string) {
	_ = os.Remove(transactionPath(backupDir, dropInDir))
}

// hashOf is the SHA-256 of a file, or "" if it is not there.
func hashOf(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}
