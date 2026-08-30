---
title: fpm-tune
weight: 0
description: Sizes PHP-FPM pools against the memory a machine actually has, using the memory its workers actually use.
---

# fpm-tune

A server hosting many PHP sites has many pools drawing on one pool of RAM.
Setting `pm.max_children` per pool by hand means either leaving capacity unused
or discovering the ceiling through an OOM kill. fpm-tune measures what each
pool's workers really cost, divides the budget between them, and — if you let
it — writes the settings back and reloads the master.

It runs standalone: it does its own discovery, its own measurement, its own
budget detection, and serves its own metrics. No other process required.

## The mental model

Three questions, answered in order, each by a different part of the tool:

1. **How much memory is there to divide?** The budget comes from the cgroup of
   the php-fpm master being managed — not the machine, the *master* — because on
   a VM the machine is never limited but the systemd slice is. See
   [The budget](how-it-decides/the-budget.md).

2. **What does one worker of each pool cost?** Measured from real worker RSS,
   watched over time, with an asymmetric memory: an estimate that rises fast when
   a pool gets more expensive and falls slowly when it gets cheaper, because
   under-sizing ends in an OOM and over-sizing only wastes headroom. See
   [Measuring workers](how-it-decides/measuring-workers.md).

3. **How should the budget be split?** Each pool gets a floor, then what is left
   is handed to the pools that are actually queueing — the ones a shortage is
   hurting right now, cheapest to fix first. See
   [Dividing the budget](how-it-decides/dividing-the-budget.md).

Nothing is written until it has been validated against a sandboxed copy of the
configuration, and a change is applied with SIGUSR2 — a graceful reload, never a
restart — so no request is dropped. If the master does not come back, the change
is rolled back. See [Safety](safety/_index.md).

## Three ways to run it

```bash
fpm-tune plan     # what it would change, and why — writes nothing
fpm-tune apply    # write the settings and reload once
fpm-tune serve    # keep measuring; add --apply to act, or --recommend to advise
```

Most people start with `plan` to see what it thinks, then run `serve` without
`--apply` as a permanent adviser — watching, publishing metrics, and writing its
conclusion to a file you can read — before ever letting it touch a host.

## The sections

- **[Getting started](getting-started/_index.md)** — install it, and the first
  run, done safely.
- **[How it decides](how-it-decides/_index.md)** — the budget, the learner, the
  allocator, and the hysteresis that decides when to move and when to hold. This
  is the part worth reading if you are going to trust it.
- **[Operating it](operating/_index.md)** — running it as a daemon, the advisory
  mode, the metrics to alert on, and what to do when php-fpm will not start.
- **[Safety](safety/_index.md)** — how it fails safe, and what it trusts on a
  shared host.
- **[Maintaining](maintaining/_index.md)** — for working on fpm-tune itself: how a
  release is cut, signed, and published to the Homebrew tap.

## The pieces underneath

- **[phpfpm](https://github.com/cboxdk/phpfpm)** — the library that discovers,
  parses, scrapes and reloads php-fpm. fpm-tune is the sizing and writing on top
  of it.
- **[fpm-exporter](https://github.com/cboxdk/fpm-exporter)** — Prometheus metrics
  for PHP-FPM, if all you want is to watch.
