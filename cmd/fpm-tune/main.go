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

	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/plan"
	"github.com/cboxdk/fpm-tune/state"
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
  fpm-tune version

`, version)
}

func runPlan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	var (
		statePath = fs.String("state", state.DefaultPath, "path to the learned baselines")
		memory    = fs.String("memory", "", "override the detected memory limit (e.g. 8G)")
		reserve   = fs.String("reserve", "", "memory to hold back from workers (e.g. 1G)")
		timeout   = fs.Duration("timeout", 15*time.Second, "budget for scraping all pools")
		verbose   = fs.Bool("verbose", false, "log what is being read")

		// How much evidence a pool needs before its own measurements are used
		// instead of the bootstrap estimate. Both must be satisfied: samples
		// alone would let a tight loop claim confidence in seconds, which
		// measures the polling interval rather than the workload.
		confSamples = fs.Int("confidence-samples", 0, "busy samples before a pool is sized from its own memory (default 20)")
		confSpan    = fs.Duration("confidence-span", 0, "time span before a pool is sized from its own memory (default 30m)")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "fpm-tune plan — propose pool sizes without changing anything\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger(*verbose)

	// Ctrl-C during a scrape should stop promptly rather than waiting out every
	// unreachable pool's dial timeout.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
	defer cancelTimeout()

	limits := budget.Detect()
	if *memory != "" {
		bytes, err := parseBytes(*memory)
		if err != nil {
			return fmt.Errorf("--memory: %w", err)
		}
		limits = limits.WithOverride(bytes)
	}

	var reserveBytes int64
	if *reserve != "" {
		var err error
		if reserveBytes, err = parseBytes(*reserve); err != nil {
			return fmt.Errorf("--reserve: %w", err)
		}
	}

	targets, err := observe.Discover(log)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("no PHP-FPM pools found. " +
			"Discovery reads the process table and needs permission to inspect other users' " +
			"processes, so this usually means it is not running as root")
	}
	log.Debug("Discovered pools", "count", len(targets))

	st, err := state.Load(*statePath)
	if err != nil {
		return err
	}

	views := observe.Sample(ctx, targets, log)

	// plan is read-only, but it still learns: running it on a cron for a week
	// before ever running apply is a legitimate and cautious way to adopt this,
	// and it should arrive with real measurements rather than a cold store.
	stateOpts := state.Options{
		ConfidenceSamples: *confSamples,
		ConfidenceSpan:    *confSpan,
	}
	plan.LearnFrom(st, views, time.Now(), stateOpts)

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
	if err := st.Save(*statePath); err != nil {
		log.Warn("Could not save learned baselines", "path", *statePath, "error", err)
	}

	if buildErr != nil {
		return buildErr
	}

	return result.Render(os.Stdout)
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
