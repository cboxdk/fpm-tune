package apply

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cboxdk/fpm-tune/allocate"
)

// sandbox builds a throwaway copy of the pool directory with a change set
// overlaid, and returns a master config that includes it.
//
// This exists because validation used to happen in production. The old order was
// write the real fragments, then run `php-fpm -t`, then put them back if it
// failed — which meant every apply, and every DRY RUN, placed unvalidated
// configuration in the directory PHP-FPM includes by glob and left it there for
// as long as the fork took. Anything that reloaded the master in that window
// adopted it: a logrotate postrotate, an unrelated deploy, an operator. And a
// dry run is the one mode whose entire promise is that it changes nothing.
//
// A crash in the same window was worse, because nothing put the files back.
//
// Validating a copy first means the live directory is only ever written with a
// change set PHP-FPM has already accepted.
func sandbox(master Master, changes []allocate.PoolPlan) (string, func(), error) {
	dir, err := os.MkdirTemp("", "fpm-tune-validate-*")
	if err != nil {
		return "", nil, fmt.Errorf("cannot create a validation sandbox: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	poolDir := filepath.Join(dir, "pool.d")
	if err := os.MkdirAll(poolDir, 0o755); err != nil {
		cleanup()

		return "", nil, fmt.Errorf("cannot create a validation sandbox: %w", err)
	}

	// The pool directory is copied whole rather than just the changed fragments:
	// a pool's settings are spread across the files that define it, and
	// validating one in isolation would miss the errors that only appear once
	// PHP-FPM has merged them — a duplicate listen, a user that does not exist.
	if err := copyConfDir(master.DropInDir, poolDir, master.ConfigPath); err != nil {
		cleanup()

		return "", nil, err
	}

	for _, pp := range changes {
		if err := safePoolName(pp.Name); err != nil {
			cleanup()

			return "", nil, err
		}
		if err := os.WriteFile(DropInPath(poolDir, pp.Name), Render(pp), 0o644); err != nil {
			cleanup()

			return "", nil, fmt.Errorf("cannot stage %s for validation: %w", pp.Name, err)
		}
	}

	configPath := filepath.Join(dir, "php-fpm.conf")
	config, err := rewriteInclude(master.ConfigPath, master.DropInDir, poolDir)
	if err != nil {
		cleanup()

		return "", nil, err
	}
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		cleanup()

		return "", nil, fmt.Errorf("cannot write the validation config: %w", err)
	}

	return configPath, cleanup, nil
}

// copyConfDir copies the pool directory into the sandbox.
//
// Every regular file, not just *.conf: the include glob is the master's to
// choose, and a fragment left out of the copy is one php-fpm merges in
// production but not in the rehearsal — which is the one thing a sandbox must
// not get wrong. Copying a file php-fpm ignores costs nothing.
func copyConfDir(src, dst, masterConfig string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		// A pool directory that does not exist yet is a legitimate state during
		// provisioning: the sandbox then holds only what is being written.
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("cannot read %s: %w", src, err)
	}

	masterConfig = filepath.Clean(masterConfig)
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		// A master config that lives inside the directory it globs would be
		// copied in as a fragment, and its own include line would then pull the
		// real pool directory into the sandbox — validating production while
		// claiming not to touch it.
		if filepath.Join(src, e.Name()) == masterConfig {
			continue
		}
		content, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), content, 0o644); err != nil {
			return fmt.Errorf("cannot stage %s: %w", e.Name(), err)
		}
	}

	return nil
}

// rewriteInclude points the master config's pool include at the sandbox.
//
// Only include lines that resolve to the real pool directory are touched.
// A master config may include several trees — a distribution's own conf.d
// alongside the pools — and redirecting all of them would validate something
// other than what will run.
func rewriteInclude(configPath, realDir, sandboxDir string) ([]byte, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", configPath, err)
	}

	realDir = filepath.Clean(realDir)
	lines := strings.Split(string(data), "\n")

	redirected := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, found := strings.Cut(trimmed, "=")
		if !found || strings.TrimSpace(key) != "include" {
			continue
		}

		pattern := strings.TrimSpace(value)
		dir := filepath.Dir(pattern)
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(filepath.Dir(configPath), dir)
		}
		if filepath.Clean(dir) != realDir {
			continue
		}

		lines[i] = "include = " + filepath.Join(sandboxDir, filepath.Base(pattern))
		redirected = true
	}

	if !redirected {
		// The drop-in directory was set explicitly and is not what the master
		// includes — a legitimate configuration, and one where the sandbox must
		// still see the fragments being validated.
		lines = append(lines, "include = "+filepath.Join(sandboxDir, "*.conf"), "")
	}

	return []byte(strings.Join(lines, "\n")), nil
}
