---
title: Spawned children
weight: 3
description: The memory a worker's own RSS misses — the ffmpeg it shelled out to — and how measuring it, the cgroup, and a workload declaration keep a media pool off the OOM killer.
---

# Spawned children

A PHP-FPM worker is not always the whole cost of a request. A worker that runs
`exec('ffmpeg …')`, resizes an image, or renders a PDF starts a **separate
process** — a separate pid, with its own resident memory that the worker's own
RSS does not include and the status page knows nothing about.

For a tool that divides a memory budget, that gap is where an OOM comes from. The
budget it sizes against — a cgroup limit, or the machine — is charged for
**everything**, children included. Sizing each worker at only its own RSS prices
the pool as if the ffmpeg were free, hands out a `pm.max_children` that fits on
paper, and blows the limit the moment the workers start transcoding.

So the children are measured, and — because measuring them has a blind spot —
declared as well.

## Two numbers per worker

Every scrape reads each worker's memory twice:

- its **own RSS**, the worker process alone; and
- its **subtree RSS**, the worker plus every process descended from it.

The gap between them is what the children cost. A plain web worker has none: its
subtree equals its own RSS. A worker mid-transcode has a 600&nbsp;MiB one.

The subtree is read from a single snapshot of the process table taken once per
round, so every pool is measured against the same instant and a host with forty
pools pays for the walk once, not forty times.

### What a snapshot cannot catch

A scrape lands every thirty seconds; an ffmpeg lives for two. Most of the time a
transcode starts and finishes entirely between two scrapes, and the subtree walk
never sees it. Point-in-time sampling systematically under-counts short-lived
children — which is exactly the memory that causes the spike.

That is what the cgroup is for.

## The cgroup is the ground truth

Where the master runs under a cgroup — a container, or a systemd service on a VM
— the kernel maintains a **high-water mark** of the memory that cgroup has used:
every process in it, children included, continuously, not sampled. It is the
number the OOM killer actually enforces against, and it catches the two-second
ffmpeg the scrape missed.

`fpm-tune` reads it (`memory.peak` on a modern kernel, `memory.max_usage_in_bytes`
on cgroup v1, the largest `memory.current` seen otherwise) and treats it as the
truth about what the pool really reached. On a **bare VM or dedicated server with
no cgroup**, there is nothing to read — and there the per-worker subtree
measurement stands on its own, which is why both exist.

## Declaring the workload

Measurement solves the steady state. It does nothing for the **first run**: a
freshly started media pool has no baseline, has been seen spawning nothing yet,
and would be sized as if it never will — right up until the first transcode
arrives and the host with it.

So a pool can say what it is, up front:

```ini
; in the pool's own php-fpm config
env[FPM_TUNE_WORKLOAD] = subprocess-heavy
```

or globally, for a host where most pools are alike:

```bash
fpm-tune serve --apply --workload subprocess-heavy
```

The classes are:

| Workload | What it means | Reserved for children |
|---|---|---|
| `web` | Workers serve requests in PHP and spawn nothing. The default. | none |
| `bursty` | A child now and then — an occasional PDF or image resize. | a little |
| `subprocess-heavy` | A child on most requests — transcoding, image processing. | a lot |

The per-pool marker wins over the global default, so a mostly-web host with one
transcode pool sets `--workload web` (or nothing) and annotates the one pool.
`media` and `children` are accepted as aliases for `subprocess-heavy`.

The declaration is only a **floor**. It keeps the pool safe before it has been
measured; once real children have been observed, the measurement takes over — and
a pool wrongly marked `web` that is caught spawning something is reserved for
anyway, because being wrong about "web" the unsafe way is an OOM.

## How it changes the plan

The child memory is held back from the budget as a **reserve**, on top of the
reserve already kept for the operating system, before the workers are sized
against what is left. The reserve is the largest of what the workload declares,
what the subtree measured, and — where there is one — what the cgroup actually
peaked at beyond its workers.

Because it is a reserve, it can only ever **reduce** how many workers a pool is
given, never increase it. Accounting for children makes this tool run fewer
workers on a tight host; it can never make a host more overcommitted than it was
before. That is the property that makes it safe to leave on.

## What you will see

In `--recommend` output, a pool that spawns children carries the split:

```ini
; reserved for spawned children: 2.0GiB (the cgroup used this much beyond its workers at its peak)
;
; transcode: measured 90MiB
;   measured per worker: median 60.0MiB, p95 90.0MiB, p99 95.0MiB, worst 95.0MiB (240 readings)
;   spawned children add up to 600.0MiB at their worst (worker 90.0MiB, worker+children 690.0MiB)
```

On `/metrics`:

- `fpm_tune_pool_subtree_rss_bytes{pool}` — worker plus children
- `fpm_tune_pool_child_rss_bytes{pool}` — the difference, what children cost
- `fpm_tune_cgroup_memory_bytes{state="current|peak"}` — the cgroup's own usage
- `fpm_tune_budget_bytes{state="reserved_children"}` — what was held back for them

A pool whose `child_rss_bytes` climbs while its `worker_rss_bytes` sits flat is a
pool doing more subprocess work than its workers show — and the one to give a
workload declaration if it does not have one.
