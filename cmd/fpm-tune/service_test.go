package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serveFlags registers the subset of serve's flags a service config can set, with
// the same names runServe uses, so applyConfig is exercised the way it runs.
func serveFlags() (*flag.FlagSet, *bool, *string, *string, *string) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "")
	recommend := fs.String("recommend", "", "")
	metrics := fs.String("metrics", ":9110", "")
	dropIn := fs.String("drop-in-dir", "", "")

	return fs, apply, recommend, metrics, dropIn
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestApplyConfigModeApply(t *testing.T) {
	fs, apply, recommend, metrics, _ := serveFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}

	path := writeConfig(t, "mode = apply\nmetrics = 127.0.0.1:9999\n")
	if err := applyConfig(fs, path); err != nil {
		t.Fatal(err)
	}
	if !*apply {
		t.Error("mode = apply did not set the apply flag")
	}
	if *metrics != "127.0.0.1:9999" {
		t.Errorf("metrics = %q, want 127.0.0.1:9999", *metrics)
	}
	if *recommend != "" {
		t.Errorf("apply mode set a recommend path: %q", *recommend)
	}
}

func TestApplyConfigAdvisoryDefaultsRecommend(t *testing.T) {
	fs, apply, recommend, _, _ := serveFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}

	path := writeConfig(t, "mode = advisory\n")
	if err := applyConfig(fs, path); err != nil {
		t.Fatal(err)
	}
	if *apply {
		t.Error("advisory mode set the apply flag")
	}
	// A watch-only daemon should still leave its conclusion somewhere.
	if *recommend != defaultRecommendPath {
		t.Errorf("recommend = %q, want the default %q", *recommend, defaultRecommendPath)
	}
}

// TestExplicitFlagOverridesConfig: the installed unit runs `serve --config` alone,
// but a one-off `serve --config … --metrics X` must let the command line win.
func TestExplicitFlagOverridesConfig(t *testing.T) {
	fs, apply, _, metrics, _ := serveFlags()
	if err := fs.Parse([]string{"-metrics", ":8000"}); err != nil {
		t.Fatal(err)
	}

	path := writeConfig(t, "mode = apply\nmetrics = 1.2.3.4:9\n")
	if err := applyConfig(fs, path); err != nil {
		t.Fatal(err)
	}
	if *metrics != ":8000" {
		t.Errorf("an explicit -metrics was overridden by the config: %q", *metrics)
	}
	if !*apply {
		t.Error("mode = apply should still apply — it was not set on the command line")
	}
}

func TestUnknownConfigKeyIsRefused(t *testing.T) {
	fs, _, _, _, _ := serveFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}

	if err := applyConfig(fs, writeConfig(t, "bogus = 1\n")); err == nil {
		t.Fatal("an unknown config key was accepted; a typo would be silently ignored")
	}
}

func TestApplyConfigRejectsABadMode(t *testing.T) {
	fs, _, _, _, _ := serveFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}

	if err := applyConfig(fs, writeConfig(t, "mode = act\n")); err == nil {
		t.Fatal("mode = act was accepted; only advisory and apply are valid")
	}
}

// TestTheRenderedConfigLoadsBack: install-service and serve --config have to agree
// on the format, or the unit it writes would not start.
func TestTheRenderedConfigLoadsBack(t *testing.T) {
	for _, mode := range []string{"advisory", "apply"} {
		path := writeConfig(t, renderServiceConfig(mode, defaultMetricsAddr))

		fs, apply, _, metrics, _ := serveFlags()
		if err := fs.Parse(nil); err != nil {
			t.Fatal(err)
		}
		if err := applyConfig(fs, path); err != nil {
			t.Fatalf("the config install-service writes does not load back (mode %s): %v", mode, err)
		}
		if (mode == "apply") != *apply {
			t.Errorf("mode %s round-tripped to apply=%v", mode, *apply)
		}
		if *metrics != defaultMetricsAddr {
			t.Errorf("metrics round-tripped to %q", *metrics)
		}
	}
}

func TestSetConfigModeRewritesTheLineAndNothingElse(t *testing.T) {
	path := writeConfig(t, "# a comment mentioning mode = whatever\nmode = advisory\nmetrics = 1.2.3.4:9\n")

	if err := setConfigMode(path, "apply"); err != nil {
		t.Fatal(err)
	}
	kv, err := loadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if kv["mode"] != "apply" {
		t.Errorf("mode = %q, want apply", kv["mode"])
	}
	if kv["metrics"] != "1.2.3.4:9" {
		t.Errorf("setConfigMode changed another setting: metrics = %q", kv["metrics"])
	}
}

func TestSetConfigModeAddsModeWhenAbsent(t *testing.T) {
	path := writeConfig(t, "metrics = x\n")

	if err := setConfigMode(path, "apply"); err != nil {
		t.Fatal(err)
	}
	kv, err := loadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if kv["mode"] != "apply" {
		t.Error("mode was not added to a config that lacked it")
	}
}

// TestSetConfigKeyUpdatesInPlace: updating one key must leave every other line
// alone — the mode an operator set with `fpm-tune mode`, a hand-edited heartbeat,
// the commented-out defaults. This is what lets `install-service -metrics X` on a
// re-run change the metrics address without clobbering the rest.
func TestSetConfigKeyUpdatesInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	original := "mode = apply\nmetrics = 127.0.0.1:9110\n# sizing = p95\nheartbeat = 30m\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := setConfigKey(path, "metrics", "0.0.0.0:9110"); err != nil {
		t.Fatal(err)
	}

	kv, err := loadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if kv["metrics"] != "0.0.0.0:9110" {
		t.Errorf("metrics = %q, want 0.0.0.0:9110", kv["metrics"])
	}
	if kv["mode"] != "apply" {
		t.Errorf("mode was clobbered: %q, want apply", kv["mode"])
	}
	if kv["heartbeat"] != "30m" {
		t.Errorf("a hand-edited key was lost: heartbeat = %q, want 30m", kv["heartbeat"])
	}
	if body, _ := os.ReadFile(path); !strings.Contains(string(body), "# sizing = p95") {
		t.Error("the commented-out default was dropped")
	}
}
