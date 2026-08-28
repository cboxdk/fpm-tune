// Package apply writes a plan to disk and asks PHP-FPM to adopt it.
//
// This is the only package that changes a running system, so its shape is
// dictated by how PHP-FPM fails rather than by what is convenient. A bad
// configuration does not degrade: the master refuses to come back from a reload
// and takes every pool with it. So nothing is reloaded before it has been
// validated, nothing is written that cannot be undone, and a master that does
// not return is put back the way it was.
package apply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/state"
	"github.com/cboxdk/phpfpm"
)

// DefaultBackupDir is where the previous drop-ins are kept while a change is in
// flight.
//
// Deliberately outside the pool-config directory. PHP-FPM includes that
// directory by glob, and a backup file that happens to match — now or after
// someone widens the pattern — would be loaded as configuration.
const DefaultBackupDir = "/var/lib/fpm-tune/backup"

// Master identifies the PHP-FPM instance being reconfigured.
//
// All the pools on one master share a single reload, which is why this is one
// struct rather than a field on each pool: the change set is applied and
// validated together, or not at all.
type Master struct {
	// Binary and ConfigPath are what `php-fpm -t` is run against. The config is
	// the master file, not the drop-in — `-t` walks the include tree, so a
	// broken fragment is caught through its parent.
	Binary     string
	ConfigPath string

	// PID is the running master to signal. Zero means write the files but do not
	// reload, which is what a provisioning run wants before PHP-FPM has started.
	PID int

	// DropInDir is where per-pool fragments are written.
	DropInDir string
}

// Options tunes when a change is worth making.
type Options struct {
	// MinInterval is the shortest time between reloads of one master.
	//
	// A reload is graceful but not free: workers finish their current request
	// and are replaced, so a host reloaded every thirty seconds spends its time
	// cycling workers instead of serving. This is the brake.
	MinInterval time.Duration

	// MinChange is the smallest relative change worth a reload, as a fraction.
	// Moving a pool from 20 workers to 21 is not worth interrupting it for.
	MinChange float64

	// SettleTime is how long the master is watched after a reload before the
	// change is called successful.
	SettleTime time.Duration

	// BackupDir overrides DefaultBackupDir.
	BackupDir string

	// DryRun renders and validates without writing anything permanent or
	// reloading. The validation is real — it is the part worth rehearsing.
	DryRun bool
}

// Defaults fills in any unset option.
func (o Options) Defaults() Options {
	if o.MinInterval <= 0 {
		o.MinInterval = 5 * time.Minute
	}
	if o.MinChange <= 0 {
		o.MinChange = 0.15
	}
	if o.SettleTime <= 0 {
		o.SettleTime = 2 * time.Second
	}
	if o.BackupDir == "" {
		o.BackupDir = DefaultBackupDir
	}

	return o
}

// Action is what happened to one pool.
type Action string

const (
	ActionApplied   Action = "applied"
	ActionUnchanged Action = "unchanged"
	ActionTooSoon   Action = "too soon"
	ActionTooSmall  Action = "too small"
)

// Outcome is what happened to one pool.
type Outcome struct {
	Pool   string
	Action Action
	From   int
	To     int
	Reason string
}

// Result is what happened overall.
type Result struct {
	Outcomes []Outcome

	// Reloaded reports whether the master was signalled. False with applied
	// pools means DryRun, or a master that is not running yet.
	Reloaded bool

	// RolledBack reports that something went wrong and the previous
	// configuration was restored.
	RolledBack bool
}

// Changed returns the pools that were actually written.
func (r Result) Changed() []Outcome {
	var out []Outcome
	for _, o := range r.Outcomes {
		if o.Action == ActionApplied {
			out = append(out, o)
		}
	}

	return out
}

// ErrValidationFailed reports that PHP-FPM rejected the rendered configuration.
// Nothing was reloaded, and the previous drop-ins have been restored.
var ErrValidationFailed = errors.New("php-fpm rejected the rendered configuration")

// ErrMasterDidNotSurvive reports that the master did not come back from the
// reload. The previous configuration has been restored and reloaded again.
var ErrMasterDidNotSurvive = errors.New("php-fpm master did not survive the reload")

