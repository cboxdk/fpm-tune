package top

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

// loaded is a model at a terminal size with a response already fetched.
func loaded(t *testing.T, resp *serve.HistoryResponse, width, height int) model {
	t.Helper()
	m := newModel(Options{Addr: "127.0.0.1:9110", Refresh: 5 * time.Second})
	m.width, m.height = width, height
	next, _ := m.Update(fetchedMsg{resp: resp, at: time.Now()})

	return next.(model)
}

// widest is the widest rendered line, ANSI stripped.
func widest(out string) int {
	w := 0
	for _, line := range strings.Split(out, "\n") {
		w = max(w, lipgloss.Width(line))
	}

	return w
}

// TestTheViewShowsThePoolsAndTheEvents: a smoke test over the whole layout
// with a day-shaped fixture, at a narrow and a wide terminal. The columns
// are named after what they are, not the daemon's field names, and the
// resize event does not say "22 to 10" twice.
func TestTheViewShowsThePoolsAndTheEvents(t *testing.T) {
	for _, width := range []int{80, 140} {
		m := loaded(t, fixture(), width, 40)
		m.selected = 1 // www-forge, the pool the CPU side holds
		out := m.View()
		for _, want := range []string{"cbox-web", "apply", "cpu ceiling on", "www-forge", "www", "22 → 10", "apply failed",
			"8.0GiB memory", "4 core(s)", "cpu (held)", "85%", "CPU/REQ", "CPU MAX", "BOUND BY", "held at the CPU max",
			"showing 20m (all)", "resized"} {
			if !strings.Contains(out, want) {
				t.Errorf("width %d: view lacks %q:\n%s", width, want, out)
			}
		}
		for _, stale := range []string{"\t", "22 to 10", "FILL/CAP", "LIMIT"} {
			if strings.Contains(out, stale) {
				t.Errorf("width %d: view still has %q:\n%s", width, stale, out)
			}
		}
		if w := widest(out); w > width {
			t.Errorf("width %d: a line is %d wide:\n%s", width, w, out)
		}
	}
}

// TestEmptyHistoryDoesNotPanic: a daemon in its first interval publishes a
// host and no rounds; the pool panels say so instead of indexing past the
// end.
func TestEmptyHistoryDoesNotPanic(t *testing.T) {
	resp := fixture()
	resp.Rounds = nil
	m := loaded(t, resp, 100, 30)
	out := m.View()
	if !strings.Contains(out, "first round") {
		t.Errorf("view with no rounds lacks the waiting line:\n%s", out)
	}
	if !strings.Contains(out, "no rounds yet") {
		t.Errorf("title with no rounds does not say so:\n%s", out)
	}
	// The panels answer on their own too, since the layout may call them.
	if !strings.Contains(m.poolsPanel(90, 0), "first round") || !strings.Contains(m.hostPanel(90, 4), "HOST") {
		t.Error("the panels did not cope with no rounds")
	}
	m.pools = []string{"www"}
	if !strings.Contains(m.detailPanel(90, 6), "first round") {
		t.Error("the detail panel did not cope with no rounds")
	}
}

