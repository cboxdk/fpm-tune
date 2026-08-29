package serve

import (
	"log/slog"
	"path/filepath"

	"github.com/cboxdk/phpfpm"
)

// ForMaster keeps only the pools belonging to the master that includes
// dropInDir.
//
// It lived in the CLI, so `plan` and `apply` scoped and `serve` did not — while
// the flag's own help said it "selects which master to manage on a host running
// several". A daemon on a two-master host therefore learned, sized and RENDERED
// the other master's pools: the budget was read from whichever master owned the
// first pool alphabetically, so the plan was sized against a cgroup limit that
// binds nothing it was writing for, and the rendered file named pools that
// master does not serve. The sandbox refused it every round, which is the
// safety net working and also a daemon that never applies anything again.
func ForMaster(targets []phpfpm.Target, dropInDir string, log *slog.Logger) []phpfpm.Target {
	if dropInDir == "" {
		configs := map[string]bool{}
		for _, t := range targets {
			configs[t.ConfigPath] = true
		}
		if len(configs) > 1 {
			// It used to say "against one master's memory limit", which was
			// true and is not any more: with pools spanning two masters there is
			// no single right limit, so the budget falls back to the MACHINE's
			// memory — which is larger than either of them, and therefore the
			// answer that overcommits.
			log.Warn("This host runs more than one PHP-FPM master and no --drop-in-dir "+
				"was given. Three things follow, and none of them is what you want: "+
				"the pools are planned together, so the budget is the whole MACHINE's "+
				"— no single master's limit applies to all of them, and the machine's is "+
				"more than either is allowed; nothing can be applied, since a write goes "+
				"to one master's pool directory; and two pools of the same name — `www` "+
				"is the default in every distribution — publish under one label, so the "+
				"metrics show one of them. Name a pool directory.",
				"masters", len(configs))
		}

		return targets
	}

	want := filepath.Clean(dropInDir)

	var kept []phpfpm.Target
	for _, t := range targets {
		for _, pattern := range IncludePatternsOf(t.ConfigPath) {
			if filepath.Clean(filepath.Dir(pattern)) == want {
				kept = append(kept, t)

				break
			}
		}
	}

	return kept
}
