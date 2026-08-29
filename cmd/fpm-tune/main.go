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
  fpm-tune serve    keep measuring, with metrics on /metrics. Add -apply to act
                    on what it finds.
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
		confSamples: fs.Int("confidence-samples", 0, "busy samples before a pool's baseline is trusted enough to CUT it; its measured cost is used from the first sample either way (default 20)"),
		confSpan:    fs.Duration("confidence-span", 0, "time span of busy evidence before a pool's baseline is trusted enough to CUT it (default 30m)"),
	}
}

// forMaster keeps the pools served by the master that includes dropInDir.
//
// With no directory named it keeps everything, and says so when that spans more
// than one master — the numbers are then a mixture, and an operator reading them
// should know before acting on them.
// refuseUnconfirmedBudget stops a write from a budget nobody could confirm.
//
// The detection falls back to the machine when php-fpm's own limit cannot be
// read, and the two numbers are indistinguishable — so a service capped at 3GiB
// gets sized against 32GiB and grown into a ceiling it never sees. Reading it
// and reporting it is right; ACTING on it is not, and --memory is how an
// operator says what the real number is.
//
// Its own function so it can be tested: it is one branch, and the branch is the
// difference between a refusal and an outage.
func refuseUnconfirmedBudget(limits budget.Limits) error {
	if limits.LookupErr == nil {
		return nil
	}

	// The error names the file it could not read, so "make it readable" is an
	// instruction rather than a direction. It used to say /proc/<pid>/cgroup,
	// which was the only case at the time and is now one of two — the other is a
	// limit file under /sys/fs/cgroup that exists and refuses.
	return fmt.Errorf("refusing to write: php-fpm's own memory limit could not be read, "+
		"so the only budget available is the machine's %s — and if php-fpm is capped "+
		"below that, sizing to it grows the pools into a ceiling they never see. Either "+
		"pass --memory with the real limit, or make the file below readable to this "+
		"user: %w", budget.HumanBytes(limits.MemoryBytes), limits.LookupErr)
}

// noPositionalArgs refuses a stray word on the command line.
//
// Go's flag package stops parsing at the first non-flag token and silently
// discards everything after it. So `fpm-tune plan -state S POOL -memory 1G`
// dropped -memory — the one flag whose whole purpose is to stop this tool
// sizing against the wrong number — and reported the entire host's memory
// instead, exit 0, no warning. A pool name is the obvious thing to type at a
// pool-sizing tool, and on `apply` the same slip discards -memory, -reserve,
// -state and -drop-in-dir and then WRITES.
func noPositionalArgs(fs *flag.FlagSet) error {
	if fs.NArg() == 0 {
		return nil
	}

	return fmt.Errorf("unexpected argument %q. fpm-tune takes no positional arguments, "+
		"and any flag after one is silently ignored — which is how a run ends up sizing "+
		"against the whole machine", fs.Arg(0))
}

