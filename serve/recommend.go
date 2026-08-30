package serve

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/apply"
	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/plan"
)

// The advisory half of this tool.
//
// `serve` without --apply already watches a host and changes nothing, which is
// a reasonable way to run permanently and is how anyone sensible starts. What
// it could not do was leave its conclusion anywhere: the numbers were in a log
// line and on a metrics endpoint, and neither is something you can put into a
// pool file.
//
// --recommend writes what the plan says, as configuration, to a path of your
// choosing that no php-fpm reads. Copy it, diff it against what you have, or
// leave it to accumulate mtimes and look at it when something changes.

// writeRecommendation records the current plan as configuration, when it
// differs from what is already there.
//
// Only on a change, deliberately: the file's modification time is then the
// answer to "when did the recommendation last move", which is the question a
// sidecar exists to answer. Rewriting identical bytes every thirty seconds
// would throw that away.
func (l *Loop) writeRecommendation(result plan.Result, now time.Time) {
	if l.cfg.RecommendPath == "" {
		return
	}

	// Never into a directory a master includes.
	//
	// What is written carries this tool's own generated marker, because the
	// point is that you can paste it — so a recommendation left in the pool
	// directory is a file php-fpm loads and this tool believes it wrote. The
	// pools would be configured by a run that was explicitly not applying
	// anything, and the repair path would treat it as its own work.
	//
	// Checked every round rather than once at startup: the master's include
	// directory is read from its configuration, and that can change under a
	// running daemon. Logged only on the TRANSITION, though — the same condition
	// every thirty seconds forever is how an operator learns to stop reading the
	// log. The startup check in New catches the explicit-directory case; this is
	// the one that can appear later.
	if l.recommendationWouldBeLoaded() {
		if !l.recommendBlocked {
			l.recommendBlocked = true
			l.log.Error("Refusing to write the recommendation: the path is inside a "+
				"directory PHP-FPM includes, and the file carries this tool's marker — "+
				"php-fpm would load it and this tool would believe it wrote it. Choose a "+
				"path outside the pool directory.",
				"path", l.cfg.RecommendPath)
		}

		return
	}
	l.recommendBlocked = false

	body, settings := renderRecommendation(result, now)

	// Compared on the SETTINGS, not on the whole file.
	//
	// The commentary carries evidence that moves every round — the reading
	// count climbs, the percentiles shift by a bucket — and none of that is a
	// change in the recommendation. The recommendation is the pm.* values; if
	// they are the same, the advice has not moved, and mtime should go on saying
	// when it last did.
	if existing, err := os.ReadFile(l.cfg.RecommendPath); err == nil {
		if settingsOf(string(existing)) == settings {
			return
		}
	}

	if err := os.MkdirAll(filepath.Dir(l.cfg.RecommendPath), 0o755); err != nil {
		l.log.Warn("Could not create the directory for the recommendation",
			"path", l.cfg.RecommendPath, "error", err)

		return
	}
	if err := os.WriteFile(l.cfg.RecommendPath, []byte(body), 0o644); err != nil {
		l.log.Warn("Could not write the recommendation",
			"path", l.cfg.RecommendPath, "error", err)

		return
	}

	l.log.Info("The recommendation changed", "path", l.cfg.RecommendPath)
}

// recommendationWouldBeLoaded reports whether the recommendation path sits
// where a master would read it.
func (l *Loop) recommendationWouldBeLoaded() bool {
	dir := filepath.Clean(filepath.Dir(l.cfg.RecommendPath))

	if l.cfg.DropInDir != "" && filepath.Clean(l.cfg.DropInDir) == dir {
		return true
	}

	masters, err := discoverMasters(l.log)
	if err != nil {
		// Cannot tell. Refusing on a failed scan would make an unrelated
		// discovery problem stop the advisory output, which is the one thing
		// this mode is for; the explicit directory above is the check that
		// matters and it has already run.
		return false
	}
	for _, m := range masters {
		for _, pattern := range IncludePatternsOf(m.ConfigPath) {
			if filepath.Clean(filepath.Dir(pattern)) == dir {
				return true
			}
		}
	}

	return false
}

// settingsOf is the configuration part of a recommendation file: everything
// from the first line that is neither a comment nor blank.
//
// Read back from the file rather than remembered in the process, so a restart
// does not rewrite an unchanged recommendation and reset the one timestamp this
// mode exists to provide.
func settingsOf(file string) string {
	lines := strings.Split(file, "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, ";") {
			continue
		}

		return strings.Join(lines[i:], "\n")
	}

	return ""
}

