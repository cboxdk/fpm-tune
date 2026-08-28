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

		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d%s\t%s\t%s\n",
			p.Name, now, p.MaxChildren, marker, budget.HumanBytes(p.Bytes), p.Reason)
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

	for _, warning := range r.Plan.Warnings {
		fmt.Fprintf(&b, "\nWARNING: %s\n", warning)
	}

	if r.Plan.CapacityExhausted {
		fmt.Fprintf(&b, "\nCAPACITY EXHAUSTED — pools marked * want more workers and there is\n"+
			"nowhere left to get them. No configuration change will help: this host\n"+
			"needs more memory, or fewer sites.\n")
	} else if anyUnmet(r) {
		fmt.Fprintf(&b, "\nPools marked * want more workers than they were given, but there is\n"+
			"headroom elsewhere — a later run will rebalance toward them.\n")
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

func anyUnmet(r Result) bool {
	for _, p := range r.Plan.Pools {
		if p.DemandUnmet {
			return true
		}
	}

	return false
}
