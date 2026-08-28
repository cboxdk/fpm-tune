// Command fpm-tune sizes PHP-FPM pools against the memory a host actually has.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"text/tabwriter"

	"github.com/cboxdk/fpm-tune/apply"
	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/lock"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/plan"
	"github.com/cboxdk/fpm-tune/serve"
	"github.com/cboxdk/fpm-tune/state"
	"github.com/cboxdk/phpfpm"
)

// version is set at build time.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		// Asking for help is not a failure. flag.ContinueOnError returns
		// ErrHelp for -h, which travelled all the way up and made
		// `fpm-tune plan --help` exit 1 — enough to fail a shell script that
		// checks its tools respond, and a confusing first impression.
		if errors.Is(err, flag.ErrHelp) {
			return
		}

		fmt.Fprintf(os.Stderr, "fpm-tune: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()

		return fmt.Errorf("no command given")
	}

	switch args[0] {
	case "plan":
		return runPlan(args[1:])
	case "apply":
		return runApply(args[1:])
	case "serve":
		return runServe(args[1:])
	case "version", "--version", "-v":
		fmt.Println(version)

		return nil
	case "help", "--help", "-h":
		usage()

		return nil
	default:
		usage()

		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `fpm-tune %s — size PHP-FPM pools against available memory

  fpm-tune plan     show what would change, and why. Changes no PHP-FPM
                    configuration; records the observation (see -no-learn).
  fpm-tune apply    write the pool settings and reload PHP-FPM.
  fpm-tune serve    keep watching and adjusting, with metrics on /metrics.
  fpm-tune version

`, version)
}

// commonFlags are the flags plan and apply share. Registered once so the two
// commands cannot drift in how they read the host — a plan that describes a
// different budget from the one apply uses would be worse than no plan.
type commonFlags struct {
	statePath   *string
	memory      *string
	reserve     *string
	timeout     *time.Duration
	verbose     *bool
	noLearn     *bool
	confSamples *int
	confSpan    *time.Duration
}

func registerCommon(fs *flag.FlagSet) commonFlags {
	return commonFlags{
		statePath: fs.String("state", state.DefaultPath, "path to the learned baselines"),
		memory:    fs.String("memory", "", "override the detected memory limit (e.g. 8G)"),
		reserve:   fs.String("reserve", "", "memory to hold back from workers (e.g. 1G)"),
		timeout:   fs.Duration("timeout", 15*time.Second, "budget for scraping all pools"),
		verbose:   fs.Bool("verbose", false, "log what is being read"),
		noLearn: fs.Bool("no-learn", false,
			"do not record this scrape. Both commands learn by default, so running plan "+
				"on a schedule before ever running apply builds a real baseline"),

		// How much evidence a pool needs before its own measurements are used
		// instead of the bootstrap estimate. Both must be satisfied: samples
		// alone would let a tight loop claim confidence in seconds, which
		// measures the polling interval rather than the workload.
		confSamples: fs.Int("confidence-samples", 0, "busy samples before a pool is sized from its own memory (default 20)"),
		confSpan:    fs.Duration("confidence-span", 0, "time span before a pool is sized from its own memory (default 30m)"),
	}
}

