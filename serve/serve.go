// Package serve runs the closed loop: observe, learn, allocate, apply.
//
// The loop samples far more often than it acts. Sampling is what builds a
// baseline worth trusting, and it costs a FastCGI call per pool; acting
// interrupts every worker in a pool and is gated by hysteresis in the apply
// package. Conflating the two rates would mean either learning slowly or
// reloading constantly.
package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/cboxdk/fpm-tune/apply"
	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/lock"
	"github.com/cboxdk/fpm-tune/metrics"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/plan"
	"github.com/cboxdk/fpm-tune/state"
	"github.com/cboxdk/phpfpm"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Config configures the loop.
type Config struct {
	// Interval is how often the pools are sampled.
	Interval time.Duration

	// NoLearn stops the loop recording what it observes.
	//
	// The flag was registered for `serve` and read by nothing: a daemon started
	// with -no-learn wrote a state file with a sample count per pool, while its
	// help said "do not record this scrape". Someone using it to watch a host
	// without disturbing a baseline was disturbing it.
	NoLearn bool

	// Discover and Sample replace the loop's two views of the outside world.
	//
	// Nil in production, where they are php-fpm itself. A test sets them to hold
	// the world still, which is the only way to assert the ORDER this loop does
	// things in — and that order is a correctness property, not a style: the
	// plan has to read the ceiling counter before the counter is overwritten
	// for the next round.
	Discover func(context.Context) ([]phpfpm.Target, error)
	Sample   func(context.Context, []phpfpm.Target) []observe.PoolView

	// StatePath is where baselines are persisted.
	StatePath string

	// SaveEvery bounds how often state reaches disk. Learning happens every
	// interval, but writing a file every fifteen seconds for the life of a host
	// is a lot of writes for data that is only read at startup.
	SaveEvery time.Duration

	// Apply enables acting on the plan. With it false the loop observes,
	// learns and publishes metrics without touching any configuration — which
	// is a legitimate way to run this permanently.
	Apply bool

	// MetricsAddr is where /metrics is served. Empty disables it.
	MetricsAddr string

	// DropInDir and BackupDir are passed through to apply.
	DropInDir string
	BackupDir string

	// MemoryOverride replaces the detected memory ceiling when non-zero.
	MemoryOverride int64
	ReserveBytes   int64

	// ScrapeTimeout bounds one round of scraping.
	ScrapeTimeout time.Duration

	ApplyOptions apply.Options
	StateOptions state.Options
}

// Defaults fills in any unset field.
func (c Config) Defaults() Config {
	if c.Interval <= 0 {
		c.Interval = 30 * time.Second
	}
	if c.StatePath == "" {
		c.StatePath = state.DefaultPath
	}
	if c.SaveEvery <= 0 {
		c.SaveEvery = 5 * time.Minute
	}
	if c.ScrapeTimeout <= 0 {
		c.ScrapeTimeout = 15 * time.Second
	}
	if c.BackupDir == "" {
		c.BackupDir = apply.DefaultBackupDir
	}

	return c
}

// Loop is the running daemon.
type Loop struct {
	cfg     Config
	log     *slog.Logger
	metrics *metrics.Collectors
	state   *state.State

	lastSaved   time.Time
	release     lock.Release
	resource    lock.Release
	resourceDir string
	reconciled  bool
	exhausted   bool
	boundAddr   string
}

// New prepares the loop, loading any existing baselines.
func New(cfg Config, log *slog.Logger) (*Loop, error) {
	cfg = cfg.Defaults()
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	// Taken before the state is read. Two processes both load the state, both
	// learn into their own copy and both write it whole, so the one that saves
	// second silently discards everything the other observed — and they write the
	// same pool fragments, each backing up the other's half-applied state.
	release, err := lock.Acquire(lock.DefaultPath(cfg.StatePath))
	if err != nil {
		return nil, err
	}

	st, err := state.Load(cfg.StatePath)
	if err != nil {
		release()

		return nil, err
	}

	return &Loop{
		cfg:     cfg,
		log:     log,
		metrics: metrics.New(),
		state:   st,
		release: release,
	}, nil
}

// Close releases the lock. Safe to call more than once.
func (l *Loop) Close() {
	if l.resource != nil {
		l.resource()
		l.resource, l.resourceDir = nil, ""
	}
	if l.release != nil {
		l.release()
		l.release = nil
	}
}

