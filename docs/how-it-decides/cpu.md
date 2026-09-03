---
title: CPU per request
weight: 7
description: Which of memory and CPU a pool runs out of first, and the ceiling --cpu holds a cpu-bound pool at.
---

# CPU per request

Memory sizing is the wrong answer for a pool whose requests compute. This page
is what fpm-tune measures on the CPU side, what the CPU table in every plan
means, and what `--cpu` changes. How the figures are collected is in the
[appendix](cpu-measurement.md).

## What it measures

php-fpm's full status page reports, for each idle worker, the CPU share of the
request it just finished: the fraction of its wall time spent on CPU, user plus
system, with the CPU of anything it spawned counted in. 100% is a request that
computed the whole time; 10% is one that mostly waited on the database. It
comes with the page fpm-tune already fetches, and is recorded on every scrape.

The median share classifies the pool: `cpu-bound` at 50% and above, `mixed`
from 20%, `i/o-bound` below. The median in millicores is what one busy worker
costs in CPU (a 90% request is 900m while a worker is busy with it), the CPU
twin of the per-worker memory cost.

## Why memory still sizes

A request that computes for most of its wall time gets slower for every worker
running beside it once the cores are full. Slower requests hold their workers
longer, fewer are free, and the queue grows without draining. The memory
budget saw none of it: there was room for eighty workers, and eight would have
been faster.

Memory and CPU fail differently, though. Over-provisioning memory ends in the
OOM killer; over-provisioning CPU ends in a slow site you can watch. So the CPU
shape is reported in every plan, and holding a pool to it is a separate
decision, off by default.

## Children

php-fpm's share counts the children a request waited for: the ffmpeg behind an
`exec()`, and whatever it spawned in turn. A transcode on eight cores for the
whole request is a share of 800%, filed as such, so on a four-core host that
pool fills its CPU at one busy worker. A child that outlives the request,
backgrounded or daemonised, is not counted; its CPU is host pressure the plan
does not read.

## Host overhead

php-fpm's figure is what a request costs inside its worker, and says nothing
about the MySQL query it waited on, the nginx that proxied it, or the kernel.
So each scrape also reads the host's CPU time and each pool's workers' own, and
fits one to the other over the natural spread of traffic. The slope is the host
overhead: the cores the host spends for every core the pool's workers spend,
1.1 on a four-core Laravel host with MySQL beside it. The fit is believed at 30
points with 0.2 cores of spread; until then a worker is priced at PHP's own
figure and the table says so.

## Fill count and ceiling

The host's CPU in millicores divided by the per-worker cost, rounded up, is the
fill count: how many of this pool's workers, all busy at once, fill the CPU. A
half-core container divides as 500m and fills at one busy worker. Past the fill
count another worker serves no more requests. The ceiling is
`ceil(fill × headroom)`, never below cores plus one, because a pool held at
exactly the fill count has no worker to spare for a request stuck on an
upstream, and short requests would wait behind long ones in the listen queue.

Headroom defaults to 2 (`--cpu-headroom`, or `cpu-headroom` in the service
config) and takes a number from 1 to 100. Two is the generous end of what
operators use by hand, because a worker too many costs a little load and a
worker too few stalls a site. It is the one judgement on this page, and the
table prints it beside the ceiling.

A pool can carry its own: `env[FPM_TUNE_CPU_HEADROOM] = 3` in the pool's
configuration, for a pool with a slow payment API behind it. The table marks
such a ceiling `(the pool's own)`. A marker that does not read as a number
from 1 to 100 is a warning in the plan and in the recommendation file, naming
the pool and the value, and the host's value is used for that pool.

Fill counts are per pool, each against the whole host, so they do not add up.
The line under the table adds them: what every measured pool would draw at its
ceiling, now and at this plan, against the CPU there is. It is a worst case,
and the one line that can say the host as a whole is short of CPU.

## What `--cpu` changes

Nothing about the measurement. Without it the plan sizes on memory, and the
table's LIMIT column says which of memory and CPU each pool runs out of first:
`cpu` when memory would allow more workers than the CPU ceiling. With `--cpu`
such a pool is held at the CPU ceiling. From today's host:

```
POOL       MODE     NOW  PLAN  MEMORY    WHY
www-forge  dynamic  10   10    419.0MiB  cpu-bound; held at its CPU ceiling of 10 rather than the 41 memory allows (the CPU table has the fill count and headroom behind it), measured 41.9MiB/worker + 37.6KiB children

CPU per request, as measured:
  POOL       TYPICAL  P90  READINGS  PHP/WORKER  BOX/WORKER  LIMIT  WHY
  www-forge  90%      95%  1460      900m        995m        cpu    cpu-bound; ~5 busy workers fill 4 core(s) with MySQL, nginx and the kernel counted (1.1× PHP's own); ceiling 10 at 2× headroom; held there (now 10); queued in 4 rounds while the box was full
```

Five busy workers at 995m fill four cores; twice that is the ceiling of 10. The
last clause counts scrapes that found requests queued while the host was at
95% or more of its CPU: another worker would not have helped.

The ceiling caps only what a pool wants, never its floor. A pool not yet
watched long enough to be cut on memory evidence keeps its configured ceiling,
because a cap below it is a cut, and the CPU path waits for the same
[confidence](measuring-workers.md#cost-and-permission) as the memory path.

## Turning it on

```bash
fpm-tune plan --cpu                    # see what the ceiling would do
fpm-tune serve --cpu                   # advisory, with the ceiling binding in the recommendation
sudo fpm-tune install-service --cpu    # the installed service; re-run with --cpu=false to switch back
```

`install-service --cpu` writes `cpu = true` to `/etc/fpm-tune/config`. The
`fpm_tune_pool_cpu_*` series on `/metrics` (see
[metrics and alerting](../operating/metrics-and-alerting.md)) and the
`cpu per request` line in the recommendation file are there either way.
