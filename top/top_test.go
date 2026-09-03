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
	// Fewer values than columns: stretched to fill, so the chart is never a
	// stub at the right of an empty axis.
	if got := string(spark([]float64{8}, 3, 8)); got != "███" {
		t.Errorf("spark of one value = %q", got)
	}
	if got := string(spark([]float64{0, 8}, 4, 8)); got != "▁▁██" {
		t.Errorf("spark of two values over four columns = %q", got)
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
			"8.0GiB memory", "4 core(s)", "held", "85%", "5/10", "span"} {
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
	from, to, rounds := m.window()
	// Twenty minutes of data on an hour's span: the axis is the twenty
	// minutes, from the oldest round, with no empty hour to its left.
	if m.span != 0 || to.Sub(from) != 39*30*time.Second || len(rounds) != 40 {
		t.Errorf("after 1: span %d, axis %s, %d rounds", m.span, to.Sub(from), len(rounds))
	}
	press("3")
	from, to, rounds = m.window()
	if m.span != 2 || len(rounds) != 40 || to.Sub(from) < 19*time.Minute {
		t.Errorf("after 3: span %d, axis %s, %d rounds", m.span, to.Sub(from), len(rounds))
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

// TestApplyFromTheViewIsTwoKeysAndTheDaemonsOwnFlags: a opens the panel
// with the plan's changes and the command it would run, Esc closes it, and
// in apply mode a is refused with a notice rather than a second writer
// racing the daemon.
func TestApplyFromTheViewIsTwoKeysAndTheDaemonsOwnFlags(t *testing.T) {
	resp := fixture()
	resp.Host.Apply = false
	resp.Host.CPUHeadroom = 1.5
	for i := range resp.Rounds[len(resp.Rounds)-1].Pools {
		if resp.Rounds[len(resp.Rounds)-1].Pools[i].Pool == "www-forge" {
			resp.Rounds[len(resp.Rounds)-1].Pools[i].Configured = 22
		}
	}
	m := newModel(Options{Addr: "x", Refresh: time.Second})
	next, _ := m.Update(fetchedMsg{resp: resp, at: time.Now()})
	m = next.(model)
	m.width, m.height = 120, 40
	press := func(k tea.KeyMsg) {
		var mm tea.Model
		mm, _ = m.Update(k)
		m = mm.(model)
	}
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if !m.confirm {
		t.Fatal("a did not open the apply panel")
	}
	out := m.View()
	for _, want := range []string{"APPLY THE PLAN?", "www-forge", "22 → 10", "apply --cpu --cpu-headroom 1.5"} {
		if !strings.Contains(out, want) {
			t.Errorf("panel lacks %q:\n%s", want, out)
		}
	}
	if pending := m.pending(); len(pending) != 1 || pending[0].Pool != "www-forge" {
		t.Errorf("pending = %+v, want www-forge only (www is at its plan)", pending)
	}
	press(tea.KeyMsg{Type: tea.KeyEsc})
	if m.confirm {
		t.Error("Esc did not close the panel")
	}

	args := applyArgs(serve.HostInfo{CPUCeiling: true, CPUHeadroom: 2}, "/usr/local/bin/fpm-tune")
	if strings.Join(args, " ") != "sudo /usr/local/bin/fpm-tune apply --cpu --cpu-headroom 2" {
		t.Errorf("args = %v", args)
	}
	if args := applyArgs(serve.HostInfo{}, "fpm-tune"); strings.Join(args, " ") != "sudo fpm-tune apply" {
		t.Errorf("args without cpu = %v", args)
	}

	// In apply mode the daemon holds the pool directory and applies on its
	// own; a from the view says so instead of racing it.
	resp.Host.Apply = true
	next, _ = m.Update(fetchedMsg{resp: resp, at: time.Now()})
	m = next.(model)
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if m.confirm || !strings.Contains(m.notice, "apply mode") {
		t.Errorf("in apply mode: confirm=%v notice=%q", m.confirm, m.notice)
	}
}
