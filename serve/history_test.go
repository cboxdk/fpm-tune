package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cboxdk/fpm-tune/allocate"
	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/observe"
	"github.com/cboxdk/fpm-tune/plan"
)

// TestTheHistoryIsARing: the newest rounds survive, oldest first, and ?last
// trims from the old end.
func TestTheHistoryIsARing(t *testing.T) {
	h := newHistory(3, 30*time.Second)
	t0 := time.Unix(1_700_000_000, 0)
	for i := 0; i < 5; i++ {
		h.record(HistorySample{At: t0.Add(time.Duration(i) * 30 * time.Second)})
	}
	for i := 0; i < 3; i++ {
		h.event(HistoryEvent{At: t0.Add(time.Duration(i) * time.Minute), Kind: EventResized, Pool: "www", From: i, To: i + 1})
	}

	rounds, events := h.snapshot(0)
	if len(rounds) != 3 || !rounds[0].At.Equal(t0.Add(60*time.Second)) || !rounds[2].At.Equal(t0.Add(120*time.Second)) {
		t.Errorf("snapshot(0) = %v, want the three newest oldest-first", rounds)
	}
	if len(events) != 3 || events[0].From != 0 || events[2].To != 3 {
		t.Errorf("events = %v", events)
	}
	if rounds, _ := h.snapshot(2); len(rounds) != 2 || !rounds[0].At.Equal(t0.Add(90*time.Second)) {
		t.Errorf("snapshot(2) = %v, want the two newest", rounds)
	}
	if rounds, _ := h.snapshot(10); len(rounds) != 3 {
		t.Errorf("snapshot(10) on a ring of 3 gave %d", len(rounds))
	}
	if newHistory(0, time.Second).samples == nil || len(newHistory(0, time.Second).samples) < 2 {
		t.Error("a ring must hold at least two rounds, or a busy ratio can never be drawn")
	}
}

// TestHistoryJSONIsServed: GET only, ?last honoured, the interval and capacity
// beside the rounds so a client knows how far back it can look.
func TestHistoryJSONIsServed(t *testing.T) {
	h := newHistory(4, 30*time.Second)
	for i := 0; i < 4; i++ {
		h.record(HistorySample{At: time.Unix(int64(i), 0), Pools: []PoolSample{{Pool: "www", Active: i}}})
	}
	h.event(HistoryEvent{Kind: EventResized, Pool: "www", From: 20, To: 10, Detail: "22 to 10"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/history.json?last=2", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, content-type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var body HistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.IntervalSeconds != 30 || body.Capacity != 4 || len(body.Rounds) != 2 || body.Rounds[1].Pools[0].Active != 3 {
		t.Errorf("body = %+v", body)
	}
	if len(body.Events) != 1 || body.Events[0].Kind != EventResized || body.Events[0].To != 10 {
		t.Errorf("events = %+v", body.Events)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/history.json", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST gave %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/history.json?last=x", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("last=x gave %d", rec.Code)
	}
}

// TestASampleCarriesTheRound: what was observed, what was planned, and the CPU
// side, joined by pool name.
func TestASampleCarriesTheRound(t *testing.T) {
	result := plan.Result{
		Views: []observe.PoolView{{Name: "www", ActiveNow: 7, QueueDepth: 3, CurrentMaxChildren: 22}},
		Plan: allocate.Plan{Pools: []allocate.PoolPlan{
			{Name: "www", MaxChildren: 10, WorkerBytes: 35 << 20, DemandUnmet: true, CPUBound: true},
		}},
		CPU: []plan.PoolCPU{{Name: "www", P50: 0.85, Samples: 40, FillWorkers: 5, Ceiling: 10, Limit: "cpu"}},
	}
	s := historySampleOf(result, time.Unix(1, 0), 0.6, true)
	if len(s.Pools) != 1 || !s.HostBusyKnown || s.HostBusyRatio != 0.6 {
		t.Fatalf("sample = %+v", s)
	}
	p := s.Pools[0]
	want := PoolSample{Pool: "www", Active: 7, Queue: 3, Configured: 22, Recommended: 10, DemandUnmet: true,
		WorkerBytes: 35 << 20, CPURatioP50: 0.85, CPUReadings: 40, CPUFill: 5, CPUCeiling: 10, CPULimited: true, CPUBound: true}
	if p != want {
		t.Errorf("pool sample = %+v, want %+v", p, want)
	}
}

// TestHostBusyRatioIsADifference: unknown on the first reading, a fraction of
// the box's millicores after that, clamped at one, and unknown again across a
// hole or a counter that went backwards.
func TestHostBusyRatioIsADifference(t *testing.T) {
	l := &Loop{}
	t0 := time.Unix(1_700_000_000, 0)
	if _, ok := l.hostBusyRatio(budget.HostCPU{BusyMicros: 0, At: t0}, true, 4000); ok {
		t.Error("the first reading gave a ratio")
	}
	// 30 seconds, 60 core-seconds busy on 4 cores: half.
	if r, ok := l.hostBusyRatio(budget.HostCPU{BusyMicros: 60_000_000, At: t0.Add(30 * time.Second)}, true, 4000); !ok || r < 0.49 || r > 0.51 {
		t.Errorf("ratio = %.2f ok=%v, want 0.5", r, ok)
	}
	if r, ok := l.hostBusyRatio(budget.HostCPU{BusyMicros: 60_000_000 + 500_000_000, At: t0.Add(60 * time.Second)}, true, 4000); !ok || r != 1 {
		t.Errorf("more busy than the box has came out as %.2f ok=%v, want clamped to 1", r, ok)
	}
	if _, ok := l.hostBusyRatio(budget.HostCPU{BusyMicros: 900_000_000, At: t0.Add(20 * time.Minute)}, true, 4000); ok {
		t.Error("a twenty-minute hole gave a ratio")
	}
	if _, ok := l.hostBusyRatio(budget.HostCPU{BusyMicros: 1_000, At: t0.Add(20*time.Minute + 30*time.Second)}, true, 4000); ok {
		t.Error("a counter that went backwards gave a ratio")
	}
	if _, ok := l.hostBusyRatio(budget.HostCPU{}, false, 4000); ok {
		t.Error("an unreadable box gave a ratio")
	}
}