// Run drives the loop until the context is cancelled.
//
// State is saved on the way out whatever the reason. A daemon that discards an
// hour of observation because it was asked to stop would make every restart a
// return to bootstrap, which is the behaviour persisting it exists to avoid.
func (l *Loop) Run(ctx context.Context) error {
	defer l.Close()

	var srv *http.Server
	if l.cfg.MetricsAddr != "" {
		var err error
		if srv, err = l.startMetrics(); err != nil {
			// Refusing to start rather than logging and carrying on. A tuner that
			// reconfigures a host every interval with no way to observe what it
			// decided is worse than one that did not start: the usual cause is a
			// second copy already bound to the port, which is also the situation
			// where two of them writing the same pool files does real damage.
			return err
		}
	}

	l.metrics.SetApplyEnabled(l.cfg.Apply)

	l.log.Info("fpm-tune running",
		"interval", l.cfg.Interval,
		"apply", l.cfg.Apply,
		"state", l.cfg.StatePath,
		"metrics", l.cfg.MetricsAddr,
	)

	// One round immediately, so a freshly started daemon publishes something
	// rather than looking dead until the first tick.
	l.round(ctx)

	ticker := time.NewTicker(l.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			l.shutdown(srv)

			return nil
		case <-ticker.C:
			l.round(ctx)
		}
	}
}

// round is one full pass. Failures are logged and the loop continues: a pool
// that is restarting, or a host whose php-fpm is briefly down, must not end the
// daemon.
func (l *Loop) round(ctx context.Context) {
	roundCtx, cancel := context.WithTimeout(ctx, l.cfg.ScrapeTimeout+30*time.Second)
	defer cancel()

	// Before discovery, not after. A previous run may have left configuration
	// php-fpm will not accept, and discovery parses the effective config to find
	// pools — so from that point on nothing is discoverable and the recovery
	// path cannot reach the master it exists to recover.
	if l.cfg.Apply && !l.reconciled {
		l.reconcile(roundCtx)
	}

	// The parsed configuration is cached for the life of the process, which is
	// right for a scrape loop and wrong for a daemon that runs for weeks: an
	// operator raising a pool's pm.max_children from 40 to 80 would be invisible,
	// and the round would keep accounting for 40 while growing a neighbour into
	// the difference. Invalidated per round so every round sees what is on disk.
	//
	// One `php-fpm -tt` per master per interval. On a host with forty pools that
	// is a few hundred milliseconds against a thirty-second round, and the
	// alternative is being wrong about the one number everything is divided by.
	l.forgetParsedConfig()

	targets, err := l.discover(roundCtx)
	if err != nil {
		l.log.Warn("Discovery failed; will retry", "error", err)

		return
	}
	if len(targets) == 0 {
		// Retried every round rather than once at startup. A master killed by
		// this tool's own file cannot be discovered, so the repair has to keep
		// looking — and it is cheap: one fork when there is nothing else to do.
		l.reconciled = false
		l.log.Warn("No PHP-FPM pools found")

		return
	}

	scrapeCtx, cancelScrape := context.WithTimeout(roundCtx, l.cfg.ScrapeTimeout)
	views := l.sample(scrapeCtx, targets)
	cancelScrape()

	now := time.Now()
	if !l.cfg.NoLearn {
		plan.LearnFrom(l.state, views, now, l.cfg.StateOptions)
	}

	// Pools removed from the host are forgotten, so a machine that has had sites
	// come and go for years does not carry every one of them forever.
	names := make([]string, 0, len(views))
	for _, v := range views {
		names = append(names, v.Name)
	}
	if dropped := l.state.Forget(names); len(dropped) > 0 {
		l.log.Info("Forgot pools that are no longer configured", "pools", dropped)
	}

	// The MASTER's limit, not this process's. On a VM the cap lives on php-fpm's
	// own systemd slice, and reading the root cgroup finds nothing at all.
	limits := budget.DetectFor(MasterPIDOf(views))
	if l.cfg.MemoryOverride > 0 {
		limits = limits.WithOverride(l.cfg.MemoryOverride)
	}

	result, err := plan.Build(plan.Input{
		At:           now,
		Limits:       limits,
		Views:        views,
		State:        l.state,
		ReserveBytes: l.cfg.ReserveBytes,
		StateOptions: l.cfg.StateOptions,
	})
	if err != nil {
		// A host that cannot fit its pools is exactly where the metrics matter,
		// so this is a warning rather than a reason to stop.
		l.log.Warn("Could not build a plan", "error", err)
		l.save(now, true)

		return
	}

	// After the plan: the counters are what the NEXT round compares against, and
	// storing them earlier made the comparison one against itself.
	if !l.cfg.NoLearn {
		plan.RecordCounters(l.state, views)
	}

	l.metrics.Update(result, l.state, l.cfg.StateOptions, float64(now.Unix()))

	// Logged on the TRANSITION, not every round. A full host stays full, and
	// repeating the same warning every fifteen seconds is how an operator learns
	// to stop reading the log — 26 identical lines in six minutes, measured. The
	// condition is on the metrics endpoint continuously, which is where a
	// persistent state belongs; the log carries the change.
	if result.Plan.CapacityExhausted != l.exhausted {
		l.exhausted = result.Plan.CapacityExhausted
		if l.exhausted {
			l.log.Warn("Capacity exhausted: a pool is short of workers and the budget is "+
				"fully committed. No configuration change will help — this host needs more "+
				"memory, or fewer sites.", "free_bytes", result.Plan.FreeBytes)
		} else {
			// Warn to match its counterpart, so the log shows the condition
			// ending as well as beginning. Logged at Info it was invisible at the
			// default level, and the pair became a warning that never cleared —
			// which reads at 3am as "still exhausted" for a problem solved hours
			// ago, and is worse than logging neither.
			l.log.Warn("No longer at capacity: there is budget to give a pool that needs it")
		}
	}

	if l.cfg.Apply && l.reconciled {
		l.applyPlan(roundCtx, result, now)
	}

	l.save(now, false)
}

