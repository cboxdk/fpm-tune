package plan

import (
	"strings"
	"testing"
)

const mib = 1 << 20

// TestAdviseMode covers the two signals it acts on and, as importantly, the cases
// it must stay quiet on — a false "switch your mode" is worse than silence,
// because acting on it churns a production pool for nothing.
func TestAdviseMode(t *testing.T) {
	tests := []struct {
		name    string
		in      adviceInput
		wantOK  bool
		wantTo  string
		wantSub string // a substring the reason must contain, when one fires
	}{
		{
			name: "static holding many idle workers is worth reclaiming",
			in: adviceInput{
				Pool: "shop", Mode: "static", Current: 30, Peak: 8,
				MaxKnown: true, Measured: true, WorkerBytes: 100 * mib,
			},
			wantOK: true, wantTo: "dynamic", wantSub: "22 sit idle",
		},
		{
			name: "static running near its ceiling is left alone",
			in: adviceInput{
				Pool: "shop", Mode: "static", Current: 12, Peak: 11,
				MaxKnown: true, Measured: true, WorkerBytes: 100 * mib,
			},
			wantOK: false,
		},
		{
			name: "static whose idle memory is trivial is not worth a nag",
			in: adviceInput{
				Pool: "tiny", Mode: "static", Current: 6, Peak: 2,
				MaxKnown: true, Measured: true, WorkerBytes: 10 * mib, // 4 idle × 10MiB = 40MiB < floor
			},
			wantOK: false,
		},
		{
			name: "static on a bootstrap estimate says nothing — the arithmetic would be a guess",
			in: adviceInput{
				Pool: "shop", Mode: "static", Current: 30, Peak: 8,
				MaxKnown: true, Measured: false, WorkerBytes: 100 * mib,
			},
			wantOK: false,
		},
		{
			name: "static whose ceiling could not be read says nothing",
			in: adviceInput{
				Pool: "shop", Mode: "static", Current: 30, Peak: 8,
				MaxKnown: false, Measured: true, WorkerBytes: 100 * mib,
			},
			wantOK: false,
		},
		{
			name: "static never seen working says nothing",
			in: adviceInput{
				Pool: "shop", Mode: "static", Current: 30, Peak: 0,
				MaxKnown: true, Measured: true, WorkerBytes: 100 * mib,
			},
			wantOK: false,
		},
		{
			name: "ondemand with a live queue is a cold-start problem",
			in: adviceInput{
				Pool: "media", Mode: "ondemand", Current: 20, Peak: 20,
				MaxKnown: true, Measured: true, WorkerBytes: 100 * mib, Queue: 7,
			},
			wantOK: true, wantTo: "dynamic", wantSub: "7 waiting",
		},
		{
			name: "ondemand that could not be fully served, even without a queue right now",
			in: adviceInput{
				Pool: "media", Mode: "ondemand", Current: 20, Peak: 20,
				MaxKnown: true, Measured: true, WorkerBytes: 100 * mib, DemandUnmet: true,
			},
			wantOK: true, wantTo: "dynamic", wantSub: "cold start",
		},
		{
			name: "ondemand that is keeping up is left alone",
			in: adviceInput{
				Pool: "media", Mode: "ondemand", Current: 20, Peak: 4,
				MaxKnown: true, Measured: true, WorkerBytes: 100 * mib,
			},
			wantOK: false,
		},
		{
			name: "dynamic is never pushed anywhere — no sustained-load signal to justify it",
			in: adviceInput{
				Pool: "api", Mode: "dynamic", Current: 30, Peak: 2,
				MaxKnown: true, Measured: true, WorkerBytes: 100 * mib, Queue: 5, DemandUnmet: true,
			},
			wantOK: false,
		},
		{
			name: "an unknown mode string is not second-guessed",
			in: adviceInput{
				Pool: "weird", Mode: "", Current: 30, Peak: 2,
				MaxKnown: true, Measured: true, WorkerBytes: 100 * mib,
			},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := adviseMode(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("adviseMode ok = %v, want %v (advice %+v)", ok, tt.wantOK, got)
			}
			if !ok {
				return
			}
			if got.From != tt.in.Mode {
				t.Errorf("From = %q, want the current mode %q", got.From, tt.in.Mode)
			}
			if got.To != tt.wantTo {
				t.Errorf("To = %q, want %q", got.To, tt.wantTo)
			}
			if got.Pool != tt.in.Pool {
				t.Errorf("Pool = %q, want %q", got.Pool, tt.in.Pool)
			}
			if tt.wantSub != "" && !strings.Contains(got.Why, tt.wantSub) {
				t.Errorf("Why = %q, want it to contain %q", got.Why, tt.wantSub)
			}
		})
	}
}
