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
	fmt.Fprintf(&b, "  reserved for the system: %s (%s)\n",
		budget.HumanBytes(r.Reserve), r.ReserveReason)
	fmt.Fprintf(&b, "  available to workers:    %s\n\n",
		budget.HumanBytes(r.Plan.TotalBytes-r.Reserve))

	if len(r.Plan.Pools) == 0 {
		fmt.Fprintf(&b, "No PHP-FPM pools found.\n")
		_, err := io.WriteString(w, b.String())

		return err
	}

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "POOL\tNOW\tPLAN\tMEMORY\tWHY")

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

		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			p.Name, now, size, budget.HumanBytes(p.Bytes), p.Reason)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(&b, "\nallocated %s of %s, %s free\n",
		budget.HumanBytes(r.Plan.AllocatedBytes),
		budget.HumanBytes(r.Plan.TotalBytes-r.Reserve),
		budget.HumanBytes(r.Plan.FreeBytes))

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

	_, err := io.WriteString(w, b.String())

	return err
}

func currentOf(r Result, name string) int {
	for _, v := range r.Views {
		if v.Name == name {
			return v.CurrentMaxChildren
		}
	}

	return 0
}
