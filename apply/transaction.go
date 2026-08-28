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

// transaction records what a run is about to do, before it does any of it.
//
// The backup files alone were half a transaction, and the missing half showed in
// two places.
//
// A fragment that did NOT exist before has no backup — there is nothing to save
// — so a run that died after creating one left it behind with no trace that
// anyone had been there. Nothing could remove it, because nothing knew it was
// ours.
//
// And a backup on its own is not evidence that restoring it is right. Reconcile
// restored whenever the current configuration failed to validate, but the
// failure may have nothing to do with the unfinished run: an operator editing an
// unrelated pool at the wrong moment would have their work reverted to a state
// this tool happened to have a copy of.
//
// Recording the hash of what was written answers both. A file whose contents
// still match what the dead run wrote is ours to undo; one that does not has
// been touched by someone else and is left alone.
type transaction struct {
	DropInDir string    `json:"drop_in_dir"`
	Files     []txnFile `json:"files"`
}

type txnFile struct {
	Path string `json:"path"`

	// Existed distinguishes a fragment that was replaced from one that was
	// created. Undoing them differs: the first is rewritten from Saved, the
	// second is deleted.
	Existed bool   `json:"existed"`
	Saved   string `json:"saved,omitempty"`

	// Wrote is the SHA-256 of the content this run put at Path.
	Wrote string `json:"wrote"`
}

// transactionPath is where a master's in-flight record lives.
//
// Scoped by drop-in directory for the same reason the backups are: nothing stops
// two masters sharing the default backup directory, and acting on another
// master's record would mean restoring configuration into a pool directory it
// was never taken from.
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
	// because reconciliation would act on half of it.
	return writeAtomic(transactionPath(backupDir, txn.DropInDir), append(data, '\n'))
}

func readTransaction(backupDir, dropInDir string) (transaction, bool) {
	data, err := os.ReadFile(transactionPath(backupDir, dropInDir))
	if err != nil {
		return transaction{}, false
	}

	var txn transaction
	if err := json.Unmarshal(data, &txn); err != nil {
		return transaction{}, false
	}
	if filepath.Clean(txn.DropInDir) != filepath.Clean(dropInDir) {
		return transaction{}, false
	}
	if err := txn.valid(dropInDir); err != nil {
		return transaction{}, false
	}

	return txn, true
}

// valid checks a record before anything acts on it.
//
// Recovery deletes and overwrites files named in here. The record is written by
// this tool into a root-owned directory, so this is hardening rather than a live
// hole — but "the file said so" is not a reason to remove a path, and a
// truncated or hand-edited record should fail closed rather than half-apply.
func (t transaction) valid(dropInDir string) error {
	dir := filepath.Clean(dropInDir)
	seen := make(map[string]bool, len(t.Files))

	for _, f := range t.Files {
		path := filepath.Clean(f.Path)
		if filepath.Dir(path) != dir {
			return fmt.Errorf("%w: %s is not in %s", errBadTransaction, path, dir)
		}
		if seen[path] {
			return fmt.Errorf("%w: %s appears twice", errBadTransaction, path)
		}
		seen[path] = true

		if f.Existed != (f.Saved != "") {
			return fmt.Errorf("%w: %s says existed=%v with saved=%q",
				errBadTransaction, path, f.Existed, f.Saved)
		}
		// A bare name: Saved is joined onto the backup directory, and "../.."
		// would reach out of it.
		if f.Saved != "" && (f.Saved != filepath.Base(f.Saved) || f.Saved == "." || f.Saved == "..") {
			return fmt.Errorf("%w: %s names backup %q, which is not a bare filename",
				errBadTransaction, path, f.Saved)
		}
		if len(f.Wrote) != sha256.Size*2 {
			return fmt.Errorf("%w: %s records a malformed hash", errBadTransaction, path)
		}
		if _, err := hex.DecodeString(f.Wrote); err != nil {
			return fmt.Errorf("%w: %s records a malformed hash", errBadTransaction, path)
		}
	}

	return nil
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
