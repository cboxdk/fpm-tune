// Package top is the terminal view of a running fpm-tune daemon.
//
// It draws nothing the daemon does not already publish: every number comes
// from /history.json on the metrics address, fetched every few seconds. The
// daemon keeps a day of rounds in memory for exactly this, so the view opens
// with the lines already drawn rather than starting from a blank axis.
//
// Read-only. It changes nothing on the host; that is what `fpm-tune mode` and
// `fpm-tune apply` are for, and keeping the two apart is the point.
package top

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/NimbleMarkets/ntcharts/linechart/timeserieslinechart"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/cboxdk/fpm-tune/budget"
	"github.com/cboxdk/fpm-tune/serve"
)

// Options configures the view.
type Options struct {
	// Addr is the daemon's metrics address, where /history.json is served.
	Addr string

	// Refresh is how often the view fetches. The daemon's interval bounds
	// how often anything changes; fetching faster than that only redraws.
	Refresh time.Duration
}

// Run draws the view until q or Ctrl-C.
func Run(ctx context.Context, opts Options) error {
	if opts.Refresh <= 0 {
		opts.Refresh = 5 * time.Second
	}
	p := tea.NewProgram(newModel(opts), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()

	return err
}

// The palette. Adaptive, so it reads on a light terminal as well as a dark
// one; the accent is the same violet the docs use.
var (
	cAccent = lipgloss.AdaptiveColor{Light: "#6D28D9", Dark: "#A78BFA"}
	cOK     = lipgloss.AdaptiveColor{Light: "#047857", Dark: "#34D399"}
	cWarn   = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FBBF24"}
	cBad    = lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#F87171"}
	cDim    = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"}
	cFaint  = lipgloss.AdaptiveColor{Light: "#D1D5DB", Dark: "#374151"}
	cText   = lipgloss.AdaptiveColor{Light: "#111827", Dark: "#F3F4F6"}
	cBusy   = lipgloss.AdaptiveColor{Light: "#2563EB", Dark: "#60A5FA"}
	cPlan   = lipgloss.AdaptiveColor{Light: "#7C3AED", Dark: "#C4B5FD"}
	cNow    = lipgloss.AdaptiveColor{Light: "#9CA3AF", Dark: "#6B7280"}
	cMemory = lipgloss.AdaptiveColor{Light: "#0F766E", Dark: "#5EEAD4"}

	sTitle  = lipgloss.NewStyle().Bold(true).Foreground(cText)
	sAccent = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	sDim    = lipgloss.NewStyle().Foreground(cDim)
	sFaint  = lipgloss.NewStyle().Foreground(cFaint)
	sOK     = lipgloss.NewStyle().Foreground(cOK)
	sWarn   = lipgloss.NewStyle().Foreground(cWarn)
	sBad    = lipgloss.NewStyle().Foreground(cBad).Bold(true)
	sBusy   = lipgloss.NewStyle().Foreground(cBusy)
	sPlan   = lipgloss.NewStyle().Foreground(cPlan)
	sNow    = lipgloss.NewStyle().Foreground(cNow)
	sMemory = lipgloss.NewStyle().Foreground(cMemory)
	sBadge  = lipgloss.NewStyle().Padding(0, 1).Bold(true)
	sPanel  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cFaint).Padding(0, 1)
	sHeader = lipgloss.NewStyle().Foreground(cDim).Bold(true)
	sRowSel = lipgloss.NewStyle().Background(lipgloss.AdaptiveColor{Light: "#EDE9FE", Dark: "#3B2A6B"}).Bold(true)
	sKey    = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	sAxis   = lipgloss.NewStyle().Foreground(cFaint)
	sLabel  = lipgloss.NewStyle().Foreground(cDim)
)

// waiting is what the pool panels say while the daemon has not finished a
// round yet: a daemon in its first interval publishes a host and no rounds.
const waiting = "waiting for the daemon's first round"

type tickMsg time.Time

type fetchedMsg struct {
	resp *serve.HistoryResponse
	err  error
	at   time.Time
}

// span is how much time the charts cover. Zero is everything the daemon
// holds.
type span struct {
	name string
	d    time.Duration
}

var spans = []span{{"1h", time.Hour}, {"6h", 6 * time.Hour}, {"all", 0}}

// model is the whole view: what was last fetched, the terminal, and the
// operator's cursor.
type model struct {
	opts   Options
	client *http.Client

	resp    *serve.HistoryResponse
	err     error
	fetched time.Time

	width, height int
	selected      int
	pools         []string // pool names in display order, from the newest round
	span          int      // index into spans

	// confirm is the apply panel: open when the operator pressed a, closed
	// by Enter (apply) or Esc. notice is one line of feedback under the
	// title, from the last apply or a refused one.
	confirm bool
	notice  string
}

// appliedMsg is the outcome of an apply run from the view.
type appliedMsg struct{ err error }

func newModel(opts Options) model {
	return model{
		opts:   opts,
		client: &http.Client{Timeout: 4 * time.Second},
		width:  100, height: 30,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.fetch(), tea.Tick(m.opts.Refresh, func(t time.Time) tea.Msg { return tickMsg(t) }))
}

// fetch reads the history once.
func (m model) fetch() tea.Cmd {
	client, addr := m.client, m.opts.Addr

	return func() tea.Msg {
		resp, err := fetchHistory(client, addr)

		return fetchedMsg{resp: resp, err: err, at: time.Now()}
	}
}