func (l *Loop) applyPlan(ctx context.Context, result plan.Result, now time.Time) {
	master, err := MasterOnHost(l.cfg.DropInDir, l.log)
	if err != nil {
		l.metrics.SetApplyBlocked("no_master")
		l.log.Warn("Cannot apply", "error", err)

		return
	}

	// The lock is taken on the directory THIS call is about to write, not on
	// whatever was current when the process last reconciled.
	//
	// Re-keying lived only inside reconcile, which runs once per process — so
	// after a PHP upgrade moved the pool directory the daemon held the lock on
	// the old one and wrote to the new. A concurrent `fpm-tune apply` found the
	// new key free and both wrote the same file; and the daemon never
	// reconciled the new tree, so a leftover record there was written over
	// unread. holdResource clears the reconciled flag when the directory
	// changes, which sends the next round through recovery first.
	if !l.holdResource(master.DropInDir) {
		return
	}
	if !l.reconciled {
		l.log.Warn("The pool directory changed under this process; reconciling before "+
			"writing to it", "dir", master.DropInDir)

		return
	}

	l.metrics.SetApplyBlocked("")
	l.state.RememberMaster(master.Binary, master.ConfigPath, master.DropInDir)

	opts := l.cfg.ApplyOptions
	opts.BackupDir = l.cfg.BackupDir

	applied, err := apply.Apply(ctx, result.Plan, master, l.state, opts, l.log)
	// Wrote, not Changed(): a run that only removes the section of a site that no
	// longer exists writes and reloads while resizing nothing, and it reached the
	// host just as much as a resize did.
	if applied.Inconclusive {
		// The settle window did not finish, so the recovery record is still
		// open. Reconciling once at startup would leave it there for the life of
		// the process while this loop kept writing on top of it.
		l.log.Warn("The reload could not be confirmed; the next round will reconcile " +
			"before writing again")
		l.reconciled = false
	}

	l.metrics.RecordApply(float64(now.Unix()), applied.Wrote, applied.RolledBack,
		len(applied.RollbackFailed), err)

	if err != nil {
		if len(applied.RollbackFailed) > 0 {
			// The worst state this tool can reach, and it has to say WHICH of the
			// two it is. "php-fpm has not been reloaded, so nothing is broken
			// yet" is true when the change was rejected before the signal, and a
			// lie when the master was signalled and did not come back — where it
			// is the difference between restarting php-fpm now and finding out
			// in the morning. The CLI had the same sentence and the same fault.
			if errors.Is(err, apply.ErrMasterDidNotSurvive) {
				l.log.Error("PHP-FPM IS DOWN AND COULD NOT BE PUT BACK. The master did "+
					"not survive the reload, and the configuration that killed it could "+
					"not be removed. Remove these by hand, then reset-failed and start "+
					"php-fpm.",
					"paths", applied.RollbackFailed, "error", err)

				return
			}

			l.log.Error("A REJECTED CONFIGURATION IS STILL IN PLACE and could not be "+
				"removed. php-fpm has not been reloaded, so nothing is broken yet — but "+
				"the next reload from any source will adopt it, and a master that fails "+
				"to start does not come back. Remove these by hand.",
				"paths", applied.RollbackFailed, "error", err)

			return
		}

		l.log.Error("Apply failed", "error", err, "rolled_back", applied.RolledBack)

		return
	}

	if changed := applied.Changed(); len(changed) > 0 {
		for _, o := range changed {
			l.log.Info("Pool resized", "pool", o.Pool, "from", o.From, "to", o.To, "reason", o.Reason)
		}
		// The applied values are the hysteresis baseline for the next round, so
		// they go to disk now rather than waiting for the save interval.
		l.save(now, true)
	}
}

