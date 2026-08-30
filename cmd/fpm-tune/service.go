package main

// Running fpm-tune in the background, and switching what it does there, without
// hand-writing a systemd unit or editing ExecStart to change mode.
//
// `install-service` writes a config file and a unit that reads it; `mode` flips the
// one line in that config and restarts. The config, not the unit, holds the mode —
// so switching between watching and acting is a command, not a unit edit and a
// daemon-reload.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	defaultConfigPath    = "/etc/fpm-tune/config"
	defaultRecommendPath = "/var/lib/fpm-tune/recommended.conf"
	defaultMetricsAddr   = "127.0.0.1:9110"
	unitPath             = "/etc/systemd/system/fpm-tune.service"
	unitName             = "fpm-tune"
)

// loadConfigFile reads a key = value file, ignoring blank lines and # / ; comments.
func loadConfigFile(path string) (map[string]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config %s: %w", path, err)
	}

	kv := map[string]string{}
	for i, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("config %s line %d: expected `key = value`, got %q", path, i+1, line)
		}
		kv[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}

	return kv, nil
}

// applyConfig folds a service config file into a serve flagset. An explicit flag on
// the command line always wins over the file, so a one-off `serve --config … --apply`
// does what it says without editing the file.
func applyConfig(fs *flag.FlagSet, path string) error {
	kv, err := loadConfigFile(path)
	if err != nil {
		return err
	}

	setOnCmdline := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setOnCmdline[f.Name] = true })

	// mode is a convenience over the -apply flag plus a default recommendation
	// path, so the file reads as a mode switch rather than a pile of flags.
	if mode, ok := kv["mode"]; ok {
		switch mode {
		case "apply":
			if !setOnCmdline["apply"] {
				_ = fs.Set("apply", "true")
			}
		case "advisory", "":
			// The default. Give a watch-only daemon somewhere to leave its
			// conclusion, unless the file or the command line names one.
			if _, named := kv["recommend"]; !named && !setOnCmdline["recommend"] {
				_ = fs.Set("recommend", defaultRecommendPath)
			}
		default:
			return fmt.Errorf("config %s: mode must be `advisory` or `apply`, got %q", path, mode)
		}
		delete(kv, "mode")
	}

	for key, val := range kv {
		if setOnCmdline[key] {
			continue
		}
		if fs.Lookup(key) == nil {
			return fmt.Errorf("config %s: unknown key %q", path, key)
		}
		if err := fs.Set(key, val); err != nil {
			return fmt.Errorf("config %s: %s = %q: %w", path, key, val, err)
		}
	}

	return nil
}

// runInstallService writes the config and systemd unit, and starts the service.
func runInstallService(args []string) error {
	fs := flag.NewFlagSet("install-service", flag.ContinueOnError)
	var (
		apply = fs.Bool("apply", false, "install in apply mode — act on the plan. The default is "+
			"advisory: watch, learn and recommend, changing nothing")
		metrics = fs.String("metrics", defaultMetricsAddr, "address for /metrics")
		print   = fs.Bool("print", false, "print the unit and config instead of installing them")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "fpm-tune install-service — run fpm-tune in the background under "+
			"systemd.\n\nWrites %s and a unit that reads it, then enables and starts the "+
			"service. Starts in advisory mode (watch and recommend, change nothing); pass "+
			"-apply to act, or switch any time with `fpm-tune mode apply`.\n\n", defaultConfigPath)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := noPositionalArgs(fs); err != nil {
		return err
	}

	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine this binary's path to write the unit: %w", err)
	}

	mode := "advisory"
	if *apply {
		mode = "apply"
	}
	config := renderServiceConfig(mode, *metrics)
	unit := renderUnit(binPath)

	if *print {
		fmt.Printf("# %s\n%s\n# %s\n%s", defaultConfigPath, config, unitPath, unit)

		return nil
	}

	// Everything below writes under /etc and talks to systemd, which needs root.
	// Refused with the fix rather than failing halfway, the same stance as the
	// installer: never escalate on its own.
	if os.Geteuid() != 0 {
		return fmt.Errorf("installing a system service needs root. Re-run with sudo:\n"+
			"  sudo %s\nor `fpm-tune install-service --print` to see the unit and config first",
			strings.Join(os.Args, " "))
	}

	if err := os.MkdirAll(filepath.Dir(defaultConfigPath), 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", filepath.Dir(defaultConfigPath), err)
	}

	// The config is preserved if it already exists, so re-running this never resets
	// a mode the operator chose with `fpm-tune mode`. The unit is always rewritten:
	// the binary may have moved.
	wroteConfig := false
	if _, statErr := os.Stat(defaultConfigPath); os.IsNotExist(statErr) {
		if err := os.WriteFile(defaultConfigPath, []byte(config), 0o644); err != nil {
			return fmt.Errorf("cannot write %s: %w", defaultConfigPath, err)
		}
		wroteConfig = true
	}

	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("cannot write %s: %w", unitPath, err)
	}

	if err := systemctl("daemon-reload"); err != nil {
		return fmt.Errorf("wrote the unit, but `systemctl daemon-reload` failed: %w", err)
	}
	if err := systemctl("enable", "--now", unitName); err != nil {
		return fmt.Errorf("wrote the unit, but `systemctl enable --now %s` failed: %w", unitName, err)
	}

	current := currentMode()
	fmt.Printf("Installed and started fpm-tune (%s).\n", current)
	if !wroteConfig {
		fmt.Printf("  kept the existing %s (mode = %s)\n", defaultConfigPath, current)
	}
	fmt.Printf("  config:   %s\n  metrics:  %s\n\n", defaultConfigPath, *metrics)
	fmt.Print("Switch mode any time (no unit edit needed):\n" +
		"  fpm-tune mode apply       # let it act on what it finds\n" +
		"  fpm-tune mode advisory    # back to watch-only\n\n" +
		"Follow it:  journalctl -u fpm-tune -f\n")

	return nil
}