func fetchHistory(client *http.Client, addr string) (*serve.HistoryResponse, error) {
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/history.json", nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s (is this an fpm-tune of at least beta.22, with serve running?)", addr, res.Status)
	}
	var body serve.HistoryResponse
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("%s did not send history JSON: %w", addr, err)
	}

	return &body, nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height

		return m, nil

	case tickMsg:
		return m, tea.Batch(m.fetch(), tea.Tick(m.opts.Refresh, func(t time.Time) tea.Msg { return tickMsg(t) }))

	case fetchedMsg:
		m.err = msg.err
		if msg.resp != nil {
			m.resp = msg.resp
			m.fetched = msg.at
			m.pools = poolNames(msg.resp)
			if m.selected >= len(m.pools) {
				m.selected = 0
			}
		}

		return m, nil

	case appliedMsg:
		if msg.err != nil {
			m.notice = sBad.Render("apply failed: " + msg.err.Error())
		} else {
			m.notice = sOK.Render("applied; the daemon reports the change on its next round")
		}

		return m, m.fetch()

	case tea.KeyMsg:
		if m.confirm {
			switch msg.String() {
			case "enter", "y":
				m.confirm = false

				return m, m.apply()
			case "esc", "n", "q", "a":
				m.confirm = false
			}

			return m, nil
		}
		// Esc is not here on purpose: it cancels the apply panel and nothing
		// else, so a stray Esc from a habit does not close the view.
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r":
			return m, m.fetch()
		case "a":
			if m.resp == nil {
				return m, nil
			}
			m.notice = ""
			if !localAddr(m.opts.Addr) {
				m.notice = sWarn.Render("this view reads " + m.opts.Addr +
					"; apply-now reaches only the daemon on this box, so run top there")

				return m, nil
			}
			m.confirm = true
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected+1 < len(m.pools) {
				m.selected++
			}
		case "tab":
			if len(m.pools) > 0 {
				m.selected = (m.selected + 1) % len(m.pools)
			}
		case "1":
			m.span = 0
		case "2":
			m.span = 1
		case "3":
			m.span = 2
		}
	}

	return m, nil
}

// pending is what an apply would change right now: the pools whose planned
// ceiling differs from the configured one, from the newest round.
func (m model) pending() []serve.PoolSample {
	if m.resp == nil || len(m.resp.Rounds) == 0 {
		return nil
	}
	var out []serve.PoolSample
	for _, p := range m.resp.Rounds[len(m.resp.Rounds)-1].Pools {
		if !p.Unknown && p.Recommended != p.Configured {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pool < out[j].Pool })

	return out
}

// applyArgs is the command an apply from the view runs: this binary's own
// apply-now, which asks the daemon to act on the plan it showed. The daemon
// has the state, the plan and the lock; the view only asks. Through sudo
// unless this is root already, because the control socket is root's; the
// terminal is handed over so a password prompt can appear.
func applyArgs(self string, root bool) []string {
	if root {
		return []string{self, "apply-now"}
	}

	return []string{"sudo", self, "apply-now"}
}

// self is this binary, for running its own apply-now: by path when the path
// still exists, by name from PATH otherwise.
func self() string {
	path, err := os.Executable()
	if err != nil {
		return "fpm-tune"
	}

	return executablePath(path, os.Stat)
}

// executablePath is the path apply-now runs by. On Linux an upgrade under a
// running top leaves os.Executable answering "/path/fpm-tune (deleted)": the
// suffix is the kernel's, not part of any name, so it is stripped and the
// path tried as the new binary is normally installed over the old one. When
// nothing is there any more, the name alone, and PATH finds it.
func executablePath(path string, stat func(string) (os.FileInfo, error)) string {
	path = strings.TrimSuffix(path, " (deleted)")
	if _, err := stat(path); err != nil {
		return "fpm-tune"
	}

	return path
}

// localAddr is whether the address the view reads from is this box. apply-now
// talks to the control socket on the box it runs on, so it can only be
// offered when the history on the screen is that daemon's; anything but a
// loopback address is read as another box. The daemon's own hostname is kept
// out of the decision on purpose: containers and cloned VMs make a hostname
// comparison wrong in both directions.
func localAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// apply runs fpm-tune apply-now in the terminal and comes back with the
// outcome. Stdin and stdout are left to tea.ExecProcess, which wires the
// terminal to them (sudo prompts on /dev/tty either way). Stderr is caught
// instead, because the terminal is redrawn the moment the command ends and
// whatever it printed is gone with it; the first line is what the notice
// then shows.
func (m model) apply() tea.Cmd {
	args := applyArgs(self(), os.Geteuid() == 0)
	cmd := exec.Command(args[0], args[1:]...) //nolint:gosec // the operator asked for exactly this
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	return tea.ExecProcess(cmd, func(err error) tea.Msg { return appliedMsg{err: applyOutcome(err, stderr.String())} })
}

// applyOutcome is the error the notice reports: the command's own first line
// of stderr when it wrote one (the reason, in its words), else the exec
// error ("exit status 1" says nothing, but it is all there is).
func applyOutcome(err error, stderr string) error {
	if err == nil {
		return nil
	}
	if line := firstLine(stderr); line != "" {
		return errors.New(line)
	}

	return err
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}

	return strings.TrimSpace(s)
}

// poolNames is the display order: the newest round's pools, by name.
func poolNames(resp *serve.HistoryResponse) []string {
	if resp == nil || len(resp.Rounds) == 0 {
		return nil
	}
	last := resp.Rounds[len(resp.Rounds)-1]
	names := make([]string, 0, len(last.Pools))
	for _, p := range last.Pools {
		names = append(names, p.Pool)
	}
	sort.Strings(names)

	return names
}

