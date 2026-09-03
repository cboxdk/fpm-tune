---
title: Reading a plan
weight: 2
description: The plan output, line by line and column by column.
---

# Reading a plan

This page walks the output of `fpm-tune plan` from top to bottom, for anyone deciding whether to let the numbers be applied. The example is a real plan from a host with two pools, `www` and `www-forge`.

## The notice

```
fpm-tune: another fpm-tune is running, so this will report without recording what it observes
```

Printed on stderr when a daemon holds the state lock. `plan` still reads the host and prints the plan, but does not record the round, so the daemon's learning is undisturbed. Run as a user who cannot write `/var/lib/fpm-tune`, it says that instead (`cannot write /var/lib/fpm-tune, so this reports without recording a baseline`) and again prints the plan.

## The budget block

```
host memory 7.5GiB, 4 CPU(s) (via /proc/meminfo)
  used by other services:  3.0GiB (left for them; cap php-fpm's cgroup for a hard limit)
  reserve kept:            1.1GiB (15% of 7.5GiB)
  available to workers:    3.5GiB
```

The first line is the memory and CPU the plan is made against, and where the number came from. `host memory … (via /proc/meminfo)` is the whole machine, used when php-fpm has no cgroup limit. `php-fpm's memory … (via php-fpm's cgroup)` is the limit on php-fpm's own cgroup, such as a `MemoryMax=` on its unit, and `container memory … (via cgroup v2)` is the container's. The cgroup figure is the one to trust, because it is what the OOM killer enforces. A fractional CPU quota prints in millicores, as in `1500m CPU`.

`used by other services` appears only on a host without a cgroup limit: `MemTotal` less `MemAvailable` less what php-fpm itself holds, so MySQL, Redis and the rest keep what they use. It is a measurement and moves with them; the hard version is a limit on php-fpm's cgroup, which is what the parenthesis suggests. See [The budget](../how-it-decides/the-budget.md).

`reserve kept` is the slice held back from workers, 15% of the total by default (`--reserve`). The parenthesis says how it was reached: `15% of 7.5GiB`, `set explicitly` for a `--reserve`, or on a small host a floor, `1.0GiB floor (a 15% share would be only 300.0MiB)`.

`available to workers` is what the pools are divided from: the total, less the neighbours, less the reserve.

## The pool table

```
POOL       MODE     NOW  PLAN  MEMORY    WHY
www        dynamic  20   20    960.0MiB  peak 2 workers busy, but not yet watched under load; held at its configured 20, estimated 48.0MiB/worker
www-forge  dynamic  10   41    1.7GiB    peak 33 workers busy; raised to 41, measured 41.9MiB/worker + 37.6KiB children
```

One row per pool, sorted by name.

- **POOL** is the pool's name as php-fpm reports it.
- **MODE** is its process manager: `static`, `dynamic` or `ondemand`. The ceiling means something different in each (a static pool runs that many workers all the time, an ondemand pool spawns up to it), and fpm-tune sizes within the mode without changing it. See [Process managers](../how-it-decides/process-managers.md).
- **NOW** is the `pm.max_children` in effect, or `-` when it could not be read.
- **PLAN** is the ceiling this plan gives the pool. A `*` after it marks a pool that wanted more than it got. A `—` marks a pool whose configuration could not be read: it is accounted for in the budget and nothing is written for it.
- **MEMORY** is PLAN times the per-worker cost: what the pool commits if every worker is in use.
- **WHY** is the evidence for the number.

### The WHY phrases

Every WHY ends with the per-worker cost and where it came from. `estimated 48.0MiB/worker` is a profile's guess for a pool not yet measured; `measured 41.9MiB/worker` is the pool's own figure, the p95 of its workers with a 10% margin, floored by the most recent peak. `+ 37.6KiB children` is the memory the workers' spawned processes cost, kept as its own term so a guess is never reported as measured; the sizing uses the two folded together. See [Measuring workers](../how-it-decides/measuring-workers.md) and [Spawned children](../how-it-decides/spawned-children.md).

The front of the line is one of these:

- `peak N workers busy; raised to M`: the pool has needed N at once and gets M, the peak with 25% on top.
- `peak N workers busy; M is enough`: the same, when M is below the current ceiling.
- `hit its ceiling; grown to M`: the pool reached its ceiling since it was last looked at, so its demand was clamped. It grows by a bounded step and converges over runs.
- `peak N workers busy, but not yet watched under load; held at its configured M`: the pool has room to spare, but has not been busy for 20 samples over 30 minutes, so the confidence gate holds it where you set it rather than cutting on a guess.
- `unchanged at M`: nothing to change.
- `wants N, budget allows M`: the budget holds the pool back. Its row is marked `*`.
- `host oversubscribed; held at M`, or `host oversubscribed; cut to M, below the N reserved for it`: the floors alone did not fit, and pools with a trusted baseline were scaled down. A `WARNING:` line below the table says the same in bytes.
- `cpu-bound; held at its CPU ceiling of M rather than the N memory allows (the CPU table has the fill count and headroom behind it)`: printed with `--cpu`, for a pool the CPU table caps.
- `current configuration could not be read; left alone`.

## The allocation line

```
allocated 2.6GiB of 3.5GiB, 869.2MiB free
```

The sum of the MEMORY column, against `available to workers`, and what is left. Free memory here is memory no pool has asked for: the allocator hands out what the pools' peaks justify, and no more.

## Worker memory, as measured

```
Worker memory, as measured:
  POOL       MEDIAN   P95      P99      WORST SEEN  READINGS
  www        16.0MiB  19.0MiB  19.0MiB  32.0MiB     2808
  www-forge  26.9MiB  38.1MiB  38.1MiB  64.0MiB     3415
  Sizing uses neither of these directly. It follows the typical
  peak, which rises fast and falls on a half-life. The spread is here for
  the decision you are making by hand: a pool whose p99 is far above its
  median has a tail, and a tail is what fills a host at the wrong moment.
```

