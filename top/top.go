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
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

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
	m := newModel(opts)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	final, err := p.Run()
	if err != nil {
		return err
	}
	if fm, ok := final.(model); ok && fm.fatal != nil {
		return fm.fatal
	}

	return nil
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
	cSpark  = lipgloss.AdaptiveColor{Light: "#2563EB", Dark: "#60A5FA"}

	sTitle  = lipgloss.NewStyle().Bold(true).Foreground(cText)
	sAccent = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	sDim    = lipgloss.NewStyle().Foreground(cDim)
	sFaint  = lipgloss.NewStyle().Foreground(cFaint)
	sOK     = lipgloss.NewStyle().Foreground(cOK)
	sWarn   = lipgloss.NewStyle().Foreground(cWarn)
	sBad    = lipgloss.NewStyle().Foreground(cBad).Bold(true)
	sSpark  = lipgloss.NewStyle().Foreground(cSpark)
	sBadge  = lipgloss.NewStyle().Padding(0, 1).Bold(true)
	sPanel  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cFaint).Padding(0, 1)
	sHeader = lipgloss.NewStyle().Foreground(cDim).Bold(true)
	sRow    = lipgloss.NewStyle()
	sRowSel = lipgloss.NewStyle().Background(lipgloss.AdaptiveColor{Light: "#EDE9FE", Dark: "#3B2A6B"}).Bold(true)
	sKey    = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
)

type tickMsg time.Time

type fetchedMsg struct {
	resp *serve.HistoryResponse
	err  error
	at   time.Time
}