// window is the time range the charts show: from the oldest round the daemon
// holds, or the chosen span back from the newest, whichever is shorter. So a
// young daemon's chart grows from a few minutes up to the span and then
// scrolls, with no empty axis to the left.
func (m model) window() (from, to time.Time, rounds []serve.HistorySample) {
	all := m.resp.Rounds
	if len(all) == 0 {
		now := time.Now()

		return now.Add(-time.Hour), now, nil
	}
	to = all[len(all)-1].At
	d := spans[m.span].d
	from = all[0].At
	if d > 0 && to.Sub(from) > d {
		from = to.Add(-d)
	}
	// Never an axis shorter than two minutes: a daemon started a moment ago
	// still needs somewhere to draw.
	if to.Sub(from) < 2*time.Minute {
		from = to.Add(-2 * time.Minute)
	}
	i := sort.Search(len(all), func(i int) bool { return !all[i].At.Before(from) })

	return from, to, all[i:]
}

// showsAll is whether the chosen span holds every round the daemon has, in
// which case the span is not what bounds the charts and the title says how
// much there is instead.
func (m model) showsAll() bool {
	r := m.resp.Rounds
	d := spans[m.span].d

	return d == 0 || len(r) < 2 || r[len(r)-1].At.Sub(r[0].At) <= d
}

// layout is how many rows each panel gets. The pools and the events keep
// their rows and the charts share what is left, until the terminal is too
// short for even that; then the cuts come in a fixed order, so the title is
// never what goes (Bubble Tea drops the top of a view taller than the
// screen, and the title is the one line that says which daemon this is).
type layout struct {
	hostChart int  // rows of the host chart; 0 keeps only its head line
	poolChart int  // rows of the selected pool's chart
	detail    bool // whether the selected pool's panel is drawn at all
	events    int  // the most events listed
	poolRows  int  // the most pools in the table, the rest behind a "… more" line
}

// detailFrame is the detail panel's rows around its chart: the border, the
// head, the legend, and the queue and cpu rows.
const detailFrame = 6

func (m model) layout() layout {
	n := len(m.pools)
	l := layout{hostChart: 5, poolChart: 8, detail: n > 0, events: 6, poolRows: n}
	switch {
	case m.height >= 50:
		l.hostChart, l.poolChart, l.events = 7, 12, 12
	case m.height < 36:
		l.hostChart, l.poolChart = 4, 6
	}
	if m.rows(l) <= m.height {
		return l
	}
	full := l

	// Short. The host chart goes first (its number is still in the head
	// line), the events shrink to the latest two, the table to a window
	// around the cursor, and the selected pool's panel gets whatever is
	// left, or goes when that is not enough for a chart worth reading.
	l.hostChart, l.events = 0, 2
	if l.poolRows > 8 {
		l.poolRows = 8
	}
	if l.detail {
		without := l
		without.detail = false
		if rest := m.height - m.rows(without) - detailFrame; rest >= 4 {
			l.poolChart = min(rest, l.poolChart)
		} else {
			l.detail = false
		}
	}
	for m.rows(l) > m.height && l.poolRows > 1 {
		l.poolRows--
	}
	// Whatever is spare after the cuts goes back to the host chart, then to
	// the events, up to what the full layout had. A chart under three rows
	// has no row left for the line once the x axis has its two, and
	// ntcharts labels the nothing "NaN", so fewer stay with the events.
	if spare := m.height - m.rows(l); spare > 0 {
		if spare >= 3 {
			l.hostChart = min(spare, full.hostChart)
			spare -= l.hostChart
		}
		if spare > 0 {
			l.events = min(l.events+spare, full.events)
		}
	}

	return l
}

// rows is how many lines a layout draws, counting each panel's border.
func (m model) rows(l layout) int {
	rows := 2 // the title and the keys
	if m.notice != "" {
		rows++
	}
	if m.confirm {
		rows += m.confirmRows()
	}
	rows += 3 + l.hostChart
	if len(m.resp.Rounds) == 0 {
		rows += 3 // the waiting panel
	} else {
		rows += 3 + l.poolRows
		if l.poolRows < len(m.pools) {
			rows++
		}
		if l.detail {
			rows += detailFrame + l.poolChart
		}
	}
	rows += 3 + max(1, min(len(m.resp.Events), l.events))

	return rows
}

// confirmRows is the apply panel's height: the border, the question, the
// apply-mode note when there is one, a line per change (or the one saying
// there is none), and the command.
func (m model) confirmRows() int {
	rows := 2 + 1 + max(1, len(m.pending())) + 1
	if m.resp.Host.Apply {
		rows++
	}

	return rows
}

