---
title: Advisory mode
weight: 2
description: Run it permanently as an adviser that writes its conclusion to a file you paste by hand, and never touches the host.
---

# Advisory mode

Watching without acting is a first-class way to run this tool, not just a step on
the way to `--apply`. A daemon without `--apply` changes nothing and never will —
but until now it could not leave its conclusion anywhere you could act on: the
numbers were in a log line and on a metrics endpoint, and neither is something
you can put into a pool file.

`--recommend` gives it somewhere to write.

```bash
fpm-tune serve --recommend /var/lib/fpm-tune/recommended.conf
```

## What it writes

PHP-FPM configuration you can read, diff, and paste — with the evidence for each
number in the comments:

```ini
; shop: peak 34 workers busy; raised to 42, measured 96.0MiB/worker
;   measured per worker: median 88.0MiB, p95 137.0MiB, p99 194.0MiB, worst 512.0MiB (4096 readings)

[shop]
pm.max_children = 42
```

The spread is there because one number cannot answer the question you are
actually asking. Sizing follows the typical peak — it rises fast and falls on a
half-life, which is what makes it safe to divide a budget by — but a pool whose
p99 is twice its median has a tail, and a tail is what fills a host at the wrong
moment. A pool that sits flat wants a different decision from one that spikes, and
only the distribution tells them apart.

A pool that has not been measured yet is labelled plainly — the number is a
profile's guess, not the pool's own memory — so you know which figures to wait on.

## Three things it does that a naive version would not

- **It refuses a path php-fpm would load.** What it writes carries this tool's
  own marker, so a recommendation left inside a directory your master includes is
  a file php-fpm loads and the tool believes it wrote — configuring a host from a
  run that was explicitly not applying anything. A path inside an included
  directory is refused, with an actionable message.
- **It is rewritten only when the settings change**, not when the file would
  differ. The commentary moves every round — the reading count climbs, a
  percentile shifts by a bucket — and none of that is a change in the advice. So
  the file's modification time answers "when did this last move", which is the
  question a sidecar exists to answer, rather than "is the daemon up", which the
  metrics answer better.
- **It carries the evidence.** Someone reading it is deciding by hand, and a bare
  `pm.max_children` tells them nothing about whether to trust it.

## The workflow

Run it with `--recommend` and no `--apply` for a day or two, through a real
traffic pattern. Diff the file against what you have. When you agree with what it
recommends — and the numbers have stopped being profile guesses — either paste the
changes yourself, or add `--apply` and let it do the writing.

The same percentiles are on `/metrics` as `estimate="p50"`, `"p95"` and `"p99"`,
if you would rather graph them than read a file.
