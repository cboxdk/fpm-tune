---
title: CPU per request
weight: 7
description: The dimension memory cannot see, measured but not yet acted on, and only when you ask.
---

# CPU per request

Everything else in this section is about memory, and memory is the wrong
dimension for a whole class of pool.

A request that computes for most of its wall time gets *slower* for every worker
that runs beside it once the cores are full. Slower requests keep their workers
busy longer, so fewer workers are free, so the queue grows, and the queue does
not drain. The memory budget saw none of it: it said there was room for eighty
workers, and there was, and eight would have been faster. Uncached WordPress is
the common case, and the people who tune it by hand land on one and a half to two
workers per core, not the fifty per core the memory arithmetic allows.

`--cpu` measures that dimension. It does not act on it.

## What it measures

php-fpm's full status page reports, for every idle worker, the CPU share of the
request it just finished: the fraction of the request's wall time spent on CPU,
user plus system, with the CPU of anything the request spawned counted in. 100%
is a request that computed the whole time; 10% is one that mostly waited on the
database. fpm-tune already fetches that page for the worker list, so the figure
costs no extra read and no extra permission.

Each reading goes into a per-pool histogram in 5% buckets, which decays the same
way the [memory histogram](measuring-workers.md) does: halved after a few
thousand readings, so a pool redeployed last month is not described by the
application it used to run.

Three filters keep the number honest:

- **Only idle workers.** A running worker reports 0 because its request is not
  finished. That is not a measurement.
- **Only requests of 50ms or more.** php-fpm computes the share from a clock
  that ticks at 100Hz, so a two-millisecond request reads as 0% or 500%
  depending on whether it caught a tick. Fifty milliseconds is five ticks.
- **Each request once.** The status page reports each worker's *last* request,
  and a worker that has served nothing since the previous scrape is still
  reporting the same one. On a quiet pool that is most workers, most of the
  time. Without the filter a single request is counted every thirty seconds for
  as long as its worker lives, and the histogram describes how idle the pool is
  rather than what its requests look like. The per-worker request counter is
  what tells the two apart.

## What it reports

```
CPU per request, as measured (--cpu):
  POOL  TYPICAL  P90  READINGS  SHAPE
  shop  70%      90%  1204      cpu-bound: ~6 busy workers saturate 4 core(s); plan allows 14
  api   10%      20%  3310      i/o-bound: ~40 busy workers saturate 4 core(s); plan allows 30
  blog  -        -    7         too few readings yet
```

`TYPICAL` is the median share; `P90` says how much heavier the heavy requests
are. The shape is coarse on purpose: `cpu-bound` above 50%, `mixed` above 20%,
`i/o-bound` below, and nothing at all under twenty readings, because twenty is a
few minutes on a busy pool and an honest "not yet" on a quiet one.

The arithmetic beside it is cores divided by the typical share, rounded up. On a
four-core host a pool at 70% keeps every core busy with about six workers at
once; past that, concurrency stops buying throughput and starts costing latency.
The plan's own ceiling is printed beside it because the gap between the two is
the whole reason to look: a memory-sized 14 against a CPU-saturating 6 is a
pool that will queue under load however much RAM it has.

The percentages are bucket floors, so they round *down*: every number this tool
prints is read as "at least this much", and arithmetic built on a share should
err toward calling a pool less CPU-bound rather than more.

## Why it sizes nothing yet

Two reasons, and they are different.

First, the figure has not been checked against enough real hosts. It is
php-fpm's number, not ours, taken from the last request each worker happened to
serve; a median of those is a fair description of a pool's shape, but it is not
yet a number anyone should divide a core count by and write to production.
Running `serve --cpu` on a few hosts for a week, and comparing what it says
against what is known about those sites, is how it earns that.

Second, memory and CPU fail differently. Over-provisioning memory ends in the
OOM killer; over-provisioning CPU ends in a slow site. The first is what this
tool exists to prevent and is why it acts. The second is a degradation an
operator can watch, which is why a report is the right first step and a ceiling
is a separate decision, with its own flag, later.

## Turning it on

```bash
fpm-tune plan --cpu                    # measure and report, this run
fpm-tune serve --cpu                   # keep measuring, so plan has a baseline
```

Or `cpu = true` in the service config. Without the flag nothing is measured,
nothing is shown, and the state file carries no CPU fields at all.
