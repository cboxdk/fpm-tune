package apply

// Turning a pool's status page on is a change to a running master like any other,
// so it goes through the same order Apply does — validate a copy, write, validate
// the real tree, reload, and put the previous file back if the master does not come
// back. It writes a SEPARATE drop-in from the sizing one: the two are different
// concerns, this is written once and rarely changes, and keeping it apart means the
// sizing file's strict pm.max_children round-trip never has to carry a string value
// or a section with no ceiling.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cboxdk/phpfpm"
)

// DefaultStatusPath is the endpoint EnableStatus configures a pool's status page
// on. It is served over the pool's own listen socket, so no extra socket or port is
// opened.
const DefaultStatusPath = "/status"

// statusGeneratedMarker identifies the status drop-in as this tool's, the way
// generatedMarker does for the sizing one — checked before the file is read back or
// replaced, so an operator's own file under this name is never overwritten.
const statusGeneratedMarker = "; Written by fpm-tune to enable the pm.status_path it scrapes. Do not edit."

// StatusDropInPath is the file EnableStatus writes.
func StatusDropInPath(dir string) string {
	return filepath.Join(dir, "zz-fpm-tune-status.conf")
}

// ErrUnsafeStatusPath reports a status path that cannot be written safely.
var ErrUnsafeStatusPath = errors.New("status path is not a safe value")

// errNotOurStatusFormat reports a status file carrying this tool's marker whose
// body it would not have produced — refused rather than salvaged, the same as the
// sizing file, because its contents are fed straight back into the next version.
var errNotOurStatusFormat = errors.New("the status drop-in carries this tool's marker but not its contents")

// StatusResult reports what EnableStatus did.
type StatusResult struct {
	// Enabled lists the pools whose status page this call turned on.
	Enabled []string

	// Reloaded reports whether the master was signalled.
	Reloaded bool

	// RolledBack reports that the master did not survive and the previous status
	// file was put back.
	RolledBack bool

	// RollbackFailed names a status file php-fpm rejected that could not be removed
	// again — the same landmine Apply guards, where the next reload adopts it.
	RollbackFailed []string
}