// gather does everything both commands need: read the host, scrape the pools,
// fold the observation into the store, and build the allocation.
func gather(ctx context.Context, c commonFlags, log *slog.Logger) (plan.Result, *state.State, error) {
	// Parsed before anything expensive, so a typo in a flag fails immediately
	// rather than after a scrape.
	var overrideBytes int64
	if *c.memory != "" {
		var err error
		if overrideBytes, err = parseBytes(*c.memory); err != nil {
			return plan.Result{}, nil, fmt.Errorf("--memory: %w", err)
		}
	}

	var reserveBytes int64
	if *c.reserve != "" {
		var err error
		if reserveBytes, err = parseBytes(*c.reserve); err != nil {
			return plan.Result{}, nil, fmt.Errorf("--reserve: %w", err)
		}
	}

	targets, err := observe.Discover(log)
	if err != nil {
		return plan.Result{}, nil, err
	}
	if len(targets) == 0 {
		return plan.Result{}, nil, fmt.Errorf("no PHP-FPM pools found. " +
			"Discovery reads the process table and needs permission to inspect other users' " +
			"processes, so this usually means it is not running as root")
	}
	log.Debug("Discovered pools", "count", len(targets))

	st, err := state.Load(*c.statePath)
	if err != nil {
		return plan.Result{}, nil, err
	}

	views := observe.Sample(ctx, targets, log)

	// The MASTER's limit, not this process's, which is why it waits for the
	// scrape. On a VM the cap lives on php-fpm's own systemd slice and the root
	// cgroup has none, so reading it here sized a 3GiB service against a 20GiB
	// machine.
	limits := budget.DetectFor(serve.MasterPIDOf(views))
	if overrideBytes > 0 {
		limits = limits.WithOverride(overrideBytes)
	}

	stateOpts := state.Options{
		ConfidenceSamples: *c.confSamples,
		ConfidenceSpan:    *c.confSpan,
	}
	if !*c.noLearn {
		plan.LearnFrom(st, views, time.Now(), stateOpts)
	}

	result, buildErr := plan.Build(plan.Input{
		Limits:       limits,
		Views:        views,
		State:        st,
		ReserveBytes: reserveBytes,
		StateOptions: stateOpts,
	})

	// After the plan, for the same reason as in serve: these counters are the
	// next run's baseline, and storing them before Build compared a reading
	// against itself.
	plan.RecordCounters(st, views)

	// Save what was learned even when the allocation itself failed. The
	// observation was still valid, and an oversubscribed host is exactly where
	// accumulating real numbers matters most.
	if !*c.noLearn {
		if err := st.Save(*c.statePath); err != nil {
			log.Warn("Could not save learned baselines", "path", *c.statePath, "error", err)
		}
	}

	return result, st, buildErr
}

func runPlan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	c := registerCommon(fs)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "fpm-tune plan — propose pool sizes without changing "+
			"any PHP-FPM configuration.\n\nIt does record what it observed, to the state file, so that "+
			"running it\non a schedule builds a real baseline before apply is ever used. -no-learn\n"+
			"turns that off.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger(*c.verbose)

	// `plan` writes nothing to php-fpm, but it does record what it learned — and
	// the state file is read whole and written whole. Without the lock a plan
	// started before a `serve --apply` round would save afterwards and erase the
	// LastAppliedAt that the reload damping depends on, so the next round would
	// reload a pool it had just reloaded.
	learn := !*c.noLearn
	if learn {
		release, err := lock.Acquire(lock.DefaultPath(*c.statePath))
		switch {
		case err == nil:
			defer release()
		case errors.Is(err, lock.ErrHeld):
			// A daemon is running. `plan` is read-only apart from recording what
			// it learned, so it reports without recording rather than refusing —
			// an operator asking what the tool thinks, on a host where the tool
			// is running, should get an answer.
			fmt.Fprintln(os.Stderr,
				"fpm-tune: another fpm-tune is running, so this will report without "+
					"recording what it observes")
			learn = false
		default:
			return err
		}
	}
	c.noLearn = &[]bool{!learn}[0]

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *c.timeout)
	defer cancelTimeout()

	result, _, err := gather(ctx, c, log)
	if err != nil {
		return err
	}

	return result.Render(os.Stdout)
}

func runApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	c := registerCommon(fs)
	var (
		dropInDir   = fs.String("drop-in-dir", "", "where the pool settings are written; also selects which master to manage on a host running several (default: the directory the master includes)")
		backupDir   = fs.String("backup-dir", apply.DefaultBackupDir, "where the previous configuration is kept while a change is in flight")
		minInterval = fs.Duration("min-interval", 0, "shortest time between reloads (default 5m)")
		minChange   = fs.Float64("min-change", 0, "smallest relative change worth a reload (default 0.15)")
		dryRun      = fs.Bool("dry-run", false, "render and validate, but write nothing and reload nothing")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "fpm-tune apply — write the pool settings and reload PHP-FPM\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger(*c.verbose)

	// Held for the whole command. Without it an interactive apply and a running
	// `serve --apply` write the same fragments, and each takes the other's
	// half-applied state as "the previous configuration" to roll back to.
	release, err := lock.Acquire(lock.DefaultPath(*c.statePath))
	if err != nil {
		return err
	}
	defer release()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	// Applying involves a reload and a settle window, so it gets more room than
	// a scrape alone.
	ctx, cancelTimeout := context.WithTimeout(ctx, *c.timeout+30*time.Second)
	defer cancelTimeout()

	opts := apply.Options{
		MinInterval: *minInterval,
		MinChange:   *minChange,
		BackupDir:   *backupDir,
		DryRun:      *dryRun,
	}

	// Before ANY of the work, and deliberately before discovery: a previous run
	// may have died between writing the fragments and validating them, leaving
	// configuration php-fpm will not accept. Discovery parses the effective
	// config to find pools, so from that point on nothing can be discovered —
	// and the recovery path could not reach the master it exists to recover.
	// The remembered master, so recovery works on a host where php-fpm is not
	// running — which is exactly the case when this tool's own file is what
	// stops it starting.
	remembered := state.MasterRef{}
	if prior, err := state.Load(*c.statePath); err == nil {
		remembered = prior.Master
	}

	master, err := serve.MasterFromMemory(*dropInDir, remembered, log)
	if err != nil {
		return err
	}

	// A second lock, on the pool directory itself. The state lock above does not
	// cover it: two runs given different --state paths take different state
	// locks and then write the same fragments and the same backups.
	releaseResource, err := lock.Acquire(lock.ResourcePath(opts.BackupDir, master.DropInDir))
	if err != nil {
		return err
	}
	defer releaseResource()

	if err := apply.Reconcile(ctx, master, opts, log); err != nil {
		return err
	}
	// The parsed configuration is cached, so a repair the reconcile just made
	// would otherwise be invisible to the scrape that follows.
	phpfpm.InvalidateConfigCache(master.Binary, master.ConfigPath)

	result, st, err := gather(ctx, c, log)
	if err != nil {
		return err
	}

	st.RememberMaster(master.Binary, master.ConfigPath, master.DropInDir)

	applied, applyErr := apply.Apply(ctx, result.Plan, master, st, opts, log)

	// Report what happened before returning any error: an operator whose reload
	// failed needs to know which pools were involved.
	renderApplied(applied, *dryRun, applyErr)

	if applyErr != nil {
		return applyErr
	}

	// The applied settings change the hysteresis baseline, so the store has to
	// record them even though gather already saved once.
	if !*c.noLearn {
		if err := st.Save(*c.statePath); err != nil {
			log.Warn("Could not save state after applying", "error", err)
		}
	}

	return nil
}

func renderApplied(res apply.Result, dryRun bool, err error) {
	if len(res.Outcomes) == 0 {
		fmt.Println("No pools to consider.")

		return
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "POOL\tACTION\tDETAIL")
	for _, o := range res.Outcomes {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", o.Pool, o.Action, o.Reason)
	}
	_ = tw.Flush()

	switch {
	case len(res.RollbackFailed) > 0:
		// The most important line this command can print. Nothing is broken yet
		// — the master was never signalled — but the rejected configuration is
		// on disk, and the next reload from any source will adopt it.
		fmt.Printf("\nROLLBACK FAILED. The rejected configuration is still in place:\n  %s\n"+
			"PHP-FPM has not been reloaded, so nothing is broken yet — but the next reload\n"+
			"from any source will adopt it and the master will not come back. Remove these\n"+
			"files before anything reloads php-fpm.\n",
			strings.Join(res.RollbackFailed, "\n  "))
	case err != nil:
		fmt.Printf("\nNothing was applied: %v\n", err)
	case dryRun:
		fmt.Println("\nDry run: the configuration was rendered and validated, then discarded.")
	case res.RolledBack:
		fmt.Println("\nRolled back — the previous configuration has been restored.")
	case res.Reloaded:
		fmt.Printf("\nReloaded PHP-FPM (%d pool(s) changed).\n", len(res.Changed()))
	case len(res.Changed()) > 0:
		fmt.Printf("\nWrote %d pool(s). No running master to reload.\n", len(res.Changed()))
	default:
		fmt.Println("\nNothing worth changing.")
	}
}

func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelWarn
	if verbose {
		level = slog.LevelDebug
	}

	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// parseBytes reads a memory size.