// View lays the panels out: a title bar, the host, the pools, the selected
// pool's charts, the events, and the keys.
func (m model) View() string {
	if m.resp == nil {
		if m.err != nil {
			return sPanel.Render(sBad.Render("Cannot reach the daemon") + "\n" + sDim.Render(m.err.Error()) +
				"\n\n" + sDim.Render("fpm-tune top reads /history.json on the metrics address; pass --addr host:port.") +
				"\n" + m.keys(m.width))
		}

		return sDim.Render("  connecting to " + m.opts.Addr + " …")
	}

	// The panel's box is the terminal less its border; the text inside it is
	// that less the padding. Every panel is given the text width, so nothing
	// it draws can wrap.
	inner := m.width - 4
	if inner < 60 {
		inner = 60
	}
	content := inner - 2
	l := m.layout()

	parts := []string{m.titleBar(inner + 2)}
	if m.notice != "" {
		parts = append(parts, fit("  "+m.notice, inner+2))
	}
	if m.confirm {
		parts = append(parts, sPanel.Width(inner).BorderForeground(cAccent).Render(m.confirmPanel(content)))
	}
	parts = append(parts, sPanel.Width(inner).Render(m.hostPanel(content, l.hostChart)))
	if len(m.resp.Rounds) == 0 {
		parts = append(parts, sPanel.Width(inner).Render(sDim.Render(waiting)))
	} else {
		parts = append(parts, sPanel.Width(inner).Render(m.poolsPanel(content, l.poolRows)))
		if l.detail {
			parts = append(parts, sPanel.Width(inner).Render(m.detailPanel(content, l.poolChart)))
		}
	}
	parts = append(parts, sPanel.Width(inner).Render(m.eventsPanel(content, l.events)), m.keys(inner+2))

	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// fit cuts a styled line to a width. JoinVertical pads every line to the
// widest, so one line past the terminal wraps them all; this is the last
// guard on the lines that carry text of unknown length.
func fit(s string, width int) string {
	return lipgloss.NewStyle().MaxWidth(width).Render(s)
}

func (m model) titleBar(width int) string {
	h := m.resp.Host
	mode := sBadge.Background(cOK).Foreground(lipgloss.Color("#062E1F")).Render("advisory")
	if h.Apply {
		mode = sBadge.Background(cWarn).Foreground(lipgloss.Color("#3A2400")).Render("apply")
	}
	cpu := sDim.Render("cpu ceiling off")
	if h.CPUCeiling {
		cpu = sAccent.Render("cpu ceiling on")
	}
	name := h.Hostname
	if name == "" {
		name = m.opts.Addr
	}
	// A hostname is normally short; a fully qualified one on a narrow
	// terminal would push the mode off the screen, and the mode matters more.
	left := sTitle.Render(" fpm-tune top ") + sAccent.Render(trunc(name, max(width/3, 8))) + "  " + mode + "  " + cpu
	version := ""
	if h.Version != "" {
		version = "  " + sDim.Render("v"+h.Version)
	}

	// The right side gives way before the left: a narrow terminal drops the
	// refresh rate, then how much history there is, then the version, then
	// the time of the last fetch (a failed fetch replaces all of it with a
	// stale message, cut to what is left beside the name and mode), so what
	// survives at 80 columns is the daemon, its mode and what the charts
	// show, in whole words.
	var right string
	if m.err != nil {
		stale := "stale: " + m.err.Error()
		if width-lipgloss.Width(left)-lipgloss.Width(version)-2 < lipgloss.Width(stale) {
			version = ""
		}
		right = sBad.Render(trunc(stale, max(width-lipgloss.Width(left)-2, 4)))
	} else {
		parts := m.status()
		for done := false; !done; {
			right = sDim.Render(strings.Join(parts, " · "))
			if width-lipgloss.Width(left)-lipgloss.Width(version)-lipgloss.Width(right) >= 2 {
				break
			}
			switch {
			case len(parts) > 2:
				parts = parts[:len(parts)-1]
			case version != "":
				version = ""
			case len(parts) > 1:
				parts = parts[:len(parts)-1]
			default:
				done = true
			}
		}
	}
	left += version
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 {
		gap = 2
	}

	return fit(left+strings.Repeat(" ", gap)+right, width)
}

// status is the right side of the title bar, in the order a narrow terminal
// keeps it: first what the charts show, then when it was fetched, then how
// much history the daemon holds (when the span is what bounds the charts),
// then how often the view fetches.
func (m model) status() []string {
	stamp := "updated never"
	if !m.fetched.IsZero() {
		stamp = "updated " + m.fetched.Format("15:04:05")
	}
	var parts []string
	switch {
	case len(m.resp.Rounds) == 0:
		parts = []string{"no rounds yet", stamp}
	case m.showsAll():
		// Every round fits the span, so the span is not what the operator
		// sees: the extent of the data is.
		parts = []string{"showing " + m.dataExtent() + " (all)", stamp}
	default:
		parts = []string{"span " + sKey.Render(spans[m.span].name), stamp, m.dataExtent() + " of data"}
	}

	return append(parts, "every "+m.opts.Refresh.String())
}

// dataExtent is how much history the daemon holds, as a word.
func (m model) dataExtent() string {
	r := m.resp.Rounds
	if len(r) < 2 {
		return "1 round"
	}

	return humanDuration(r[len(r)-1].At.Sub(r[0].At))
}

// humanDuration is a duration the way an operator says it: seconds while
// the daemon is new, minutes up to an hour and a half, then hours and
// minutes ("1h59m"), because "1.9h" is a sum nobody wants to do.
func humanDuration(d time.Duration) string {
	switch {
	case d < 2*time.Minute:
		return fmt.Sprintf("%.0fs", d.Seconds())
	case d < 90*time.Minute:
		return fmt.Sprintf("%.0fm", d.Minutes())
	}
	hours, mins := int(d.Hours()), int(d.Minutes())%60
	if mins == 0 {
		return fmt.Sprintf("%dh", hours)
	}

	return fmt.Sprintf("%dh%dm", hours, mins)
}

// chart is a time-series line chart with the view's axes and the chosen
// window, braille-drawn. One per panel per frame; they are cheap.
func (m model) chart(width, height int, yMax float64, yLabel func(float64) string) timeserieslinechart.Model {
	from, to, _ := m.window()
	c := timeserieslinechart.New(width, height,
		timeserieslinechart.WithAxesStyles(sAxis, sLabel),
		// Local time on the axis, like the event list; the daemon stamps
		// rounds in UTC.
		timeserieslinechart.WithXLabelFormatter(func(_ int, v float64) string {
			return time.Unix(int64(v), 0).Local().Format("15:04")
		}),
		timeserieslinechart.WithYLabelFormatter(func(_ int, v float64) string { return yLabel(v) }),
		timeserieslinechart.WithXYSteps(xSteps(width), ySteps(height)),
		timeserieslinechart.WithYRange(0, yMax),
		timeserieslinechart.WithTimeRange(from, to),
	)

	return c
}

func xSteps(width int) int {
	switch {
	case width < 50:
		return 3
	case width < 100:
		return 5
	default:
		return 8
	}
}

// ySteps is how many rows apart the y labels are. ntcharts labels every
// step rows up from the origin, over the rows the chart has once the x axis
// has taken two, so the top of the range is labelled only when the step
// divides those rows; a step that does not leaves a short chart reading
// "0%" alone, with no scale at all. The smallest reasonable step that
// divides, else the rows themselves, which labels the two ends.
func ySteps(height int) int {
	rows := height - 2
	if rows <= 1 {
		return 1
	}
	for _, step := range []int{3, 2, 4, 5} {
		if rows%step == 0 {
			return step
		}
	}

	return rows
}

// hostPanel is the box: its budget, how busy its CPU is now, and the chart
// of that over the window; a short terminal keeps the head line alone.
func (m model) hostPanel(width, height int) string {
	h := m.resp.Host
	_, _, rounds := m.window()

	var now string
	nowStyle := sDim
	if len(rounds) > 0 && rounds[len(rounds)-1].HostBusyKnown {
		v := rounds[len(rounds)-1].HostBusyRatio
		now = fmt.Sprintf("%.0f%%", v*100)
		switch {
		case v >= 0.9:
			nowStyle = sBad
		case v >= 0.7:
			nowStyle = sWarn
		default:
			nowStyle = sOK
		}
	} else {
		now = "-"
	}
	head := sHeader.Render("HOST  ") +
		fmt.Sprintf("%s memory · %s", budget.HumanBytes(h.MemoryBytes), budget.HumanMillicores(h.CPUMillicores)) +
		sDim.Render("  ("+h.Source+")") +
		"      " + sHeader.Render("CPU busy ") + nowStyle.Render(now)
	if height <= 0 {
		return fit(head, width)
	}

	c := m.chart(width, height, 1, func(v float64) string { return fmt.Sprintf("%.0f%%", v*100) })
	c.SetDataSetStyle("busy", sBusy)
	for _, r := range rounds {
		if r.HostBusyKnown {
			c.PushDataSet("busy", timeserieslinechart.TimePoint{Time: r.At, Value: r.HostBusyRatio})
		}
	}
	c.DrawBrailleAll()

	return fit(head, width) + "\n" + c.View()
}

// poolWidth is the POOL column: 14 unless the terminal is wide enough to
// spend more on long names, and then the longest name up to 24. Below that
// a long name is cut in the middle, so two that differ only at the end (a
// site's "-prod" and "-stage" pools) stay apart.
func poolWidth(names []string, wide bool) int {
	w := 14
	if !wide {
		return w
	}
	for _, n := range names {
		w = max(w, min(len([]rune(n)), 24))
	}

	return w
}

// poolsPanel is the table: one pool per row, the newest round's numbers,
// with a window of maxRows around the cursor when the terminal has not the
// rows for every pool.
func (m model) poolsPanel(width, maxRows int) string {
	_, _, rounds := m.window()
	if len(rounds) == 0 {
		rounds = m.resp.Rounds
	}
	if len(rounds) == 0 {
		return sDim.Render(waiting)
	}
	last := rounds[len(rounds)-1]
	byName := make(map[string]serve.PoolSample, len(last.Pools))
	for _, p := range last.Pools {
		byName[p.Pool] = p
	}

	// The fixed columns take the pool column plus 52 characters, and the
	// sparkline's own gap two more; the sparkline gets what is left, up to
	// a reasonable length, and goes entirely when there is no room for one
	// worth reading. At 80 columns that is six, which is a trend.
	poolW := poolWidth(m.pools, m.width >= 100)
	sparkWidth := width - poolW - 54
	if sparkWidth > 24 {
		sparkWidth = 24
	}
	if sparkWidth < 6 {
		sparkWidth = 0
	}
	sparkHeader := ""
	if sparkWidth > 0 {
		label := "BUSY OVER TIME"
		if sparkWidth < len(label) {
			label = "TREND"
		}
		sparkHeader = fmt.Sprintf("%-*s  ", sparkWidth, label)
	}
	header := fmt.Sprintf("%-*s %5s %5s %5s %5s  %s%7s %7s %s",
		poolW, "POOL", "BUSY", "QUEUE", "MAX", "PLAN", sparkHeader, "CPU/REQ", "CPU MAX", "BOUND BY")
	lines := []string{sHeader.Render(header)}
	start, end := rowWindow(len(m.pools), m.selected, maxRows)
	for i := start; i < end; i++ {
		name := m.pools[i]
		p := byName[name]
		cpu := "-"
		if p.CPUReadings >= 20 {
			cpu = fmt.Sprintf("%.0f%%", p.CPURatioP50*100)
		}
		cpuMax := "-"
		if p.CPUCeiling > 0 {
			cpuMax = fmt.Sprintf("%d", p.CPUCeiling)
		}
		queue := sDim.Render(fmt.Sprintf("%5d", p.Queue))
		if p.Queue > 0 {
			queue = sWarn.Render(fmt.Sprintf("%5d", p.Queue))
		}
		plan := fmt.Sprintf("%5d", p.Recommended)
		if p.Unknown {
			plan = sDim.Render("    —")
		} else if p.Recommended != p.Configured {
			plan = sAccent.Render(plan)
		}
		sparkCell := ""
		if sparkWidth > 0 {
			sparkCell = colorSpark(busySeries(rounds, name), sparkWidth, 1) + "  "
		}
		row := fmt.Sprintf("%-*s %5d %s %5d %s  %s%7s %7s %s",
			poolW, truncMiddle(name, poolW), p.Active, queue, p.Configured, plan,
			sparkCell, cpu, cpuMax, boundBy(p))
		if i == m.selected {
			row = sRowSel.Render(row)
		}
		lines = append(lines, fit(row, width))
	}
	if above, below := start, len(m.pools)-end; above > 0 || below > 0 {
		var more string
		switch {
		case above > 0 && below > 0:
			more = fmt.Sprintf("… %d above · %d below", above, below)
		case above > 0:
			more = fmt.Sprintf("… %d more above", above)
		default:
			more = fmt.Sprintf("… %d more", below)
		}
		lines = append(lines, sDim.Render(more))
	}

	return strings.Join(lines, "\n")
}

// rowWindow is the slice of n rows that a table of maxRows shows: the whole
// table when it fits, else the rows around the selected one.
func rowWindow(n, selected, maxRows int) (start, end int) {
	if maxRows <= 0 || maxRows >= n {
		return 0, n
	}
	start = selected - maxRows/2
	if start < 0 {
		start = 0
	}
	if start+maxRows > n {
		start = n - maxRows
	}

	return start, start + maxRows
}

// boundBy is the BOUND BY column: which side of the budget set the plan.
// "cpu (held)" is a pool the CPU side is holding at its ceiling, "cpu" one
// it lowered, "memory" one it looked at and left to memory, and a dash a
// pool the CPU side has too few readings on to say.
func boundBy(p serve.PoolSample) string {
	switch {
	case p.CPUBound:
		return sAccent.Render("cpu (held)")
	case p.CPULimited:
		return sWarn.Render("cpu")
	case p.CPUReadings >= 20:
		return sOK.Render("memory")
	default:
		return sDim.Render("-")
	}
}

// busySeries is a pool's busy workers as a share of its ceiling, round by
// round, for the table's sparkline. Each round is scaled by its own ceiling,
// so the history of a pool that was resized since is drawn as how full it
// was then, not clipped red against a ceiling it did not have.
func busySeries(rounds []serve.HistorySample, name string) []float64 {
	return seriesOf(rounds, name, func(s serve.PoolSample) float64 {
		if s.Configured <= 0 {
			return -1
		}

		return float64(s.Active) / float64(s.Configured)
	})
}

func poolValue(r serve.HistorySample, name string, f func(serve.PoolSample) float64) float64 {
	for _, p := range r.Pools {
		if p.Pool == name {
			return f(p)
		}
	}

	return -1
}

// detailPanel is the selected pool: a legend with the current numbers, then
// one chart with busy workers against the ceilings, then the queue and the
// CPU share as sparklines beside their values.
func (m model) detailPanel(width, height int) string {
	name := m.pools[m.selected]
	_, _, rounds := m.window()
	if len(rounds) == 0 {
		rounds = m.resp.Rounds
	}
	if len(rounds) == 0 {
		return sDim.Render(waiting)
	}
	var last serve.PoolSample
	for _, p := range rounds[len(rounds)-1].Pools {
		if p.Pool == name {
			last = p
		}
	}

	head := sHeader.Render("POOL  ") + sAccent.Render(name) + sDim.Render(fmt.Sprintf("  %s/worker · %d cpu readings",
		budget.HumanBytes(last.WorkerBytes), last.CPUReadings))

	// The ceilings the busy line is drawn against, and the y range that
	// holds all of them.
	yMax := 1.0
	for _, r := range rounds {
		for _, p := range r.Pools {
			if p.Pool != name {
				continue
			}
			for _, v := range []int{p.Active, p.Configured, p.Recommended, p.CPUCeiling, p.MemoryCeiling} {
				if float64(v) > yMax {
					yMax = float64(v)
				}
			}
		}
	}
	yMax = yMax * 1.1

	legend := []string{
		sBusy.Render("●") + " busy " + sTitle.Render(fmt.Sprintf("%d", last.Active)),
		sNow.Render("●") + " max " + fmt.Sprintf("%d", last.Configured),
		sPlan.Render("●") + " plan " + fmt.Sprintf("%d", last.Recommended),
	}
	if last.MemoryCeiling > 0 {
		legend = append(legend, sMemory.Render("●")+" memory ceiling "+fmt.Sprintf("%d", last.MemoryCeiling))
	}
	if last.CPUCeiling > 0 {
		legend = append(legend, sWarn.Render("●")+" CPU max "+fmt.Sprintf("%d", last.CPUCeiling))
	}
	if last.CPUBound {
		legend = append(legend, sAccent.Render("held at the CPU max"))
	}

	c := m.chart(width, height, yMax, func(v float64) string { return fmt.Sprintf("%.0f", v) })
	c.SetDataSetStyle("busy", sBusy)
	c.SetDataSetStyle("now", sNow)
	c.SetDataSetStyle("plan", sPlan)
	c.SetDataSetStyle("memory", sMemory)
	c.SetDataSetStyle("cap", sWarn)
	for _, r := range rounds {
		for _, p := range r.Pools {
			if p.Pool != name {
				continue
			}
			c.PushDataSet("busy", timeserieslinechart.TimePoint{Time: r.At, Value: float64(p.Active)})
			c.PushDataSet("now", timeserieslinechart.TimePoint{Time: r.At, Value: float64(p.Configured)})
			c.PushDataSet("plan", timeserieslinechart.TimePoint{Time: r.At, Value: float64(p.Recommended)})
			if p.MemoryCeiling > 0 {
				c.PushDataSet("memory", timeserieslinechart.TimePoint{Time: r.At, Value: float64(p.MemoryCeiling)})
			}
			if p.CPUCeiling > 0 {
				c.PushDataSet("cap", timeserieslinechart.TimePoint{Time: r.At, Value: float64(p.CPUCeiling)})
			}
		}
	}
	c.DrawBrailleAll()

	// Queue and CPU share: a value, then a sparkline, so the number is
	// beside its name and the line is beside the number.
	queue := seriesOf(rounds, name, func(s serve.PoolSample) float64 { return float64(s.Queue) })
	cpu := seriesOf(rounds, name, func(s serve.PoolSample) float64 {
		if s.CPUReadings < 20 {
			return -1
		}

		return s.CPURatioP50
	})
	queueVal := fmt.Sprintf("%d waiting", last.Queue)
	cpuVal := cpuLabel(last, width < 100)
	labelW := lipgloss.Width(queueVal)
	if w := lipgloss.Width(cpuVal); w > labelW {
		labelW = w
	}
	sparkW := width - 8 - labelW - 2
	if sparkW < 10 {
		sparkW = 10
	}
	// The queue against the pool's ceiling, like the busy workers: a queue
	// as long as the pool is the pool a second time over, which is where
	// red belongs. Scaled to its own peak, one waiting request would be red.
	// The CPU share is in one colour: a request that is mostly CPU is a
	// fact about the code, not a problem, and amber would say otherwise.
	tail := fit(sHeader.Render("queue   ")+pad(queueVal, labelW)+"  "+colorSpark(queue, sparkW, float64(max(last.Configured, 1))), width) + "\n" +
		fit(sHeader.Render("cpu     ")+pad(cpuVal, labelW)+"  "+plainSpark(cpu, sparkW, 1, sMemory), width)

	return fit(head, width) + "\n" + fit(strings.Join(legend, "   "), width) + "\n" + c.View() + "\n" + tail
}

func seriesOf(rounds []serve.HistorySample, name string, f func(serve.PoolSample) float64) []float64 {
	out := make([]float64, 0, len(rounds))
	for _, r := range rounds {
		out = append(out, poolValue(r, name, f))
	}

	return out
}

// pad right-pads a styled string to a visible width.
func pad(s string, width int) string {
	if n := width - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}

	return s
}

