---
title: Dividing the budget
weight: 2
description: The allocator: floors first, then demand to the pools that are queueing, and what happens when the host is out of capacity.
---

# Dividing the budget

Given the memory available to workers, the pools, and each pool's per-worker
cost, the allocator returns a plan: how many workers each pool gets. This page
is how it decides, and what it does when the numbers do not fit.

The allocator is pure computation, with no I/O and no clock. The same inputs
always give the same plan, and the one invariant it holds is that a plan never
commits more memory than the budget. A randomised sweep over hundreds of
thousands of generated plans checks it.

## Floors first

Every pool gets a floor before anything is handed out by demand, so a busy pool
cannot starve a quiet one into being unable to answer at all. A pool's floor is
its configured ceiling until it has been watched under load long enough to be
cut (see [cost and permission](measuring-workers.md#cost-and-permission));
after that the default floor is 2 workers. A pool whose configuration could not
be read is floored at the larger of its configured ceiling and its remembered
peak, and is never written.

## Then demand

What is left after the floors goes to what each pool wants. A pool that has
never run out of workers wants its peak concurrency plus 25% slack, so ordinary
variation does not queue. A pool that hit its ceiling cannot show how much more
it wanted, so it grows to 1.5 times its current size (or its peak, whichever is
larger), bounded at 1.5 times the peak-plus-slack so the sequence converges
instead of feeding on itself. A coarse bound of 50 workers per core caps any
pool, whatever memory allows.

Pools that are queueing right now, or that hit their ceiling since the last
look, are served first, and among them the cheapest fix first: the pool whose
shortfall costs the fewest bytes, because that takes the most sites out of
their queue for the memory there is. Ordered by worker count instead, a pool
two workers short at 100 MiB each would take one worker and stay queued, while
the same 100 MiB would have cleared a pool five workers short at 20 MiB. The
rest share what remains in proportion to their gaps.

## When the floors do not fit

On a host oversubscribed before tuning begins, refusing outright would leave
whatever is configured in place, which is worse. So the floors are scaled back
in proportion, never below one worker, and the plan warns that the host is
oversubscribed and by how much.

Two kinds of pool are protected even then. A pool without a trusted baseline is
not cut on a guess, so only the pools with one give way while they can absorb
the shortfall. A pool that cannot be written keeps its floor whatever happens,
because cutting it frees memory on paper only: the bytes go to a neighbour that
is written, and the untouched pool comes back at the size it always had.

If one worker per pool does not fit, no arrangement is valid. The allocator
refuses and names the most expensive pool, because on a host with many sites
the total says it is impossible and only the name says which pool made it so.

## Out of capacity

A pool still short in a finished plan is short because the budget ran out. The
slack has already been taken from the quiet pools and given to the busy ones by
the time a plan exists, so the next round will not rearrange anything, and
moving a shortfall between pools would cost a reload each without making it
smaller. The plan marks such pools with `*` and prints a block saying the host
is out of capacity; [reading a plan](../getting-started/reading-a-plan.md) has
the line. On `/metrics` it is `fpm_tune_pool_demand_unmet{pool}` and
`fpm_tune_capacity_exhausted` (see
[metrics and alerting](../operating/metrics-and-alerting.md)). The host needs
more memory, or fewer sites.