// gather does everything both commands need: read the host, scrape the pools,
// fold the observation into the store, and build the allocation.
func gather(ctx context.Context, c commonFlags, dropInDir string, log *slog.Logger) (plan.Result, *state.State, error) {
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

	targets, err := observe.Discover(ctx, log)
	if err != nil {
		return plan.Result{}, nil, err
	}
	if len(targets) == 0 {
		return plan.Result{}, nil, noPoolsError()
	}
	log.Debug("Discovered pools", "count", len(targets))

	// One master's pools, when one was named.
	//
	// Every pool on the host was planned together and then sized against ONE
	// master's cgroup limit, which is incoherent on a host running two PHP
	// versions: the budget belongs to one of them and the pools to both. `apply`
	// already refuses that situation and tells the operator to name a directory;
	// `plan` did not offer the flag at all, so there was nothing to answer with.
	targets = serve.ForMaster(targets, dropInDir, log)
	if len(targets) == 0 {
		return plan.Result{}, nil, fmt.Errorf(
			"no pools belong to a master that includes %s", dropInDir)
	}

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
	// One clock for the round. Learning and planning both move time-based state,
	// and two calls to time.Now() a few hundred milliseconds apart is two
	// different rounds as far as a decay measured in time is concerned.
	now := time.Now()
	if !*c.noLearn {
		plan.LearnFrom(st, views, now, stateOpts)
	}

	result, buildErr := plan.Build(plan.Input{
		At:           now,
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
	dropInDir := fs.String("drop-in-dir", "",
		"plan for the master that includes this pool directory; without it a host "+
			"running several masters is planned as one, against one of their memory limits")
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
	if err := noPositionalArgs(fs); err != nil {
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

	result, _, err := gather(ctx, c, *dropInDir, log)
	if err != nil {
		return err
	}

	return result.Render(os.Stdout)
}

func runApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	c := registerCommon(fs)
	var (
		dropInDir = fs.String("drop-in-dir", "", "where the pool settings are written; also selects which master to manage on a host running several (default: the directory the master includes)")
		backupDir = fs.String("backup-dir", apply.DefaultBackupDir, "what is needed to undo a change and to repair a host: the previous "+
			"configuration while a change is in flight, the record of what is in flight, "+
			"and where php-fpm lives. Not scratch space — a rule that cleans it takes "+
			"away both")
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
	if err := noPositionalArgs(fs); err != nil {
		return err
	}

	// The same contradiction `serve --apply --no-learn` is refused for, and it
	// bites harder here because a single run looks like it worked.
	//
	// -no-learn suppresses the save, and the save is what carries LastAppliedAt
	// — the record hysteresis reads to refuse a reload within five minutes of
	// the last one. So every `apply --no-learn` was free to reload a pool the
	// previous one had just reloaded, on a busy host, indefinitely.
	if *c.noLearn && !*dryRun {
		return errors.New("-no-learn cannot be used with apply: applying has to record " +
			"what it wrote, or the next run reloads the pool this one just reloaded. Use " +
			"-dry-run to see what would happen without recording anything, or drop " +
			"-no-learn")
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
		// Same reason as in the daemon: with an explicit drop-in directory the
		// transaction record can supply the rest, so an unfinished change is
		// still recoverable on a host where php-fpm will not start and the state
		// file is gone.
		if !errors.Is(err, serve.ErrNoMaster) || *dropInDir == "" {
			return err
		}
		master = apply.Master{DropInDir: *dropInDir}
	}

	// A second lock, on the pool directory itself. The state lock above does not
	// cover it: two runs given different --state paths take different state
	// locks and then write the same fragments and the same backups.
	releaseResource, err := lock.Acquire(lock.ResourcePath(master.DropInDir))
	if err != nil {
		return err
	}
	defer releaseResource()

	// Not on a dry run. Reconcile removes files, rewrites them from backups and
	// signals the master — none of which a dry run may do, and the README
	// promises it does not. The promise was false: this ran before the dry-run
	// check and Reconcile never reads the flag.
	//
	// Skipped rather than made conditional inside Reconcile, because a rehearsal
	// of a repair is not a thing anyone asked for: what the operator needs is to
	// be told there is one waiting.
	if *dryRun {
		if path, found, _ := apply.PendingRepair(opts.BackupDir, master.DropInDir); found {
			fmt.Fprintf(os.Stderr,
				"fpm-tune: a previous run left an unfinished change to %s. "+
					"Running without --dry-run resolves it first.\n", path)
		}
	} else if _, err := apply.Reconcile(ctx, master, opts, log); err != nil {
		return err
	}
	// The parsed configuration is cached, so a repair the reconcile just made
	// would otherwise be invisible to the scrape that follows.
	phpfpm.InvalidateConfigCache(master.Binary, master.ConfigPath)

	result, st, err := gather(ctx, c, master.DropInDir, log)
	if err != nil {
		return err
	}

	// A budget nobody could confirm is not a budget to write from.
	//
	// The detection falls back to the machine when php-fpm's own cgroup cannot
	// be read, and the two numbers are indistinguishable — so a service capped
	// at 3GiB gets sized against 32GiB and grown into a ceiling it never sees.
	// Reading it and reporting it is right; ACTING on it is not, and --memory is
	// how an operator says what the real number is.
	if err := refuseUnconfirmedBudget(result.Budget); err != nil {
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

// noPoolsError explains an empty discovery in the order the causes actually
// occur. It used to name permissions and nothing else, which sends someone to
// check their privileges when the answer is usually that php-fpm is not
// running — and the two look identical from here.
func noPoolsError() error {
	return errors.New("no PHP-FPM pools found. " +
		"Either no php-fpm master is running — check with `systemctl status php-fpm` " +
		"or `pgrep -a php-fpm` — or this process cannot see it: discovery reads the " +
		"process table, and inspecting another user's processes needs root")
}

func renderApplied(res apply.Result, dryRun bool, err error) {
	if len(res.Outcomes) == 0 {
		fmt.Println("No pools to consider.")

		return
	}

	// The heading says whether this is what HAPPENED or what was going to.
	//
	// The actions are decided before anything is written, and nothing downgrades
	// them when the write, the validation or the reload fails — so a run that
	// could not write printed "shop applied 12 to 5" four lines above "Nothing
	// was applied", and the machine-readable half was the wrong one.
	heading := "DONE"
	if err != nil || res.RolledBack || dryRun {
		heading = "WOULD"
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "POOL\t%s\tDETAIL\n", heading)
	for _, o := range res.Outcomes {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", o.Pool, o.Action, o.Reason)
	}
	_ = tw.Flush()

	switch {
	case len(res.RollbackFailed) > 0:
		// The most important line this command can print, and it has to say
		// which of the two states the host is in. The message used to assert
		// "nothing is broken yet" unconditionally; that is only true when the
		// change was rejected before the master was signalled. When the master
		// was signalled and did not come back, php-fpm is already down.
		files := strings.Join(res.RollbackFailed, "\n  ")
		if errors.Is(err, apply.ErrMasterDidNotSurvive) {
			fmt.Printf("\nPHP-FPM IS DOWN AND COULD NOT BE PUT BACK.\n"+
				"The master did not survive the reload, and the configuration that killed\n"+
				"it could not be removed:\n  %s\n"+
				"Remove these files, then `systemctl reset-failed php-fpm && systemctl "+
				"start php-fpm`.\n", files)

			break
		}
		fmt.Printf("\nROLLBACK FAILED. The rejected configuration is still in place:\n  %s\n"+
			"PHP-FPM has not been reloaded, so nothing is broken yet — but the next reload\n"+
			"from any source will adopt it and the master will not come back. Remove these\n"+
			"files before anything reloads php-fpm.\n", files)
	case res.RolledBack:
		// Before the generic error, not after it. A rollback ALWAYS carries an
		// error — that is what caused it — so this case could never be reached,
		// and the one outcome an operator most needs to recognise was reported
		// as "nothing was applied", which reads like nothing happened.
		fmt.Printf("\nRolled back — the previous configuration has been restored.\nThe change "+
			"was refused: %v\n", err)
	case err != nil:
		fmt.Printf("\nNothing was applied: %v\n", err)
	case dryRun:
		fmt.Println("\nDry run: the configuration was rendered and validated, then discarded.")
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
		dropInDir = fs.String("drop-in-dir", "", "where the pool settings are written; also selects which master to manage on a host running several (default: the directory the master includes)")
		backupDir = fs.String("backup-dir", apply.DefaultBackupDir, "what is needed to undo a change and to repair a host: the previous "+
			"configuration while a change is in flight, the record of what is in flight, "+
			"and where php-fpm lives. Not scratch space — a rule that cleans it takes "+
			"away both")
		minInterval = fs.Duration("min-interval", 0, "shortest time between reloads (default 5m)")
		minChange   = fs.Float64("min-change", 0, "smallest relative change worth a reload (default 0.15)")
		saveEvery   = fs.Duration("save-every", 5*time.Minute, "how often learned baselines reach disk")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "fpm-tune serve — keep measuring PHP-FPM, and act on it "+
			"with -apply\n\nWithout -apply the loop observes, learns and publishes "+
			"metrics without\ntouching any configuration. Note that the self-repair is "+
			"part of applying:\na watch-only daemon will not fix a host whose master "+
			"this tool's own file\nis stopping from starting.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := noPositionalArgs(fs); err != nil {
		return err
	}

	// Contradictory, and silently resolving it either way is worse than saying
	// so. -no-learn means "record nothing about what you see"; -apply has to
	// record what it WROTE, because the reload damping reads it back and a
	// daemon that forgets its own changes reloads a pool it reloaded a minute
	// ago. Watching without recording is a real thing to want; acting without
	// recording is not.
	if *doApply && *c.noLearn {
		return errors.New("-apply and -no-learn contradict each other: applying has to " +
			"record what it wrote, or the next round reloads the pool it just reloaded. " +
			"Drop -no-learn to act, or drop -apply to watch")
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
		Interval:  *interval,
		StatePath: *c.statePath,
		SaveEvery: *saveEvery,
		Apply:     *doApply,
		NoLearn:   *c.noLearn,

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
