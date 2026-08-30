package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/cboxdk/fpm-tune/apply"
	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/phpfpm"
)

// TestTheOutcomeAnOperatorHasToActOnIsPrinted.
//
// renderApplied's switch decides what a person reads at the end of an apply, and
// two of its arms were wrong in ways that only show up on the bad days.
//
// A rollback ALWAYS carries an error — the error is what caused it — so the
// generic `err != nil` arm matched first and the rollback arm could not be
// reached. The one outcome that most needs recognising was reported as "nothing
// was applied", which reads like nothing happened.
//
// And the rollback-failed message asserted "PHP-FPM has not been reloaded, so
// nothing is broken yet" whatever had happened. That is true when the change
// was rejected before the signal. When the master WAS signalled and did not come
// back, php-fpm is down, and the line telling the operator otherwise is the
// difference between restarting it now and finding out in the morning.
func TestTheOutcomeAnOperatorHasToActOnIsPrinted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		res     apply.Result
		err     error
		want    []string
		notWant []string
	}{
		{
			name: "rolled back after a rejection",
			res:  apply.Result{Outcomes: []apply.Outcome{{Pool: "shop", Action: "held"}}, RolledBack: true},
			err:  fmt.Errorf("%w: php-fpm exited 78", apply.ErrValidationFailed),
			want: []string{"Rolled back", "restored", "exited 78"},
			// The arm that used to swallow it.
			notWant: []string{"Nothing was applied"},
		},
		{
			name: "the master is gone and the file is stuck",
			res: apply.Result{
				Outcomes:       []apply.Outcome{{Pool: "shop", Action: "applied"}},
				RollbackFailed: []string{"/etc/php-fpm.d/zz-fpm-tune.conf"},
			},
			err:  fmt.Errorf("%w: it exited during the reload", apply.ErrMasterDidNotSurvive),
			want: []string{"PHP-FPM IS DOWN", "zz-fpm-tune.conf", "systemctl"},
			// The reassurance that would be a lie here.
			notWant: []string{"nothing is broken yet"},
		},
		{
			name: "rejected before the master was signalled",
			res: apply.Result{
				Outcomes:       []apply.Outcome{{Pool: "shop", Action: "held"}},
				RollbackFailed: []string{"/etc/php-fpm.d/zz-fpm-tune.conf"},
			},
			err:     fmt.Errorf("%w: php-fpm exited 78", apply.ErrValidationFailed),
			want:    []string{"ROLLBACK FAILED", "nothing is broken yet"},
			notWant: []string{"PHP-FPM IS DOWN"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := capture(t, func() { renderApplied(tc.res, false, tc.err) })

			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("the output does not mention %q:\n%s", want, got)
				}
			}
			for _, not := range tc.notWant {
				if strings.Contains(got, not) {
					t.Errorf("the output says %q, which is not true here:\n%s", not, got)
				}
			}
		})
	}
}

// TestNoPoolsDoesNotBlameRootFirst: the commonest cause is that php-fpm is not
// running, and sending an operator to check their privileges first costs them
// the obvious answer.
func TestNoPoolsDoesNotBlameRootFirst(t *testing.T) {
	err := noMasterError()

	msg := err.Error()
	if !strings.Contains(msg, "systemctl status php-fpm") && !strings.Contains(msg, "pgrep") {
		t.Errorf("the message does not suggest checking whether php-fpm is running:\n%s", msg)
	}
	if root := strings.Index(msg, "root"); root >= 0 && root < strings.Index(msg, "running") {
		t.Errorf("permissions are blamed before the process is even looked for:\n%s", msg)
	}
}

// TestUnstatusedPoolsErrorPointsToEnableStatus: when a master IS running but its
// pools have no status page, the message must name that, not send the operator to
// look for a master that is up — and it must name the command that fixes it.
func TestUnstatusedPoolsErrorPointsToEnableStatus(t *testing.T) {
	err := unstatusedPoolsError([]phpfpm.Unstatused{
		// The same pool twice, as a host mid-reload reports it from two masters.
		{Name: "www", ConfigPath: "/etc/php/8.4/fpm/php-fpm.conf"},
		{Name: "www", ConfigPath: "/etc/php/8.4/fpm/php-fpm.conf"},
	})

	msg := err.Error()
	for _, want := range []string{"a php-fpm master is running", "pm.status_path", "fpm-tune enable-status", "www"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not mention %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "no php-fpm master is running") {
		t.Errorf("the message blames a missing master while one is running:\n%s", msg)
	}
	if strings.Contains(msg, "www, www") {
		t.Errorf("a pool reported by two masters is listed twice:\n%s", msg)
	}
}

// capture collects what a function writes to stdout.
func capture(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w

	fn()

	_ = w.Close()
	os.Stdout = saved

	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := r.Read(buf)
		b.Write(buf[:n])
		if rerr != nil {
			break
		}
	}

	return b.String()
}