// reconcile repairs what an unfinished run left behind, once per process.
//
// Failure is not fatal to the loop: the daemon keeps observing and publishing
// metrics, which is exactly what an operator needs while the host is in a state
// nobody can write to. It simply does not apply until the repair succeeds.
func (l *Loop) reconcile(ctx context.Context) {
	// From memory when nothing is running, because that is exactly when this
	// matters: a master this tool's own file will not let start has no process
	// to discover.
	opts := l.cfg.ApplyOptions
	opts.BackupDir = l.cfg.BackupDir

	master, err := MasterFromMemory(l.cfg.DropInDir, l.state.Master, l.log)
	if err != nil {
		// Nothing running and nothing remembered. The transaction record carries
		// the binary and config for exactly this case, and Reconcile fills the
		// master in from it — but only if it is called, and requiring an
		// identified master to get there made that promise empty whenever the
		// state file was missing, corrupt, or from an older version.
		if !errors.Is(err, ErrNoMaster) {
			l.log.Warn("Cannot identify the master to reconcile against", "error", err)

			return
		}
		// The sidecar beside the backups, when no directory was named.
		//
		// Returning here made the promise above empty in exactly the case it is
		// for: a daemon started with no --drop-in-dir on a host whose master
		// will not start has nothing to discover, nothing remembered in a state
		// file that may be missing, and so no directory to reconcile — which is
		// the one thing standing between the host and being repaired.
		master = apply.Master{DropInDir: l.cfg.DropInDir}
		if master.DropInDir == "" {
			master = apply.RememberedMaster(l.cfg.BackupDir, l.cfg.DropInDir)
		}
		if master.DropInDir == "" {
			return
		}
	}

	// Re-keyed when the directory changes, and released when the work fails.
	//
	// It used to be taken once and held for the life of the process. Two things
	// went wrong with that. A daemon that could not repair a leftover change kept
	// the lock while doing nothing with it, so the operator's manual `fpm-tune
	// apply` — the obvious way to fix it — was refused, and the only way out was
	// stopping the daemon, which the error text does not suggest. And a PHP
	// upgrade in place moves the pool directory: the lock stayed keyed on the old
	// one while writes went to the new, so a concurrent apply found the new key
	// free and both wrote the same file.
	if !l.holdResource(master.DropInDir) {
		return
	}

	acted, err := apply.Reconcile(ctx, master, opts, l.log)
	if acted {
		// Counted when something was actually undone, completed or removed —
		// not when the attempt failed, which is what this used to count and is
		// the opposite of what the name says.
		l.metrics.RecordRepair()
	}
	if err != nil {
		// Released, so the operator's own `fpm-tune apply` is not refused by a
		// daemon that has given up on the very thing they are trying to fix.
		if l.resource != nil {
			l.resource()
			l.resource, l.resourceDir = nil, ""
		}
		l.metrics.SetApplyBlocked("unrepaired")
		l.log.Error("A previous run left configuration this could not repair; not applying",
			"error", err)

		return
	}

	// A repair the reconcile just made would otherwise be invisible to the
	// scrape that follows, because the parsed configuration is cached.
	phpfpm.InvalidateConfigCache(master.Binary, master.ConfigPath)
	l.reconciled = true
}

