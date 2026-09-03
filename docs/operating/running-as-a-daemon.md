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

## History: a day of rounds

Every round leaves one sample in a ring in the daemon's memory, every apply and
every change the daemon notices leaves an event, and the metrics address serves
them as JSON:

```
curl -s 127.0.0.1:9110/history.json | jq '.rounds[-1]'
curl -s '127.0.0.1:9110/history.json?last=120' | jq '.rounds[].pools[] | select(.pool=="www") | .active'
```

A round carries, per pool, what was observed (busy workers, listen queue, the
configured ceiling), what was planned (the recommended ceiling, whether demand
went unmet, the per-worker cost sized on, what memory alone would have set, so
a CPU-capped pool shows the gap) and the CPU side (median share, readings, fill
count, ceiling, whether the pool is CPU-limited and whether it was held at the
CPU ceiling), plus how busy the box's CPU was over the interval. A `host`
object carries the hostname, the version, the mode, whether the CPU ceiling is
on and where the budget came from; that is what `top`'s title bar reads.
Events are resizes (pool, from, to, reason), failed applies, rollbacks,
repairs, and `changed`: a ceiling that moved without the daemon moving it, a
hand edit, a deploy, or an `fpm-tune apply` run beside it.

`--history` sets how far back it reaches (a day by default; the ring holds
history ÷ interval rounds and a thousand events). It starts empty at every
daemon start and is never written to disk: it is for a dashboard or a terminal
UI to draw a line from, not a store. Prometheus is the place for anything that
must outlive a restart.

It is served on the same address as `/metrics`, with the same absence of
authentication, and it says more than the metrics do: pool names, hostname,
version, and a day of per-pool load. Bind the address to loopback (the
installed service does) or a private network, and reach it from elsewhere over
an SSH tunnel.

## Watching it: `fpm-tune top`

```bash
fpm-tune top                       # the installed service
fpm-tune top --addr 10.0.0.5:9110  # another host's daemon
```

A terminal view of that history: the box's CPU over time, every pool with its
busy workers, queue, ceiling now and planned, CPU share, fill count and which
resource limits it, the selected pool's charts, and every event (resizes,
outside changes, failed applies, rollbacks, repairs) since the daemon started.
Arrow keys (or `j`/`k`) pick a pool, `1`/`2`/`3` set the span (an hour, six,
everything), `r` refreshes, `a` applies, `q` quits.

It reads and changes nothing on its own. `a` opens the plan's pending changes,
pool by pool, and Enter runs `fpm-tune apply-now` in the terminal (through
`sudo`, unless you are root), which asks the daemon to apply the plan it
showed. The daemon does the writing, with its own state, lock and flags, so
what you saw is what gets applied, and it records the resize as an event. It
stays in whatever mode it is in. `--addr` reads another host's history over
plain HTTP, for looking; `a` is refused there, because `apply-now` reaches only
the daemon on the box it runs on.

## Applying once: `fpm-tune apply-now`

```bash
sudo fpm-tune apply-now
```

The daemon holds the state lock for as long as it runs, so `fpm-tune apply`
beside it is refused: two writers of one state file is how an hour of learning
gets discarded. `apply-now` is the way to act on a watching daemon's plan
without switching it to apply mode: it asks the daemon, over a control socket,
to run one round with applying forced on and the
[hysteresis](../how-it-decides/hysteresis.md) waived (you have read the plan
and asked for it), and prints what changed. The daemon stays in the mode it
was in; on an apply-mode daemon, `apply-now` is a way to skip the damping for a
change you have already read. This is the two-part way to run it: the daemon
watches and plans, a person applies, from the terminal or from `top`.

The socket is `/var/lib/fpm-tune/control.sock` (`--control` on both `serve` and
`apply-now` moves it), created mode 0600 and owned by the daemon's user, so
`apply-now` needs root. Each call is one full round with the damping off, so
do not script it in a loop: that is `--apply` with the safeguards removed.

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