// TestAStrayWordIsRefusedRatherThanIgnored.
//
// Go's flag package stops parsing at the first non-flag token and silently
// discards everything after it. So `fpm-tune plan -state S www -memory 1G`
// dropped -memory — the one flag whose whole purpose is to stop this tool
// sizing against the wrong number — reported the entire machine's memory, and
// exited 0 with nothing to notice.
//
// A pool name is the obvious thing to type at a pool-sizing tool. On `apply`
// the same slip discards -memory, -reserve, -state and -drop-in-dir, and then
// writes.
func TestAStrayWordIsRefusedRatherThanIgnored(t *testing.T) {
	for _, command := range []string{"plan", "apply", "serve"} {
		t.Run(command, func(t *testing.T) {
			fs := flag.NewFlagSet(command, flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			_ = fs.String("state", "", "")
			memory := fs.String("memory", "", "")

			if err := fs.Parse([]string{"-state", "s.json", "www", "-memory", "1G"}); err != nil {
				t.Fatal(err)
			}

			// The state of affairs the guard exists for: parsing succeeded and
			// -memory was thrown away.
			if *memory != "" {
				t.Skip("the flag package no longer stops at a positional argument")
			}

			err := noPositionalArgs(fs)
			if err == nil {
				t.Fatal("a stray word was accepted, and every flag after it silently ignored")
			}
			if !strings.Contains(err.Error(), "www") {
				t.Errorf("the error does not name the argument that caused it: %v", err)
			}
			if !strings.Contains(err.Error(), "ignored") {
				t.Errorf("the error does not say what happened to the other flags: %v", err)
			}
		})
	}
}

// TestAFailedApplyDoesNotPrintATableOfThingsItDid.
//
// The actions are decided before anything is written, and nothing downgrades
// them when the write, the validation or the reload fails. So a run against a
// read-only pool directory printed
//
//	POOL  ACTION   DETAIL
//	shop  applied  12 to 5
//
// four lines above "Nothing was applied: permission denied" — two contradictory
// statements on one screen, and the tabular half, the one people read first, was
// the wrong one.
func TestAFailedApplyDoesNotPrintATableOfThingsItDid(t *testing.T) {
	res := apply.Result{Outcomes: []apply.Outcome{
		{Pool: "shop", Action: "applied", Reason: "12 to 5"},
	}}

	failed := capture(t, func() {
		renderApplied(res, false, errors.New("permission denied"))
	})
	if strings.Contains(failed, "DONE") {
		t.Errorf("a run that applied nothing headed its table as things done:\n%s", failed)
	}
	if !strings.Contains(failed, "WOULD") {
		t.Errorf("the table does not say these are proposals:\n%s", failed)
	}

	succeeded := capture(t, func() {
		renderApplied(apply.Result{
			Outcomes: res.Outcomes,
			Reloaded: true,
		}, false, nil)
	})
	if !strings.Contains(succeeded, "DONE") {
		t.Errorf("a successful apply does not say so:\n%s", succeeded)
	}
}

// TestAWriteIsRefusedFromABudgetNobodyConfirmed.
//
// The detection falls back to the machine's memory when php-fpm's own limit
// cannot be read, and the two numbers are indistinguishable. A service capped
// at 3GiB on a 32GiB host would be sized against 32GiB and grown into a ceiling
// it never sees.
//
// The message has to name the FILE, because "make it readable" is otherwise a
// direction rather than an instruction — and there are now two files it can be:
// /proc/<pid>/cgroup, and a limit under /sys/fs/cgroup that exists and refuses.
func TestAWriteIsRefusedFromABudgetNobodyConfirmed(t *testing.T) {
	err := refuseUnconfirmedBudget(budget.Limits{
		MemoryBytes: 32 * 1024 * 1024 * 1024,
		Source:      budget.SourceMemInfo,
		LookupErr: fmt.Errorf("cannot read the memory limit at %s: %w",
			"/sys/fs/cgroup/system.slice/php-fpm.service/memory.max", os.ErrPermission),
	})
	if err == nil {
		t.Fatal("a write was allowed from a budget nobody could confirm")
	}
	for _, want := range []string{"memory.max", "--memory", "32.0GiB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so an operator cannot act on it:\n%v",
				want, err)
		}
	}

	// A confirmed budget is not refused, or the tool never writes at all.
	if err := refuseUnconfirmedBudget(budget.Limits{
		MemoryBytes: 3 << 30, Source: budget.SourceCgroupProcess,
	}); err != nil {
		t.Errorf("a budget read straight from php-fpm's own cgroup was refused: %v", err)
	}
}