// cpuLabel is the cpu row's value: the share of a request that is CPU, how
// many workers at that share fill the cores, and the ceiling that gives;
// shorter words on a narrow terminal.
func cpuLabel(p serve.PoolSample, short bool) string {
	if p.CPUReadings < 20 {
		return sDim.Render("too few readings yet")
	}
	share := fmt.Sprintf("%.0f%% of each request is CPU", p.CPURatioP50*100)
	fill := fmt.Sprintf(" · %d workers fill the cores · CPU max %d", p.CPUFill, p.CPUCeiling)
	if short {
		share = fmt.Sprintf("%.0f%% cpu/req", p.CPURatioP50*100)
		fill = fmt.Sprintf(" · CPU max %d", p.CPUCeiling)
	}
	s := share
	if p.CPUCeiling > 0 {
		s += fill
	}

	return s
}

func (m model) eventsPanel(width, maxEvents int) string {
	events := m.resp.Events
	lines := []string{sHeader.Render("EVENTS")}
	if len(events) == 0 {
		lines = append(lines, sDim.Render("no events since the daemon started"))
	}
	if len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
	}
	for i := len(events) - 1; i >= 0; i-- {
		lines = append(lines, eventLine(events[i], width))
	}

	return strings.Join(lines, "\n")
}

// eventLine is one event, every kind in one shape: the time, a glyph and a
// verb, then the pool and the change when the event has them, then the
// daemon's own words in whatever room is left.
func eventLine(e serve.HistoryEvent, width int) string {
	var glyph, verb string
	switch e.Kind {
	case serve.EventResized:
		glyph, verb = sOK.Render("↑"), "resized"
		if e.To < e.From {
			glyph = sWarn.Render("↓")
		}
	case serve.EventChanged:
		glyph, verb = sDim.Render("⇄"), "changed outside"
	case serve.EventApplyFailed:
		glyph, verb = sBad.Render("✗"), sBad.Render("apply failed")
	case serve.EventRolledBack:
		glyph, verb = sWarn.Render("↩"), sWarn.Render("rolled back")
	case serve.EventRollbackFailed:
		glyph, verb = sBad.Render("✗"), sBad.Render("rollback failed")
	case serve.EventRepaired:
		glyph, verb = sOK.Render("✓"), sOK.Render("repaired")
	default:
		glyph, verb = sBad.Render("•"), sBad.Render(strings.ReplaceAll(e.Kind, "_", " "))
	}
	line := sDim.Render(e.At.Local().Format("15:04:05")) + "  " + glyph + " " + verb
	if e.Pool != "" {
		line += " " + sAccent.Render(e.Pool)
	}
	if e.From != 0 || e.To != 0 {
		line += fmt.Sprintf(" %d → %d", e.From, e.To)
	}
	// The daemon's detail on a resize is "22 to 10", which the arrow has
	// just said.
	detail := e.Detail
	if detail == fmt.Sprintf("%d to %d", e.From, e.To) {
		detail = ""
	}
	if room := width - lipgloss.Width(line) - 2; detail != "" && room >= 4 {
		line += sDim.Render("  " + trunc(detail, room))
	}

	return fit(line, width)
}

