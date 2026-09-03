---
title: CPU measurement
weight: 8
description: Appendix on how the CPU figures in the plan are collected, filtered and fitted.
---

# CPU measurement

This is the appendix to [CPU per request](cpu.md): how the numbers in the CPU
table are collected, which readings are thrown away, and how the host overhead
is fitted. Read it when a figure in the table looks wrong.

## The histogram

Each accepted reading goes into a per-pool histogram of the CPU share. The
buckets are 5% wide up to 100%, 25% wide to 400%, and 100% wide to 3200%, 61 in
all; anything past 3200% lands in the last one. The steps widen with the share
because the arithmetic built on it (cores over share) cares about 5% near the
bottom and about 100% near the top. A share above 100% is a request that
waited for children on several cores, and filing it at 100% would tell the
plan one busy worker costs a fraction of what it does.

TYPICAL and P90 in the table are the floors of the median and 90th-percentile
buckets, so they round down: every number is read as "at least this much", and
the workers-per-core arithmetic errs toward calling a pool less cpu-bound. The
first bucket prints as `<5%`. Like the memory histogram, every bucket is halved
once the count passes 4096 readings, so a pool redeployed last month is not
described by the application it used to run.

## Three filters

- **Not fpm-tune's own requests.** The status call and the opcache probe it
  sends every scrape are recognised by their request URI and skipped. On a
  quiet pool they are the only requests that move a counter, and a large
  opcache's probe computes for well over the floor below, so without this a
  staging pool reads as cpu-bound from being watched.
- **Requests of 50 ms or more.** php-fpm computes the share from the process
  clock, which ticks at 100 Hz on most kernels. A two-millisecond request
  either caught a tick or did not, and reads as 0% or 500% accordingly. Fifty
  milliseconds is five ticks, which bounds the error at about a fifth, and
  short requests are the ones a worker-count decision matters least for.
- **Idle workers only.** php-fpm fills in the figure for an idle worker; a
  running one reports zero, which is not a measurement.

## Each request once

The status page reports each worker's last request, and a worker that has
served nothing since the previous scrape is still reporting the same one.
Without a dedupe a single request would be counted every 30 seconds for as long
as the worker lives, and the distribution would describe how idle the pool is.
So fpm-tune remembers each pid's request counter as of the last reading and
skips a worker whose counter has not moved. A counter that went down is a
recycled pid, a new worker wearing an old number, and its request is as new as
any.

php-fpm counts a request the moment it starts, so a worker seen running keeps
whatever counter was remembered before it; the request it is on is then new
when it completes. A request still running when the scrape lands is the long,
CPU-heavy one this measurement exists to see.

## Twenty readings

A pool's shape is called, and its per-worker cost, fill count and ceiling
computed, once its histogram holds 20 readings: a few minutes on a busy pool,
and `too few readings yet` on a quiet one. The ceiling may bind only for a pool
that is also trusted enough to be cut on memory evidence.

## The host-overhead fit

Each scrape reads two more things. The host's cumulative CPU time comes from
`/proc/stat`, or from the cgroup's `cpu.stat` where a `cpu.max` quota bounds
php-fpm, since inside a container `/proc/stat` is the machine. Each worker's
cumulative CPU time, own plus reaped children, comes from `/proc/<pid>/stat`.
The difference since the last scrape over the wall time between them is the
cores each pool used and the cores the host used. Intervals shorter than 5
seconds or longer than 5 minutes are skipped, as is a counter that went
backwards.

The pool's cores are the x of a least-squares line and the host's cores the y:
host cores ≈ base + overhead × pool cores. The slope is the host overhead. A
point is added to a pool's fit only when that pool accounted for at least 70%
of all pool CPU in the interval, so a quiet neighbour is not charged for a busy
one. An idle round, with all pools together under 0.02 cores, gives every pool
the point (0, host cores), which is the base load the intercept absorbs.

The fit keeps five running sums with a forgetting factor of 0.9975 per point,
an effective window of about 400 scrapes, so a pool redeployed last month is
not described by the application it used to run. It is believed once it has 30
points and the standard deviation of x is at least 0.2 cores: a line through
points that all sit at the same load has no slope to give. A slope below 1 is
read as 1, since the host cannot spend less than the pool did, and a slope
above 20 is discarded as a pool whose traffic coincides with something else
entirely. A week of ordinary traffic has the peaks and troughs the fit needs;
no load test is required.

## Starved rounds

Beside the fit, each scrape counts the rounds in which a pool had requests in
its listen queue while the host was at 95% or more of its CPU. That is the
direct observation that another worker would not have helped, and the count is
the last clause of the pool's WHY in the CPU table and
`fpm_tune_pool_cpu_starved_rounds` on `/metrics`.
