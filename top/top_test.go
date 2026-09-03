package top

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cboxdk/fpm-tune/serve"
)

// TestSparkKeepsSpikesAndHoles: a column is the maximum of its bucket, so a
// spike is never averaged away; a bucket with nothing known is a dot, not a
// zero; newest is at the right.
func TestSparkKeepsSpikesAndHoles(t *testing.T) {
	series := []float64{0, 0, 8, 0, -1, -1, 4, 8}
	got := string(spark(series, 4, 8))
	// Buckets of two: [0,0] [8,0] [-1,-1] [4,8] → ▁ █ · █
	if got != "▁█·█" {
		t.Errorf("spark = %q, want ▁█·█", got)
	}
	// Fewer values than columns: left-padded with blanks, newest at the right.
	if got := string(spark([]float64{8}, 3, 8)); got != "  █" {
		t.Errorf("spark of one value = %q", got)
	}
	// Above scale clips at the top rather than indexing past the runes.
	if got := string(spark([]float64{40}, 1, 8)); got != "█" {
		t.Errorf("clipped = %q", got)
	}
	if got := string(spark(nil, 3, 8)); got != "   " {
		t.Errorf("empty = %q", got)
	}
}

func fixture() *serve.HistoryResponse {
	t0 := time.Unix(1_700_000_000, 0)
	resp := &serve.HistoryResponse{
		IntervalSeconds: 30, Capacity: 2880,
		Host: serve.HostInfo{Hostname: "cbox-web", Version: "0.1.0-beta.22", Apply: true, CPUCeiling: true,
			MemoryBytes: 8 << 30, CPUMillicores: 4000, Source: "/proc/meminfo"},
	}
	for i := 0; i < 40; i++ {
		resp.Rounds = append(resp.Rounds, serve.HistorySample{
			At: t0.Add(time.Duration(i) * 30 * time.Second), HostBusyRatio: float64(i%10) / 10, HostBusyKnown: i > 0,
			Pools: []serve.PoolSample{
				{Pool: "www-forge", Active: i % 8, Queue: int64(i % 3), Configured: 10, Recommended: 10, WorkerBytes: 35 << 20,
					CPURatioP50: 0.85, CPUReadings: 400, CPUFill: 5, CPUCeiling: 10, CPULimited: true, CPUBound: true},
				{Pool: "www", Active: 1, Configured: 20, Recommended: 20, WorkerBytes: 48 << 20},
			},
		})
	}
	resp.Events = []serve.HistoryEvent{
		{At: t0.Add(5 * time.Minute), Kind: serve.EventResized, Pool: "www-forge", From: 22, To: 10, Detail: "22 to 10"},
		{At: t0.Add(9 * time.Minute), Kind: serve.EventApplyFailed, Detail: "php-fpm -t rejected the drop-in"},
	}

	return resp
}

// TestTheViewShowsThePoolsAndTheEvents: a smoke test over the whole layout
// with a day-shaped fixture, at a narrow and a wide terminal.
func TestTheViewShowsThePoolsAndTheEvents(t *testing.T) {
	for _, width := range []int{80, 140} {
		m := newModel(Options{Addr: "127.0.0.1:9110", Refresh: 5 * time.Second})
		m.width, m.height = width, 40
		next, _ := m.Update(fetchedMsg{resp: fixture(), at: time.Now()})
		m = next.(model)
		out := m.View()
		for _, want := range []string{"cbox-web", "apply", "cpu ceiling on", "www-forge", "www", "22 → 10", "apply failed",
			"8.0GiB memory", "4 core(s)", "held", "85%", "5/10", "last "} {
			if !strings.Contains(out, want) {
				t.Errorf("width %d: view lacks %q:\n%s", width, want, out)
			}
		}
		if strings.Contains(out, "\t") {
			t.Errorf("width %d: a tab reached the terminal", width)
		}
	}
}

// TestKeysMoveTheCursorAndTheWindow: the cursor stays inside the pool list,
// the number keys change how many rounds the charts span, q quits.
func TestKeysMoveTheCursorAndTheWindow(t *testing.T) {
	m := newModel(Options{Addr: "x", Refresh: time.Second})
	next, _ := m.Update(fetchedMsg{resp: fixture(), at: time.Now()})
	m = next.(model)
	if len(m.pools) != 2 || m.pools[0] != "www" {
		t.Fatalf("pools = %v", m.pools)
	}
	press := func(k string) {
		var mm tea.Model
		mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		m = mm.(model)
	}
	press("j")
	press("j")
	if m.selected != 1 {
		t.Errorf("selected = %d after two downs on two pools, want 1", m.selected)
	}
	press("k")
	press("k")
	if m.selected != 0 {
		t.Errorf("selected = %d, want 0", m.selected)
	}
	press("1")
	if m.window != 60 || len(m.rounds()) != 40 {
		t.Errorf("window = %d rounds shown %d", m.window, len(m.rounds()))
	}
	press("3")
	if m.window != 0 {
		t.Errorf("window = %d after 3, want 0 (all)", m.window)
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Error("q did not quit")
	}
}

// TestFetchHistoryTalksToTheDaemon: a good answer decodes, a wrong status is
// an error that names the address.
func TestFetchHistoryTalksToTheDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/history.json" {
			http.NotFound(w, r)

			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"interval_seconds":30,"capacity":10,"host":{"hostname":"h"},"rounds":[],"events":[]}`))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	resp, err := fetchHistory(srv.Client(), addr)
	if err != nil || resp.Host.Hostname != "h" || resp.Capacity != 10 {
		t.Errorf("fetch = %+v, %v", resp, err)
	}

	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	if _, err := fetchHistory(old.Client(), strings.TrimPrefix(old.URL, "http://")); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("an old daemon without the endpoint gave %v", err)
	}
}