// TestAFailedSaveAfterASuccessfulApplyIsAnError.
//
// The record carries LastAppliedAt, which is what stops the next run reloading
// a pool this one just reloaded. A failed save after a successful apply is
// therefore a host that will be reloaded again in a minute — and the command
// exited 0, with a warning in a log nobody was watching.
func TestAFailedSaveAfterASuccessfulApplyIsAnError(t *testing.T) {
	err := reportUnsavedApply(os.ErrPermission, "/var/lib/fpm-tune/state.json")
	if err == nil {
		t.Fatal("a failed save after a live apply was reported as success")
	}
	for _, want := range []string{"was applied", "reload them again", "state.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not mention %q, so it reads either as a total "+
				"failure or as nothing at all:\n%v", want, err)
		}
	}

	if err := reportUnsavedApply(nil, "/x"); err != nil {
		t.Errorf("a successful save was reported as an error: %v", err)
	}
}

// TestTheDaemonSaysWhatItIsDoing.
//
// A one-shot command hands back a table, and a warning there is an
// interruption. A daemon prints nothing and runs for weeks: its log IS its
// output, and one that mentions only problems cannot answer "what has this been
// doing", which is the first question anyone asks of a process that has been
// running for a month.
//
// Both used to log at Warn, and the symptom was fixed in the wrong place — the
// line recording a pool being resized was RAISED to Warn so it would appear at
// all, which makes a routine success look like a problem to anything reading
// levels. The level says what kind of thing a message is; the default says how
// much you want to hear.
func TestTheDaemonSaysWhatItIsDoing(t *testing.T) {
	if !newDaemonLogger(false).Enabled(context.Background(), slog.LevelInfo) {
		t.Error("a daemon at its default level says nothing about what it does; the only " +
			"record of a pool being resized is invisible")
	}
	if newLogger(false).Enabled(context.Background(), slog.LevelInfo) {
		t.Error("a one-shot command narrates its way to a result it is about to print")
	}

	// And -verbose reaches Debug for both, which is the flag's whole job.
	for name, l := range map[string]*slog.Logger{
		"daemon": newDaemonLogger(true), "one-shot": newLogger(true),
	} {
		if !l.Enabled(context.Background(), slog.LevelDebug) {
			t.Errorf("-verbose does not reach Debug for the %s logger", name)
		}
	}
}

// TestAnInterruptedReloadIsNotReportedAsDone.
//
// If a one-shot apply is interrupted after the signal but before the settle
// window proves the master survived, Apply returns Inconclusive with no error.
// The switch's res.Reloaded case then printed "Reloaded PHP-FPM" and the command
// exited 0 — telling an operator, and any script, that the master came back when
// nothing proved it did.
//
// The daemon already handles this by reconciling next round; the CLI has to tell
// the same truth in its output and its exit status.
func TestAnInterruptedReloadIsNotReportedAsDone(t *testing.T) {
	res := apply.Result{
		Outcomes:     []apply.Outcome{{Pool: "shop", Action: "applied", Reason: "12 to 8"}},
		Reloaded:     true,
		Wrote:        true,
		Inconclusive: true,
	}

	out := capture(t, func() { renderApplied(res, false, nil) })
	if strings.Contains(out, "Reloaded PHP-FPM") {
		t.Errorf("an interrupted reload was reported as a completed one:\n%s", out)
	}
	if !strings.Contains(out, "interrupted") || !strings.Contains(out, "apply again") {
		t.Errorf("the output does not tell the operator what state they are in or what to "+
			"do:\n%s", out)
	}
}

// TestApplyExitReportsBothTroubleStates.
//
// applyExit is the status a completed apply hands back. Two of its cases are
// the ones a script must not read as done, and neither shows up in the exit
// status by accident: a failed state save after a good apply (the hysteresis
// brake is not on disk), and an unconfirmed reload (the master was signalled and
// the run ended before it was seen to survive).
func TestApplyExitReportsBothTroubleStates(t *testing.T) {
	// Clean apply, clean save: zero.
	if err := applyExit(apply.Result{Reloaded: true}, nil, "/x"); err != nil {
		t.Errorf("a clean apply exited non-zero: %v", err)
	}

	// The save failed. That is the more urgent of the two and is reported first.
	err := applyExit(apply.Result{Reloaded: true, Inconclusive: true}, os.ErrPermission, "/x")
	if err == nil || !strings.Contains(err.Error(), "was applied") {
		t.Errorf("a failed save after a live apply was not reported: %v", err)
	}

	// Save fine, reload unconfirmed: still non-zero, so a driving script does
	// not treat it as finished.
	if err := applyExit(apply.Result{Reloaded: true, Inconclusive: true}, nil, "/x"); err == nil {
		t.Error("an interrupted reload exited zero; a script sees it as done when nothing " +
			"proved the master came back")
	}
}
