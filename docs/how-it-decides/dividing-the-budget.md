---
title: Dividing the budget
weight: 4
description: The allocator. Floors first, then the pools a shortage is actually hurting, cheapest fix first; and what happens when the floors do not fit.
---

# Dividing the budget

Given a budget, an inventory of pools, and each pool's measured cost, the
allocator returns a plan: how many workers each pool gets. It is pure
computation with no I/O, which is what makes it exhaustively testable. The
one invariant it must never break is that a plan never commits more memory than
the budget.

## Floors first

Every pool gets a floor before anything is distributed by demand: enough
workers to serve at all, so a busy pool cannot starve a quiet one into being
unable to answer a single request. A pool's floor is what it is configured for
until the tool has enough evidence to justify cutting it; see
[cost versus permission](measuring-workers.md#cost-and-permission-are-different-questions).

## Then demand, to the pools a shortage is hurting

Whatever is left after the floors is handed out by demand. This is the whole
argument for dividing one budget across pools rather than sizing each alone: take
headroom from an idle pool and give it to a busy one.

But "busy" has to mean the right thing. A pool's *gap* is how far it is from what
it would like; a pool's *listen queue* is requests waiting right now. The
allocator serves the pools that are actually queueing first (a pool at its
ceiling is turning its queue into latency someone is measuring), and among those,
the **cheapest fix first**: the pool that needs the fewest bytes to come out of
the queue, because that resolves the most sites with the memory available.

Getting this wrong is not abstract. Ordered by worker *count* instead of *cost*,
a pool two workers short at 100MiB each would sort ahead of one five workers
short at 20MiB, take a single worker, and stay queued, while the same 100MiB
would have taken the second pool entirely out of the queue. Two sites down
instead of one.

## Saturation grows a pool past what it has been seen to need

A pool that keeps hitting `pm.max_children` cannot be seen to need more (the
ceiling is what is stopping it), so the tool grows it off its own previous size
by a step, bounded by the evidence so the sequence converges instead of
compounding. Without that bound, raising the ceiling lets the pool fill it, which
raises the ceiling again: positive feedback that stops only when the host runs
out of memory.

## When the floors do not fit

On a host oversubscribed before tuning even begins (the floors themselves add up
to more than the budget), refusing outright would leave whatever is configured in
place, which is worse. So the floors are scaled back proportionally, never below
one worker, and the plan says the host is oversubscribed and by how much.

Two kinds of pool are protected even here:

- **A pool with no trusted baseline** is not cut on a guess. Scaling everything
  uniformly would cut healthy pools that nobody had reason to touch.
- **A pool that cannot be written** (one whose current configuration could not
  be read) is reserved at what it holds and never cut, because cutting it frees
  memory on paper only: the tool skips it, the bytes go to a pool that *is*
  written, and the untouched pool comes back at the size it always had, leaving
  the host over budget by exactly the fiction.

If even one worker per pool will not fit, no arrangement is valid. The tool
refuses, and names the most expensive pool, because on a host with many sites,
the total says it is impossible but only the name says which pool made it so.

## Telling "needs more" from "machine is full"

Two metrics, and either one is the signal that no configuration change will help:

```
fpm_tune_pool_demand_unmet{pool}   # this pool wanted more than it got
fpm_tune_capacity_exhausted        # ...and that is true of at least one pool
```

By the time a plan exists, headroom has already been taken from the idle pools
and given to the busy ones, so a pool still short in the finished plan is short
because the budget ran out, not because the next run might rearrange something.
In that state the tool stops rearranging and says so: moving a shortfall between
pools costs a reload of each and does not make it smaller. The machine needs more
RAM, or fewer sites.