// model is the whole view: what was last fetched, the terminal, and the
// operator's cursor.
type model struct {
	opts   Options
	client *http.Client

	resp    *serve.HistoryResponse
	err     error
	fetched time.Time
	fatal   error

	width, height int
	selected      int
	pools         []string // pool names in display order, from the newest round

	// window is how many rounds the charts span: 1 for the whole ring, or a
	// number of rounds. Cycled with the number keys.
	window int
}

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

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "r":
			return m, m.fetch()
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected+1 < len(m.pools) {
				m.selected++
			}
		case "1":
			m.window = 60
		case "2":
			m.window = 360
		case "3":
			m.window = 0
		}
	}

	return m, nil
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

	inner := m.width - 4 // panel border and padding
	if inner < 40 {
		inner = 40
	}

	parts := []string{
		m.titleBar(),
		sPanel.Width(inner).Render(m.hostPanel(inner)),
		sPanel.Width(inner).Render(m.poolsPanel(inner)),
	}
	if len(m.pools) > 0 {
		parts = append(parts, sPanel.Width(inner).Render(m.detailPanel(inner)))
	}
	parts = append(parts, sPanel.Width(inner).Render(m.eventsPanel(inner)), m.keys())

	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (m model) titleBar() string {
	h := m.resp.Host
	mode := sBadge.Background(cOK).Foreground(lipgloss.Color("#062E1F")).Render("advisory")
	if h.Apply {
		mode = sBadge.Background(cWarn).Foreground(lipgloss.Color("#3A2400")).Render("apply")
	}
	cpu := sDim.Render("cpu ceiling off")
	if h.CPUCeiling {
		cpu = sAccent.Render("cpu ceiling on")
	}
	stamp := "never"
	if !m.fetched.IsZero() {
		stamp = m.fetched.Format("15:04:05")
	}
	status := sDim.Render(fmt.Sprintf("updated %s · every %s", stamp, m.opts.Refresh))
	if m.err != nil {
		status = sBad.Render("stale: " + m.err.Error())
	}
	name := h.Hostname
	if name == "" {
		name = m.opts.Addr
	}
	left := sTitle.Render(" fpm-tune top ") + sAccent.Render(name) + "  " + mode + "  " + cpu
	if h.Version != "" {
		left += "  " + sDim.Render("v"+h.Version)
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", status)
}

// rounds is the slice of rounds the charts span.
func (m model) rounds() []serve.HistorySample {
	r := m.resp.Rounds
	if m.window > 0 && m.window < len(r) {
		r = r[len(r)-m.window:]
	}

	return r
}

func (m model) hostPanel(width int) string {
	h := m.resp.Host
	rounds := m.rounds()
	busy := make([]float64, 0, len(rounds))
	for _, r := range rounds {
		if r.HostBusyKnown {
			busy = append(busy, r.HostBusyRatio)
		} else {
			busy = append(busy, -1)
		}
	}
	span := spanLabel(rounds, m.resp.IntervalSeconds)
	var now string
	if len(busy) > 0 && busy[len(busy)-1] >= 0 {
		now = fmt.Sprintf("%3.0f%%", busy[len(busy)-1]*100)
	} else {
		now = "  -"
	}

	line1 := sHeader.Render("HOST  ") +
		fmt.Sprintf("%s memory · %s", budget.HumanBytes(h.MemoryBytes), budget.HumanMillicores(h.CPUMillicores)) +
		sDim.Render("  ("+h.Source+")")
	sparkWidth := width - 22
	if sparkWidth < 10 {
		sparkWidth = 10
	}
	line2 := sHeader.Render("CPU   ") + colorSpark(busy, sparkWidth, 1) + " " + busyStyle(busy).Render(now) + sDim.Render("  "+span)

	return line1 + "\n" + line2
}

func busyStyle(busy []float64) lipgloss.Style {
	if len(busy) == 0 || busy[len(busy)-1] < 0 {
		return sDim
	}
	switch v := busy[len(busy)-1]; {
	case v >= 0.9:
		return sBad
	case v >= 0.7:
		return sWarn
	default:
		return sOK
	}
}

func (m model) poolsPanel(width int) string {
	rounds := m.rounds()
	last := rounds[len(rounds)-1]
	byName := make(map[string]serve.PoolSample, len(last.Pools))
	for _, p := range last.Pools {
		byName[p.Pool] = p
	}

	sparkWidth := 16
	if width < 96 {
		sparkWidth = 8
	}
	header := fmt.Sprintf("%-14s %6s %5s %5s %5s  %-*s  %6s  %8s  %-6s",
		"POOL", "BUSY", "QUEUE", "NOW", "PLAN", sparkWidth, "BUSY OVER TIME", "CPU", "FILL/CAP", "LIMIT")
	lines := []string{sHeader.Render(header)}
	for i, name := range m.pools {
		p := byName[name]
		series := make([]float64, 0, len(rounds))
		for _, r := range rounds {
			series = append(series, poolValue(r, name, func(s serve.PoolSample) float64 { return float64(s.Active) }))
		}
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
		row := fmt.Sprintf("%-14s %6d %s %5d %s  %s  %6s  %8s  %s",
			trunc(name, 14), p.Active, queue, p.Configured, plan,
			colorSpark(series, sparkWidth, ceiling), cpu, fill, limit)
		if i == m.selected {
			row = sRowSel.Render(row)
		} else {
			row = sRow.Render(row)
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

func (m model) detailPanel(width int) string {
	name := m.pools[m.selected]
	rounds := m.rounds()
	var last serve.PoolSample
	for _, p := range rounds[len(rounds)-1].Pools {
		if p.Pool == name {
			last = p
		}
	}
	sparkWidth := width - 24
	if sparkWidth < 10 {
		sparkWidth = 10
	}
	series := func(f func(serve.PoolSample) float64) []float64 {
		out := make([]float64, 0, len(rounds))
		for _, r := range rounds {
			out = append(out, poolValue(r, name, f))
		}

		return out
	}
	active := series(func(s serve.PoolSample) float64 { return float64(s.Active) })
	queue := series(func(s serve.PoolSample) float64 { return float64(s.Queue) })
	cpu := series(func(s serve.PoolSample) float64 {
		if s.CPUReadings < 20 {
			return -1
		}

		return s.CPURatioP50
	})
	recommended := series(func(s serve.PoolSample) float64 { return float64(s.Recommended) })

	head := sHeader.Render("POOL  ") + sAccent.Render(name) + sDim.Render(fmt.Sprintf("  %s/worker · %d cpu readings · %s",
		budget.HumanBytes(last.WorkerBytes), last.CPUReadings, spanLabel(rounds, m.resp.IntervalSeconds)))
	ceiling := float64(last.Configured)
	if ceiling <= 0 {
		ceiling = maxOf(active)
	}
	row := func(label string, s []float64, scale float64, val string) string {
		return sHeader.Render(fmt.Sprintf("%-6s", label)) + colorSpark(s, sparkWidth, scale) + " " + val
	}
	lines := []string{
		head,
		row("busy", active, ceiling, fmt.Sprintf("%d of %d", last.Active, last.Configured)),
		row("plan", recommended, maxOf(recommended), fmt.Sprintf("%d", last.Recommended)),
		row("queue", queue, maxOf(queue), fmt.Sprintf("%d", last.Queue)),
		row("cpu", cpu, 1, cpuLabel(last)),
	}

	return strings.Join(lines, "\n")
}

func cpuLabel(p serve.PoolSample) string {
	if p.CPUReadings < 20 {
		return sDim.Render("too few readings yet")
	}
	s := fmt.Sprintf("%.0f%% of a request on CPU", p.CPURatioP50*100)
	if p.CPUCeiling > 0 {
		s += fmt.Sprintf(" · %d fill the box · ceiling %d", p.CPUFill, p.CPUCeiling)
	}
	if p.CPUBound {
		s += " · " + sAccent.Render("held there")
	}

	return s
}

func (m model) eventsPanel(width int) string {
	events := m.resp.Events
	lines := []string{sHeader.Render("EVENTS")}
	if len(events) == 0 {
		lines = append(lines, sDim.Render("nothing applied since the daemon started"))
	}
	max := 6
	if m.height > 40 {
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

func (m model) keys() string {
	k := func(key, what string) string { return sKey.Render(key) + sDim.Render(" "+what) }

	return "  " + strings.Join([]string{k("↑↓", "pool"), k("1", "last hour"), k("2", "6 hours"), k("3", "all"), k("r", "refresh"), k("q", "quit")}, "   ")
}

// spanLabel says how much time the charts cover.
func spanLabel(rounds []serve.HistorySample, intervalSeconds float64) string {
	if len(rounds) < 2 {
		return "1 round"
	}
	d := rounds[len(rounds)-1].At.Sub(rounds[0].At)
	if d <= 0 {
		d = time.Duration(float64(len(rounds)) * intervalSeconds * float64(time.Second))
	}
	switch {
	case d < 2*time.Minute:
		return fmt.Sprintf("last %.0fs", d.Seconds())
	case d < 2*time.Hour:
		return fmt.Sprintf("last %.0fm", d.Minutes())
	default:
		return fmt.Sprintf("last %.1fh", d.Hours())
	}
}

// The sparkline. Eight block heights, one column per bucket of rounds, with
// the newest at the right. A value below zero is "unknown" and draws as a dot,
// so a hole in the history looks like a hole and not like zero.
var sparkChars = []rune("▁▂▃▄▅▆▇█")

// spark resamples a series to width columns, taking the maximum in each
// bucket so a spike is never averaged away, and scales it to scale (the value
// that fills the column). Values above scale clip at the top.
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
	// Newest at the right: the last `width` buckets of the series, each bucket
	// covering len/width values when there are more values than columns.
	per := float64(len(series)) / float64(width)
	if per < 1 {
		per = 1
	}
	for col := width - 1; col >= 0; col-- {
		hi := len(series) - int(float64(width-1-col)*per)
		lo := hi - int(per)
		if lo < 0 {
			lo = 0
		}
		if hi <= 0 {
			break
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
				b.WriteString(sSpark.Render(string(r)))
			}
		}
	}

	return b.String()
}

func maxOf(s []float64) float64 {
	m := 0.0
	for _, v := range s {
		if v > m {
			m = v
		}
	}
	if m <= 0 {
		return 1
	}

	return m
}

func trunc(s string, n int) string {
	if n < 4 || len(s) <= n {
		return s
	}

	return s[:n-1] + "…"
}

// ErrNoDaemon is what Run returns when the address never answered.
var ErrNoDaemon = errors.New("no daemon answered")
