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
	"crypto/sha256"
	"encoding/hex"
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

	// PID is the running master to signal.
	//
	// Zero means "no master is running": write the files and do not reload,
	// which is what provisioning wants before PHP-FPM starts. It must NOT be
	// used for "a master is running but I could not find it" — that combination
	// writes configuration, skips the reload, records the pools as applied and
	// never retries. Set NoMasterExpected to say the zero is deliberate.
	PID int

	// NoMasterExpected confirms that PID == 0 means there is nothing to reload,
	// rather than a master this caller failed to identify.
	NoMasterExpected bool

	// IncludePattern is the glob the master actually includes pool files by,
	// when it could be read.
	//
	// Checked against the fragments about to be written, because writing one the
	// master does not include is the quietest possible failure: `php-fpm -t`
	// passes, the reload succeeds, the change is recorded as applied, and the
	// running configuration is exactly what it was. A master including
	// [a-y]*.conf, or naming its pool files individually, does not pick up
	// zz-fpm-tune-www.conf.
	IncludePattern string

	// PIDFile is the master's pid file, when it has one.
	//
	// Used only to recognise the master after a reload: a daemonized php-fpm
	// re-execs into a NEW pid and rewrites this file, and without it a perfectly
	// successful reload looks like a master that died.
	PIDFile string

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
	//
	// It governs GROWTH. Shrinking is damped harder — see ShrinkMinChange.
	MinChange float64

	// ShrinkMinChange and ShrinkMinInterval damp shrinking harder than growing.
	//
	// The asymmetry is deliberate and it is what stops the tool oscillating. A
	// symmetric threshold lets a pool cross it in both directions on adjacent
	// rounds: demand rises, the pool grows, the peak decays, the pool shrinks
	// back, demand rises again — a host reloading every pool every few minutes,
	// each reload individually justified. Making the down leg need a larger
	// change held for longer breaks the cycle, and it breaks it in the safe
	// direction: the cost of growing a little too eagerly is some unused memory,
	// and the cost of shrinking too eagerly is requests queueing behind workers
	// that no longer exist.
	//
	// Zero means twice MinChange and four times MinInterval.
	ShrinkMinChange   float64
	ShrinkMinInterval time.Duration

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
	if o.ShrinkMinChange <= 0 {
		o.ShrinkMinChange = o.MinChange * 2
	}
	if o.ShrinkMinInterval <= 0 {
		o.ShrinkMinInterval = o.MinInterval * 4
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
	ActionUnknown   Action = "not written"

	// ActionHeldBack is a growth the budget cannot pay for yet, because the
	// reduction that would fund it is inside its own time brake.
	ActionHeldBack Action = "held back"
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
	// configuration WAS restored. Only true when every file went back.
	RolledBack bool

	// RollbackFailed reports the case that must never be confused with the one
	// above: something went wrong, and putting it back did not work either.
	//
	// The files php-fpm just rejected are still in the pool directory. The
	// master has not been reloaded, so nothing is broken yet — but the next
	// reload from any source, a logrotate or an unrelated deploy, adopts them,
	// and a master that fails to initialise does not come back. Verified against
	// a real master: a drop-in naming a pool that no longer exists exits `-t`
	// with 78 and kills the master permanently on the next SIGUSR2.
	RollbackFailed []string
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

// ErrNotIncluded reports that the fragments would be written somewhere the
// master does not read.
var ErrNotIncluded = errors.New("php-fpm does not include the files this would write")

// ErrMasterUnknown reports that no master PID was given and the caller did not
// confirm that none is expected. See Master.NoMasterExpected.
var ErrMasterUnknown = errors.New("no php-fpm master to reload, and none was expected to be absent")

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

	changes = requireShrinksWithGrowth(plan, changes, &result)

	if len(changes) == 0 {
		return result, nil
	}

	// Refused BEFORE anything is written. This used to be checked after the
	// fragments were already on disk, so a caller that could not identify the
	// master left new configuration in the pool directory and then had to take
	// it back — and if the restore failed, or the process died, or anything
	// reloaded in between, the change was adopted with no master known to have
	// accepted it.
	// Not for a dry run: it reloads nothing, so it does not need a master, and
	// rehearsing a change set on a host where php-fpm is not up yet is a
	// legitimate thing to want.
	if !opts.DryRun && master.PID <= 0 && !master.NoMasterExpected {
		return result, ErrMasterUnknown
	}

	// A fragment php-fpm does not include is a change that validates, reloads,
	// records itself as applied, and does nothing at all.
	if err := includesOurFragments(master, changes); err != nil {
		return result, err
	}

	// Validated in a sandbox BEFORE anything is written live. The old order
	// wrote the real fragments first and put them back if `-t` failed, which
	// left unvalidated configuration in the directory PHP-FPM globs for as long
	// as the fork took — adopted by anything that reloaded in that window, and
	// left behind entirely if the process died there.
	if err := validateSandboxed(ctx, master, changes); err != nil {
		return result, err
	}

	if opts.DryRun {
		return result, nil
	}

	backups, err := writeDropIns(master, changes, opts, log)
	if err != nil {
		return result, err
	}

	// Validated again, now against the real tree. The sandbox is a faithful copy
	// but it is still a copy: a permission this process does not have, a path
	// that only resolves in place. Cheap insurance on the one path that can take
	// the host down.
	if err := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); err != nil {
		// Nothing has been signalled yet, so restoring here means the running
		// master never saw any of this.
		result.RollbackFailed = restore(backups, log)
		result.RolledBack = len(result.RollbackFailed) == 0
		if result.RolledBack {
			clearTransaction(opts.BackupDir, master.DropInDir)
		}

		if !result.RolledBack {
			return result, fmt.Errorf("%w: %w; AND the rejected configuration could not be "+
				"removed from %s — the next reload from any source will adopt it",
				ErrValidationFailed, err, strings.Join(result.RollbackFailed, ", "))
		}

		return result, fmt.Errorf("%w: %w", ErrValidationFailed, err)
	}

	if master.PID <= 0 {
		// Provisioning: the files are in place for whenever PHP-FPM starts.
		log.Info("Wrote pool configuration; no running master to reload", "pools", len(changes))
		phpfpm.InvalidateConfigCache(master.Binary, master.ConfigPath)
		cleanup(backups, log)
		clearTransaction(opts.BackupDir, master.DropInDir)
		record(st, changes, now)

		return result, nil
	}

	// PIDFile and ConfigPath let a reload be confirmed when the master comes
	// back under a NEW pid, which is what a daemonized reload does — php-fpm's
	// own default. Watching only the original pid reported those as a dead
	// master and rolled good changes back.
	newPID, err := phpfpm.ReloadAndWait(ctx, phpfpm.ReloadTarget{
		PID:        master.PID,
		PIDFile:    master.PIDFile,
		ConfigPath: master.ConfigPath,
	}, opts.SettleTime, log)
	if err != nil {
		// A cancelled context is not a dead master. The settle watch takes the
		// context, so a SIGTERM to fpm-tune during the watch surfaced here as
		// "the master did not survive" — and the handler rolled the (valid,
		// already-adopted) configuration back and reloaded a second time, on the
		// way out, on a master that was perfectly healthy. Shutting the daemon
		// down must not reconfigure the host.
		if ctxErr := ctx.Err(); ctxErr != nil {
			log.Warn("Interrupted while watching the master settle; the change stands",
				"pid", master.PID, "error", ctxErr)
			result.Reloaded = true
			phpfpm.InvalidateConfigCache(master.Binary, master.ConfigPath)
			cleanup(backups, log)
			clearTransaction(opts.BackupDir, master.DropInDir)
			record(st, changes, now)

			return result, nil
		}

		// Distinguished from a master that died, because the operator's next
		// move differs entirely. A reload that was never delivered means the
		// process we were about to signal is not the master — a stale pid, or a
		// number the kernel has since reused — and nothing happened to php-fpm
		// at all.
		neverSignalled := errors.Is(err, phpfpm.ErrNotAMaster)

		reason := ErrMasterDidNotSurvive
		if neverSignalled {
			reason = ErrMasterUnknown
			log.Error("The pid we were about to reload is not a php-fpm master; nothing was signalled",
				"pid", master.PID, "error", err)
		} else {
			log.Error("Master did not survive the reload; restoring the previous configuration",
				"pid", master.PID, "error", err)
		}
		result.RollbackFailed = restore(backups, log)
		result.RolledBack = len(result.RollbackFailed) == 0
		if result.RolledBack {
			clearTransaction(opts.BackupDir, master.DropInDir)
		}

		// Best effort: the master is already gone, so this may do nothing. It
		// costs one signal and is the difference between a host that recovers
		// when the master is restarted and one that comes back to the config
		// that broke it.
		if !neverSignalled {
			if rerr := phpfpm.Reload(master.PID); rerr != nil {
				log.Debug("Could not reload after restoring", "error", rerr)
			}
		}

		return result, fmt.Errorf("%w: %w", reason, err)
	}

	result.Reloaded = true
	if newPID != master.PID {
		log.Info("The master reloaded into a new pid", "was", master.PID, "now", newPID)
	}

	// The parsed configuration is cached, so without this the next scrape reads
	// the settings from before this change and reports a pool as configured for
	// what it used to be. Observed as a pool showing 4 workers hours after it had
	// been set to 12.
	phpfpm.InvalidateConfigCache(master.Binary, master.ConfigPath)

	cleanup(backups, log)
	clearTransaction(opts.BackupDir, master.DropInDir)
	record(st, changes, now)

	return result, nil
}