// Apply writes the pools that are worth changing and reloads the master.
//
// The order is the safety property. Files are backed up, then written, then
// validated, and only then is anything signalled — so a configuration PHP-FPM
// will not accept never reaches a running master. If the master fails to come
// back anyway, the previous files are restored and it is reloaded again.
func Apply(
	ctx context.Context,
	plan allocate.Plan,
	master Master,
	st *state.State,
	opts Options,
	log *slog.Logger,
) (Result, error) {
	opts = opts.Defaults()
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	var result Result
	now := time.Now()

	changes := make([]allocate.PoolPlan, 0, len(plan.Pools))
	for _, pp := range plan.Pools {
		outcome, worth := decide(pp, st, opts, now)
		result.Outcomes = append(result.Outcomes, outcome)
		if worth {
			changes = append(changes, pp)
		}
	}
	sort.Slice(result.Outcomes, func(i, j int) bool {
		return result.Outcomes[i].Pool < result.Outcomes[j].Pool
	})

	if len(changes) == 0 {
		return result, nil
	}
	if opts.DryRun {
		// Still validate: rehearsing the part that can take the host down is the
		// point of a dry run, and it costs one fork.
		return result, validateRendered(ctx, master, changes, opts, log)
	}

	backups, err := writeDropIns(master, changes, opts)
	if err != nil {
		return result, err
	}

	if err := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); err != nil {
		// Nothing has been signalled yet, so restoring here means the running
		// master never saw any of this.
		restore(backups, log)
		result.RolledBack = true

		return result, fmt.Errorf("%w: %w", ErrValidationFailed, err)
	}

	if master.PID <= 0 {
		// Provisioning: the files are in place for whenever PHP-FPM starts.
		log.Info("Wrote pool configuration; no running master to reload", "pools", len(changes))
		phpfpm.InvalidateConfigCache(master.Binary, master.ConfigPath)
		cleanup(backups, log)
		record(st, changes, now)

		return result, nil
	}

	if err := phpfpm.ReloadAndWait(ctx, master.PID, opts.SettleTime, log); err != nil {
		log.Error("Master did not survive the reload; restoring the previous configuration",
			"pid", master.PID, "error", err)
		restore(backups, log)
		result.RolledBack = true

		// Best effort: the master is already gone, so this may do nothing. It
		// costs one signal and is the difference between a host that recovers
		// when the master is restarted and one that comes back to the config
		// that broke it.
		if rerr := phpfpm.Reload(master.PID); rerr != nil {
			log.Debug("Could not reload after restoring", "error", rerr)
		}

		return result, fmt.Errorf("%w: %w", ErrMasterDidNotSurvive, err)
	}

	result.Reloaded = true

	// The parsed configuration is cached, so without this the next scrape reads
	// the settings from before this change and reports a pool as configured for
	// what it used to be. Observed as a pool showing 4 workers hours after it had
	// been set to 12.
	phpfpm.InvalidateConfigCache(master.Binary, master.ConfigPath)

	cleanup(backups, log)
	record(st, changes, now)

	return result, nil
}

// decide reports whether a pool's plan is worth a reload.
func decide(pp allocate.PoolPlan, st *state.State, opts Options, now time.Time) (Outcome, bool) {
	out := Outcome{Pool: pp.Name, To: pp.MaxChildren, Action: ActionUnchanged}

	var ps *state.PoolState
	if st != nil {
		ps = st.Pools[pp.Name]
	}

	current := 0
	if ps != nil {
		current = ps.LastAppliedMaxChildren
	}
	out.From = current

	if current == pp.MaxChildren {
		out.Reason = fmt.Sprintf("already at %d", pp.MaxChildren)

		return out, false
	}

	// A pool never configured by this tool has no baseline to compare against,
	// so the first write always goes through.
	if current == 0 {
		out.Action = ActionApplied
		out.Reason = "first configuration"

		return out, true
	}

	if ps != nil && !ps.LastAppliedAt.IsZero() && now.Sub(ps.LastAppliedAt) < opts.MinInterval {
		out.Action = ActionTooSoon
		out.Reason = fmt.Sprintf("last changed %s ago, waiting %s between reloads",
			now.Sub(ps.LastAppliedAt).Round(time.Second), opts.MinInterval)

		return out, false
	}

	delta := float64(pp.MaxChildren-current) / float64(current)
	if delta < 0 {
		delta = -delta
	}
	if delta < opts.MinChange {
		out.Action = ActionTooSmall
		out.Reason = fmt.Sprintf("%d to %d is a %.0f%% change, below the %.0f%% threshold",
			current, pp.MaxChildren, delta*100, opts.MinChange*100)

		return out, false
	}

	out.Action = ActionApplied
	out.Reason = fmt.Sprintf("%d to %d", current, pp.MaxChildren)

	return out, true
}