// EnableStatus turns the status page on for the pools in `enable`, so a tool that
// scrapes pm.status can see them, and reloads the master.
//
// It keeps the page on for any pool this tool enabled before that STILL exists:
// `exists` is every pool the master currently serves, so a section for one that has
// been removed is dropped rather than left behind — a [pool] with a status path and
// no listen is a pool definition php-fpm refuses to start on, the same landmine the
// sizing file guards against.
func EnableStatus(
	ctx context.Context,
	master Master,
	enable []string,
	exists map[string]bool,
	statusPath string,
	opts Options,
	log *slog.Logger,
) (StatusResult, error) {
	opts = opts.Defaults()
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if statusPath == "" {
		statusPath = DefaultStatusPath
	}
	if err := safeStatusPath(statusPath); err != nil {
		return StatusResult{}, err
	}
	for _, name := range enable {
		if err := safePoolName(name); err != nil {
			return StatusResult{}, err
		}
	}

	path := StatusDropInPath(master.DropInDir)

	// What the file holds now, so re-running is idempotent and pools enabled on an
	// earlier run are carried forward rather than dropped.
	existing, err := parseStatusOurs(path)
	if err != nil {
		return StatusResult{}, err
	}

	desired := map[string]string{}
	// Carry forward only what is still real. A carried-forward section for a pool
	// whose own configuration has since been removed would be a [pool] with a
	// status path and no listen — which php-fpm will not start on.
	for name := range existing {
		if exists == nil || exists[name] {
			desired[name] = statusPath
		}
	}
	var enabled []string
	for _, name := range enable {
		if _, had := desired[name]; !had {
			enabled = append(enabled, name)
		}
		desired[name] = statusPath
	}

	// Nothing to enable and nothing real to keep: the file, if any, is stale. There
	// is nothing to turn on here, and removing a possibly-shared file is not this
	// path's job — a no-op is the honest answer.
	if len(desired) == 0 {
		return StatusResult{}, nil
	}

	rendered := renderStatus(desired)

	// Already exactly this: no reload for a no-op. A reload cycles workers, and
	// doing it to write bytes that are already on disk is the churn the tool is
	// otherwise careful to avoid.
	if current, rerr := os.ReadFile(path); rerr == nil && bytes.Equal(current, rendered) {
		return StatusResult{}, nil
	}

	// A status file the master does not include would validate, reload, and leave
	// the page still off — so the pool stays unstatused and this runs again every
	// time, for ever. Fail loudly instead of looping.
	if err := statusIncluded(master, path); err != nil {
		return StatusResult{}, err
	}

	// Validated against a copy first, so nothing unchecked is ever placed where
	// php-fpm reads it — the sizing path's hard-won order, reused.
	if err := validateStatusSandboxed(ctx, master, path, rendered); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return StatusResult{}, fmt.Errorf("ran out of time before the status configuration "+
				"could be checked; nothing was written: %w", ctxErr)
		}

		return StatusResult{}, err
	}

	if opts.DryRun {
		return StatusResult{Enabled: enabled}, nil
	}

	if !master.NoMasterExpected && master.PID <= 0 {
		return StatusResult{}, ErrMasterUnknown
	}

	// An in-memory backup is enough here, unlike the sizing plan. What this writes
	// is a single valid directive it has just validated, so a crash that loses the
	// backup leaves a harmless file rather than half a budget — there is nothing
	// for an on-disk recovery record to protect.
	b := backup{path: path}
	if old, rerr := os.ReadFile(path); rerr == nil {
		b.content, b.existed = old, true
	} else if !os.IsNotExist(rerr) {
		return StatusResult{}, fmt.Errorf("cannot read the existing %s to back it up, and "+
			"writing over a file that could not be preserved is how a rollback deletes a "+
			"configuration instead of restoring it: %w", path, rerr)
	}

	if err := writeAtomic(path, rendered); err != nil {
		return StatusResult{}, fmt.Errorf("cannot write %s: %w", path, err)
	}

	// Validated again against the real tree. A permission the sandbox did not have,
	// a path that only resolves in place — cheap insurance on the one operation that
	// can take the host down.
	if err := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); err != nil {
		if rerr := restore(b, log); rerr != nil {
			return StatusResult{RollbackFailed: []string{path}},
				fmt.Errorf("%w: %w; AND the rejected file could not be removed from %s — the "+
					"next reload from any source will adopt it", ErrValidationFailed, err, path)
		}

		return StatusResult{RolledBack: true}, fmt.Errorf("%w: %w", ErrValidationFailed, err)
	}

	if master.PID <= 0 {
		// Provisioning: the page is enabled for whenever php-fpm starts.
		phpfpm.InvalidateConfigCache(master.Binary, master.ConfigPath)

		return StatusResult{Enabled: enabled}, nil
	}

	if _, err := phpfpm.ReloadAndWait(ctx, phpfpm.ReloadTarget{
		PID:        master.PID,
		PIDFile:    master.PIDFile,
		ConfigPath: master.ConfigPath,
	}, opts.SettleTime, log); err != nil {
		neverSignalled := errors.Is(err, phpfpm.ErrNotAMaster)
		reason := ErrMasterDidNotSurvive
		if neverSignalled {
			reason = ErrMasterUnknown
		}

		if rerr := restore(b, log); rerr != nil {
			return StatusResult{RollbackFailed: []string{path}},
				fmt.Errorf("%w: %w; AND the file could not be taken back out of %s",
					reason, err, path)
		}

		// Best effort, exactly as Apply: the master is likely already gone, but
		// this is the difference between a host that recovers on restart and one
		// that comes back to the file that broke it.
		if !neverSignalled {
			if rerr := phpfpm.ReloadMaster(master.PID, master.ConfigPath); rerr != nil {
				log.Debug("Could not reload after restoring", "error", rerr)
			}
		}

		return StatusResult{RolledBack: true}, fmt.Errorf("%w: %w", reason, err)
	}

	// The parsed configuration is cached, so without this the next scrape would not
	// see the page we just turned on and would report the pool as unstatused still.
	phpfpm.InvalidateConfigCache(master.Binary, master.ConfigPath)

	return StatusResult{Enabled: enabled, Reloaded: true}, nil
}

