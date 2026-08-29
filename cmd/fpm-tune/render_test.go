package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/cboxdk/fpm-tune/apply"
	"github.com/cboxdk/fpm-tune/budget"
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
	err := noPoolsError()

	msg := err.Error()
	if !strings.Contains(msg, "systemctl status php-fpm") && !strings.Contains(msg, "pgrep") {
		t.Errorf("the message does not suggest checking whether php-fpm is running:\n%s", msg)
	}
	if root := strings.Index(msg, "root"); root >= 0 && root < strings.Index(msg, "running") {
		t.Errorf("permissions are blamed before the process is even looked for:\n%s", msg)
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