// save writes state, either because enough time has passed or because something
// happened that the next start must not lose.
func (l *Loop) save(now time.Time, force bool) {
	if l.cfg.NoLearn {
		// Nothing was recorded, so writing would replace whatever a previous run
		// learned with a file that has learned nothing.
		return
	}
	if !force && now.Sub(l.lastSaved) < l.cfg.SaveEvery {
		return
	}

	if err := l.state.Save(l.cfg.StatePath); err != nil {
		l.log.Warn("Could not save state", "path", l.cfg.StatePath, "error", err)

		return
	}
	l.lastSaved = now
}

// discover and sample are the loop's only two views of the outside world, and
// they are indirected so a test can hold them still.
//
// Not indulgence: the ORDER in which this loop learns, plans and records
// counters is a correctness property — the ceiling counter has to be read by the
// plan before it is overwritten for the next round, and getting that wrong once
// meant the growth signal never fired in any round since it was written. Nothing
// could assert it, because the only way in was through a real php-fpm.
func (l *Loop) discover(ctx context.Context) ([]phpfpm.Target, error) {
	find := observe.Discover
	if l.cfg.Discover != nil {
		find = func(context.Context, *slog.Logger) ([]phpfpm.Target, error) {
			return l.cfg.Discover(ctx)
		}
	}

	targets, err := find(ctx, l.log)
	if err != nil {
		return nil, err
	}

	// Scoped AFTER the source, whichever source it was.
	//
	// The filter first sat inside the production branch, so an injected one
	// skipped it — and a test then proved something the daemon does not do. That
	// is the same shape as the fault this filter exists for: a rule that lives
	// on one path and not the other. A seam has to supply the world, not decide
	// what is done with it.
	return ForMaster(targets, l.cfg.DropInDir, l.log), nil
}

func (l *Loop) sample(ctx context.Context, targets []phpfpm.Target) []observe.PoolView {
	if l.cfg.Sample != nil {
		return l.cfg.Sample(ctx, targets)
	}

	return observe.Sample(ctx, targets, l.log)
}

// holdResource takes the pool-directory lock, re-keying it when the directory
// has changed. It reports whether the lock is held.
//
// A directory change clears the reconciled flag, because a tree this process has
// never looked at may be carrying a record from a run that did not finish.
func (l *Loop) holdResource(dropInDir string) bool {
	if l.resourceDir == dropInDir && l.resource != nil {
		return true
	}

	if l.resource != nil {
		l.resource()
		l.resource = nil
	}
	l.reconciled = false

	release, err := lock.Acquire(lock.ResourcePath(dropInDir))
	if err != nil {
		l.metrics.SetApplyBlocked("lock")
		l.log.Error("Cannot take the pool-directory lock; not applying",
			"dir", dropInDir, "error", err)

		return false
	}
	l.resource, l.resourceDir = release, dropInDir

	return true
}

func (l *Loop) startMetrics() (*http.Server, error) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(l.metrics.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// Bound here rather than inside the goroutine so that a port already in use
	// is an error the caller sees, not a line in a log nobody reads.
	ln, err := net.Listen("tcp", l.cfg.MetricsAddr)
	if err != nil {
		return nil, fmt.Errorf("cannot serve metrics on %s: %w", l.cfg.MetricsAddr, err)
	}

	// The address actually bound, not the one asked for. They differ whenever
	// the request carries port 0, and then the configured string is the one
	// thing that cannot tell anyone where to point a scraper.
	l.boundAddr = ln.Addr().String()

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			l.log.Error("Metrics server stopped", "addr", l.boundAddr, "error", err)
		}
	}()

	return srv, nil
}

// BoundMetricsAddr is where the metrics endpoint is actually listening, once
// started. Empty before that, and when no address was configured.
// Written by startMetrics before Run enters its loop, and not touched
// afterwards, so no lock is needed.
func (l *Loop) BoundMetricsAddr() string { return l.boundAddr }

