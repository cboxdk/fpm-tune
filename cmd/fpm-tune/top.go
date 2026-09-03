package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cboxdk/fpm-tune/top"
)

// runTop draws the daemon's history in the terminal. Read-only: it changes
// nothing, and points at the metrics address the installed service uses
// unless told otherwise.
func runTop(args []string) error {
	fs := flag.NewFlagSet("top", flag.ContinueOnError)
	addr := fs.String("addr", "", "the daemon's metrics address, where /history.json is served "+
		"(default: the installed service's, or 127.0.0.1:9110)")
	refresh := fs.Duration("refresh", 5*time.Second, "how often to fetch")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "fpm-tune top — watch a running fpm-tune: busy workers, queues, the CPU\n"+
			"side and every resize, drawn from the day of rounds the daemon keeps.\n\n"+
			"Reads /history.json on the metrics address and changes nothing.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := noPositionalArgs(fs); err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	return top.Run(ctx, top.Options{Addr: topAddr(*addr), Refresh: *refresh})
}

// topAddr resolves where to look: the flag, else the installed service's
// metrics address with a wildcard host turned into loopback, else the
// default.
func topAddr(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if kv, err := loadConfigFile(defaultConfigPath); err == nil {
		if m := strings.TrimSpace(kv["metrics"]); m != "" {
			return loopbackFor(m)
		}
	}

	return defaultMetricsAddr
}

// loopbackFor turns "0.0.0.0:9110" or ":9110" into "127.0.0.1:9110": a
// wildcard is where the daemon listens, not where a client dials.
func loopbackFor(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}

	return net.JoinHostPort(host, port)
}