// decide reports whether a pool's plan is worth a reload.
func decide(pp allocate.PoolPlan, st *state.State, opts Options, now time.Time) (Outcome, bool) {
	out := Outcome{Pool: pp.Name, To: pp.MaxChildren, Action: ActionUnchanged}

	// A pool whose current configuration could not be read is never written.
	// The plan reserved memory for it so a neighbour could not take it, but
	// setting a ceiling requires knowing the one being replaced.
	if pp.Unknown {
		out.Action = ActionUnknown
		out.Reason = "current configuration could not be read"

		return out, false
	}

	var ps *state.PoolState
	if st != nil {
		ps = st.Pools[pp.Name]
	}

	// The OBSERVED ceiling, not the one this tool remembers setting. They differ
	// whenever anything else has touched the pool — a hand edit, a deploy that
	// replaced the fragment, someone deleting the drop-in to undo a change — and
	// in every one of those cases the memory is the wrong answer: the tool
	// concluded the pool was already where it had put it and did nothing, so an
	// undone change stayed undone and a hand edit was never reconciled.
	current := pp.Current
	if current <= 0 && ps != nil {
		current = ps.LastAppliedMaxChildren
	}
	out.From = current

	if current == pp.MaxChildren {
		out.Reason = fmt.Sprintf("already at %d", pp.MaxChildren)

		return out, false
	}

	// A pool with no readable current value and no history has nothing to
	// compare against, so the first write goes through.
	if current <= 0 {
		out.Action = ActionApplied
		out.Reason = "first configuration"

		return out, true
	}

	shrinking := pp.MaxChildren < current

	minChange, minInterval, direction := opts.MinChange, opts.MinInterval, "growth"
	if shrinking {
		minChange, minInterval, direction = opts.ShrinkMinChange, opts.ShrinkMinInterval, "shrink"
	}

	if ps != nil && !ps.LastAppliedAt.IsZero() && now.Sub(ps.LastAppliedAt) < minInterval {
		out.Action = ActionTooSoon
		out.Reason = fmt.Sprintf("last changed %s ago, waiting %s between %s reloads",
			now.Sub(ps.LastAppliedAt).Round(time.Second), minInterval, direction)

		return out, false
	}

	delta := float64(pp.MaxChildren-current) / float64(current)
	if delta < 0 {
		delta = -delta
	}
	if delta < minChange {
		out.Action = ActionTooSmall
		out.Reason = fmt.Sprintf("%d to %d is a %.0f%% change, below the %.0f%% %s threshold",
			current, pp.MaxChildren, delta*100, minChange*100, direction)

		return out, false
	}

	out.Action = ActionApplied
	out.Reason = fmt.Sprintf("%d to %d", current, pp.MaxChildren)

	return out, true
}