// backup is one drop-in's previous state.
type backup struct {
	path string
	// content is what was there before; existed distinguishes an empty file from
	// no file, because undoing them differs — one is rewritten, the other is
	// deleted.
	content []byte
	existed bool
	saved   string
}

// writeDropIns saves the current fragments and writes the new ones.
func writeDropIns(master Master, changes []allocate.PoolPlan, opts Options) ([]backup, error) {
	if err := os.MkdirAll(opts.BackupDir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create backup directory: %w", err)
	}

	backups := make([]backup, 0, len(changes))
	for _, pp := range changes {
		path := DropInPath(master.DropInDir, pp.Name)

		b := backup{path: path}
		if content, err := os.ReadFile(path); err == nil {
			b.content, b.existed = content, true
			b.saved = filepath.Join(opts.BackupDir, filepath.Base(path)+".bak")
			if err := os.WriteFile(b.saved, content, 0o644); err != nil {
				restore(backups, nil)

				return nil, fmt.Errorf("cannot back up %s: %w", path, err)
			}
		}
		backups = append(backups, b)

		if err := os.WriteFile(path, Render(pp), 0o644); err != nil {
			restore(backups, nil)

			return nil, fmt.Errorf("cannot write %s: %w", path, err)
		}
	}

	return backups, nil
}

// restore puts the previous fragments back.
func restore(backups []backup, log *slog.Logger) {
	for _, b := range backups {
		var err error
		if b.existed {
			err = os.WriteFile(b.path, b.content, 0o644)
		} else {
			// There was no fragment before, so undoing means removing ours
			// rather than leaving an empty one behind.
			err = os.Remove(b.path)
			if os.IsNotExist(err) {
				err = nil
			}
		}
		if err != nil && log != nil {
			log.Error("Could not restore previous configuration", "path", b.path, "error", err)
		}
		if b.saved != "" {
			_ = os.Remove(b.saved)
		}
	}
}

// cleanup removes the saved copies once a change has stuck.
func cleanup(backups []backup, log *slog.Logger) {
	for _, b := range backups {
		if b.saved == "" {
			continue
		}
		if err := os.Remove(b.saved); err != nil && !os.IsNotExist(err) && log != nil {
			log.Debug("Could not remove backup", "path", b.saved, "error", err)
		}
	}
}

// validateRendered checks a change set without keeping it.
func validateRendered(ctx context.Context, master Master, changes []allocate.PoolPlan, opts Options, log *slog.Logger) error {
	backups, err := writeDropIns(master, changes, opts)
	if err != nil {
		return err
	}
	defer restore(backups, log)

	if err := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); err != nil {
		return fmt.Errorf("%w: %w", ErrValidationFailed, err)
	}

	return nil
}

func record(st *state.State, changes []allocate.PoolPlan, at time.Time) {
	if st == nil {
		return
	}
	for _, pp := range changes {
		st.RecordApplied(pp.Name, pp.MaxChildren, at)
	}
}

// DropInPath is where one pool's fragment lives.
//
// The zz- prefix makes it sort last in the include glob, so it wins over
// anything the distribution or the operator has already placed there.
func DropInPath(dir, pool string) string {
	return filepath.Join(dir, "zz-fpm-tune-"+pool+".conf")
}

// Render produces one pool's fragment.
//
// Only pm.* keys are written, and the pool's own configuration is not touched.
// PHP-FPM merges a section defined across several included files, so repeating
// the section header with just these keys overrides them and leaves listen, user
// and everything else exactly as the operator wrote it.
func Render(pp allocate.PoolPlan) []byte {
	var b strings.Builder

	b.WriteString("; Written by fpm-tune. Do not edit.\n")
	b.WriteString(";\n")
	b.WriteString("; This file overrides only the pm.* settings below; the pool's own\n")
	b.WriteString("; configuration is left alone. Delete it to return to the values\n")
	b.WriteString("; configured elsewhere.\n")
	if pp.Reason != "" {
		fmt.Fprintf(&b, ";\n; %s\n", pp.Reason)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "[%s]\n", pp.Name)
	fmt.Fprintf(&b, "pm.max_children = %d\n", pp.MaxChildren)

	// The spare settings are only meaningful for dynamic pools, and writing them
	// for a static one is a configuration error PHP-FPM will refuse.
	if pp.StartServers > 0 {
		fmt.Fprintf(&b, "pm.start_servers = %d\n", pp.StartServers)
		fmt.Fprintf(&b, "pm.min_spare_servers = %d\n", pp.MinSpare)
		fmt.Fprintf(&b, "pm.max_spare_servers = %d\n", pp.MaxSpare)
	}

	return []byte(b.String())
}