// renderRecommendation is the pool configuration this plan describes, with the
// evidence for it in comments.
//
// The evidence is not decoration. Somebody reading this is deciding by hand,
// and a bare pm.max_children tells them nothing about whether to trust it — the
// spread of what the workers actually measured is the thing that makes the
// number arguable rather than merely stated.
func renderRecommendation(result plan.Result, now time.Time) (file, settings string) {
	var b strings.Builder

	b.WriteString("; PHP-FPM pool settings recommended by fpm-tune.\n")
	b.WriteString(";\n")
	b.WriteString("; NOTHING READS THIS FILE. It is written by a daemon running without\n")
	b.WriteString("; --apply, which means it has changed nothing and will change nothing.\n")
	b.WriteString("; Copy what you want into the directory your master includes.\n")
	b.WriteString(";\n")
	b.WriteString("; These are the numbers the plan reaches NOW. A live --apply would damp\n")
	b.WriteString("; them: it refuses to move a pool that has changed recently or by too\n")
	b.WriteString("; little, because every change costs a reload. Deciding by hand, you do\n")
	b.WriteString("; not need that, so it is not applied here.\n")
	b.WriteString(";\n")
	fmt.Fprintf(&b, "; As of %s\n", now.Format(time.RFC3339))
	fmt.Fprintf(&b, "; %s\n", result.Budget.Describe())
	fmt.Fprintf(&b, "; reserved for the system: %s (%s)\n",
		budget.HumanBytes(result.Reserve), result.ReserveReason)
	if result.ChildReserve > 0 {
		fmt.Fprintf(&b, "; reserved for spawned children: %s (%s)\n",
			budget.HumanBytes(result.ChildReserve), result.ChildReserveReason)
	}
	fmt.Fprintf(&b, "; allocated %s of %s\n",
		budget.HumanBytes(result.Plan.AllocatedBytes),
		budget.HumanBytes(result.Plan.TotalBytes-result.Reserve))

	// The cgroup's own high-water, where there is one. It counts the children
	// the per-worker numbers below miss, so it is the honest ceiling to read the
	// allocation against — and if it sits well above what the workers sum to,
	// this host is spending memory on spawned processes that the sizing does not
	// yet see.
	if result.HasCgroupUsage {
		fmt.Fprintf(&b, "; cgroup used %s now, %s at its peak (workers AND everything "+
			"they spawned — the number the OOM killer enforces against)\n",
			budget.HumanBytes(result.CgroupUsage.CurrentBytes),
			budget.HumanBytes(result.CgroupUsage.PeakBytes))
	}

	for _, w := range result.Plan.Warnings {
		fmt.Fprintf(&b, ";\n; WARNING: %s\n", w)
	}

	spread := map[string]plan.PoolDistribution{}
	for _, d := range result.Distribution {
		spread[d.Name] = d
	}

	bootstrapped := map[string]bool{}
	for _, name := range result.Bootstrapped {
		bootstrapped[name] = true
	}

	writable := make([]allocate.PoolPlan, 0, len(result.Plan.Pools))
	for _, p := range result.Plan.Pools {
		if p.Unknown {
			// The plan reserves for it and never writes it, so a recommendation
			// naming it would be one nobody can act on: setting a ceiling means
			// knowing the one being replaced, and this pool's could not be read.
			fmt.Fprintf(&b, ";\n; %s: not recommended — %s\n", p.Name, p.Reason)

			continue
		}

		b.WriteString(";\n")
		fmt.Fprintf(&b, "; %s: %s\n", p.Name, p.Reason)
		if d, ok := spread[p.Name]; ok {
			fmt.Fprintf(&b, ";   measured per worker: median %s, p95 %s, p99 %s, worst %s "+
				"(%d readings)\n",
				budget.HumanBytes(d.P50), budget.HumanBytes(d.P95),
				budget.HumanBytes(d.P99), budget.HumanBytes(d.WorstSeen), d.Samples)

			// The children line appears only for a pool that actually spawned
			// something, so a plain web pool's recommendation is not cluttered
			// with a zero. Where it does appear, it is the memory this pool costs
			// beyond its workers — an ffmpeg, an imagemagick — which the per-worker
			// numbers above do not include and which sizing, today, does not yet
			// account for. Read it as a warning that the worker numbers understate
			// this pool.
			if child := d.ChildBytes(); child > 0 {
				fmt.Fprintf(&b, ";   spawned children add up to %s at their worst "+
					"(worker %s, worker+children %s) — NOT yet in the sizing below\n",
					budget.HumanBytes(child),
					budget.HumanBytes(d.WorkerHighWater),
					budget.HumanBytes(d.SubtreeHighWater))
			}
		}
		if bootstrapped[p.Name] {
			fmt.Fprintf(&b, ";   NOT YET MEASURED — this is a profile's guess, not this "+
				"pool's own memory. Leave fpm-tune running and it becomes a measurement.\n")
		}

		writable = append(writable, p)
	}

	if len(writable) == 0 {
		b.WriteString(";\n; Nothing to recommend: no pool here can be written.\n")

		return b.String(), ""
	}

	rendered := apply.Render(writable)
	b.WriteString("\n")
	b.Write(rendered)

	return b.String(), settingsOf(string(rendered))
}