func (l *Loop) shutdown(srv *http.Server) {
	l.log.Info("Stopping")

	// Detached from the cancelled context that got us here: shutdown work must
	// not be cancelled by the cancellation that requested it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()

	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			l.log.Debug("Metrics server did not stop cleanly", "error", err)
		}
	}

	if err := l.state.Save(l.cfg.StatePath); err != nil {
		l.log.Error("Could not save state on shutdown; this restart returns to bootstrap",
			"path", l.cfg.StatePath, "error", err)
	}
}

// Metrics exposes the collectors, for tests and for embedding.
func (l *Loop) Metrics() *metrics.Collectors { return l.metrics }

// State exposes the store, for tests.
func (l *Loop) State() *state.State { return l.state }

// MasterPIDOf picks the master serving these pools, for budget detection.
// discoverMasters is the process-table scan, indirected so the rules built on
// top of it can be tested. Which master is picked on a host running several, and
// what happens when none is running, are decisions with consequences — and both
// were reachable only by having php-fpm installed in the right shape.
var discoverMasters = phpfpm.DiscoverMasters

func MasterPIDOf(views []observe.PoolView) int {
	pid := 0
	for _, v := range views {
		if v.Target.PID <= 0 {
			continue
		}
		if pid == 0 {
			pid = v.Target.PID

			continue
		}
		if v.Target.PID != pid {
			// Pools from more than one master. Picking the first is picking one
			// host's memory limit to size another host's pools against, which is
			// exactly what reading the ROOT cgroup used to do — the same fault,
			// arrived at from a different direction. Zero means "no opinion",
			// and the caller falls back to the machine, which is at least a
			// limit that binds all of them.
			return 0
		}
	}

	return pid
}

// forgetParsedConfig drops the cached effective configuration, so the next
// discovery reads what is actually on disk.
func (l *Loop) forgetParsedConfig() {
	if m := l.state.Master; m.Known() {
		phpfpm.InvalidateConfigCache(m.Binary, m.ConfigPath)

		return
	}

	// Nothing remembered yet — an early round, or a host this process has not
	// written to. Clearing the lot costs a re-parse it was about to do anyway.
	phpfpm.InvalidateConfigCache("", "")
}

// MasterOnHost identifies the PHP-FPM instance to reconfigure, WITHOUT reading
// its configuration.
//
// Used before anything else, because the configuration may be the problem. A run
// that died between writing a fragment and validating it leaves one php-fpm will
// not accept; discovery parses the effective config to find pools, so from that
// point on nothing can be discovered at all and the recovery path cannot reach
// the master it exists to recover. Observed as "no PHP-FPM pools found" — blamed
// on permissions — against a perfectly healthy master.
func MasterOnHost(dropInDir string, log *slog.Logger) (apply.Master, error) {
	return masterOnHost(dropInDir, nil, log)
}

// MasterFromMemory finds the installation even when nothing is running, using
// what a previous run recorded.
//
// The moment this matters is the moment discovery cannot help: if this tool's
// own file is what stops php-fpm from starting, there is no process to scan for,
// so the repair could never run and the master stayed down through every restart
// attempt — with fpm-tune alongside it, having caused it.
func MasterFromMemory(dropInDir string, remembered state.MasterRef, log *slog.Logger) (apply.Master, error) {
	return masterOnHost(dropInDir, &remembered, log)
}

