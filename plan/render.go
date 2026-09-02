package plan

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/cboxdk/fpm-tune/budget"
)

// Render writes the plan for a human.
//
// The rationale column is not decoration. This tool proposes changes to a
// running server's configuration, and an operator who cannot see why a pool is
// being cut from thirty workers to eight has no basis for letting it happen.
// Every number here is followed by the evidence for it.
func (r Result) Render(w io.Writer) error {
	var b strings.Builder

	fmt.Fprintf(&b, "%s\n", r.Budget.Describe())
	// The reserve is a percentage headroom plus, on a shared host, the memory other
	// services and the OS are using. Shown apart so the smaller "available to
	// workers" is not a mystery: one line the operator tunes (the headroom), one
	// they cannot (what the neighbours hold).
	if r.Budget.NeighborBytes > 0 {
		fmt.Fprintf(&b, "  used by other services:  %s (left for them; cap php-fpm's cgroup for a hard limit)\n",
			budget.HumanBytes(r.Budget.NeighborBytes))
	}
	fmt.Fprintf(&b, "  headroom kept:           %s (%s)\n",
		budget.HumanBytes(r.Reserve-r.Budget.NeighborBytes), r.ReserveReason)
	fmt.Fprintf(&b, "  available to workers:    %s\n\n",
		budget.HumanBytes(r.Plan.TotalBytes-r.Reserve))

	if len(r.Plan.Pools) == 0 {
		fmt.Fprintf(&b, "No PHP-FPM pools found.\n")
		_, err := io.WriteString(w, b.String())

		return err
	}

	// Mode per pool, so the number reads in context: pm.max_children means a
	// different thing for a static pool (the running worker count) than for an
	// ondemand one (a ceiling it spawns up to).
	modeOf := make(map[string]string, len(r.Views))
	for _, v := range r.Views {
		modeOf[v.Name] = v.ProcessManager
	}

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "POOL\tMODE\tNOW\tPLAN\tMEMORY\tWHY")

	// Sorted by name so two runs against the same host are diffable.
	ordered := make([]int, len(r.Plan.Pools))
	for i := range ordered {
		ordered[i] = i
	}
	sort.Slice(ordered, func(a, b int) bool {
		return r.Plan.Pools[ordered[a]].Name < r.Plan.Pools[ordered[b]].Name
	})

	for _, idx := range ordered {
		p := r.Plan.Pools[idx]
		now := "-"
		if cur := currentOf(r, p.Name); cur > 0 {
			now = fmt.Sprintf("%d", cur)
		}

		marker := ""
		if p.DemandUnmet {
			marker = " *"
		}

		// A pool whose configuration could not be read is accounted for in the
		// budget and cannot be WRITTEN: setting a ceiling means replacing a known
		// one. Printing a plan number for it in the same column as the pools that
		// will change reads as a promise, and the operator comes back to find that
		// pool exactly as it was.
		size := fmt.Sprintf("%d%s", p.MaxChildren, marker)
		if p.Unknown {
			size = "—"
		}

		mode := modeOf[p.Name]
		if mode == "" {
			mode = "?"
		}

		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			p.Name, mode, now, size, budget.HumanBytes(p.Bytes), p.Reason)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(&b, "\nallocated %s of %s, %s free\n",
		budget.HumanBytes(r.Plan.AllocatedBytes),
		budget.HumanBytes(r.Plan.TotalBytes-r.Reserve),
		budget.HumanBytes(r.Plan.FreeBytes))

	if len(r.Distribution) > 0 {
		fmt.Fprintf(&b, "\nWorker memory, as measured:\n")

		dt := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(dt, "  POOL\tMEDIAN\tP95\tP99\tWORST SEEN\tREADINGS")
		for _, d := range r.Distribution {
			_, _ = fmt.Fprintf(dt, "  %s\t%s\t%s\t%s\t%s\t%d\n",
				d.Name, budget.HumanBytes(d.P50), budget.HumanBytes(d.P95),
				budget.HumanBytes(d.P99), budget.HumanBytes(d.WorstSeen), d.Samples)
		}
		if err := dt.Flush(); err != nil {
			return err
		}

		fmt.Fprintf(&b, "  Sizing uses neither of these directly — it follows the typical\n"+
			"  peak, which rises fast and falls on a half-life. The spread is here for\n"+
			"  the decision you are making by hand: a pool whose p99 is far above its\n"+
			"  median has a tail, and a tail is what fills a host at the wrong moment.\n")
	}

	if len(r.CPU) > 0 {
		fmt.Fprintf(&b, "\nCPU per request, as measured:\n")

		ct := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(ct, "  POOL\tTYPICAL\tP90\tREADINGS\tPER WORKER\tLIMIT\tWHY")
		for _, c := range r.CPU {
			_, _ = fmt.Fprintf(ct, "  %s\t%s\t%s\t%d\t%s\t%s\t%s\n",
				c.Name, c.Percent(c.P50), c.Percent(c.P90), c.Samples,
				c.PerWorker(), cpuLimit(c), cpuWhy(c, r.HostCPU.Millicores))
		}
		if err := ct.Flush(); err != nil {
			return err
		}

		if h := r.HostCPU; h.Known > 0 && h.Millicores > 0 {
			fmt.Fprintf(&b, "  If every measured pool ran its ceiling busy at once: %s now, %s at this\n"+
				"  plan, against %s.\n",
				budget.HumanMillicores(h.NeededNow), budget.HumanMillicores(h.NeededAtPlan),
				budget.HumanMillicores(h.Millicores))
		}
		if r.CPUCeiling {
			fmt.Fprintf(&b, "  --cpu is on: a cpu-limited pool is held at the busy workers that fill the\n"+
				"  CPU, and its row in the plan table says so.\n")
		} else {
			fmt.Fprintf(&b, "  Sizing uses memory. A cpu-limited pool only gets slower past the workers\n"+
				"  that fill the CPU; pass --cpu to hold it there.\n")
		}
	}

	if len(r.Bootstrapped) > 0 {
		fmt.Fprintf(&b, "\nEstimated, not yet measured: %s\n", strings.Join(r.Bootstrapped, ", "))
		fmt.Fprintf(&b, "  These pools have not been watched long enough to size from their own\n"+
			"  memory use. Leave fpm-tune running and the numbers become measurements.\n")
	}

	if len(r.Unreachable) > 0 {
		fmt.Fprintf(&b, "\nCould not be read: %s\n", strings.Join(r.Unreachable, ", "))
		fmt.Fprintf(&b, "  Their current allocation is left alone. A pool that is merely restarting\n"+
			"  must not have its memory handed to its neighbours.\n")
	}

	// Said only when it does not fit, and said as what it is: not a prediction,
	// but the ceiling nobody was checking.
	if allocatable := r.Plan.TotalBytes - r.Reserve; r.WorstCaseBytes > allocatable {
		fmt.Fprintf(&b, "\nIf every pool filled its ceiling with the largest worker ever seen\n"+
			"from it, this plan would need %s against %s. That is a rare\n"+
			"combination and not what the sizing assumes — but if this host OOMs, it is\n"+
			"the arithmetic to look at.\n",
			budget.HumanBytes(r.WorstCaseBytes), budget.HumanBytes(allocatable))
	}

	for _, warning := range r.Plan.Warnings {
		fmt.Fprintf(&b, "\nWARNING: %s\n", warning)
	}

	// One arm, because there is one state.
	//
	// The second used to say a later run would rebalance toward the pools marked
	// *, and it could not be reached: the allocator sets CapacityExhausted from
	// the same unmet demand the marks come from. The rebalancing has already
	// happened by the time a plan exists — headroom was taken from the idle
	// pools and given to the busy ones — so a pool still short in a FINISHED
	// plan is short because the budget ran out, and telling an operator to wait
	// for the next run would have been advice to wait for nothing.
	if r.Plan.CapacityExhausted {
		fmt.Fprintf(&b, "\nCAPACITY EXHAUSTED — pools marked * want more workers and there is\n"+
			"nowhere left to get them: %s free against the %s one more worker would\n"+
			"cost the cheapest of them. No configuration change will help; this host\n"+
			"needs more memory, or fewer sites.\n",
			budget.HumanBytes(r.Plan.FreeBytes), budget.HumanBytes(r.Plan.ShortfallBytes))
	}

	// Advisory, and last, because it changes nothing: a mode fits a workload or
	// it doesn't, and fpm-tune only ever sizes within the mode you chose.
	if len(r.Advice) > 0 {
		fmt.Fprintf(&b, "\nWorth a look — the mode these pools run may not fit their workload\n"+
			"(fpm-tune won't change it; that's your call):\n")
		for _, a := range r.Advice {
			fmt.Fprintf(&b, "  %s (%s → %s): %s\n", a.Pool, a.From, a.To, a.Why)
		}
	}

	_, err := io.WriteString(w, b.String())

	return err
}

func cpuLimit(c PoolCPU) string {
	if c.Limit == "" {
		return "-"
	}

	return c.Limit
}

// cpuWhy is the WHY column: the arithmetic beside the ceilings, so the gap
// between what fills the CPU and what the pool is allowed is on one line.
func cpuWhy(c PoolCPU, hostMillicores int) string {
	if c.Shape == "" {
		return "too few readings yet"
	}
	if c.FillWorkers == 0 {
		return c.Shape
	}

	line := fmt.Sprintf("%s; ~%d busy workers fill %s", c.Shape, c.FillWorkers, budget.HumanMillicores(hostMillicores))
	switch {
	case c.Capped:
		line += fmt.Sprintf("; held there (now %d)", c.Current)
	case c.Allowed > 0:
		line += fmt.Sprintf("; plan allows %d (now %d)", c.Allowed, c.Current)
	case c.Current > 0:
		line += fmt.Sprintf("; now %d", c.Current)
	}

	return line
}

func currentOf(r Result, name string) int {
	for _, v := range r.Views {
		if v.Name == name {
			return v.CurrentMaxChildren
		}
	}

	return 0
}
