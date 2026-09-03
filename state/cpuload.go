package state

import (
	"math"
	"time"
)

// What a busy worker costs the BOX, measured rather than assumed.
//
// php-fpm's per-request figure (cpu.go) says what a request costs inside its
// worker. It says nothing about the MySQL query the request waited on, the
// nginx that proxied it, or the kernel work outside the worker. How much that
// is depends on the host — a tenth again on one four-core Laravel box, more
// where the pages lean on the database — and guessing it in either direction
// mis-sizes the pool, so it is measured.
//
// So each scrape also reads the box's total CPU time, and each pool's workers'
// own CPU time, and regresses the one on the other over the natural spread of
// traffic: host busy cores ≈ base + overhead × pool cores. The slope is how
// much of the box one PHP core drags along with it. Fitted per pool, on the
// scrapes where that pool was the one doing the work, so a quiet neighbour
// does not get charged for a busy one.
//
// It needs no load test. A week of ordinary traffic has peaks and troughs, and
// the fit only asks for spread.

// Fit is a running least-squares line, y on x, with a forgetting factor so a
// pool redeployed last month is not described by the application it used to
// run. Five numbers, all sums; the line is derived when asked.
type Fit struct {
	N     float64 `json:"n,omitempty"`
	SumX  float64 `json:"sum_x,omitempty"`
	SumY  float64 `json:"sum_y,omitempty"`
	SumXX float64 `json:"sum_xx,omitempty"`
	SumXY float64 `json:"sum_xy,omitempty"`
}

// fitForget is applied to every sum before a point is added: the effective
// window is about 1/(1-fitForget) points, four hundred scrapes — a few hours at
// the daemon's interval, long enough for a day's peaks to be in it.
const fitForget = 0.9975

// Add folds one point in.
func (f *Fit) Add(x, y float64) {
	f.N = f.N*fitForget + 1
	f.SumX = f.SumX*fitForget + x
	f.SumY = f.SumY*fitForget + y
	f.SumXX = f.SumXX*fitForget + x*x
	f.SumXY = f.SumXY*fitForget + x*y
}

// Line returns the slope and intercept of y on x, and the standard deviation
// of x, which is what says whether the slope means anything: a fit through
// points that all sit at the same x has no slope to give.
func (f Fit) Line() (slope, intercept, sdX float64, ok bool) {
	if f.N < 2 {
		return 0, 0, 0, false
	}
	meanX := f.SumX / f.N
	meanY := f.SumY / f.N
	varX := f.SumXX/f.N - meanX*meanX
	if varX <= 0 {
		return 0, 0, 0, false
	}
	slope = (f.SumXY/f.N - meanX*meanY) / varX
	intercept = meanY - slope*meanX

	return slope, intercept, math.Sqrt(varX), true
}

// HostCPUSeen is the box's cumulative CPU time as of the last scrape, so the
// next one can take the difference.
type HostCPUSeen struct {
	BusyMicros int64     `json:"busy_micros"`
	At         time.Time `json:"at"`
}

// CPULoadSample is what one scrape learned about the box and the pools' share
// of it, handed to LearnCPULoad by the caller that read both.
type CPULoadSample struct {
	// BusyMicros is the box's cumulative CPU time; see budget.HostCPU.
	BusyMicros int64

	// Millicores is the CPU the box has, so busy time can be read as a
	// fraction of it.
	Millicores int

	At time.Time
}

// The gates on what the fit is asked to say.
const (
	// maxCPULoadGap is the longest interval between two readings across which
	// the difference is believed. Nothing was watched in a longer hole.
	maxCPULoadGap = 5 * time.Minute

	// minCPULoadGap keeps two scrapes a second apart from producing a
	// division by almost nothing.
	minCPULoadGap = 5 * time.Second

	// dominantShare is how much of the scrape's total pool CPU one pool must
	// account for before the box's busy time is attributed to it.
	dominantShare = 0.7

	// idleCores is a scrape's total pool CPU below which the box is taken to be
	// idle of PHP, and its busy time is the base the fit's intercept describes.
	idleCores = 0.02

	// starvedBusyRatio is the box busy fraction at or above which a queue is a
	// queue the CPU caused.
	starvedBusyRatio = 0.95
)

