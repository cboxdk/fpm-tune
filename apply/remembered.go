package apply

import (
	"encoding/json"
	"os"
	"path/filepath"
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
const rememberedMasterFile = "master.json"

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
	_ = writeAtomic(filepath.Join(backupDir, rememberedMasterFile), append(data, '\n'))
}

// RememberedMaster is rememberedMaster for callers outside this package: the
// daemon needs the pool directory to reconcile at all, and on the host this
// matters for it has no other way to learn it.
func RememberedMaster(backupDir string) Master {
	ref := rememberedMaster(backupDir)

	return Master{Binary: ref.Binary, ConfigPath: ref.ConfigPath, DropInDir: ref.DropInDir, PIDFile: ref.PIDFile}
}

// rememberedMaster reads it back. A zero value when there is nothing to read,
// which the caller treats as "no hint".
func rememberedMaster(backupDir string) rememberedMasterRef {
	if backupDir == "" {
		return rememberedMasterRef{}
	}

	data, err := os.ReadFile(filepath.Join(backupDir, rememberedMasterFile))
	if err != nil {
		return rememberedMasterRef{}
	}

	var ref rememberedMasterRef
	if err := json.Unmarshal(data, &ref); err != nil {
		return rememberedMasterRef{}
	}

	return ref
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
