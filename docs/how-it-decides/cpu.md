---
title: CPU per request
weight: 7
description: Which of memory and CPU a pool runs out of first. Always measured, reported in every plan, and allowed to cap a pool only with --cpu.
---

# CPU per request

Everything else in this section is about memory, and memory is the wrong
dimension for a whole class of pool.

A request that computes for most of its wall time gets *slower* for every worker
that runs beside it once the cores are full. Slower requests keep their workers
busy longer, so fewer workers are free, so the queue grows, and the queue does
not drain. The memory budget saw none of it: it said there was room for eighty
workers, and there was, and eight would have been faster. Uncached WordPress is
the common case. People who tune it by hand land on one and a half to two
workers per core, not the fifty per core this tool's coarse sanity bound allows.

So every plan asks one more question per pool.

## What it measures

php-fpm's full status page reports, for every idle worker, the CPU share of the
request it just finished: the fraction of the request's wall time spent on CPU,
user plus system, with the CPU of anything the request spawned counted in. 100%
is a request that computed the whole time; 10% is one that mostly waited on the
database. fpm-tune already fetches that page for the worker list, so the figure
costs no extra read and no extra permission, and it is recorded on every scrape.
There is no switch to turn on a week early.

Each reading goes into a per-pool histogram, which decays the same way the
[memory histogram](measuring-workers.md) does: halved after a few thousand
readings, so a pool redeployed last month is not described by the application
it used to run. The buckets are 5% wide up to 100%, then 25% wide to 400%,
then 100% wide to 3200%, because a share above 100% is not a misread (see
[Children](#children)).

## Children

php-fpm's figure counts the CPU of the children a request *waited for*: the
ffmpeg behind an `exec()` or a `proc_close()`, and whatever that ffmpeg spawned
in turn, all the way down. A transcode that ran on eight cores for the whole
request is a share of 800%, and it is filed as such: on a four-core host that
pool's CPU fills at one busy worker. This is the same footprint the
[spawned-children](spawned-children.md) memory measurement covers, seen from
the CPU side.

What the figure cannot see is a child that outlives the request: a job
backgrounded with `&`, or a process that daemonised. Their CPU is real, but it
is no longer this request's, and counting it against the pool would size the
pool for work that queues elsewhere. That is host pressure, and it belongs to a separate signal (load
average, the cgroup's throttling counters) this tool does not read yet.

Three filters decide which readings count:

- **Only idle workers.** A running worker reports 0 because its request is not
  finished. php-fpm counts a request the moment it starts, so a worker seen
  running with its counter already moved is remembered as it was *before*, and
  the request is counted once it completes. A request still running when the
  scrape lands is the long, CPU-heavy one this exists to see.
- **Only requests of 50ms or more.** php-fpm computes the share from a clock
  that ticks at 100Hz, so a two-millisecond request reads as 0% or 500%
  depending on whether it caught a tick: one ten-millisecond tick over a
  two-millisecond request. Fifty milliseconds is five ticks.
- **Each request once, and never our own.** The status page reports each
  worker's *last* request, and a worker that has served nothing since the
  previous scrape is still reporting the same one. The per-worker request
  counter tells the two apart. And the two requests fpm-tune itself sends every
  scrape, the status call and the opcache probe, are kept out. On a quiet pool
  they are the only requests that move a counter, and a large opcache's probe
  computes for well over 50ms; without the filter, a staging pool reads as
  cpu-bound from being watched.

## What a busy worker costs the box

php-fpm's figure is what a request costs inside its worker. It says nothing
about the MySQL query the request waited on, the nginx that proxied it, or the
kernel work outside the worker. How much that is depends on the host: on a
four-core box serving a Laravel site with MySQL alongside it measured a tenth
again; a host whose pages lean on the database will measure more. The plan
should not guess at it, so it measures it.

So each scrape also reads the box's total CPU time (`/proc/stat`, or php-fpm's
cgroup where a CPU quota bounds it) and each pool's workers' own CPU time, and
fits the one to the other over the natural spread of traffic: box busy cores ≈
base + overhead × pool cores. The slope is how much of the box one PHP core
drags along with it. It is fitted per pool, on the scrapes where that pool did
most of the PHP work, so a quiet neighbour is not charged for a busy one, and
it forgets slowly, so a pool redeployed last month is not described by the
application it used to run.

The fit is believed once it has thirty points and the pool's own CPU has varied
by a fifth of a core across them. A week of ordinary traffic has peaks and
troughs; no load test is needed. Until then the report prices a worker at PHP's
own figure and says the rest of the box is not measured yet.

A busy worker's cost to the box is its PHP share times that overhead: 850m in
PHP at 1.1× is 935m on the box, and four cores fill at five such workers.

## What it reports

```
CPU per request, as measured:
  POOL  TYPICAL  P90  READINGS  PHP/WORKER  BOX/WORKER  LIMIT   WHY
  shop  70%      90%  1204      700m        1470m       cpu     cpu-bound; ~3 busy workers fill 4 core(s) with MySQL, nginx and the kernel counted (2.1× PHP's own); ceiling 6 at 2× headroom; plan allows 14 (now 40)
  api   10%      20%  3310      100m        -           memory  i/o-bound; ~40 busy workers fill 4 core(s) by PHP's own CPU (the rest of the box not measured yet); ceiling 80 at 2× headroom; plan allows 30 (now 30)
  blog  -        -    7         -           -           -       too few readings yet
  If every measured pool ran its ceiling busy at once: 61800m now, 23580m at this
  plan, against 4 core(s).
  Sizing uses memory. A cpu-limited pool only gets slower past the workers
  that fill the CPU; pass --cpu to hold it at the ceiling shown.
```

`TYPICAL` is the median share; `P90` says how much heavier the heavy requests
are. Both are bucket floors, so they round *down*: every number this tool prints
is read as "at least this much", and arithmetic built on a share should err
toward calling a pool less CPU-bound rather than more. The first bucket prints
as `<5%`, because `0%` would claim a measurement of nothing.

`PHP/WORKER` is the same median in millicores, the unit a container quota is
written in: a 70% request costs 700m of CPU for as long as a worker is busy
with it. `BOX/WORKER` is what that worker costs the whole box once the fit
above can say, and a dash until it can. Both are the CPU twin of the per-worker
memory the rest of the plan is built on.

The `WHY` column does the arithmetic. The host's CPU, in millicores, divided by
the per-worker cost is how many of this pool's workers, all busy at once, fill
it: the fill count. Past it, concurrency stops buying throughput and starts
costing latency. The ceiling is the fill count with headroom on top (see
below). The plan's ceiling and the one in effect now are printed beside it,
because the gap between them and the ceiling is the whole reason to look: a
pool allowed 14, running 40, that fills the CPU at 3 will queue under load
however much RAM it has. A pool that has had requests queued while the box was
full says so too; that is the direct observation that another worker would not
have helped.

`LIMIT` is that comparison made for you. `cpu` when the ceiling memory allows is
above the CPU ceiling; `memory` when it is not. A pool with fewer than twenty
readings gets no verdict, because twenty is a few minutes on a busy pool and a
fair "not yet" on a quiet one.

## Headroom

The fill count is the throughput optimum and nothing more. Past it another
worker serves no more requests; but a pool held at exactly it has no worker to
spare for a request stuck on an upstream, and short requests wait behind long
ones in the listen queue rather than sharing the CPU. So the ceiling is the fill
count times a headroom, two by default (`--cpu-headroom`, or `cpu-headroom` in
the service config), and never fewer workers than cores plus one. Operators
who size cpu-bound PHP by hand land between one and a half and two workers per
core; two is the generous end, chosen because the cost of a worker too many is
a little load average and the cost of one too few is a stalled site.

The headroom is the one figure on this page that is a judgement rather than a
measurement, and the report prints it beside the ceiling so it is never
mistaken for one.

A pool can carry its own. `env[FPM_TUNE_CPU_HEADROOM] = 3` in the pool's
configuration gives that pool three times its fill count while the rest of the
host keeps the default, which is what a pool with a slow payment API behind it
wants: workers to wait in while the CPU is full, without every other pool
getting the same. The report marks such a ceiling "(the pool's own)". The
marker takes a number between one and one hundred, as does `--cpu-headroom`;
a marker that does not read as one is a warning in the plan and in the
recommendation file, naming the pool and the value, and the host's value is
used for that pool. The serve loop does not log it, so a pool's marker is
worth checking with `fpm-tune plan --cpu` after it is set.

Every pool is measured against the whole host, so the fill counts do not add up
across pools. The line under the table is where they do: what every measured
pool would draw if it ran its ceiling busy at once, now and at this plan,
against the CPU there is. That is a worst case, not a prediction. It is also the
one line that can say the host as a whole is short of CPU.

The shape word is coarse on purpose: `cpu-bound` at a median of 50% and above,
`mixed` from 20%, `i/o-bound` below. The point is to separate a pool whose
workers compute from one whose workers wait.

## What `--cpu` does

Without it, the report is all this does. Sizing stays on memory, and the plan
tells you where that is the wrong answer.

With `--cpu`, the ceiling is allowed to bind: a cpu-limited pool is held at
the workers that fill the CPU plus headroom instead of the number memory
allows, and its row in the plan table says so:

```
POOL  MODE     NOW  PLAN  MEMORY    WHY
shop  dynamic  40   6     600.0MiB  cpu-bound; held at its CPU ceiling of 6 rather than the 14 memory allows (the CPU table has the fill count and headroom behind it), measured 100.0MiB/worker
```

In the CPU table the same pool's WHY ends `; ceiling 6 at 2× headroom; held
there (now 40)`, and the line under the table becomes `--cpu is on: a
cpu-limited pool is held at its ceiling, the workers that fill the CPU plus
headroom, and its row in the plan table says so.`

The cap has two limits.

It caps only what a pool *wants*, never its floor. A pool that has not been
watched long enough to be cut on memory evidence keeps its configured ceiling
as its floor, and the CPU ceiling does nothing to it: a cap below the
configured ceiling *is* a cut, and it waits for the same
[confidence](measuring-workers.md) the memory path needs before it may cut.
Twenty readings say what shape a pool's requests have. They are not permission
to take workers away from a pool this tool has not seen through its traffic
pattern.

And it is off by default. Memory and CPU fail differently: over-provisioning
memory ends in the OOM killer, over-provisioning CPU ends in a slow site. The
first is what this tool exists to prevent and is why it acts on memory without
being asked. The second is a degradation you can watch, so the report comes
first and the ceiling is a separate decision.

## Turning the ceiling on

```bash
fpm-tune plan --cpu                    # see what the cap would do
fpm-tune serve --cpu                   # let it bind, in advisory or apply mode as usual
sudo fpm-tune install-service --cpu    # the same for the installed service; re-run to switch
```

Or `cpu = true` in the service config, which is what `install-service --cpu`
writes. `--cpu-headroom 1.5` (or `cpu-headroom = 1.5`) tightens the ceiling;
the floor of cores plus one holds either way. The measurement, the report, the
`/metrics` series (`fpm_tune_pool_cpu_ratio`,
`fpm_tune_pool_cpu_readings`, `fpm_tune_pool_cpu_fill_workers`,
`fpm_tune_pool_cpu_limited`; see [Alerting](../operating/alerting.md)) and the
line in the recommendation file are there either way.

## Fractional CPU

A container quota of half a core is half a core. The plan's first line says so
(`500m CPU`), and everything on this page divides by the quota itself, in
millicores, so a half-core container is told its CPU fills at one busy worker,
not two. The allocator's coarse fifty-per-core bound still rounds the quota up
to a whole core, because half a core still runs one worker at a time and a
bound of zero would be no bound at all. Where the fill count comes out under
the floor a pool may run, the cap holds the pool at that floor.
