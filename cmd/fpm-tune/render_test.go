package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cboxdk/fpm-tune/apply"
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
