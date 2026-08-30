---
title: Measuring workers
weight: 2
description: How the learner decides what a worker costs, why cost and permission-to-shrink are different questions, and why it believes an expensive reading instantly.
---

# Measuring workers

The learner's job is to answer one question — what does one worker of this pool
cost? — from noisy observations. Too low and the host OOMs; too high and every
site is throttled. Almost everything it does is a refusal to be fooled by a
reading that looks like evidence and is not.

## Bootstrap, then learned

With no history, a pool is sized from a workload profile — the same guess a
hand-written config makes, and honest about being a guess. As the tool
accumulates trustworthy measurements it switches to sizing each pool on its own
observed worker memory. Baselines persist to `state.json`, so a restart does not
begin from zero.

## Cost and permission are different questions

There are two things you might mean by "trust a pool's measurements", and
conflating them is a way to overcommit a host:

- **What does a worker COST?** Answered by any measurement there is. A number
  taken from this pool's own workers beats a profile's guess whatever the
  confidence, because the bytes were real.
- **May the pool be SHRUNK?** A different question. Sizing a pool *down* on a
  baseline that has not been watched through a real traffic pattern is how a tool
  like this causes the outage it was installed to prevent. Until a pool has been
  observed working, long enough and often enough, its floor holds at whatever it
  is configured for and the first run can only ever help.

So a pool can be measured (its cost is known) without being reducible (there is
not yet enough evidence to cut it). Keeping those apart is why a first run is
safe: it will grow a queueing pool on real numbers, but it will not shrink a
quiet one on thirty seconds of evidence.

## Fast up, slow down

The estimate is asymmetric on purpose. A worker that costs more than expected
puts the whole budget at risk, so a single expensive reading raises the estimate
to the full reading on the same scrape. A worker that costs *less* is only an
opportunity, so the estimate falls gradually, on a half-life measured in time —
following the day rather than being pinned to its busiest hour.

Under-sizing is the failure that ends in an OOM kill. Over-sizing costs unused
headroom on one pool. The asymmetry spends caution where the cost of being wrong
is highest.

## It never learns from an idle pool

A worker that has served three requests is far smaller than one that has served
five hundred — PHP returns large allocations to the operating system, so an idle
survivor genuinely shrinks. That smaller reading is a lull, not a cheaper
application, and believing it is how a quiet night leaves the morning sized for
workers that do not exist.

So a smaller reading only moves the estimate down when the pool was actually
*working* — measured as a request rate, not a raw count, because a count per
scrape makes the scrape interval an input. The threshold is deliberately a
cliff, placed where the wrong answer wastes memory rather than losing the host: a
pool below roughly a request a second is treated as idle, and its estimate held.

## Time only counts if it was watched

The estimate falls on a half-life, so elapsed time is the weight it falls by —
which is right while the looking is regular and wrong the moment it stops.
Restart the daemon for a package upgrade while php-fpm keeps serving, and the
first reading back is hours old; counting those hours in full would collapse the
estimate on a single sample. Each pool remembers how often it is actually looked
at, and a gap can never move the estimate further than one ordinary scrape would.

## Small pools are measured too

Two shapes of pool would otherwise be invisible and get a profile guess for ever:

- **A pool that never runs two workers at once** — an ondemand site at low
  traffic. Its readings count toward what it *costs* (so the host is budgeted for
  it) but not toward permission to shrink it (a single worker is a measurement,
  not a traffic pattern).
- **A pool that recycles its workers before they warm up** — a low
  `pm.max_requests`, so no worker ever serves enough requests to have loaded the
  application. After a long stretch of learning nothing, its young workers are
  read anyway. A young worker is worse evidence than an old one, and much better
  evidence than a table — but such a reading may only ever *raise* the estimate,
  never lower it, because a cold worker's small footprint is not evidence the
  application got cheaper.

## The distribution, alongside the estimate

The sizing number is one number, because the allocator needs one cost per worker.
But one number cannot answer the question you ask when deciding by hand: how bad
does this pool get? A pool whose median worker is 60MiB with a p99 of 400MiB is a
different pool from one that sits flat at 90MiB, and the estimate hides the first
while the high-water mark is only the second.

So each pool also keeps a log-spaced histogram of every worker reading, and
reports its median, p95, p99 and worst-seen — in `plan`, in the recommendation
file, and on `/metrics` as `estimate="p50"/"p95"/"p99"`. It is a description of
what has been seen, kept deliberately apart from what the tool decides to
reserve, and it forgets: an all-time histogram of a pool redeployed six months
ago describes an application that no longer exists, so it fades as new readings
arrive.
