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

	lastSaved  time.Time
	release    lock.Release
	reconciled bool
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

	targets, err := observe.Discover(l.log)
	if err != nil {
		l.log.Warn("Discovery failed; will retry", "error", err)

		return
	}
	if len(targets) == 0 {
		l.log.Warn("No PHP-FPM pools found")

		return
	}

	scrapeCtx, cancelScrape := context.WithTimeout(roundCtx, l.cfg.ScrapeTimeout)
	views := observe.Sample(scrapeCtx, targets, l.log)
	cancelScrape()

	now := time.Now()
	plan.LearnFrom(l.state, views, now, l.cfg.StateOptions)

	// Pools removed from the host are forgotten, so a machine that has had sites
	// come and go for years does not carry every one of them forever.
	names := make([]string, 0, len(views))
	for _, v := range views {
		names = append(names, v.Name)
	}
	if dropped := l.state.Forget(names); len(dropped) > 0 {
		l.log.Info("Forgot pools that are no longer configured", "pools", dropped)
	}

	limits := budget.Detect()
	if l.cfg.MemoryOverride > 0 {
		limits = limits.WithOverride(l.cfg.MemoryOverride)
	}

	result, err := plan.Build(plan.Input{
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
	plan.RecordCounters(l.state, views)

	l.metrics.Update(result, l.state, l.cfg.StateOptions, float64(now.Unix()))

	if result.Plan.CapacityExhausted {
		l.log.Warn("Capacity exhausted: a pool is short of workers and the budget is fully committed",
			"free_bytes", result.Plan.FreeBytes)
	}

	if l.cfg.Apply && l.reconciled {
		l.applyPlan(roundCtx, result, now)
	}

	l.save(now, false)
}

func (l *Loop) applyPlan(ctx context.Context, result plan.Result, now time.Time) {
	master, err := MasterOnHost(l.cfg.DropInDir, l.log)
	if err != nil {
		l.log.Warn("Cannot apply", "error", err)

		return
	}

	opts := l.cfg.ApplyOptions
	opts.BackupDir = l.cfg.BackupDir

	applied, err := apply.Apply(ctx, result.Plan, master, l.state, opts, l.log)
	if err != nil {
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
	master, err := MasterOnHost(l.cfg.DropInDir, l.log)
	if err != nil {
		if !errors.Is(err, ErrNoMaster) {
			l.log.Warn("Cannot identify the master to reconcile against", "error", err)
		}

		return
	}

	opts := l.cfg.ApplyOptions
	opts.BackupDir = l.cfg.BackupDir

	if err := apply.Reconcile(ctx, master, opts, l.log); err != nil {
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
	if !force && now.Sub(l.lastSaved) < l.cfg.SaveEvery {
		return
	}

	if err := l.state.Save(l.cfg.StatePath); err != nil {
		l.log.Warn("Could not save state", "path", l.cfg.StatePath, "error", err)

		return
	}
	l.lastSaved = now
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

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			l.log.Error("Metrics server stopped", "addr", l.cfg.MetricsAddr, "error", err)
		}
	}()

	return srv, nil
}

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
	masters, err := phpfpm.DiscoverMasters(log)
	if err != nil {
		return apply.Master{}, fmt.Errorf("cannot scan for PHP-FPM masters: %w", err)
	}

	if len(masters) == 0 {
		return apply.Master{}, ErrNoMaster
	}

	// An explicit drop-in directory picks the master out. Without this the
	// advice in the error below was not actually followable: setting the
	// directory did nothing to disambiguate, so an operator told to run once per
	// master had no way to say which one they meant.
	if dropInDir != "" && len(masters) > 1 {
		want := filepath.Clean(dropInDir)

		var matched []phpfpm.Master
		for _, m := range masters {
			if filepath.Clean(IncludeDirOf(m.ConfigPath)) == want {
				matched = append(matched, m)
			}
		}
		if len(matched) > 0 {
			masters = matched
		}
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
		Binary:         m.Binary,
		ConfigPath:     m.ConfigPath,
		PID:            m.PID,
		PIDFile:        PIDFileOf(m.ConfigPath),
		DropInDir:      dropInDir,
		IncludePattern: IncludePatternOf(m.ConfigPath),
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