// confirmPanel is what Enter would do: the plan's changes, pool by pool,
// and the command that makes them.
func (m model) confirmPanel(width int) string {
	lines := []string{sAccent.Render("APPLY THE PLAN?") + "  " + sKey.Render("Enter") +
		sDim.Render(" asks the daemon to apply it now, ") + sKey.Render("Esc") + sDim.Render(" cancels")}
	if m.resp.Host.Apply {
		lines = append(lines, sDim.Render("the daemon is in apply mode and would get to this on its own; Enter skips its reload damping"))
	}
	pending := m.pending()
	if len(pending) == 0 {
		lines = append(lines, sDim.Render("nothing to change: every pool is already at its planned ceiling"))
	}
	for _, p := range pending {
		arrow := sOK.Render("↑")
		if p.Recommended < p.Configured {
			arrow = sWarn.Render("↓")
		}
		why := ""
		if p.CPUBound {
			why = sDim.Render("  held at the CPU max")
		}
		lines = append(lines, fmt.Sprintf("  %s %-14s %d → %d%s", arrow, p.Pool, p.Configured, p.Recommended, why))
	}
	// The command by its base name: the path is this binary's own and adds
	// nothing but length.
	shown := applyArgs(filepath.Base(self()), os.Geteuid() == 0)
	lines = append(lines, sDim.Render(trunc("runs: "+strings.Join(shown, " "), width)))
	for i := range lines {
		lines[i] = fit(lines[i], width)
	}

	return strings.Join(lines, "\n")
}

