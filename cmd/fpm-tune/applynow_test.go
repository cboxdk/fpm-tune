package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cboxdk/fpm-tune/serve"
)

// TestDescribeOutcomeIsWhatAPersonReads: each resize is one line of pool,
// from → to and the plan's reason; a round that changed nothing prints its
// message; and a failure prints nothing here, because it goes back as the
// error and main prints that once — twice would be the same words stacked.
func TestDescribeOutcomeIsWhatAPersonReads(t *testing.T) {
	cases := []struct {
		name string
		out  serve.ApplyOutcome
		want string
	}{
		{
			name: "two resizes",
			out: serve.ApplyOutcome{Changed: []serve.HistoryEvent{
				{Kind: serve.EventResized, Pool: "www", From: 22, To: 10, Detail: "22 to 10"},
				{Kind: serve.EventResized, Pool: "shop", From: 8, To: 12, Detail: "8 to 12"},
			}},
			want: "www  22 → 10  22 to 10\nshop  8 → 12  8 to 12\n",
		},
		{
			name: "nothing changed",
			out:  serve.ApplyOutcome{Message: "nothing was applied: every pool is at its plan"},
			want: "nothing was applied: every pool is at its plan\n",
		},
		{
			name: "a failure",
			out:  serve.ApplyOutcome{Error: "cannot take the pool-directory lock"},
			want: "",
		},
		{
			name: "a failure beside a message",
			out:  serve.ApplyOutcome{Message: "nothing was applied", Error: "the host could not be written"},
			want: "",
		},
	}
	for _, c := range cases {
		if got := describeOutcome(c.out); got != c.want {
			t.Errorf("%s: describeOutcome = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestApplyNowRefusesWhatItCannotMean: an unknown flag and a positional
// argument are errors before the socket is touched, and a daemon that is not
// there is an error that names the socket it looked for, so the operator
// knows which daemon (or which --control) was meant.
func TestApplyNowRefusesWhatItCannotMean(t *testing.T) {
	// The flag package prints usage on a parse error; keep it out of the
	// test's output.
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devnull.Close() }()
	stderr := os.Stderr
	os.Stderr = devnull
	defer func() { os.Stderr = stderr }()

	if err := runApplyNow([]string{"--no-such-flag"}); err == nil {
		t.Error("an unknown flag was accepted")
	}
	if err := runApplyNow([]string{"www"}); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("a positional argument gave %v", err)
	}

	missing := filepath.Join(t.TempDir(), "nobody.sock")
	err = runApplyNow([]string{"--control", missing})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("a missing daemon gave %v, want an error naming %s", err, missing)
	}
}