// LearnCPULoad folds one scrape's CPU readings into the box-cost fits.
//
// Each pool's workers carry their cumulative CPU ticks (own plus reaped
// children); the per-pool difference since the last scrape, over the wall time
// between them, is the cores that pool used. The box's difference over the
// same time is the cores everything used. A pool that did most of the PHP work
// this interval gets the point (its cores, the box's cores); an interval with
// no PHP work at all gives every pool the point (0, the box's cores), which is
// the base load the intercept absorbs.
//
// It also counts the rounds a pool had requests queued while the box was
// full: the direct observation that another worker would not have helped.
func (s *State) LearnCPULoad(obs []Observation, sample CPULoadSample) {
	at := sample.At
	if at.IsZero() {
		at = time.Now()
	}

	// Each pool's own CPU this interval, from its workers' counters. Done
	// before the host delta is checked, so the per-pid counters are kept
	// current on every scrape whatever the box said.
	cores := make([]float64, len(obs))
	var total float64
	var wall time.Duration
	if s.HostCPU != nil {
		wall = at.Sub(s.HostCPU.At)
	}
	for i, o := range obs {
		ps := s.forWriting(o.MasterConfig, o.Pool, at)
		ticks := ps.poolCPUTicks(o.Workers)
		if wall > 0 {
			cores[i] = float64(ticks) / 100 / wall.Seconds()
			total += cores[i]
		}
	}

	prev := s.HostCPU
	s.HostCPU = &HostCPUSeen{BusyMicros: sample.BusyMicros, At: at}
	if prev == nil {
		return
	}
	hostCores, busyRatio, ok := HostBusy(*prev, *s.HostCPU, sample.Millicores)
	if !ok {
		return
	}

	for i, o := range obs {
		ps := s.forWriting(o.MasterConfig, o.Pool, at)
		switch {
		case total < idleCores:
			ps.BoxCost.Add(0, hostCores)
		case cores[i] >= dominantShare*total:
			ps.BoxCost.Add(cores[i], hostCores)
		}
		if o.QueueDepth > 0 && busyRatio >= starvedBusyRatio {
			ps.CPUStarvedRounds++
		}
	}
}

// HostBusy turns two consecutive box CPU readings into the cores busy in
// between, and that as a share of the cores the box has (clamped at one: a
// box cannot be busier than it is, and a reading straddling a quota change
// says otherwise). Unknown across a gap shorter than minCPULoadGap or longer
// than maxCPULoadGap, when the counter went backwards (a reboot, or a
// different source than last time), and when the box has no known capacity.
func HostBusy(prev, now HostCPUSeen, millicores int) (cores, ratio float64, ok bool) {
	wall := now.At.Sub(prev.At)
	busyMicros := now.BusyMicros - prev.BusyMicros
	if wall < minCPULoadGap || wall > maxCPULoadGap || busyMicros < 0 || millicores <= 0 {
		return 0, 0, false
	}
	cores = float64(busyMicros) / float64(wall.Microseconds())
	ratio = min(cores/(float64(millicores)/1000), 1)

	return cores, ratio, true
}

// poolCPUTicks is the CPU this pool's workers spent since the last scrape, in
// USER_HZ ticks, from each worker's cumulative counter. A pid not seen before
// contributes everything it has, which is its whole life so far — it was born
// inside the interval. A pid whose counter went DOWN is a new worker wearing an
// old number, and is treated the same way. A worker that exited between two
// scrapes takes what it spent since the last one with it: an undercount, on
// the side that says the box costs less, which is the side that adds workers.
func (ps *PoolState) poolCPUTicks(workers []WorkerSample) int64 {
	seen := make(map[int]int64, len(workers))
	var delta int64
	for _, w := range workers {
		if w.PID <= 0 || w.CPUTicks < 0 {
			continue
		}
		seen[w.PID] = w.CPUTicks
		if prev, ok := ps.CPUTicksSeen[w.PID]; ok && w.CPUTicks >= prev {
			delta += w.CPUTicks - prev
		} else {
			delta += w.CPUTicks
		}
	}
	ps.CPUTicksSeen = seen
	if len(ps.CPUTicksSeen) == 0 {
		ps.CPUTicksSeen = nil
	}

	return delta
}

// BoxOverhead is how many cores the box spends for every core this pool's
// workers spend: 1.0 for a pool whose requests cost the box nothing beyond
// PHP, 2.1 for one that spends as much again in MySQL and nginx. Known only
// once the fit has seen enough spread; until then the caller uses PHP's own
// figure and says so.
func (ps *PoolState) BoxOverhead(opts Options) (float64, bool) {
	opts = opts.Defaults()
	slope, _, sdX, ok := ps.BoxCost.Line()
	if !ok || ps.BoxCost.N < float64(opts.MinBoxCostSamples) || sdX < opts.MinBoxCostSpread {
		return 0, false
	}
	// Below one the fit says the box spends less than the pool did, which is
	// noise; above the ceiling it is a pool whose traffic happens to coincide
	// with something else entirely, and no worker count fixes that.
	if slope < 1 {
		slope = 1
	}
	if slope > maxBoxOverhead {
		return 0, false
	}

	return slope, true
}

// maxBoxOverhead is the slope past which the fit is describing something other
// than this pool's requests.
const maxBoxOverhead = 20