// requireShrinksWithGrowth pulls in the reductions a growth cannot fit without,
// and holds back the growths that still do not fit.
//
// The allocator divides ONE budget, so a pool being cut and a pool being grown
// in the same plan are two halves of the same decision. Filtering them
// independently breaks that: the growth clears the hysteresis threshold, the
// matching cut is a few percent and does not, and what reaches the host is the
// half that spends memory without the half that frees it — a plan that fit the
// budget applied as one that does not. Reproduced at three pools on 8GiB: the
// balanced plan fit with 300MiB to spare and the damped subset committed 9.1GiB.
//
// Two rules keep the coupling from becoming a hole in the damping next door.
//
// It pulls in only as much as the arithmetic demands, largest saving first.
// Forcing every reduction through whenever anything grew would be simpler and,
// on a host with several pools, means something is nearly always growing — so
// every shrink would fire the moment it was proposed, which is the oscillation
// the thresholds exist to stop.
//
// And it only overrides the SIZE threshold, never the time brake. A pool
// reloaded a minute ago must not be reloaded again because a neighbour wants to
// grow; the reload is the cost being managed. When the memory can only come
// from a pool that is still inside its interval, the growth waits instead.
// Deferring a growth costs some queueing on one pool. Taking the memory anyway
// would overcommit the host, and taking it by reloading a pool every minute
// would be the flap.
func requireShrinksWithGrowth(plan allocate.Plan, changes []allocate.PoolPlan, result *Result) []allocate.PoolPlan {
	limit := plan.TotalBytes - plan.ReserveBytes
	if limit <= 0 {
		return changes
	}

	committed := commitmentOf(plan, changes)
	if committed <= limit {
		return changes
	}

	action := make(map[string]Action, len(result.Outcomes))
	for _, o := range result.Outcomes {
		action[o.Pool] = o.Action
	}

	included := make(map[string]bool, len(changes))
	for _, pp := range changes {
		included[pp.Name] = true
	}

	type reduction struct {
		pool  allocate.PoolPlan
		frees int64
	}

	var available []reduction
	for _, pp := range plan.Pools {
		if pp.Unknown || included[pp.Name] || pp.Current <= 0 || pp.MaxChildren >= pp.Current {
			continue
		}
		// Only the ones damped for being small. A pool inside its reload
		// interval stays there.
		if action[pp.Name] != ActionTooSmall {
			continue
		}
		available = append(available, reduction{pp, int64(pp.Current-pp.MaxChildren) * pp.WorkerBytes})
	}
	sort.Slice(available, func(i, j int) bool { return available[i].frees > available[j].frees })

	for _, r := range available {
		if committed <= limit {
			break
		}

		changes = append(changes, r.pool)
		included[r.pool.Name] = true
		committed -= r.frees

		setOutcome(result, r.pool.Name, ActionApplied, fmt.Sprintf(
			"%d to %d, applied below the threshold because another pool is growing into this memory",
			r.pool.Current, r.pool.MaxChildren))
	}

	if committed <= limit {
		return changes
	}

	// The reductions available were not enough, so the growths have to give.
	// Largest first: dropping one big claim beats dropping several small ones,
	// and the small ones are the cheapest to satisfy on the next round.
	var growths []allocate.PoolPlan
	for _, pp := range changes {
		if pp.Current > 0 && pp.MaxChildren > pp.Current {
			growths = append(growths, pp)
		}
	}
	sort.Slice(growths, func(i, j int) bool {
		return int64(growths[i].MaxChildren-growths[i].Current)*growths[i].WorkerBytes >
			int64(growths[j].MaxChildren-growths[j].Current)*growths[j].WorkerBytes
	})

	for _, g := range growths {
		if committed <= limit {
			break
		}

		delete(included, g.Name)
		committed -= int64(g.MaxChildren-g.Current) * g.WorkerBytes

		setOutcome(result, g.Name, ActionHeldBack, fmt.Sprintf(
			"%d to %d needs memory another pool is holding, and that pool cannot be "+
				"reloaded again yet", g.Current, g.MaxChildren))
	}

	kept := changes[:0]
	for _, pp := range changes {
		if included[pp.Name] {
			kept = append(kept, pp)
		}
	}

	return kept
}