// runMode flips the service's mode in the config file and restarts it.
func runMode(args []string) error {
	fs := flag.NewFlagSet("mode", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "fpm-tune mode [advisory|apply] — show or switch what the "+
			"background service does.\n\nWith no argument it prints the current mode. With "+
			"`advisory` or `apply` it rewrites %s and restarts the service.\n\n"+
			"  advisory   watch, learn and recommend — change nothing\n"+
			"  apply      also act on the plan (write pm.* and reload)\n\n", defaultConfigPath)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Printf("mode = %s (%s)\n", currentMode(), defaultConfigPath)

		return nil
	}
	if len(rest) > 1 {
		return fmt.Errorf("one mode at a time, got %v", rest)
	}
	mode := rest[0]
	if mode != "advisory" && mode != "apply" {
		return fmt.Errorf("mode must be `advisory` or `apply`, got %q", mode)
	}

	if _, err := os.Stat(defaultConfigPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no service config at %s — run `fpm-tune install-service` first",
			defaultConfigPath)
	}

	if os.Geteuid() != 0 {
		return fmt.Errorf("switching the service mode needs root. Re-run with sudo:\n  sudo %s",
			strings.Join(os.Args, " "))
	}

	if err := setConfigMode(defaultConfigPath, mode); err != nil {
		return err
	}
	if err := systemctl("restart", unitName); err != nil {
		return fmt.Errorf("set mode to %s in %s, but `systemctl restart %s` failed: %w",
			mode, defaultConfigPath, unitName, err)
	}

	fmt.Printf("Switched to %s and restarted fpm-tune.\n", mode)

	return nil
}

// currentMode reports the mode the config file names, or "advisory" when it is
// unset or unreadable — the safe default.
func currentMode() string {
	kv, err := loadConfigFile(defaultConfigPath)
	if err != nil {
		return "advisory"
	}
	if kv["mode"] == "apply" {
		return "apply"
	}

	return "advisory"
}

// setConfigMode rewrites (or adds) the `mode` line in the config, leaving the rest
// of the file — an operator's edits and comments — untouched.
func setConfigMode(path, mode string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", path, err)
	}

	lines := strings.Split(string(body), "\n")
	replaced := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if key, _, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(key) == "mode" {
			lines[i] = "mode = " + mode
			replaced = true

			break
		}
	}
	if !replaced {
		lines = append([]string{"mode = " + mode}, lines...)
	}

	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

// systemctl runs a systemctl subcommand, or reports plainly when it is not there.
func systemctl(args ...string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl not found — this host is not running systemd; "+
			"run `%s serve --config %s` under whatever supervises it", os.Args[0], defaultConfigPath)
	}
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr

	return cmd.Run()
}

func renderServiceConfig(mode, metrics string) string {
	return fmt.Sprintf(`# fpm-tune service settings, read by: fpm-tune serve --config %s
#
# Switch mode without editing this file:
#   fpm-tune mode apply       # let it act on what it finds
#   fpm-tune mode advisory    # watch and recommend only, change nothing
# or edit "mode" below and: systemctl restart fpm-tune

# advisory: watch, learn, publish metrics, write a recommendation — change nothing.
# apply:    also act on the plan (write pm.* and reload, never restart).
mode = %s

# Where /metrics is served. Empty disables it.
metrics = %s

# On a host running several php-fpm masters, name the pool directory of the one to
# manage. Unset is correct for a single master.
# drop-in-dir =

# In advisory mode, the recommendation is written here for you to read and paste.
# recommend = %s
`, defaultConfigPath, mode, metrics, defaultRecommendPath)
}

func renderUnit(binPath string) string {
	return fmt.Sprintf(`[Unit]
Description=fpm-tune — size PHP-FPM pools against available memory
# Wants, not Requires: a supervisor that dies with the thing it supervises cannot
# repair it.
Wants=php-fpm.service
After=php-fpm.service

[Service]
ExecStart=%s serve --config %s
Restart=on-failure
RestartSec=5
# systemd owns the state directory with sensible permissions, rather than the tool
# creating it under whatever umask it inherited.
StateDirectory=fpm-tune
StateDirectoryMode=0700

[Install]
WantedBy=multi-user.target
`, binPath, defaultConfigPath)
}