// TestTheTitleGivesWayBeforeWrapping: JoinVertical pads every line to the
// widest, so a title bar past the terminal wraps the whole view. The right
// side drops its pieces in order, the version goes last, and a stale
// message or a long notice is cut to the width.
func TestTheTitleGivesWayBeforeWrapping(t *testing.T) {
	m := loaded(t, fixture(), 80, 40)
	m.err = errors.New("Get \"http://127.0.0.1:9110/history.json\": dial tcp 127.0.0.1:9110: connect: connection refused")
	m.notice = sWarn.Render(strings.Repeat("a long notice ", 12))
	out := m.View()
	if w := widest(out); w > 80 {
		t.Errorf("a line is %d wide at 80 columns:\n%s", w, out)
	}
	if !strings.Contains(out, "stale: Get") {
		t.Errorf("the stale message went entirely:\n%s", out)
	}
	m.resp.Host.Hostname = "a-very-long-hostname.internal.example.org"
	if out := m.View(); widest(out) > 80 || !strings.Contains(out, "apply") {
		t.Errorf("a long hostname at 80 columns:\n%s", out)
	}

	// At a comfortable width every piece is there. Narrower, the refresh
	// rate goes first, then the version, then the time of the fetch; what
	// the charts show stays, whole.
	m = loaded(t, fixture(), 140, 40)
	wide := m.titleBar(138)
	for _, want := range []string{"every 5s", "v0.1.0-beta.22", "updated", "showing 20m (all)"} {
		if !strings.Contains(wide, want) {
			t.Errorf("wide title lacks %q: %s", want, wide)
		}
	}
	mid := m.titleBar(105)
	if strings.Contains(mid, "every 5s") || !strings.Contains(mid, "updated") || !strings.Contains(mid, "v0.1.0") || lipgloss.Width(mid) > 105 {
		t.Errorf("title at 105 = %q (%d wide)", mid, lipgloss.Width(mid))
	}
	narrow := m.titleBar(70)
	if strings.Contains(narrow, "updated") || strings.Contains(narrow, "v0.1.0") || !strings.Contains(narrow, "showing 20m (all)") ||
		lipgloss.Width(narrow) > 70 {
		t.Errorf("title at 70 = %q (%d wide)", narrow, lipgloss.Width(narrow))
	}
}

// TestAShortTerminalKeepsTheTitle: at 80x24 the view fits, with the fixture
// and with a dozen pools, and the title and the selected pool are on it.
func TestAShortTerminalKeepsTheTitle(t *testing.T) {
	many := fixture()
	for i := range many.Rounds {
		for j := 0; j < 10; j++ {
			many.Rounds[i].Pools = append(many.Rounds[i].Pools, serve.PoolSample{
				Pool: fmt.Sprintf("site-%02d", j), Active: 1, Configured: 5, Recommended: 5})
		}
	}
	for _, tc := range []struct {
		name     string
		resp     *serve.HistoryResponse
		selected int
	}{
		{"two pools", fixture(), 1},
		{"twelve pools, last selected", many, 11},
		{"twelve pools, first selected", many, 0},
	} {
		m := loaded(t, tc.resp, 80, 24)
		m.selected = tc.selected
		out := m.View()
		lines := strings.Count(out, "\n") + 1
		if lines > 24 {
			t.Errorf("%s: %d lines at 24 rows:\n%s", tc.name, lines, out)
		}
		if !strings.Contains(out, "fpm-tune top") || !strings.Contains(out, m.pools[tc.selected]) {
			t.Errorf("%s: the title or the selected pool %q is missing:\n%s", tc.name, m.pools[tc.selected], out)
		}
		if w := widest(out); w > 80 {
			t.Errorf("%s: a line is %d wide:\n%s", tc.name, w, out)
		}
		// A chart squeezed under three rows has no graph and labels it NaN.
		if strings.Contains(out, "NaN") {
			t.Errorf("%s: a chart with no rows was drawn:\n%s", tc.name, out)
		}
	}
	// A dozen pools at 24 rows are a window with a count of the rest.
	m := loaded(t, many, 80, 24)
	if out := m.View(); !strings.Contains(out, "more") {
		t.Errorf("twelve pools at 24 rows show no \"… more\" line:\n%s", out)
	}
	// The tall layout is untouched.
	m = loaded(t, fixture(), 120, 60)
	if l := m.layout(); l.hostChart != 7 || l.poolChart != 12 || !l.detail || l.poolRows != 2 {
		t.Errorf("tall layout = %+v", l)
	}
	// The window follows the cursor.
	if s, e := rowWindow(12, 11, 8); s != 4 || e != 12 {
		t.Errorf("window around the last of 12 = %d..%d", s, e)
	}
	if s, e := rowWindow(12, 0, 8); s != 0 || e != 8 {
		t.Errorf("window around the first of 12 = %d..%d", s, e)
	}
	if s, e := rowWindow(3, 2, 8); s != 0 || e != 3 {
		t.Errorf("window over 3 rows = %d..%d", s, e)
	}
}