// commitmentOf is what the host would be running with these changes applied and
// everything else left where it is.
func commitmentOf(plan allocate.Plan, changes []allocate.PoolPlan) int64 {
	included := make(map[string]bool, len(changes))
	for _, pp := range changes {
		included[pp.Name] = true
	}

	var total int64
	for _, pp := range plan.Pools {
		n := pp.Current
		if included[pp.Name] || n <= 0 {
			n = pp.MaxChildren
		}
		total += int64(n) * pp.WorkerBytes
	}

	return total
}

func setOutcome(result *Result, pool string, action Action, reason string) {
	for i := range result.Outcomes {
		if result.Outcomes[i].Pool == pool {
			result.Outcomes[i].Action = action
			result.Outcomes[i].Reason = reason
		}
	}
}

// includesOurFragments checks that the master would actually read what is about
// to be written.
func includesOurFragments(master Master, changes []allocate.PoolPlan) error {
	if master.IncludePattern == "" {
		// The pattern could not be read, or the drop-in directory was set
		// explicitly and does not correspond to one. Nothing to check against;
		// refusing here would break a legitimate configuration.
		return nil
	}

	for _, pp := range changes {
		name := filepath.Base(DropInPath(master.DropInDir, pp.Name))

		ok, err := filepath.Match(filepath.Base(master.IncludePattern), name)
		if err != nil || !ok {
			return fmt.Errorf("%w: %s would be written as %s, which %q does not match — "+
				"the change would validate and reload and have no effect",
				ErrNotIncluded, pp.Name, name, master.IncludePattern)
		}
	}

	return nil
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

	// rendered is what this run is about to write, kept so the transaction can
	// record its hash before anything reaches the live directory.
	rendered []byte
}

