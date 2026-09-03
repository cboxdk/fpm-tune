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
	"context"
	"encoding/json"
	"fmt"
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
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "r":
			return m, m.fetch()
		case "a":
			if m.resp == nil {
				return m, nil
			}
			m.notice = ""
			if other := m.otherHost(); other != "" {
				m.notice = sWarn.Render("this is " + other + "'s history; apply-now reaches only the daemon on this box, so run top there")

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
// still exists (an upgrade under a running top leaves "(deleted)" on it), by
// name from PATH otherwise.
func self() string {
	path, err := os.Executable()
	if err != nil {
		return "fpm-tune"
	}
	if _, err := os.Stat(path); err != nil {
		return "fpm-tune"
	}

	return path
}

// otherHost names the daemon's host when it is not this one, and is empty
// when it is (or when either side is unknown). The history can come from any
// address; apply-now can only reach the daemon on the box it runs on.
func (m model) otherHost() string {
	here, err := os.Hostname()
	if err != nil || m.resp == nil || m.resp.Host.Hostname == "" || m.resp.Host.Hostname == here {
		return ""
	}

	return m.resp.Host.Hostname
}

// apply runs fpm-tune apply-now in the terminal and comes back with the outcome.
func (m model) apply() tea.Cmd {
	args := applyArgs(self(), os.Geteuid() == 0)
	cmd := exec.Command(args[0], args[1:]...) //nolint:gosec // the operator asked for exactly this

	return tea.ExecProcess(cmd, func(err error) tea.Msg { return appliedMsg{err: err} })
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

// View lays the panels out: a title bar, the host, the pools, the selected
// pool's charts, the events, and the keys.
func (m model) View() string {
	if m.resp == nil {
		if m.err != nil {
			return sPanel.Render(sBad.Render("Cannot reach the daemon") + "\n" + sDim.Render(m.err.Error()) +
				"\n\n" + sDim.Render("fpm-tune top reads /history.json on the metrics address; pass --addr host:port.") +
				"\n" + m.keys())
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

	// Chart heights by what the terminal has: the pools and the events keep
	// their rows, the two charts share what is left.
	hostH, poolH := 5, 8
	if m.height < 36 {
		hostH, poolH = 4, 6
	}
	if m.height >= 50 {
		hostH, poolH = 7, 12
	}

	parts := []string{m.titleBar(content + 4)}
	if m.notice != "" {
		parts = append(parts, "  "+m.notice)
	}
	if m.confirm {
		parts = append(parts, sPanel.Width(inner).BorderForeground(cAccent).Render(m.confirmPanel(content)))
	}
	parts = append(parts,
		sPanel.Width(inner).Render(m.hostPanel(content, hostH)),
		sPanel.Width(inner).Render(m.poolsPanel(content)),
	)
	if len(m.pools) > 0 {
		parts = append(parts, sPanel.Width(inner).Render(m.detailPanel(content, poolH)))
	}
	parts = append(parts, sPanel.Width(inner).Render(m.eventsPanel(content)), m.keys())

	return lipgloss.JoinVertical(lipgloss.Left, parts...)
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
	left := sTitle.Render(" fpm-tune top ") + sAccent.Render(name) + "  " + mode + "  " + cpu
	if h.Version != "" {
		left += "  " + sDim.Render("v"+h.Version)
	}

	stamp := "never"
	if !m.fetched.IsZero() {
		stamp = m.fetched.Format("15:04:05")
	}
	right := sDim.Render(fmt.Sprintf("span %s · %s of data · updated %s · every %s",
		sKey.Render(spans[m.span].name), m.dataExtent(), stamp, m.opts.Refresh))
	if m.err != nil {
		right = sBad.Render("stale: " + m.err.Error())
	}
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 {
		gap = 2
	}

	return left + strings.Repeat(" ", gap) + right
}

// dataExtent is how much history the daemon holds, as a word.
func (m model) dataExtent() string {
	r := m.resp.Rounds
	if len(r) < 2 {
		return "1 round"
	}

	return humanDuration(r[len(r)-1].At.Sub(r[0].At))
}

func humanDuration(d time.Duration) string {
	switch {
	case d < 2*time.Minute:
		return fmt.Sprintf("%.0fs", d.Seconds())
	case d < 2*time.Hour:
		return fmt.Sprintf("%.0fm", d.Minutes())
	default:
		return fmt.Sprintf("%.1fh", d.Hours())
	}
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
		timeserieslinechart.WithXYSteps(xSteps(width), 3),
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

	c := m.chart(width, height, 1, func(v float64) string { return fmt.Sprintf("%.0f%%", v*100) })
	c.SetDataSetStyle("busy", sBusy)
	for _, r := range rounds {
		if r.HostBusyKnown {
			c.PushDataSet("busy", timeserieslinechart.TimePoint{Time: r.At, Value: r.HostBusyRatio})
		}
	}
	c.DrawBrailleAll()

	return head + "\n" + c.View()
}

func (m model) poolsPanel(width int) string {
	_, _, rounds := m.window()
	if len(rounds) == 0 {
		rounds = m.resp.Rounds
	}
	last := rounds[len(rounds)-1]
	byName := make(map[string]serve.PoolSample, len(last.Pools))
	for _, p := range last.Pools {
		byName[p.Pool] = p
	}

	// The fixed columns take 67 characters; the sparkline gets what is left,
	// up to a reasonable length, and goes entirely when there is no room for
	// one worth reading.
	sparkWidth := width - 67
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
	header := fmt.Sprintf("%-14s %6s %5s %5s %5s  %s%6s  %8s  %-6s",
		"POOL", "BUSY", "QUEUE", "NOW", "PLAN", sparkHeader, "CPU", "FILL/CAP", "LIMIT")
	lines := []string{sHeader.Render(header)}
	for i, name := range m.pools {
		p := byName[name]
		series := seriesOf(rounds, name, func(s serve.PoolSample) float64 { return float64(s.Active) })
		ceiling := float64(p.Configured)
		if ceiling <= 0 {
			ceiling = 1
		}
		cpu := "-"
		if p.CPUReadings >= 20 {
			cpu = fmt.Sprintf("%3.0f%%", p.CPURatioP50*100)
		}
		fill := "-"
		if p.CPUCeiling > 0 {
			fill = fmt.Sprintf("%d/%d", p.CPUFill, p.CPUCeiling)
		}
		limit := sDim.Render("-")
		switch {
		case p.CPUBound:
			limit = sAccent.Render("held")
		case p.CPULimited:
			limit = sWarn.Render("cpu")
		case p.CPUReadings >= 20:
			limit = sOK.Render("memory")
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
			sparkCell = colorSpark(series, sparkWidth, ceiling) + "  "
		}
		row := fmt.Sprintf("%-14s %6d %s %5d %s  %s%6s  %8s  %s",
			trunc(name, 14), p.Active, queue, p.Configured, plan,
			sparkCell, cpu, fill, limit)
		if i == m.selected {
			row = sRowSel.Render(row)
		}
		lines = append(lines, row)
	}

	return strings.Join(lines, "\n")
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
		sNow.Render("●") + " now " + fmt.Sprintf("%d", last.Configured),
		sPlan.Render("●") + " plan " + fmt.Sprintf("%d", last.Recommended),
	}
	if last.MemoryCeiling > 0 {
		legend = append(legend, sMemory.Render("●")+" memory ceiling "+fmt.Sprintf("%d", last.MemoryCeiling))
	}
	if last.CPUCeiling > 0 {
		legend = append(legend, sWarn.Render("●")+" cpu ceiling "+fmt.Sprintf("%d", last.CPUCeiling))
	}
	if last.CPUBound {
		legend = append(legend, sAccent.Render("held at the ceiling"))
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
	tail := sHeader.Render("queue   ") + pad(queueVal, labelW) + "  " + colorSpark(queue, sparkW, float64(max(last.Configured, 1))) + "\n" +
		sHeader.Render("cpu     ") + pad(cpuVal, labelW) + "  " + colorSpark(cpu, sparkW, 1)

	return head + "\n" + strings.Join(legend, "   ") + "\n" + c.View() + "\n" + tail
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

// cpuLabel is the cpu row's value: the share, the fill count and the ceiling;
// shorter words on a narrow terminal.
func cpuLabel(p serve.PoolSample, short bool) string {
	if p.CPUReadings < 20 {
		return sDim.Render("too few readings yet")
	}
	share := fmt.Sprintf("%.0f%% of a request on CPU", p.CPURatioP50*100)
	fill := fmt.Sprintf(" · %d fill the box · ceiling %d", p.CPUFill, p.CPUCeiling)
	if short {
		share = fmt.Sprintf("%.0f%% cpu", p.CPURatioP50*100)
		fill = fmt.Sprintf(" · %d fill · cap %d", p.CPUFill, p.CPUCeiling)
	}
	s := share
	if p.CPUCeiling > 0 {
		s += fill
	}

	return s
}

func (m model) eventsPanel(width int) string {
	events := m.resp.Events
	lines := []string{sHeader.Render("EVENTS")}
	if len(events) == 0 {
		lines = append(lines, sDim.Render("no events since the daemon started"))
	}
	max := 6
	if m.height > 44 {
		max = 12
	}
	if len(events) > max {
		events = events[len(events)-max:]
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		var what string
		switch e.Kind {
		case serve.EventResized:
			arrow := sOK.Render("↑")
			if e.To < e.From {
				arrow = sWarn.Render("↓")
			}
			what = fmt.Sprintf("%s %s %d → %d", arrow, sAccent.Render(e.Pool), e.From, e.To)
		case serve.EventChanged:
			what = fmt.Sprintf("%s %s %d → %d", sDim.Render("⇄"), sAccent.Render(e.Pool), e.From, e.To)
		case serve.EventRepaired:
			what = sOK.Render("repaired the host")
		case serve.EventRolledBack:
			what = sWarn.Render("rolled back")
		default:
			what = sBad.Render(strings.ReplaceAll(e.Kind, "_", " "))
		}
		detail := ""
		if e.Detail != "" {
			detail = sDim.Render("  " + trunc(e.Detail, width-40))
		}
		lines = append(lines, sDim.Render(e.At.Local().Format("15:04:05"))+"  "+what+detail)
	}

	return strings.Join(lines, "\n")
}

// confirmPanel is what Enter would do: the plan's changes, pool by pool,
// and the command that makes them.
func (m model) confirmPanel(width int) string {
	lines := []string{sAccent.Render("APPLY THE PLAN?") + sDim.Render("  Enter asks the daemon to apply it now, Esc cancels")}
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
			why = sDim.Render("  held at the cpu ceiling")
		}
		lines = append(lines, fmt.Sprintf("  %s %-14s %d → %d%s", arrow, p.Pool, p.Configured, p.Recommended, why))
	}
	// The command by its base name: the path is this binary's own and adds
	// nothing but length.
	shown := applyArgs(filepath.Base(self()), os.Geteuid() == 0)
	lines = append(lines, sDim.Render(trunc("runs: "+strings.Join(shown, " "), width)))

	return strings.Join(lines, "\n")
}

func (m model) keys() string {
	k := func(key, what string) string { return sKey.Render(key) + sDim.Render(" "+what) }
	items := []string{k("↑↓", "pool"), k("1", "hour"), k("2", "six hours"), k("3", "all"), k("a", "apply"), k("r", "refresh"), k("q", "quit")}

	return "  " + strings.Join(items, "   ")
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
// is: blue when there is room, amber past 70%, red at the top.
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

func trunc(s string, n int) string {
	if n < 4 || len(s) <= n {
		return s
	}

	return s[:n-1] + "…"
}
