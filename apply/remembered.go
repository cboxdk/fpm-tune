package apply

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// The master this tool last wrote for, kept beside the backups.
//
// Recovery needs a binary and a config path before it can ask php-fpm anything.
// It gets them from the transaction record when there is one, and from the
// caller otherwise — but the caller has neither when discovery failed, and
// discovery fails precisely because php-fpm is down, which is the case this
// whole path exists for. The state file carries the same thing and is not
// enough: it can be missing, deleted to reset the baselines, or written by a
// version that did not record it.
//
// Written by the code that has just proved the master is real, next to the
// files that describe what was done to it.
// Keyed by drop-in directory, the way transactions and backups are.
//
// One unkeyed file per backup directory was wrong the moment two masters share
// one — which is the default, since the backup directory has a default. The
// last successful apply's master overwrote the note, and a repair for the OTHER
// master then filled in that one's binary and config: it validated a tree it was
// not about to touch, found it fine, and returned without repairing the host
// that was actually down.
func rememberedMasterFile(dropInDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(dropInDir)))

	return hex.EncodeToString(sum[:4]) + "-master.json"
}

type rememberedMasterRef struct {
	Binary     string `json:"binary"`
	ConfigPath string `json:"config_path"`
	DropInDir  string `json:"drop_in_dir,omitempty"`
	PIDFile    string `json:"pid_file,omitempty"`
}

// rememberMaster records where php-fpm lives. Best effort: failing an apply
// because a hint could not be written would trade a working change for a
// convenience.
func rememberMaster(backupDir string, master Master) {
	if backupDir == "" || master.Binary == "" || master.ConfigPath == "" {
		return
	}

	data, err := json.Marshal(rememberedMasterRef{
		Binary: master.Binary, ConfigPath: master.ConfigPath,
		DropInDir: master.DropInDir, PIDFile: master.PIDFile,
	})
	if err != nil {
		return
	}
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return
	}
	_ = writeAtomic(filepath.Join(backupDir, rememberedMasterFile(master.DropInDir)), append(data, '\n'))
}

// RememberedMaster is rememberedMaster for callers outside this package: the
// daemon needs the pool directory to reconcile at all, and on the host this
// matters for it has no other way to learn it.
// The daemon needs the pool directory itself when it was given none, so it
// looks by directory when it has one and scans otherwise.
func RememberedMaster(backupDir, dropInDir string) Master {
	ref := rememberedMaster(backupDir, dropInDir)
	if ref.DropInDir == "" && dropInDir == "" {
		ref = onlyRememberedMaster(backupDir)
	}

	return Master{Binary: ref.Binary, ConfigPath: ref.ConfigPath, DropInDir: ref.DropInDir, PIDFile: ref.PIDFile}
}

// rememberedMaster reads it back. A zero value when there is nothing to read,
// which the caller treats as "no hint".
func rememberedMaster(backupDir, dropInDir string) rememberedMasterRef {
	if backupDir == "" || dropInDir == "" {
		return rememberedMasterRef{}
	}

	data, err := os.ReadFile(filepath.Join(backupDir, rememberedMasterFile(dropInDir)))
	if err != nil {
		return rememberedMasterRef{}
	}

	var ref rememberedMasterRef
	if err := json.Unmarshal(data, &ref); err != nil {
		return rememberedMasterRef{}
	}

	// And it must describe the directory it was asked about. The key already
	// says so, but this is a note used to decide what to run and what to
	// remove — a hash collision or a hand-copied backup directory should not be
	// enough to point a repair at another host's php-fpm.
	if filepath.Clean(ref.DropInDir) != filepath.Clean(dropInDir) {
		return rememberedMasterRef{}
	}

	return ref
}

// onlyRememberedMaster is the note when there is exactly ONE, for a daemon
// started with no pool directory at all.
//
// Exactly one, deliberately: with several, there is no way to tell which host
// is the one that is down, and guessing points a repair at a master that is
// fine. The operator names a directory in that case, and the error says so.
func onlyRememberedMaster(backupDir string) rememberedMasterRef {
	if backupDir == "" {
		return rememberedMasterRef{}
	}

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return rememberedMasterRef{}
	}

	var found rememberedMasterRef
	count := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "-master.json") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(backupDir, e.Name()))
		if rerr != nil {
			continue
		}
		var ref rememberedMasterRef
		if json.Unmarshal(data, &ref) != nil || ref.DropInDir == "" {
			continue
		}
		found = ref
		count++
	}
	if count != 1 {
		return rememberedMasterRef{}
	}

	return found
}

// filledFrom completes a Master from a remembered reference, filling only what
// is missing. What the caller knows always wins: the hint is from the last
// successful apply and the host may have moved since.
func (m Master) filledFrom(ref rememberedMasterRef) Master {
	if m.Binary == "" {
		m.Binary = ref.Binary
	}
	if m.ConfigPath == "" {
		m.ConfigPath = ref.ConfigPath
	}
	if m.DropInDir == "" {
		m.DropInDir = ref.DropInDir
	}
	if m.PIDFile == "" {
		m.PIDFile = ref.PIDFile
	}

	return m
}