// TestShortChartsStillHaveAScale: the y axis is labelled at both ends
// whatever the height, so a four-row host chart reads 0% and 100% rather
// than 0% alone.
func TestShortChartsStillHaveAScale(t *testing.T) {
	m := loaded(t, fixture(), 100, 30)
	for h := 4; h <= 9; h++ {
		out := m.hostPanel(90, h)
		if !strings.Contains(out, "0%") || !strings.Contains(out, "100%") {
			t.Errorf("host chart at height %d lacks a scale:\n%s", h, out)
		}
	}
	if got := ySteps(4); got != 2 {
		t.Errorf("ySteps(4) = %d, want 2 (rows 0 and 2 of 2)", got)
	}
	if got := ySteps(7); got != 5 {
		t.Errorf("ySteps(7) = %d, want 5 (the ends of 5 rows)", got)
	}
}

// TestKeysMoveTheCursorAndTheWindow: the cursor stays inside the pool list,
// the number keys change how many rounds the charts span, q quits and Esc
// does not.
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
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil {
		t.Error("Esc outside the apply panel did something")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Error("q did not quit")
	}

	// The keys bar says what the keys do, in the words the badge uses, and
	// while the panel is open only the two keys that close it.
	bar := m.keys(100)
	for _, want := range []string{"↑↓/tab", "1h", "6h", "all", "quit"} {
		if !strings.Contains(bar, want) {
			t.Errorf("keys bar lacks %q: %s", want, bar)
		}
	}
	m.confirm = true
	bar = m.keys(100)
	if !strings.Contains(bar, "Enter") || !strings.Contains(bar, "cancel") || strings.Contains(bar, "quit") {
		t.Errorf("keys bar with the panel open = %s", bar)
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
// a view reading another box's daemon is refused with a notice rather than
// a second writer racing the daemon.
func TestApplyFromTheViewIsTwoKeysAndTheDaemonsOwnFlags(t *testing.T) {
	resp := fixture()
	resp.Host.Apply = false
	resp.Host.CPUHeadroom = 1.5
	for i := range resp.Rounds[len(resp.Rounds)-1].Pools {
		if resp.Rounds[len(resp.Rounds)-1].Pools[i].Pool == "www-forge" {
			resp.Rounds[len(resp.Rounds)-1].Pools[i].Configured = 22
		}
	}
	m := loaded(t, resp, 120, 40)
	press := func(k tea.KeyMsg) {
		var mm tea.Model
		mm, _ = m.Update(k)
		m = mm.(model)
	}
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if !m.confirm {
		t.Fatal("a did not open the apply panel")
	}

	// Another box's history: the panel does not open, because apply-now
	// would reach the daemon on THIS box and apply its plan, not the one
	// on the screen. The address decides, not the hostname: the daemon's
	// hostname is wrong both ways in containers and cloned VMs.
	away := m
	away.confirm = false
	away.opts.Addr = "10.0.0.5:9110"
	next, _ := away.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	away = next.(model)
	if away.confirm || !strings.Contains(away.notice, "10.0.0.5:9110") {
		t.Errorf("a on another box's history: confirm=%v notice=%q", away.confirm, away.notice)
	}
	out := m.View()
	for _, want := range []string{"APPLY THE PLAN?", "www-forge", "22 → 10", "apply-now", "Enter", "Esc", "held at the CPU max"} {
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

	// The daemon does the applying: the view only asks it to, through the
	// control socket, with no flags of its own to get wrong.
	if args := applyArgs("/usr/local/bin/fpm-tune", false); strings.Join(args, " ") != "sudo /usr/local/bin/fpm-tune apply-now" {
		t.Errorf("args = %v", args)
	}
	if args := applyArgs("/usr/local/bin/fpm-tune", true); strings.Join(args, " ") != "/usr/local/bin/fpm-tune apply-now" {
		t.Errorf("root args = %v", args)
	}

	// In apply mode the panel says the daemon would get there on its own,
	// and still offers the immediate apply.
	resp.Host.Apply = true
	next, _ = m.Update(fetchedMsg{resp: resp, at: time.Now()})
	m = next.(model)
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if !m.confirm || !strings.Contains(m.View(), "apply mode") {
		t.Errorf("in apply mode: confirm=%v", m.confirm)
	}
}

// TestLocalAddrIsLoopbackOrNothing: the addresses the view can read from
// and still offer apply-now.
func TestLocalAddrIsLoopbackOrNothing(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:9110": true, "localhost:9110": true, ":9110": true, "[::1]:9110": true, "127.0.0.1": true, "": true,
		"10.0.0.5:9110": false, "cbox-web:9110": false, "[2001:db8::1]:9110": false, "0.0.0.0:9110": false,
	} {
		if got := localAddr(addr); got != want {
			t.Errorf("localAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

// TestApplyOutcomeIsTheCommandsOwnWords: the notice shows the first line
// the command wrote, or the exec error when it wrote nothing.
func TestApplyOutcomeIsTheCommandsOwnWords(t *testing.T) {
	failed := errors.New("exit status 1")
	if err := applyOutcome(failed, "fpm-tune: the daemon is not running\nrun fpm-tune install-service first\n"); err == nil ||
		err.Error() != "fpm-tune: the daemon is not running" {
		t.Errorf("outcome with stderr = %v", err)
	}
	if err := applyOutcome(failed, "  \n"); !errors.Is(err, failed) {
		t.Errorf("outcome with empty stderr = %v, want the exec error", err)
	}
	if err := applyOutcome(nil, "a warning nobody needs"); err != nil {
		t.Errorf("outcome of a success = %v", err)
	}
	m := loaded(t, fixture(), 100, 30)
	next, _ := m.Update(appliedMsg{err: errors.New("the daemon is not running")})
	if notice := next.(model).notice; !strings.Contains(notice, "apply failed: the daemon is not running") {
		t.Errorf("notice = %q", notice)
	}
}

// TestExecutablePathSurvivesAnUpgrade: Linux reports the running binary as
// "(deleted)" once it has been replaced on disk; the new one at that path
// is what apply-now should run, and the name alone when nothing is there.
func TestExecutablePathSurvivesAnUpgrade(t *testing.T) {
	exists := func(want string) func(string) (os.FileInfo, error) {
		return func(p string) (os.FileInfo, error) {
			if p == want {
				return nil, nil
			}

			return nil, os.ErrNotExist
		}
	}
	if got := executablePath("/usr/local/bin/fpm-tune (deleted)", exists("/usr/local/bin/fpm-tune")); got != "/usr/local/bin/fpm-tune" {
		t.Errorf("after an upgrade = %q", got)
	}
	if got := executablePath("/usr/local/bin/fpm-tune", exists("/usr/local/bin/fpm-tune")); got != "/usr/local/bin/fpm-tune" {
		t.Errorf("in place = %q", got)
	}
	if got := executablePath("/tmp/build/fpm-tune (deleted)", exists("/usr/local/bin/fpm-tune")); got != "fpm-tune" {
		t.Errorf("gone = %q, want the name from PATH", got)
	}
}

// TestEventsAreOneShapeAndStayInsideTheWidth: every kind has a glyph and a
// verb, a long pool name does not push the line past the panel, and the
// detail is cut by rune, never inside a multibyte character.
func TestEventsAreOneShapeAndStayInsideTheWidth(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	long := strings.Repeat("p", 30)
	for _, tc := range []struct {
		e    serve.HistoryEvent
		want []string
	}{
		{serve.HistoryEvent{At: at, Kind: serve.EventResized, Pool: long, From: 10, To: 22, Detail: "10 to 22"}, []string{"↑ resized", long, "10 → 22"}},
		{serve.HistoryEvent{At: at, Kind: serve.EventResized, Pool: "www", From: 22, To: 10, Detail: "22 to 10"}, []string{"↓ resized www 22 → 10"}},
		{serve.HistoryEvent{At: at, Kind: serve.EventChanged, Pool: "www", From: 10, To: 12, Detail: "edited outside the daemon"}, []string{"⇄ changed outside www 10 → 12", "edited"}},
		{serve.HistoryEvent{At: at, Kind: serve.EventApplyFailed, Detail: "php-fpm -t rejected the drop-in"}, []string{"✗ apply failed", "rejected"}},
		{serve.HistoryEvent{At: at, Kind: serve.EventRolledBack, Detail: "reload failed"}, []string{"↩ rolled back"}},
		{serve.HistoryEvent{At: at, Kind: serve.EventRollbackFailed, Detail: "still down"}, []string{"✗ rollback failed"}},
		{serve.HistoryEvent{At: at, Kind: serve.EventRepaired}, []string{"✓ repaired"}},
	} {
		line := eventLine(tc.e, 74)
		for _, want := range tc.want {
			if !strings.Contains(line, want) {
				t.Errorf("%s: line lacks %q: %s", tc.e.Kind, want, line)
			}
		}
		if strings.Contains(line, "to 10") || strings.Contains(line, "to 22") {
			t.Errorf("%s: the arrow's numbers repeated: %s", tc.e.Kind, line)
		}
		if w := lipgloss.Width(line); w > 74 {
			t.Errorf("%s: %d wide: %s", tc.e.Kind, w, line)
		}
	}

	// A detail with a multibyte rune at the cut.
	e := serve.HistoryEvent{At: at, Kind: serve.EventApplyFailed, Detail: strings.Repeat("ø", 200)}
	line := eventLine(e, 60)
	if w := lipgloss.Width(line); w > 60 || !strings.HasSuffix(line, "…") || strings.Contains(line, "\uFFFD") {
		t.Errorf("multibyte detail: %d wide, %q", w, line)
	}
	if got := trunc("ååååå", 4); got != "ååå…" {
		t.Errorf("trunc by rune = %q", got)
	}
	if got := trunc("abc", 2); got != "abc" {
		t.Errorf("trunc below the minimum = %q", got)
	}
}

// TestSpanBadgeAndDurations: the title says "showing 20m (all)" while the
// data is shorter than the span and "span 1h" once it is not, and durations
// read as an operator says them.
func TestSpanBadgeAndDurations(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second: "45s", 20 * time.Minute: "20m", 89 * time.Minute: "89m",
		90 * time.Minute: "1h30m", 119 * time.Minute: "1h59m", 2 * time.Hour: "2h", 25*time.Hour + 5*time.Minute: "25h5m",
	} {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%s) = %q, want %q", d, got, want)
		}
	}
	m := loaded(t, fixture(), 120, 40)
	if s := m.status(); s[0] != "showing 20m (all)" || len(s) != 3 {
		t.Errorf("status with 20m on a 1h span = %q", s)
	}
	// Two hours of data on the hour span: the span bounds the charts.
	resp := fixture()
	t0 := resp.Rounds[0].At
	for i := range resp.Rounds {
		resp.Rounds[i].At = t0.Add(time.Duration(i) * 3 * time.Minute)
	}
	m = loaded(t, resp, 120, 40)
	if s := m.status(); !strings.Contains(s[0], "span") || len(s) != 4 || s[2] != "1h57m of data" {
		t.Errorf("status with 2h on a 1h span = %q", s)
	}
	if s := m.status(); strings.Contains(s[0], "1 round") {
		t.Errorf("status = %q", s)
	}
	resp.Rounds = resp.Rounds[:1]
	m = loaded(t, resp, 120, 40)
	if s := m.status(); s[0] != "showing 1 round (all)" {
		t.Errorf("status with one round = %q", s)
	}
}

// TestLongPoolNamesStayApart: a wide terminal widens the POOL column to the
// longest name (to a point); a narrow one keeps 14 and cuts the middle, so
// "-prod" and "-stage" are still told apart.
func TestLongPoolNamesStayApart(t *testing.T) {
	names := []string{"www", "www-forge-customer-portal-prod", "www-forge-customer-portal-stage"}
	if got := poolWidth(names, true); got != 24 {
		t.Errorf("wide pool column = %d, want 24 (capped)", got)
	}
	if got := poolWidth(names, false); got != 14 {
		t.Errorf("narrow pool column = %d, want 14", got)
	}
	if got := poolWidth([]string{"www", "www-forge"}, true); got != 14 {
		t.Errorf("wide pool column with short names = %d, want 14", got)
	}
	prod, stage := truncMiddle(names[1], 14), truncMiddle(names[2], 14)
	if prod == stage || len([]rune(prod)) != 14 || !strings.HasSuffix(prod, "-prod") || !strings.HasPrefix(prod, "www-for") {
		t.Errorf("truncMiddle = %q and %q", prod, stage)
	}
	if got := truncMiddle("www", 14); got != "www" {
		t.Errorf("truncMiddle of a short name = %q", got)
	}

	resp := fixture()
	for i := range resp.Rounds {
		resp.Rounds[i].Pools[0].Pool = names[1]
	}
	for _, width := range []int{80, 120} {
		m := loaded(t, resp, width, 40)
		out := m.View()
		if w := widest(out); w > width {
			t.Errorf("width %d with a long pool name: a line is %d wide:\n%s", width, w, out)
		}
		if !strings.Contains(out, "-prod") {
			t.Errorf("width %d: the end of the long name is gone:\n%s", width, out)
		}
	}
}

// TestBusySeriesIsScaledPerRound: the table's sparkline is each round's
// busy workers over that round's ceiling, so the history before a resize
// shows how full the pool was then, not how it compares to the ceiling now.
func TestBusySeriesIsScaledPerRound(t *testing.T) {
	rounds := []serve.HistorySample{
		{Pools: []serve.PoolSample{{Pool: "www", Active: 10, Configured: 10}}},
		{Pools: []serve.PoolSample{{Pool: "www", Active: 10, Configured: 20}}},
		{Pools: []serve.PoolSample{{Pool: "www", Active: 3, Configured: 0}}},
		{Pools: []serve.PoolSample{{Pool: "other", Active: 3, Configured: 3}}},
	}
	got := busySeries(rounds, "www")
	if len(got) != 4 || got[0] != 1 || got[1] != 0.5 || got[2] != -1 || got[3] != -1 {
		t.Errorf("busySeries = %v", got)
	}
	// The cpu share is one colour, whatever its level.
	if s := plainSpark([]float64{0.1, 1, -1}, 3, 1, sMemory); !strings.Contains(s, "▁") || !strings.Contains(s, "█") || !strings.Contains(s, "·") {
		t.Errorf("plainSpark = %q", s)
	}
}

// TestCPULabelSaysWhatTheNumbersMean: the cpu row explains the share, the
// fill and the ceiling in words, shorter on a narrow terminal.
func TestCPULabelSaysWhatTheNumbersMean(t *testing.T) {
	p := serve.PoolSample{CPURatioP50: 0.85, CPUReadings: 400, CPUFill: 5, CPUCeiling: 10}
	if got := cpuLabel(p, false); got != "85% of each request is CPU · 5 workers fill the cores · CPU max 10" {
		t.Errorf("long label = %q", got)
	}
	if got := cpuLabel(p, true); got != "85% cpu/req · CPU max 10" {
		t.Errorf("short label = %q", got)
	}
	p.CPUCeiling = 0
	if got := cpuLabel(p, false); got != "85% of each request is CPU" {
		t.Errorf("label without a ceiling = %q", got)
	}
	p.CPUReadings = 3
	if got := cpuLabel(p, false); !strings.Contains(got, "too few") {
		t.Errorf("label with few readings = %q", got)
	}
}
