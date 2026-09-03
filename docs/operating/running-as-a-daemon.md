---
title: Running as a daemon
weight: 1
description: What the serve loop does each round, and the difference between watching and acting.
---

# Running as a daemon

```bash
fpm-tune serve            # watch, learn, publish metrics, change nothing
fpm-tune serve --apply    # also act on the plan
```

Without `--apply`, `serve` is a permanent observer: every interval it discovers
the pools, scrapes them, folds the readings into the learned baselines, builds a
plan, and publishes it as metrics. It touches no configuration. This is a
reasonable way to run it forever, as a source of sizing metrics, or as an
adviser (see [Advisory mode](advisory-mode.md)). It is where anyone sensible
starts.

With `--apply`, the same loop also writes the plan when a change clears the
[hysteresis thresholds](../how-it-decides/hysteresis.md), reloads the master, and
repairs the host if its own file is ever what stops php-fpm from starting.

## In the background, under systemd

You do not have to write a unit. One command installs and starts it:

```bash
sudo fpm-tune install-service          # advisory (watch and recommend)
sudo fpm-tune install-service --apply  # act on the plan
sudo fpm-tune install-service --cpu    # also let the CPU measurement cap a pool
```

It writes `/etc/fpm-tune/config` and a unit that reads it, then enables and starts
the service. `--print` shows both without installing anything.

The **mode lives in the config, not the unit**, so switching between watching and
acting is a command, no unit edit, no `daemon-reload`:

```bash
sudo fpm-tune mode apply       # let it act on what it finds
sudo fpm-tune mode advisory    # back to watch-only
fpm-tune mode                  # print the current mode
```

Each rewrites the one line in `/etc/fpm-tune/config` and restarts the service. The
sensible path is `install-service` (advisory), leave it a day or two to build a
baseline, read the recommendation, then `mode apply`. Follow it with
`journalctl -u fpm-tune -f`.

## What a round does

Each interval, in order:

1. **Reconcile**, if a previous run left something unfinished, before discovery,
   because a broken configuration a previous run left would stop the master from
   parsing, and from that point on nothing is discoverable.
2. **Discover** the masters and their pools, re-reading the effective
   configuration each round so a `pm.max_children` an operator changed by hand is
   seen rather than assumed.
3. **Scrape** each pool's status and worker memory.
4. **Learn**: fold the readings into the baselines.
5. **Plan**: read the budget from the master's cgroup, divide it.
6. **Record** the ceiling counters, for the next round to compare against.
7. **Apply** (with `--apply`), if a change is worth a reload.
8. **Publish** the plan as metrics, and (with `--recommend`) write it as
   configuration.

## The self-repair is part of applying

A daemon without `--apply` will not fix a host whose master this tool's own file
is stopping from starting. The repair is part of acting, not part of watching.
If you run it as an adviser, keep in mind that it will diagnose that situation in
its metrics and logs but not resolve it. See [Recovering a host](recovering.md).

## What it logs

`serve` logs at info level, because a daemon's log is its output: it says when it
resized a pool, when a host is no longer at capacity, when a recommendation
changed. A daemon that logged only problems could not answer "what has this been
doing", the first question anyone asks of a process that has been up for a
month. Pass `--verbose` for the per-scrape detail.

The line worth watching is the recommendation itself:

```
level=INFO msg="Pool recommendation" pool=www now=20 recommend=24 why="peak 18 workers busy; raised to 24, measured 52.0MiB/worker"
```

It is logged the first time a pool is seen and **when the recommended
`pm.max_children` changes**, not every round, and not on the per-scrape wobble of
the peak. So the log reads as a running account of what it would set and why, rather
than a wall of identical lines. In `--apply` mode you see this when the plan
concludes it, and the separate "resized" line when a change actually clears the
[hysteresis thresholds](../how-it-decides/hysteresis.md) and reaches the master.

To keep the log from going silent on a quiet host, the same line is re-logged as a
**heartbeat** every `--heartbeat` interval (default 30 minutes; `heartbeat` in the
config, `0` to disable) even when nothing changed, a steady sign of life, and each
one carries the current `why`, so you watch the measurement firm up over the first
hours. For a continuous view rather than a pulse, scrape `/metrics`; for per-scrape
detail, run with `--verbose`.
