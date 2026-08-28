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

	// IncludePatterns are the globs the master actually includes pool files by,
	// when they could be read.
	//
	// Checked against the fragments about to be written, because writing one the
	// master does not include is the quietest possible failure: `php-fpm -t`
	// passes, the reload succeeds, the change is recorded as applied, and the
	// running configuration is exactly what it was. A master including
	// [a-y]*.conf, or naming its pool files individually, does not pick up
	// zz-fpm-tune-www.conf.
	IncludePatterns []string

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

	// A section for a pool that no longer exists is a landmine, and removing one
	// is cleanup rather than a resize — so it is not subject to the thresholds
	// that stop this tool churning.
	//
	// A pool defined ONLY by this file has no listen and no user, so php-fpm
	// refuses to start at all. Observed on a VM: a site was removed, php-fpm was
	// reloaded before the next round noticed, and the master died and stayed
	// dead through six systemd restart attempts. The operator removing a site
	// reloads php-fpm as part of doing so, which makes this the likely order
	// rather than an exotic one.
	stale := staleSections(master, plan)
	if len(changes) == 0 && len(stale) == 0 {
		return result, nil
	}
	if len(stale) > 0 {
		log.Warn("Removing overrides for pools that no longer exist", "pools", stale)
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

	// The file holds every pool this tool overrides, not just the ones changing
	// now: it is replaced whole, so writing only the changes would drop the
	// others' overrides.
	pools := overrideSet(plan, changes, st)
	rendered := Render(pools)

	b, err := writeDropIn(master, pools, opts, log)
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
		if rerr := restore(b, log); rerr != nil {
			result.RollbackFailed = []string{b.path}

			return result, fmt.Errorf("%w: %w; AND the rejected configuration could not be "+
				"removed from %s — the next reload from any source will adopt it",
				ErrValidationFailed, err, b.path)
		}

		result.RolledBack = true
		commit(opts.BackupDir, master.DropInDir, b, log)

		return result, fmt.Errorf("%w: %w", ErrValidationFailed, err)
	}

	if master.PID <= 0 {
		// Provisioning: the file is in place for whenever PHP-FPM starts.
		log.Info("Wrote pool configuration; no running master to reload", "pools", len(pools))
		phpfpm.InvalidateConfigCache(master.Binary, master.ConfigPath)
		commit(opts.BackupDir, master.DropInDir, b, log)
		record(st, changes, now)

		return result, nil
	}

	// Recorded BEFORE the signal, because the record's job is to describe what
	// might have happened. Writing it afterwards leaves a window in which the
	// master has adopted the change and the record still says nobody was told.
	markSignalled(opts.BackupDir, master, b, rendered)

	// PIDFile and ConfigPath let a reload be confirmed when the master comes
	// back under a NEW pid, which is what a daemonized reload does — php-fpm's
	// own default. Watching only the original pid reported those as a dead
	// master and rolled good changes back.
	_, err = phpfpm.ReloadAndWait(ctx, phpfpm.ReloadTarget{
		PID:        master.PID,
		PIDFile:    master.PIDFile,
		ConfigPath: master.ConfigPath,
	}, opts.SettleTime, log)
	if err != nil {
		// A cancelled context is not a dead master, and it is not a healthy one
		// either: the signal was delivered, but the settle window never proved
		// the master stayed up. Rolling back would undo a change that may well
		// have been adopted; declaring success would delete the only record of
		// it. So the record STAYS, and the next start settles the question.
		if ctxErr := ctx.Err(); ctxErr != nil {
			log.Warn("Interrupted while watching the master settle; leaving the change "+
				"recorded so the next start can finish checking it",
				"pid", master.PID, "error", ctxErr)
			result.Reloaded = true
			phpfpm.InvalidateConfigCache(master.Binary, master.ConfigPath)
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

		if rerr := restore(b, log); rerr != nil {
			result.RollbackFailed = []string{b.path}

			return result, fmt.Errorf("%w: %w; AND the configuration could not be taken "+
				"back out of %s", reason, err, b.path)
		}
		result.RolledBack = true

		// Best effort: the master is already gone, so this may do nothing. It
		// costs one signal and is the difference between a host that recovers
		// when the master is restarted and one that comes back to the config
		// that broke it.
		if !neverSignalled {
			if rerr := phpfpm.ReloadMaster(master.PID, master.ConfigPath); rerr != nil {
				log.Debug("Could not reload after restoring", "error", rerr)
			}
		}

		commit(opts.BackupDir, master.DropInDir, b, log)

		return result, fmt.Errorf("%w: %w", reason, err)
	}

	result.Reloaded = true

	// The parsed configuration is cached, so without this the next scrape reads
	// the settings from before this change and reports a pool as configured for
	// what it used to be. Observed as a pool showing 4 workers hours after it had
	// been set to 12.
	phpfpm.InvalidateConfigCache(master.Binary, master.ConfigPath)

	commit(opts.BackupDir, master.DropInDir, b, log)
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

	// Apply is exported, so the plan need not have come from allocate.Compute.
	// A pool costed at zero makes growth free and the arithmetic meaningless,
	// and an implausible cost overflows it; either way the safe answer is to
	// leave the damping alone rather than compute a budget from nonsense.
	for _, pp := range plan.Pools {
		if pp.WorkerBytes <= 0 || pp.WorkerBytes > maxPlausibleWorkerBytes ||
			pp.MaxChildren > maxPlausibleChildren || pp.Current > maxPlausibleChildren {
			return changes
		}
	}

	committed := commitmentOf(plan, changes)
	if committed <= limit {
		return changes
	}

	// On a host that is already full, redistribution is churn. Measured on a
	// five-site VM under sustained load: shop went 7 → 6 → 7 → 6, because api
	// wanted more, the budget was fully committed, and the coupling cut shop
	// below its own threshold to pay for it — whereupon shop's own demand
	// justified growing it straight back. Every cycle costs a reload of both
	// pools and changes nothing, because the shortfall is real and moving it
	// around does not make it smaller.
	//
	// Reductions a pool deserves ON ITS OWN MERITS still apply: those cleared
	// the threshold in decide and never reach here. What is refused is buying a
	// neighbour's growth with a cut nobody could justify otherwise, on a host
	// where the operator has already been told to add memory or move sites.
	if plan.CapacityExhausted {
		return holdGrowths(plan, changes, result, limit)
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

// maxPlausibleWorkerBytes and maxPlausibleChildren bound the inputs to the
// budget arithmetic. A terabyte per worker or a million children is not a
// configuration; multiplying them wraps int64 and turns an overcommit check into
// a permission slip.
const (
	maxPlausibleWorkerBytes = int64(1) << 40
	maxPlausibleChildren    = 1_000_000
)

// holdGrowths defers the growths a full host cannot pay for.
//
// The safe direction. Deferring a growth costs queueing on one pool; taking the
// memory anyway overcommits the host, and taking it by cutting a neighbour
// below its own threshold buys a reload every interval for no lasting change.
func holdGrowths(
	plan allocate.Plan,
	changes []allocate.PoolPlan,
	result *Result,
	limit int64,
) []allocate.PoolPlan {
	growing := make([]allocate.PoolPlan, 0, len(changes))
	kept := make([]allocate.PoolPlan, 0, len(changes))

	for _, pp := range changes {
		if pp.Current > 0 && pp.MaxChildren > pp.Current {
			growing = append(growing, pp)

			continue
		}
		kept = append(kept, pp)
	}

	// Largest claim first: dropping one big one beats dropping several small
	// ones, and the small ones are cheapest to satisfy on the next round.
	sort.Slice(growing, func(i, j int) bool {
		return int64(growing[i].MaxChildren-growing[i].Current)*growing[i].WorkerBytes >
			int64(growing[j].MaxChildren-growing[j].Current)*growing[j].WorkerBytes
	})

	for _, g := range growing {
		if commitmentOf(plan, append(kept, g)) <= limit {
			kept = append(kept, g)

			continue
		}

		setOutcome(result, g.Name, ActionHeldBack, fmt.Sprintf(
			"%d to %d, held back: the host is already fully committed, and taking the "+
				"memory from a neighbour would cost a reload of both without making the "+
				"shortfall any smaller", g.Current, g.MaxChildren))
	}

	return kept
}

// staleSections lists pools this file still overrides that are no longer there.
func staleSections(master Master, plan allocate.Plan) []string {
	body, err := os.ReadFile(DropInPath(master.DropInDir))
	if err != nil {
		return nil
	}

	live := make(map[string]bool, len(plan.Pools))
	for _, pp := range plan.Pools {
		live[pp.Name] = true
	}

	var stale []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
		if name != "" && !live[name] {
			stale = append(stale, name)
		}
	}

	return stale
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
	if len(master.IncludePatterns) == 0 {
		// The master config could not be read. Nothing to check against, and
		// refusing here would break provisioning against a host where php-fpm is
		// not installed yet.
		return nil
	}

	path := DropInPath(master.DropInDir)

	for _, pattern := range master.IncludePatterns {
		// The WHOLE path, not the basename. Comparing basenames meant a drop-in
		// directory the master does not include at all still passed as long as
		// the filename matched the glob — so an operator who pointed
		// --drop-in-dir at the wrong place got a successful apply, a successful
		// reload, a recorded change, and no effect whatsoever.
		if ok, err := filepath.Match(pattern, path); err == nil && ok {
			return nil
		}
	}

	return fmt.Errorf("%w: %s would be written to %s, which none of %v includes — "+
		"the change would validate and reload and have no effect",
		ErrNotIncluded, filepath.Base(path), path, master.IncludePatterns)
}

// backup is the drop-in file's previous state.
type backup struct {
	path string
	// content is what was there before; existed distinguishes an empty file from
	// no file, because undoing them differs — one is rewritten, the other is
	// deleted.
	content []byte
	existed bool
	saved   string
}

// writeDropIn records the change and then makes it, in that order.
//
// Everything is prepared and recorded BEFORE the single live write, so a run
// that dies has left a record of what it intended — the only thing that makes
// the leftovers recoverable, since a file that did not exist before has no
// backup to find.
//
// The write itself is one temp-and-rename, so there is no partial state to
// reason about afterwards: the file holds the old bytes or the new ones.
func writeDropIn(master Master, pools []allocate.PoolPlan, opts Options, log *slog.Logger) (backup, error) {
	if err := os.MkdirAll(opts.BackupDir, 0o755); err != nil {
		return backup{}, fmt.Errorf("cannot create backup directory: %w", err)
	}

	for _, pp := range pools {
		if err := safePoolName(pp.Name); err != nil {
			return backup{}, err
		}
	}

	path := DropInPath(master.DropInDir)
	rendered := Render(pools)

	b := backup{path: path}
	if content, err := os.ReadFile(path); err == nil {
		b.content, b.existed = content, true
		b.saved = filepath.Join(opts.BackupDir, backupName(master.DropInDir, path))

		if err := writeAtomic(b.saved, content); err != nil {
			return backup{}, fmt.Errorf("cannot back up %s: %w", path, err)
		}
	}

	txn := transaction{
		DropInDir:  master.DropInDir,
		Binary:     master.Binary,
		ConfigPath: master.ConfigPath,
		Path:       path,
		Existed:    b.existed,
		Wrote:      hashBytes(rendered),
		Phase:      PhaseWritten,
	}
	if b.saved != "" {
		txn.Saved = filepath.Base(b.saved)
	}

	if err := writeTransaction(opts.BackupDir, txn); err != nil {
		// Nothing has been written yet, so there is nothing to take back — but
		// the saved copy would otherwise be orphaned with no record naming it.
		if rerr := restore(b, log); rerr != nil {
			return backup{}, fmt.Errorf("%w (and the previous configuration could not be "+
				"restored: %w)", err, rerr)
		}

		return backup{}, err
	}

	if err := writeAtomic(path, rendered); err != nil {
		// The record is cleared only if the file really is back the way it was.
		// Clearing it regardless meant a failed write whose rollback ALSO failed
		// left new configuration on disk with nothing recording that it was
		// ours — so the next start would sweep the only backup and never look at
		// the file again.
		if rerr := restore(b, log); rerr != nil {
			return backup{}, fmt.Errorf("cannot write %s: %w (and it could not be taken "+
				"back out: %w)", path, err, rerr)
		}
		clearTransaction(opts.BackupDir, master.DropInDir)

		return backup{}, fmt.Errorf("cannot write %s: %w", path, err)
	}

	return b, nil
}

// markSignalled records that the master has been sent SIGUSR2.
//
// Written before the signal, because the record's job is to describe what MIGHT
// have happened. Recording it afterwards would leave a window in which the
// master had adopted the change and the record still said nobody had been told.
func markSignalled(backupDir string, master Master, b backup, rendered []byte) {
	txn := transaction{
		DropInDir:  master.DropInDir,
		Binary:     master.Binary,
		ConfigPath: master.ConfigPath,
		Path:       b.path,
		Existed:    b.existed,
		Wrote:      hashBytes(rendered),
		Phase:      PhaseSignalled,
	}
	if b.saved != "" {
		txn.Saved = filepath.Base(b.saved)
	}

	_ = writeTransaction(backupDir, txn)
}

// restore puts the previous file back, and reports whether it could.
//
// The return value is the point. This used to log and move on while the caller
// set RolledBack unconditionally, so `fpm-tune apply` printed "the previous
// configuration has been restored" with the configuration php-fpm had just
// rejected still sitting in the pool directory, armed for whatever reloads next.
func restore(b backup, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if b.path == "" {
		return nil
	}

	var err error
	if b.existed {
		err = writeAtomic(b.path, b.content)
	} else {
		// There was no file before, so undoing means removing ours rather than
		// leaving an empty one behind.
		err = os.Remove(b.path)
		if os.IsNotExist(err) {
			err = nil
		}
	}
	if err != nil {
		log.Error("Could not restore previous configuration", "path", b.path, "error", err)

		return err
	}

	return nil
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

// commit closes a transaction: the record goes first, then the copy it took.
//
// The order is the point. Removing the backup first left a window in which a
// record still named a saved copy that was already gone, so a run that died
// there handed the next recovery a manifest it could not honour. With the record
// cleared first, a death in the window leaves an orphaned .bak that nothing
// reads, which the next run sweeps.
func commit(backupDir, dropInDir string, b backup, log *slog.Logger) {
	clearTransaction(backupDir, dropInDir)

	if b.saved == "" {
		return
	}
	if err := os.Remove(b.saved); err != nil && !os.IsNotExist(err) && log != nil {
		log.Debug("Could not remove backup", "path", b.saved, "error", err)
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

// DropInPath is the single file this tool writes.
//
// One file for every pool it overrides, not one per pool. The per-pool layout
// meant a run could die between writes and leave half a plan on disk — and half
// a plan validates perfectly: the growth without the reduction that funds it
// passes `php-fpm -t` and commits the host past its budget. Recovery then had to
// work out which of N files had landed, from hashes, with no way to see what the
// master had actually adopted.
//
// One atomic rename removes that whole class. The file holds either the old
// bytes or the new ones. It also makes the change set indivisible, which is what
// the allocator assumes: it divides ONE budget, so its reductions and its
// growths are two halves of one decision.
//
// The zz- prefix makes it sort last in the include glob, so it wins over
// anything the distribution or the operator has already placed there.
func DropInPath(dir string) string {
	return filepath.Join(dir, "zz-fpm-tune.conf")
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

// Render produces the whole override file.
//
// Only pm.* keys are written, and the pools' own configuration is not touched.
// PHP-FPM merges a section defined across several included files, so repeating
// each section header with just these keys overrides them and leaves listen,
// user and everything else exactly as the operator wrote it.
func Render(pools []allocate.PoolPlan) []byte {
	var b strings.Builder

	b.WriteString("; Written by fpm-tune. Do not edit.\n")
	b.WriteString(";\n")
	b.WriteString("; This file overrides only the pm.* settings below; each pool's own\n")
	b.WriteString("; configuration is left alone. Delete it to return to the values\n")
	b.WriteString("; configured elsewhere.\n")

	sorted := append([]allocate.PoolPlan(nil), pools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	for _, pp := range sorted {
		b.WriteString("\n")
		if pp.Reason != "" {
			fmt.Fprintf(&b, "; %s\n", pp.Reason)
		}
		fmt.Fprintf(&b, "[%s]\n", pp.Name)
		fmt.Fprintf(&b, "pm.max_children = %d\n", pp.MaxChildren)

		// The spare settings are only meaningful for dynamic pools, and writing
		// them for a static one is a configuration error PHP-FPM will refuse.
		if pp.StartServers > 0 {
			fmt.Fprintf(&b, "pm.start_servers = %d\n", pp.StartServers)
			fmt.Fprintf(&b, "pm.min_spare_servers = %d\n", pp.MinSpare)
			fmt.Fprintf(&b, "pm.max_spare_servers = %d\n", pp.MaxSpare)
		}
	}

	return []byte(b.String())
}

// overrideSet is what the file should contain after this change.
//
// Every pool this tool already overrides, plus the ones changing now. Writing
// only the changed pools would delete the others' overrides, because the file is
// replaced whole; writing every pool in the plan would capture pools nobody
// asked to manage, so that a later hand edit to their own config would be
// silently overridden.
func overrideSet(plan allocate.Plan, changes []allocate.PoolPlan, st *state.State) []allocate.PoolPlan {
	changing := make(map[string]allocate.PoolPlan, len(changes))
	for _, pp := range changes {
		changing[pp.Name] = pp
	}

	out := make([]allocate.PoolPlan, 0, len(plan.Pools))
	for _, pp := range plan.Pools {
		if changed, ok := changing[pp.Name]; ok {
			out = append(out, changed)

			continue
		}
		if pp.Unknown {
			continue
		}
		// Previously written by this tool and not changing now: it keeps the
		// value it already has, rather than being dropped out of the file.
		if st != nil {
			if ps := st.Pools[pp.Name]; ps != nil && ps.LastAppliedMaxChildren > 0 {
				held := pp
				held.MaxChildren = ps.LastAppliedMaxChildren
				held.Reason = "unchanged"
				out = append(out, held)
			}
		}
	}

	return out
}