// keys is the bottom line: the keys that do something right now, which
// while the apply panel is open are the two that close it.
func (m model) keys(width int) string {
	k := func(key, what string) string { return sKey.Render(key) + sDim.Render(" "+what) }
	if m.confirm {
		return fit("  "+k("Enter", "apply now")+"   "+k("Esc", "cancel"), width)
	}
	items := []string{k("↑↓/tab", "pool"), k("1", "1h"), k("2", "6h"), k("3", "all"), k("a", "apply"), k("r", "refresh"), k("q", "quit")}

	return fit("  "+strings.Join(items, "   "), width)
}

// The sparkline used in the table and for the queue and CPU rows. Eight
// block heights, one column per bucket of rounds, newest at the right. A
// value below zero is "unknown" and draws as a dot, so a hole in the history
// looks like a hole and not like zero.
var sparkChars = []rune("▁▂▃▄▅▆▇█")

// spark resamples a series to width columns, taking the maximum in each
// bucket so a spike is never averaged away, and scales it to scale (the value
// that fills the column). Values above scale clip at the top. A series with
// fewer values than columns is stretched, each value over several columns,
// so the chart fills its width whatever the history holds; the time axis
// runs left to right either way.
func spark(series []float64, width int, scale float64) []rune {
	out := make([]rune, width)
	for i := range out {
		out[i] = ' '
	}
	if len(series) == 0 || width <= 0 {
		return out
	}
	if scale <= 0 {
		scale = 1
	}
	n := len(series)
	for col := 0; col < width; col++ {
		lo := col * n / width
		hi := (col + 1) * n / width
		if hi <= lo {
			hi = lo + 1
		}
		if hi > n {
			hi = n
		}
		v, known := -1.0, false
		for _, x := range series[lo:hi] {
			if x >= 0 {
				known = true
				if x > v {
					v = x
				}
			}
		}
		switch {
		case !known:
			out[col] = '·'
		default:
			idx := int(v / scale * float64(len(sparkChars)-1))
			if idx < 0 {
				idx = 0
			}
			if idx >= len(sparkChars) {
				idx = len(sparkChars) - 1
			}
			out[col] = sparkChars[idx]
		}
	}

	return out
}

