// Command fpm-tune sizes PHP-FPM pools against the memory a host actually has.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/cboxdk/fpm-tune/apply"
	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/plan"
	"github.com/cboxdk/fpm-tune/state"
	"github.com/cboxdk/phpfpm"
)

// version is set at build time.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
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

  fpm-tune plan     show what would change, and why. Writes nothing.
  fpm-tune apply    write the pool settings and reload PHP-FPM.
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
	limits := budget.Detect()
	if *c.memory != "" {
		bytes, err := parseBytes(*c.memory)
		if err != nil {
			return plan.Result{}, nil, fmt.Errorf("--memory: %w", err)
		}
		limits = limits.WithOverride(bytes)
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
		fmt.Fprintf(os.Stderr, "fpm-tune plan — propose pool sizes without changing anything\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger(*c.verbose)

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
		dropInDir   = fs.String("drop-in-dir", "", "where pool fragments are written (default: alongside the discovered pool configs)")
		backupDir   = fs.String("backup-dir", apply.DefaultBackupDir, "where the previous fragments are kept while a change is in flight")
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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	// Applying involves a reload and a settle window, so it gets more room than
	// a scrape alone.
	ctx, cancelTimeout := context.WithTimeout(ctx, *c.timeout+30*time.Second)
	defer cancelTimeout()

	result, st, err := gather(ctx, c, log)
	if err != nil {
		return err
	}

	master, err := masterFor(result, *dropInDir)
	if err != nil {
		return err
	}

	applied, applyErr := apply.Apply(ctx, result.Plan, master, st, apply.Options{
		MinInterval: *minInterval,
		MinChange:   *minChange,
		BackupDir:   *backupDir,
		DryRun:      *dryRun,
	}, log)

	// Report what happened before returning any error: an operator whose reload
	// failed needs to know which pools were involved.
	renderApplied(applied, *dryRun)

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

// masterFor works out which PHP-FPM to reconfigure.
//
// Every pool discovered on a host normally belongs to one master, and they share
// a single reload. Two masters would need two reloads and two validations, which
// is a real configuration but not one this command handles yet — so it says so
// rather than reconfiguring one and silently ignoring the other.
func masterFor(result plan.Result, dropInDir string) (apply.Master, error) {
	binaries := map[string]phpfpmTarget{}
	for _, v := range result.Views {
		if v.Target.Binary == "" || v.Target.ConfigPath == "" {
			continue
		}
		binaries[v.Target.Binary+"::"+v.Target.ConfigPath] = phpfpmTarget{
			binary: v.Target.Binary, config: v.Target.ConfigPath,
		}
	}

	switch len(binaries) {
	case 0:
		return apply.Master{}, fmt.Errorf("could not determine the php-fpm binary and config to validate against")
	case 1:
	default:
		var names []string
		for _, t := range binaries {
			names = append(names, t.config)
		}
		sort.Strings(names)

		return apply.Master{}, fmt.Errorf(
			"this host runs %d PHP-FPM masters (%s); apply reconfigures one at a time, "+
				"so run it once per master with --drop-in-dir set accordingly",
			len(binaries), strings.Join(names, ", "))
	}

	var target phpfpmTarget
	for _, t := range binaries {
		target = t
	}

	master := apply.Master{
		Binary:     target.binary,
		ConfigPath: target.config,
		DropInDir:  dropInDir,
	}
	if master.DropInDir == "" {
		// The pool fragments live beside the master config's include directory.
		// Defaulting to the config's own directory is wrong on Debian, where the
		// master is /etc/php/8.2/fpm/php-fpm.conf and pools are in pool.d — so
		// require it rather than guessing.
		master.DropInDir = defaultDropInDir(target.config)
	}
	if master.DropInDir == "" {
		return master, fmt.Errorf("could not locate the pool configuration directory; pass --drop-in-dir")
	}

	if pid, err := phpfpm.MasterPID(pidFileFor(target.config)); err == nil {
		master.PID = pid
	}

	return master, nil
}

type phpfpmTarget struct{ binary, config string }

// defaultDropInDir finds the directory the master config includes pools from.
func defaultDropInDir(configPath string) string {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "include") {
			continue
		}
		_, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		pattern := strings.TrimSpace(value)
		if dir := filepath.Dir(pattern); dir != "." && dir != "/" {
			return dir
		}
	}

	return ""
}

// pidFileFor reads the master's pid file location from its config.
func pidFileFor(configPath string) string {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "pid") {
			continue
		}
		if _, value, found := strings.Cut(line, "="); found {
			return strings.TrimSpace(value)
		}
	}

	return ""
}

func renderApplied(res apply.Result, dryRun bool) {
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

// parseBytes accepts plain bytes or a K/M/G/T suffix, so an operator can write
// --memory 8G rather than counting zeroes.
func parseBytes(raw string) (int64, error) {
	if raw == "" {
		return 0, fmt.Errorf("empty size")
	}

	mult := int64(1)
	digits := raw

	switch raw[len(raw)-1] {
	case 'K', 'k':
		mult, digits = 1<<10, raw[:len(raw)-1]
	case 'M', 'm':
		mult, digits = 1<<20, raw[:len(raw)-1]
	case 'G', 'g':
		mult, digits = 1<<30, raw[:len(raw)-1]
	case 'T', 't':
		mult, digits = 1<<40, raw[:len(raw)-1]
	}

	var n int64
	if _, err := fmt.Sscanf(digits, "%d", &n); err != nil {
		return 0, fmt.Errorf("%q is not a size (try 512M or 8G)", raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%q is not a positive size", raw)
	}

	return n * mult, nil
}
