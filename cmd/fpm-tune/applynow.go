package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/cboxdk/fpm-tune/serve"
	"github.com/cboxdk/fpm-tune/state"
)

// runApplyNow asks the running daemon to apply its plan once. The daemon
// keeps watching in whatever mode it is in; this is the one-off act an
// operator takes after reading what it would do.
func runApplyNow(args []string) error {
	fs := flag.NewFlagSet("apply-now", flag.ContinueOnError)
	control := fs.String("control", filepath.Join(filepath.Dir(state.DefaultPath), "control.sock"),
		"the daemon's control socket")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "fpm-tune apply-now — ask the running daemon to apply its plan once.\n\n"+
			"The daemon holds the state and the plan; this tells it to act on the plan it\n"+
			"showed, now, with the reload damping waived, and stays in the mode it is in.\n"+
			"Needs root: the control socket is root's. fpm-tune top's a key runs this.\n\n")
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

	out, err := serve.ApplyNow(ctx, *control)
	if err != nil {
		return err
	}
	fmt.Print(describeOutcome(out))
	if out.Error != "" {
		return errors.New(out.Error)
	}

	return nil
}

// describeOutcome is the outcome as lines a person reads. A failure is not
// among them: it goes back as the error, which main prints once.
func describeOutcome(out serve.ApplyOutcome) string {
	s := ""
	for _, c := range out.Changed {
		s += fmt.Sprintf("%s  %d → %d  %s\n", c.Pool, c.From, c.To, c.Detail)
	}
	if len(out.Changed) == 0 && out.Message != "" && out.Error == "" {
		s += out.Message + "\n"
	}

	return s
}