func masterOnHost(dropInDir string, remembered *state.MasterRef, log *slog.Logger) (apply.Master, error) {
	masters, err := discoverMasters(log)
	if err != nil {
		return apply.Master{}, fmt.Errorf("cannot scan for PHP-FPM masters: %w", err)
	}

	// An explicit drop-in directory picks the master out. Done before the
	// "is anything running" question, because on a host with several masters —
	// a development machine, or one running two PHP versions — none of the
	// others being ours is the same as ours not running.
	if dropInDir != "" && len(masters) > 0 {
		want := filepath.Clean(dropInDir)

		var matched []phpfpm.Master
		for _, m := range masters {
			for _, pattern := range IncludePatternsOf(m.ConfigPath) {
				if filepath.Clean(filepath.Dir(pattern)) == want {
					matched = append(matched, m)

					break
				}
			}
		}
		masters = matched
	}

	if len(masters) == 0 {
		if remembered == nil || !remembered.Known() {
			return apply.Master{}, ErrNoMaster
		}

		// Nothing running, but a previous run recorded where it lives. PID stays
		// zero: there is nothing to signal, and the caller must not mistake that
		// for "provisioning, go ahead".
		m := apply.Master{
			Binary:          remembered.Binary,
			ConfigPath:      remembered.ConfigPath,
			DropInDir:       remembered.DropInDir,
			IncludePatterns: IncludePatternsOf(remembered.ConfigPath),
		}
		if dropInDir != "" {
			m.DropInDir = dropInDir
		}

		return m, nil
	}

	if len(masters) > 1 {
		paths := make([]string, 0, len(masters))
		for _, m := range masters {
			paths = append(paths, fmt.Sprintf("pid %d (%s)", m.PID, m.ConfigPath))
		}

		return apply.Master{}, fmt.Errorf(
			"this host runs %d PHP-FPM masters — %s. Apply handles one at a time; "+
				"pass --drop-in-dir to say which one",
			len(masters), strings.Join(paths, ", "))
	}

	m := masters[0]
	master := apply.Master{
		Binary:          m.Binary,
		ConfigPath:      m.ConfigPath,
		PID:             m.PID,
		PIDFile:         PIDFileOf(m.ConfigPath),
		DropInDir:       dropInDir,
		IncludePatterns: IncludePatternsOf(m.ConfigPath),
	}
	if master.DropInDir == "" {
		master.DropInDir = IncludeDirOf(m.ConfigPath)
	}
	if master.DropInDir == "" {
		return master, errors.New("could not locate the pool configuration directory; set it explicitly")
	}

	return master, nil
}

// ErrNoMaster reports that no php-fpm master is running.
var ErrNoMaster = errors.New("no running PHP-FPM master found")

// MasterFrom works out which PHP-FPM instance a plan belongs to.
//
// Every pool on a host normally belongs to one master and they share a single
// reload. Two masters would need two reloads and two validations — a real
// configuration, but not one handled here, so it says so rather than
// reconfiguring one and silently ignoring the other.
func MasterFrom(result plan.Result, dropInDir string) (apply.Master, error) {
	type target struct{ binary, config string }

	found := map[string]target{}
	for _, v := range result.Views {
		if v.Target.Binary == "" || v.Target.ConfigPath == "" {
			continue
		}
		found[v.Target.Binary+"::"+v.Target.ConfigPath] = target{v.Target.Binary, v.Target.ConfigPath}
	}

	switch len(found) {
	case 0:
		return apply.Master{}, errors.New("could not determine which php-fpm binary and config to validate against")
	case 1:
	default:
		return apply.Master{}, fmt.Errorf(
			"this host runs %d PHP-FPM masters; apply handles one at a time, "+
				"so run it once per master with the drop-in directory set accordingly", len(found))
	}

	var t target
	for _, v := range found {
		t = v
	}

	master := apply.Master{Binary: t.binary, ConfigPath: t.config, DropInDir: dropInDir}
	if master.DropInDir == "" {
		master.DropInDir = IncludeDirOf(t.config)
	}
	if master.DropInDir == "" {
		return master, errors.New("could not locate the pool configuration directory; set it explicitly")
	}

	master.PID = masterPID(result, t.config)
	master.PIDFile = PIDFileOf(t.config)

	return master, nil
}

// ErrMasterUnidentified reports that pools were found but the master serving
// them could not be identified.
//
// This is deliberately an error rather than a quiet fallback. Apply treats
// PID == 0 as "provisioning: write the files, there is nothing to reload" — which
// is correct before PHP-FPM starts and catastrophic afterwards, because it
// records the pools as applied and never retries. The official php:8.3-fpm image
// ships `pid` commented out, so on the most common deployment there is no pid
// file at all: files written, master untouched, state poisoned, silent forever.
var ErrMasterUnidentified = errors.New("PHP-FPM is running but its master process could not be identified")

// masterPID resolves the running master.
//
// Discovery scanned the process table and had the pid in hand, so that is the
// primary source; the pid file is a fallback for a caller that supplied targets
// without going through discovery.
func masterPID(result plan.Result, configPath string) int {
	for _, v := range result.Views {
		if v.Target.PID > 0 {
			return v.Target.PID
		}
	}

	if pidFile := PIDFileOf(configPath); pidFile != "" {
		if pid, err := phpfpm.MasterPID(pidFile); err == nil {
			return pid
		}
	}

	return 0
}