// colorSpark renders a sparkline with each column coloured by how full it
// is: blue when there is room, amber past 70%, red at the top. For series
// where the top is a problem: workers against their ceiling, a queue.
func colorSpark(series []float64, width int, scale float64) string {
	runes := spark(series, width, scale)
	var b strings.Builder
	for _, r := range runes {
		switch r {
		case ' ':
			b.WriteRune(' ')
		case '·':
			b.WriteString(sFaint.Render("·"))
		default:
			level := strings.IndexRune(string(sparkChars), r)
			switch {
			case level >= 7:
				b.WriteString(sBad.Render(string(r)))
			case level >= 5:
				b.WriteString(sWarn.Render(string(r)))
			default:
				b.WriteString(sBusy.Render(string(r)))
			}
		}
	}

	return b.String()
}

// plainSpark renders a sparkline in one style, for a series where a high
// value is not a warning and the traffic-light colours would lie.
func plainSpark(series []float64, width int, scale float64, style lipgloss.Style) string {
	var b strings.Builder
	for _, r := range spark(series, width, scale) {
		switch r {
		case ' ':
			b.WriteRune(' ')
		case '·':
			b.WriteString(sFaint.Render("·"))
		default:
			b.WriteString(style.Render(string(r)))
		}
	}

	return b.String()
}

// trunc cuts a string to n characters, the last of them an ellipsis. By
// rune, so a multibyte character at the cut is dropped whole rather than
// left as half a sequence the terminal draws as garbage.
func trunc(s string, n int) string {
	r := []rune(s)
	if n < 4 || len(r) <= n {
		return s
	}

	return string(r[:n-1]) + "…"
}

// truncMiddle cuts a name to n characters by taking the middle out, keeping
// the start and the end, which is where pool names differ.
func truncMiddle(s string, n int) string {
	r := []rune(s)
	if n < 4 || len(r) <= n {
		return s
	}
	head := n / 2
	tail := n - 1 - head

	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}