// renderStatus produces the whole status file: one section per pool, each turning
// the status page on and nothing else.
func renderStatus(pools map[string]string) []byte {
	var b strings.Builder

	b.WriteString(statusGeneratedMarker + "\n")
	b.WriteString(";\n")
	b.WriteString("; fpm-tune sizes each pool from its live status page; this turns that page on\n")
	b.WriteString("; over the pool's own socket. Delete this file to turn it back off.\n")

	names := make([]string, 0, len(pools))
	for name := range pools {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		b.WriteString("\n")
		fmt.Fprintf(&b, "[%s]\n", name)
		fmt.Fprintf(&b, "pm.status_path = %s\n", pools[name])
	}

	return []byte(b.String())
}

// parseStatusOurs reads back the status file this tool wrote, refusing a file it
// did not write or would not have produced.
func parseStatusOurs(path string) (map[string]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}

		return nil, fmt.Errorf("%w: cannot read %s to see what already has a status page: %w",
			ErrUnreadableDropIn, path, err)
	}
	if !isStatusOurs(body) {
		return nil, fmt.Errorf("%w: %s exists and was not written by this tool. Move it "+
			"aside, or point --drop-in-dir somewhere else, and this will write its own",
			ErrForeignDropIn, path)
	}

	owned := map[string]string{}

	var current string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			if current == "" {
				return nil, fmt.Errorf("%w: %s has an empty section header", errNotOurStatusFormat, path)
			}
			if _, seen := owned[current]; seen {
				return nil, fmt.Errorf("%w: %s names pool %q twice", errNotOurStatusFormat, path, current)
			}
			owned[current] = ""

			continue
		}
		if current == "" {
			return nil, fmt.Errorf("%w: %s has a setting before any section", errNotOurStatusFormat, path)
		}

		key, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(key) != "pm.status_path" {
			return nil, fmt.Errorf("%w: %s contains %q, which this tool does not write there",
				errNotOurStatusFormat, path, line)
		}
		v := strings.TrimSpace(value)
		if err := safeStatusPath(v); err != nil {
			return nil, fmt.Errorf("%w: %s sets pm.status_path to %q", errNotOurStatusFormat, path, v)
		}
		owned[current] = v
	}

	// A section with no status path is not something this tool would have written.
	for name, v := range owned {
		if v == "" {
			return nil, fmt.Errorf("%w: %s declares pool %q with no pm.status_path",
				errNotOurStatusFormat, path, name)
		}
	}

	return owned, nil
}

func isStatusOurs(body []byte) bool {
	return strings.HasPrefix(strings.TrimSpace(string(body)), statusGeneratedMarker)
}

// safeStatusPath rejects a value that could not be written back into an ini file as
// a single directive. The tool sets "/status"; this guards a hand-edited file read
// back through parseStatusOurs.
func safeStatusPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%w: empty", ErrUnsafeStatusPath)
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("%w: %q does not start with /", ErrUnsafeStatusPath, p)
	case strings.ContainsAny(p, " \t\r\n\x00;#[]"):
		return fmt.Errorf("%w: %q contains an unsafe character", ErrUnsafeStatusPath, p)
	}

	return nil
}

// statusIncluded checks the master would actually read the status file.
func statusIncluded(master Master, path string) error {
	if len(master.IncludePatterns) == 0 {
		// The master config could not be read; nothing to check against, and
		// refusing here would break provisioning against a host where php-fpm is
		// not installed yet.
		return nil
	}
	for _, pattern := range master.IncludePatterns {
		if ok, err := filepath.Match(pattern, path); err == nil && ok {
			return nil
		}
	}

	return fmt.Errorf("%w: %s would be written to %s, which none of %v includes — turning "+
		"the page on there would validate and reload and have no effect",
		ErrNotIncluded, filepath.Base(path), path, master.IncludePatterns)
}

// validateStatusSandboxed checks the status file against a copy of the pool
// directory, so nothing unvalidated is ever placed where php-fpm reads it.
func validateStatusSandboxed(ctx context.Context, master Master, path string, content []byte) error {
	configPath, cleanup, err := sandboxReplacing(master, path, content)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := phpfpm.Validate(ctx, master.Binary, configPath); err != nil {
		return fmt.Errorf("%w: %w", ErrValidationFailed, err)
	}

	return nil
}