//
// Accepts a plain byte count or a K/M/G/T suffix, with an optional "i" and an
// optional trailing "B" — so 8G, 8GB, 8Gi and 8GiB all mean the same thing.
// Every unit is binary; the "i" is accepted because that is how this tool PRINTS
// sizes, and a program that will not read its own output is a trap.
//
// Anything left over after the number and the suffix is an error rather than
// something to ignore. Sscanf stopped at the first non-digit and reported
// success, so --reserve 512MB parsed as 512 BYTES and silently handed the whole
// host to workers — the one outcome this tool exists to prevent, produced by a
// spelling of the unit that looks entirely reasonable.
func parseBytes(raw string) (int64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, fmt.Errorf("empty size")
	}

	// Peel the optional trailing B, then the optional i, then the unit.
	digits := strings.TrimSuffix(strings.TrimSuffix(trimmed, "B"), "b")
	digits = strings.TrimSuffix(strings.TrimSuffix(digits, "i"), "I")

	mult := int64(1)
	if digits != "" {
		switch digits[len(digits)-1] {
		case 'K', 'k':
			mult, digits = 1<<10, digits[:len(digits)-1]
		case 'M', 'm':
			mult, digits = 1<<20, digits[:len(digits)-1]
		case 'G', 'g':
			mult, digits = 1<<30, digits[:len(digits)-1]
		case 'T', 't':
			mult, digits = 1<<40, digits[:len(digits)-1]
		}
	}

	// ParseInt rather than Sscanf: it rejects what it cannot consume instead of
	// stopping at it and reporting success.
	n, err := strconv.ParseInt(strings.TrimSpace(digits), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size (try 512M, 8G or 8GiB)", raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%q is not a positive size", raw)
	}
	if n > (1<<63-1)/mult {
		return 0, fmt.Errorf("%q is too large", raw)
	}

	return n * mult, nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	c := registerCommon(fs)
	var (
		interval    = fs.Duration("interval", 30*time.Second, "how often to sample the pools")
		metricsAddr = fs.String("metrics", ":9110", "address for /metrics (empty disables it)")
		doApply     = fs.Bool("apply", false,
			"act on the plan. Without it the loop observes, learns and publishes metrics "+
				"without touching any configuration, which is a reasonable way to run permanently")
		dropInDir   = fs.String("drop-in-dir", "", "where the pool settings are written; also selects which master to manage on a host running several (default: the directory the master includes)")
		backupDir   = fs.String("backup-dir", apply.DefaultBackupDir, "where the previous configuration is kept while a change is in flight")
		minInterval = fs.Duration("min-interval", 0, "shortest time between reloads (default 5m)")
		minChange   = fs.Float64("min-change", 0, "smallest relative change worth a reload (default 0.15)")
		saveEvery   = fs.Duration("save-every", 5*time.Minute, "how often learned baselines reach disk")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "fpm-tune serve — keep watching and adjusting\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger(*c.verbose)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var memoryOverride, reserveBytes int64
	if *c.memory != "" {
		var err error
		if memoryOverride, err = parseBytes(*c.memory); err != nil {
			return fmt.Errorf("--memory: %w", err)
		}
	}
	if *c.reserve != "" {
		var err error
		if reserveBytes, err = parseBytes(*c.reserve); err != nil {
			return fmt.Errorf("--reserve: %w", err)
		}
	}

	loop, err := serve.New(serve.Config{
		Interval:       *interval,
		StatePath:      *c.statePath,
		SaveEvery:      *saveEvery,
		Apply:          *doApply,
		MetricsAddr:    *metricsAddr,
		DropInDir:      *dropInDir,
		BackupDir:      *backupDir,
		MemoryOverride: memoryOverride,
		ReserveBytes:   reserveBytes,
		ScrapeTimeout:  *c.timeout,
		ApplyOptions: apply.Options{
			MinInterval: *minInterval,
			MinChange:   *minChange,
		},
		StateOptions: state.Options{
			ConfidenceSamples: *c.confSamples,
			ConfidenceSpan:    *c.confSpan,
		},
	}, log)
	if err != nil {
		return err
	}

	return loop.Run(ctx)
}
