package serve

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/plan"
)

func newLogLoop(t *testing.T, heartbeat time.Duration) (*Loop, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	l := &Loop{
		cfg: Config{HeartbeatEvery: heartbeat},
		log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}

	return l, &buf
}

func onePool(now int) plan.Result {
	return plan.Result{Plan: allocate.Plan{Pools: []allocate.PoolPlan{
		{Name: "web", Current: 10, MaxChildren: now},
	}}}
}

// TestLogPlanReportsOnChangeNotEveryRound: a watching operator wants to see the
// recommendation, but a line every round — every 30s, for the life of a host —
// trains people to stop reading the log. With the heartbeat off, it reports on
// first sight and on change, and stays quiet while the recommendation holds.
func TestLogPlanReportsOnChangeNotEveryRound(t *testing.T) {
	l, buf := newLogLoop(t, 0) // heartbeat disabled
	base := time.Unix(1_700_000_000, 0)

	l.logPlan(onePool(10), base) // first sight -> logged
	l.logPlan(onePool(10), base) // unchanged   -> quiet
	l.logPlan(onePool(12), base) // changed     -> logged
	l.logPlan(onePool(12), base) // unchanged   -> quiet

	if got := strings.Count(buf.String(), "Pool recommendation"); got != 2 {
		t.Errorf("logged %d recommendations, want 2 (first sight + the one change):\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "now=10") || !strings.Contains(buf.String(), "recommend=12") {
		t.Errorf("the log did not carry the now/recommend the operator wants:\n%s", buf.String())
	}
}

// TestLogPlanHeartbeatReLogsAnUnchangedRecommendation: with the heartbeat on, an
// unchanged recommendation is re-logged after the interval — a sign of life — but
// not every round in between, so the log has a pulse without becoming noise.
func TestLogPlanHeartbeatReLogsAnUnchangedRecommendation(t *testing.T) {
	l, buf := newLogLoop(t, 10*time.Minute)
	base := time.Unix(1_700_000_000, 0)

	l.logPlan(onePool(10), base)                     // first sight -> logged
	l.logPlan(onePool(10), base.Add(5*time.Minute))  // within the interval -> quiet
	l.logPlan(onePool(10), base.Add(11*time.Minute)) // past it -> a sign of life

	if got := strings.Count(buf.String(), "Pool recommendation"); got != 2 {
		t.Errorf("logged %d times, want 2 (first sight + one heartbeat), not every round:\n%s", got, buf.String())
	}
}

// TestLogPlanSkipsAnUnreadablePool: a pool whose current configuration could not be
// read has no meaningful "now", and its plan is not something to act on.
func TestLogPlanSkipsAnUnreadablePool(t *testing.T) {
	l, buf := newLogLoop(t, 0)

	l.logPlan(plan.Result{Plan: allocate.Plan{Pools: []allocate.PoolPlan{
		{Name: "web", Current: 0, MaxChildren: 8, Unknown: true},
	}}}, time.Unix(1_700_000_000, 0))

	if strings.Contains(buf.String(), "Pool recommendation") {
		t.Errorf("a pool whose configuration could not be read was logged:\n%s", buf.String())
	}
}
