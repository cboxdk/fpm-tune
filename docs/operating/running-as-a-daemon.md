---
title: Running as a daemon
weight: 1
description: What the serve loop does each round, and the difference between watching and acting.
---

# Running as a daemon

```bash
fpm-tune serve            # watch, learn, publish metrics — change nothing
fpm-tune serve --apply    # also act on the plan
```

Without `--apply`, `serve` is a permanent observer: every interval it discovers
the pools, scrapes them, folds the readings into the learned baselines, builds a
plan, and publishes it as metrics. It touches no configuration. This is a
reasonable way to run it forever — as a source of sizing metrics, or as an
adviser (see [Advisory mode](advisory-mode.md)) — and it is where anyone sensible
starts.

With `--apply`, the same loop also writes the plan when a change clears the
[hysteresis thresholds](../how-it-decides/hysteresis.md), reloads the master, and
repairs the host if its own file is ever what stops php-fpm from starting.

## What a round does

Each interval, in order:

1. **Reconcile**, if a previous run left something unfinished — before discovery,
   because a broken configuration a previous run left would stop the master from
   parsing, and from that point on nothing is discoverable.
2. **Discover** the masters and their pools, re-reading the effective
   configuration each round so a `pm.max_children` an operator changed by hand is
   seen rather than assumed.
3. **Scrape** each pool's status and worker memory.
4. **Learn** — fold the readings into the baselines.
5. **Plan** — read the budget from the master's cgroup, divide it.
6. **Record** the ceiling counters, for the next round to compare against.
7. **Apply** (with `--apply`), if a change is worth a reload.
8. **Publish** the plan as metrics, and — with `--recommend` — write it as
   configuration.

## The self-repair is part of applying

A daemon without `--apply` will not fix a host whose master this tool's own file
is stopping from starting — the repair is part of acting, not part of watching.
If you run it as an adviser, keep in mind that it will diagnose that situation in
its metrics and logs but not resolve it. See [Recovering a host](recovering.md).

## What it logs

`serve` logs at info level, because a daemon's log is its output: it says when it
resized a pool, when a host is no longer at capacity, when a recommendation
changed. A daemon that logged only problems could not answer "what has this been
doing" — the first question anyone asks of a process that has been up for a
month. Pass `--verbose` for the per-scrape detail.
