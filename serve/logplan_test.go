package serve

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/plan"
)

// TestLogPlanReportsOnChangeNotEveryRound: a watching operator wants to see the
// recommendation, but a line every round — every 30s, for the life of a host —
// trains people to stop reading the log. So it reports on first sight and on change,
// and stays quiet while the recommendation holds.
func TestLogPlanReportsOnChangeNotEveryRound(t *testing.T) {
	var buf bytes.Buffer
	l := &Loop{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))}

	res := func(n int) plan.Result {
		return plan.Result{Plan: allocate.Plan{Pools: []allocate.PoolPlan{
			{Name: "web", Current: 10, MaxChildren: n},
		}}}
	}

	l.logPlan(res(10)) // first sight -> logged
	l.logPlan(res(10)) // unchanged   -> quiet
	l.logPlan(res(12)) // changed     -> logged
	l.logPlan(res(12)) // unchanged   -> quiet

	if got := strings.Count(buf.String(), "Pool recommendation"); got != 2 {
		t.Errorf("logged %d recommendations, want 2 (first sight + the one change):\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "now=10") || !strings.Contains(buf.String(), "recommend=12") {
		t.Errorf("the log did not carry the now/recommend the operator wants:\n%s", buf.String())
	}
}

// TestLogPlanSkipsAnUnreadablePool: a pool whose current configuration could not be
// read has no meaningful "now", and its plan is not something to act on.
func TestLogPlanSkipsAnUnreadablePool(t *testing.T) {
	var buf bytes.Buffer
	l := &Loop{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))}

	l.logPlan(plan.Result{Plan: allocate.Plan{Pools: []allocate.PoolPlan{
		{Name: "web", Current: 0, MaxChildren: 8, Unknown: true},
	}}})

	if strings.Contains(buf.String(), "Pool recommendation") {
		t.Errorf("a pool whose configuration could not be read was logged:\n%s", buf.String())
	}
}
