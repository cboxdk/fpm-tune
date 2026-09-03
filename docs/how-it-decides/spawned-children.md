---
title: Spawned children
weight: 4
description: The memory a worker's own RSS misses, the ffmpeg it shelled out to, and how it is measured and declared.
---

# Spawned children

A worker that runs `exec('ffmpeg …')`, resizes an image or renders a PDF starts
a separate process with its own memory, which the worker's RSS does not include
and the status page knows nothing about. The budget is charged for it all the
same. This page is how that memory is measured, why it is also declared, and
what it does to the plan.

## Two numbers per worker

Every scrape reads each worker twice: its own RSS, and its subtree RSS, the
worker plus every process descended from it. The difference is what the
children cost. A plain web worker has none; a worker mid-transcode has a
600 MiB one. The subtree comes from one snapshot of the process table per
round, so a host with forty pools pays for the walk once.

A snapshot misses what lives and dies between two scrapes. A scrape lands every
30 seconds and an ffmpeg may live for two, so short children are under-counted,
and they are the memory that spikes.

## The cgroup high-water mark

Where the master runs under a cgroup, the kernel keeps a high-water mark of
everything the cgroup has used, children included, continuously rather than
sampled. fpm-tune reads `memory.peak` (kernel 5.19), `memory.max_usage_in_bytes`
on cgroup v1, or the largest `memory.current` it has seen, and reports it in
the recommendation file and on `/metrics`:

```
; cgroup used 209.9MiB now, 460.2MiB at its peak (workers AND everything they spawned, the number the OOM killer enforces against)
```

It is the number the OOM killer enforces against, and the one that catches the
child a sample missed. Sizing does not use it; compare it with the budget when
a plan looks optimistic. On a host without a cgroup the subtree measurement
stands alone.

## Declaring the workload

Measurement covers the steady state. It does nothing for the first run: a
freshly started media pool has spawned nothing yet and would be sized as if it
never will, until the first transcode. So a pool can say what it is in its own
configuration:

```ini
env[FPM_TUNE_WORKLOAD] = subprocess-heavy
```

or the host can set a default for pools that declare nothing:

```bash
fpm-tune plan --workload subprocess-heavy
```

| Class | Aliases | Assumption | Reserved per worker before measurement |
|---|---|---|---|
| `web` | `api`, `simple` | workers spawn nothing (the default) | none |
| `bursty` | | a 256 MiB child on a quarter of the workers at once | 64 MiB |
| `subprocess-heavy` | `subprocess`, `media`, `children` | a 512 MiB child on every worker | 512 MiB |

Names are case-insensitive, and the pool's marker wins over the host default. A
marker that matches none of these falls back to the host default and the plan
warns, naming the pool and the value. The declaration is a floor: once children
have been measured at more than it, the measurement takes over, and a pool
marked `web` that is caught spawning is reserved for anyway.

## How it changes the plan

The child memory is folded into each worker's cost, and the allocator divides
the budget by that cost as it does for a worker's own memory. A
`subprocess-heavy` pool gets fewer workers; it never makes the plan fail, and
the allocator's guarantee of never committing more than the budget covers the
children too.

The measured figure is amortised. It is the high-water mark of a scrape's total
child memory divided by the pool's worker count, taking the larger of the
workers in that scrape and the pool's peak concurrency. A pool where two of
eight workers were each running a 600 MiB ffmpeg records 150 MiB per worker,
so multiplied back by eight it reserves the 1.2 GiB that was there, and a
scrape that catches a two-worker ondemand pool mid-transcode does not record a
whole child as the per-worker cost. The figure only climbs, until the baseline
is reset.

## What you will see

The plan's WHY column shows the child part as its own term:

```
www-forge  dynamic  10   41    1.7GiB    peak 33 workers busy; raised to 41, measured 41.9MiB/worker + 37.6KiB children
```

The recommendation file carries the total, and the worst single
worker-plus-children seen:

```
; reserved for spawned children: 376.5KiB (folded into each worker's cost, sized to the workers planned)
;   plus ~37.6KiB of children per worker (folded into the sizing; worst single worker+children seen 168.7MiB)
```

On `/metrics`: `fpm_tune_pool_subtree_rss_bytes{pool}` is the worst single
worker's whole footprint, `fpm_tune_pool_child_rss_bytes{pool}` the child
memory folded into each worker, `fpm_tune_cgroup_memory_bytes{state="current"|"peak"}`
the cgroup's own usage, and `fpm_tune_budget_bytes{state="reserved_children"}`
what the plan committed to children in total. A pool whose child bytes climb
while its worker bytes sit flat is the one to give a declaration.

The CPU those children burn is counted on the other axis: php-fpm's per-request
CPU share includes every child the request waited for, so a transcode shows as
a share above 100% in the [CPU table](cpu.md) even when the memory sample
missed it.