// writeDropIns saves the current fragments and writes the new ones.
func writeDropIns(master Master, changes []allocate.PoolPlan, opts Options, log *slog.Logger) ([]backup, error) {
	if err := os.MkdirAll(opts.BackupDir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create backup directory: %w", err)
	}

	// Everything is prepared and recorded BEFORE the first live write. A run
	// that dies partway through then leaves a record of what it intended, which
	// is the only thing that makes the leftovers recoverable — a fragment that
	// did not exist before has no backup, so without this there was nothing at
	// all to say it was ours.
	backups := make([]backup, 0, len(changes))
	txn := transaction{DropInDir: master.DropInDir}

	for _, pp := range changes {
		if err := safePoolName(pp.Name); err != nil {
			return nil, err
		}

		path := DropInPath(master.DropInDir, pp.Name)
		rendered := Render(pp)

		b := backup{path: path, rendered: rendered}
		if content, err := os.ReadFile(path); err == nil {
			b.content, b.existed = content, true
			b.saved = filepath.Join(opts.BackupDir, backupName(master.DropInDir, path))
		}

		backups = append(backups, b)
		txn.Files = append(txn.Files, txnFile{
			Path:    path,
			Existed: b.existed,
			Saved:   filepath.Base(b.saved),
			Wrote:   hashBytes(rendered),
		})
	}

	for i := range backups {
		if !backups[i].existed {
			continue
		}
		if err := writeAtomic(backups[i].saved, backups[i].content); err != nil {
			restore(backups[:i], log)

			return nil, fmt.Errorf("cannot back up %s: %w", backups[i].path, err)
		}
	}

	if err := writeTransaction(opts.BackupDir, txn); err != nil {
		restore(backups, log)

		return nil, err
	}

	for i := range backups {
		if err := writeAtomic(backups[i].path, backups[i].rendered); err != nil {
			restore(backups, log)
			clearTransaction(opts.BackupDir, master.DropInDir)

			return nil, fmt.Errorf("cannot write %s: %w", backups[i].path, err)
		}
	}

	return backups, nil
}