The distribution of a worker's memory over every reading taken. MEDIAN, P95 and P99 are percentiles of it, WORST SEEN is the largest single worker ever read, and READINGS is how many workers have been read. A pool whose p99 sits far above its median has a tail. `www` has 2808 readings and is still `estimated`: readings say what a worker costs, and the confidence gate (busy samples over a span) says whether the pool has been watched under load.

## CPU per request, as measured

```
CPU per request, as measured:
  POOL       TYPICAL  P90  READINGS  PHP/WORKER  BOX/WORKER  LIMIT  WHY
  www        -        -    0         -           -           -      too few readings yet
  www-forge  90%      95%  1460      900m        995m        cpu    cpu-bound; ~5 busy workers fill 4 core(s) with MySQL, nginx and the kernel counted (1.1× PHP's own); ceiling 10 at 2× headroom; plan allows 41 (now 10); queued in 4 rounds while the box was full
  If every measured pool ran its ceiling busy at once: 9950m now, 40795m at this
  plan, against 4 core(s).
  Sizing uses memory. A cpu-limited pool only gets slower past the workers
  that fill the CPU; pass --cpu to hold it at the ceiling shown.
```

Which of memory and CPU each pool runs out of first, from php-fpm's own per-request CPU figure.

- **TYPICAL** and **P90** are the share of a request's wall time spent on CPU, at the median and the 90th percentile. `<5%` is the lowest bucket. A dash means too few readings.
- **READINGS** is the number of requests measured. The shape counts after 20.
- **PHP/WORKER** is what one busy worker costs in millicores, PHP's own CPU: the median share in thousandths of a core.
- **BOX/WORKER** is the same worker's cost to the whole host, MySQL, nginx and the kernel included, once the host-overhead fit has 30 points and 0.2 cores of spread. Until then it is a dash and the WHY says `by PHP's own CPU (the rest of the box not measured yet)`. See [CPU measurement](../how-it-decides/cpu-measurement.md).
- **LIMIT** is `cpu` when the pool's ceiling is above the workers that fill the CPU, and `memory` otherwise.
- **WHY** gives the shape (`cpu-bound` from 50% of a request on CPU, `mixed` from 20%, `i/o-bound` below that), the fill count (`~5 busy workers fill 4 core(s)`), the host overhead (`1.1× PHP's own`), the CPU ceiling with its headroom (`ceiling 10 at 2× headroom`, with `(the pool's own)` when the pool set `env[FPM_TUNE_CPU_HEADROOM]`), what the plan does with it (`plan allows 41 (now 10)` without `--cpu`, `held there (now 10)` with it), and how many rounds found requests queued while the host's CPU was full.

The `If every measured pool ran its ceiling busy at once` line adds the pools up: the millicores every measured pool would draw at its current ceilings and at this plan's, against the host. It is a worst case rather than a prediction; here the 41 workers the plan gives `www-forge` could draw ten times the host.

The last line depends on the flag. Without `--cpu`, sizing uses memory and the table is a report. With `--cpu`, the pool's row in the plan table reads `cpu-bound; held at its CPU ceiling of 10 rather than the 41 memory allows`, and the footer reads:

```
  --cpu is on: a cpu-limited pool is held at its ceiling, the workers that
  fill the CPU plus headroom, and its row in the plan table says so.
```

See [CPU](../how-it-decides/cpu.md).

## Estimated, not yet measured

```
Estimated, not yet measured: www
  These pools have not been watched long enough to size from their own
  memory use. Leave fpm-tune running and the numbers become measurements.
```

The pools whose row says `estimated`. A pool that could not be scraped this round is listed under `Could not be read:` instead, with its current allocation left alone, so a pool that is restarting does not have its memory handed to its neighbours.

## The worst-case line

```
If every pool filled its ceiling with the largest worker ever seen
from it, this plan would need 9.7GiB against 3.5GiB. That is a rare
combination and not what the sizing assumes, but if this host OOMs, it is
the arithmetic to look at.
```

Printed only when it does not fit: every pool at its ceiling, every worker at that pool's WORST SEEN. The sizing uses the p95 floored by the recent peak, so this is above what the plan commits. After an OOM, this is the number to look at.

## Out of capacity

When a pool is marked `*` and there is nothing left to give it, the plan ends with this block, where FREE is the free memory and COST is what one more worker costs the cheapest pool that wanted one:

```
CAPACITY EXHAUSTED: pools marked * want more workers and there is
nowhere left to get them: FREE free against the COST one more worker would
cost the cheapest of them. No configuration change will help; this host
needs more memory, or fewer sites.
```

By the time a plan exists, the slack has already moved: the quiet pools have given up what they were not using. A pool still short in a finished plan is short because the budget ran out, so the fix is more memory or fewer sites. The daemon publishes the same state as `fpm_tune_capacity_exhausted`; see [Metrics and alerting](../operating/metrics-and-alerting.md).

## Mode suggestion

```
Mode suggestion: the mode these pools run may not fit their workload
(fpm-tune will not change it; that is your call):
```

Printed last, and only when a measured pool's process manager looks wrong for its traffic. Each line names the pool and the change, `static → dynamic` or `ondemand → dynamic`, and gives the reason. A static pool is listed when its busiest moment used well under its ceiling, with the idle workers and the memory they hold named. An ondemand pool is listed when requests queued or demand went unmet, since ondemand spawns each worker on demand and a burst waits on cold starts. fpm-tune sizes within whatever mode a pool runs and never changes it; the suggestion is for you.
