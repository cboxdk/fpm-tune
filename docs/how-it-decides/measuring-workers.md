---
title: Measuring workers
weight: 3
description: What one worker costs, which readings count, how fast the estimate moves, and when a pool may be shrunk.
---

# Measuring workers

The learner answers one question from noisy readings: what does one worker of
this pool cost? Too low and the host OOMs; too high and every site is
throttled. This page is which readings it believes, how fast the estimate
moves, and what separates knowing a pool's cost from being allowed to shrink
it.

## Bootstrap, then learned

A pool with no history is sized from a profile guess of 48 MiB per worker, and
the plan labels it `estimated`. From the first accepted reading the pool is
sized from its own workers and labelled `measured`. Baselines are kept in
`/var/lib/fpm-tune/state.json`, so a restart does not start over; deleting the
file does.

## PSS, not RSS

A warm worker's resident set is mostly shared pages: the opcache segment, the
libraries, the copy-on-write pages it still shares with the master. RSS charges
each of those in full to every worker, so twenty workers count a 512 MiB
opcache twenty times. The sizing reads PSS from `/proc/<pid>/smaps_rollup`
instead, where every shared page is divided among the processes mapping it, so
the sum across a pool is what the pool costs the host.

`smaps_rollup` needs kernel 4.14 and permission to read another process's
memory map. Where either is missing the reading falls back to RSS, which is
safe and less precise. The [spawned-children](spawned-children.md) figure stays
on RSS on purpose: it is the difference of two RSS reads.

## Which readings count

A worker that has served three requests has not loaded the application yet, and
an idle pool is made of such workers. So a scrape counts only when it has at
least 2 workers that have each served 20 requests, and the reading is the
largest of them: the tail is what fills a host, and the mean of a scrape is
half cold workers. Two shapes of pool would otherwise never be measured:

- A pool that never runs two workers at once, such as a quiet ondemand site.
  One mature worker is accepted as its cost, but not as evidence it may be
  shrunk.
- A pool whose `pm.max_requests` recycles workers before they mature. After 20
  consecutive rounds of serving requests with no mature worker, its young
  workers are read anyway, and such a reading may only raise the estimate,
  never lower it.

## Fast up, slow down

The estimate is asymmetric. A reading above it moves the estimate halfway to
the reading in one scrape (a weight of 0.5), and the most recent scrape's peak
floors the sizing regardless, so a deploy that makes workers heavier is caught
in one scrape. A reading below it pulls the estimate down on a 30-minute
half-life. Under-sizing ends in the OOM killer; over-sizing costs unused memory
on one pool.

A smaller reading counts only while the pool is working: at least one request
per second since the last scrape, from the pool's own request counter. PHP
returns large allocations to the OS, so an idle survivor shrinks, and believing
that leaves the morning sized for workers that do not exist. Cron, uptime
checks and crawlers stay under the threshold.

Elapsed time is only evidence while it was watched. A gap longer than 12 hours
refuses the reading as evidence of decay, and any gap may move the estimate no
further than one ordinary scrape would, so a daemon restarted after a package
upgrade does not collapse the estimate on its first reading back.

## Cost and permission

Cost and permission to shrink are different questions. A pool's cost is taken
from any reading there is, because the bytes were real whatever the confidence.
Shrinking a pool needs a baseline watched through a real traffic pattern: 20
busy scrapes spread over at least 30 minutes between the first and the last.
Until then the pool's floor is its configured ceiling, the plan's WHY says
`not yet watched under load; held at its configured N`, and the first run can
only ever help. `--confidence-samples` and `--confidence-span` set the two
numbers.

## Peak concurrency

The demand side is the most workers seen busy at once. PHP-FPM's own counter
resets on every reload, and fpm-tune reloads, so the peak is remembered in the
state file. A higher reading replaces it at once; a lower one is ignored for 24
hours, after which the peak halves the distance to what is being seen each
scrape, so a pool that has quietened gives its slack back over a few rounds.

## The distribution

The sizing number is one number, because the allocator needs one cost per
worker, but it cannot say how bad a pool gets. Each pool also keeps a
log-spaced histogram of every worker reading, cold ones included, and reports
its median, p95, p99 and worst seen in `plan`, in the recommendation file, and
on `/metrics` as `estimate="p50"`, `"p95"`, `"p99"` and `"high_water"`. Every
bucket is halved once the histogram passes 4096 readings, so a pool redeployed
months ago is not described by the application it used to run.

## The sizing basis

The default, `--sizing p95`, sizes on the 95th percentile of that histogram
plus a 10% margin, floored by the most recent scrape's peak. A single heavy
request is the top of the distribution and does not move p95, so the pool is
not sized on its worst minute; the floor still catches a deploy in one scrape.

`--sizing peak` sizes on the typical peak instead: the estimate described
above, which rises in one scrape and decays on the 30-minute half-life, floored
by the most recent peak. It is the more conservative basis. The all-time high
is reported as WORST SEEN and never sizes anything. `--sizing p99`, or any
percentile from 50 to 100, moves the steady basis. In the service config the
key is `sizing`.