// restore puts the previous fragments back.
// restore puts the previous fragments back, and reports the ones it could not.
//
// The return value is the point. This used to log and move on while the caller
// set RolledBack unconditionally, so `fpm-tune apply` printed "the previous
// configuration has been restored" with the configuration php-fpm had just
// rejected still sitting in the pool directory, armed for whatever reloads next.
func restore(backups []backup, log *slog.Logger) []string {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	var failed []string
	for _, b := range backups {
		var err error
		if b.existed {
			err = writeAtomic(b.path, b.content)
		} else {
			// There was no fragment before, so undoing means removing ours
			// rather than leaving an empty one behind.
			err = os.Remove(b.path)
			if os.IsNotExist(err) {
				err = nil
			}
		}
		if err != nil {
			log.Error("Could not restore previous configuration", "path", b.path, "error", err)
			failed = append(failed, b.path)

			// The saved copy is the only remaining route back, so it stays.
			continue
		}
		if b.saved != "" {
			_ = os.Remove(b.saved)
		}
	}

	return failed
}

// writeAtomic writes a file via a temporary name and a rename, so a reader never
// sees a half-written fragment and a crash never leaves one.
func writeAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()

		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()

		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}

	return os.Rename(tmpName, path)
}

// backupName scopes a saved fragment to the master it came from.
//
// MasterFrom refuses a host with more than one master and tells the operator to
// run once per master with the drop-in directory set — but nothing stops both
// runs sharing the default backup directory. Reconcile would then find the other
// master's saved fragments, decide they belonged to this one, and write them
// into a pool directory they were never taken from.
//
// The prefix is the drop-in directory the file came out of, so each master only
// ever reconciles its own.
func backupName(dropInDir, path string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(dropInDir)))

	return hex.EncodeToString(sum[:4]) + "-" + filepath.Base(path) + ".bak"
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

// validateSandboxed checks a change set against a copy of the pool directory,
// so nothing unvalidated is ever placed where PHP-FPM would read it.
func validateSandboxed(ctx context.Context, master Master, changes []allocate.PoolPlan) error {
	configPath, cleanup, err := sandbox(master, changes)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := phpfpm.Validate(ctx, master.Binary, configPath); err != nil {
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

// ErrUnsafePoolName reports a pool name that cannot be used as a filename.
var ErrUnsafePoolName = errors.New("pool name is not usable as a filename")

// safePoolName rejects a name that would escape the drop-in directory.
//
// Pool names come from section headers in root-owned configuration, so this is
// hardening rather than a live hole — but php-fpm genuinely accepts a section
// called [a/../../tmp/pwn] and reports it verbatim from `-tt`, so the name that
// reaches this package is not guaranteed to be a bare word. Joining it into a
// path put files outside the pool directory entirely.
func safePoolName(pool string) error {
	switch {
	case pool == "":
		return fmt.Errorf("%w: empty", ErrUnsafePoolName)
	case strings.ContainsAny(pool, `/\`):
		return fmt.Errorf("%w: %q contains a path separator", ErrUnsafePoolName, pool)
	case strings.ContainsAny(pool, "\n\r\x00"):
		return fmt.Errorf("%w: %q contains a control character", ErrUnsafePoolName, pool)
	case pool == "." || pool == "..":
		return fmt.Errorf("%w: %q", ErrUnsafePoolName, pool)
	}

	return nil
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
