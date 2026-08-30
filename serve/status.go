package serve

import (
	"context"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/cboxdk/fpm-tune/apply"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/phpfpm"
)

// EnsureStatus turns the status page on for any pool on `master` that has none, so
// a scrape can see it, and returns what it enabled.
//
// Shared by `fpm-tune apply` and the serve loop so both bootstrap a stock php-fpm
// the same way: its default www pool ships pm.status_path off, so a host that has
// never had it turned on has nothing to measure until it is. A no-op when every pool
// already exposes one.
func EnsureStatus(ctx context.Context, master apply.Master, statusPath, backupDir string, log *slog.Logger) (apply.StatusResult, error) {
	toEnable, exists, err := UnstatusedFor(ctx, master, log)
	if err != nil {
		return apply.StatusResult{}, err
	}
	if len(toEnable) == 0 {
		return apply.StatusResult{}, nil
	}

	return apply.EnableStatus(ctx, master, toEnable, exists, statusPath,
		apply.Options{BackupDir: backupDir}, log)
}

// UnstatusedFor lists the pools on `master` that have no status page, and the full
// set of pool names it serves — the latter so EnableStatus can drop a
// carried-forward section for a pool that has since been removed.
func UnstatusedFor(ctx context.Context, master apply.Master, log *slog.Logger) ([]string, map[string]bool, error) {
	targets, err := observe.Discover(ctx, log)
	if err != nil {
		return nil, nil, err
	}
	unstatused, err := phpfpm.UnstatusedPools(ctx, log)
	if err != nil {
		return nil, nil, err
	}

	exists := map[string]bool{}
	for _, t := range targets {
		if sameMasterConfig(t.ConfigPath, master.ConfigPath) {
			exists[t.Name] = true
		}
	}

	// Deduped by name: a host mid-reload carries two master processes for the same
	// config, each reporting the same pool, and enabling — or reporting — it twice
	// reads as a bug.
	seen := map[string]bool{}
	var toEnable []string
	for _, u := range unstatused {
		if sameMasterConfig(u.ConfigPath, master.ConfigPath) && !seen[u.Name] {
			seen[u.Name] = true
			toEnable = append(toEnable, u.Name)
			exists[u.Name] = true
		}
	}
	sort.Strings(toEnable)

	return toEnable, exists, nil
}

// sameMasterConfig reports whether two config paths name the same master.
func sameMasterConfig(a, b string) bool {
	return a != "" && b != "" && filepath.Clean(a) == filepath.Clean(b)
}
