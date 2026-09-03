---
title: Advisory mode
weight: 4
description: The recommendation file an advisory daemon writes, and how to act on it by hand.
---

# Advisory mode

This page is for the person who wants fpm-tune's numbers without letting it write them. In advisory mode the daemon watches, learns, publishes metrics and writes its conclusion to a file. It changes nothing.

## The recommendation file

The installed service starts in advisory mode and writes `/var/lib/fpm-tune/recommended.conf` (`recommend` in the config; `--recommend` on a hand-run `serve`). Nothing reads it. It is PHP-FPM configuration to read, diff and paste, with the evidence in the comments:

```ini
; PHP-FPM pool settings recommended by fpm-tune.
;
; NOTHING READS THIS FILE. It is written by a daemon running without
; --apply, which means it has changed nothing and will change nothing.
; Copy what you want into the directory your master includes.
;
; As of 2026-09-03T14:18:33Z
;
; www-forge: cpu-bound; 10 busy workers fill the CPU, so held there rather than the 41 memory allows, measured 35.2MiB/worker + 37.6KiB children
;   measured per worker: median 26.9MiB, p95 32.0MiB, p99 38.1MiB, worst 38.1MiB (2992 readings)
```

The pool sections follow, in the same form as `zz-fpm-tune.conf`. A pool not yet measured is marked `NOT YET MEASURED`: its number is a profile's guess. The numbers are undamped: the [hysteresis](../how-it-decides/hysteresis.md) exists because every change the daemon makes costs a reload, and deciding by hand you do not need it.

The file is rewritten only when the `pm.*` settings change, not when the commentary moves, so its modification time says when the advice last moved. Each rewrite logs `The recommendation changed`.

A path inside a directory the master includes is refused. The file carries this tool's own marker, so php-fpm would load it and fpm-tune would take it for its own work. With `--drop-in-dir` it is refused at startup; otherwise every round, logged once.

## The workflow

Run advisory for a day or two, until the pools you care about are measured. Then act on the file one of three ways:

1. Paste the sections you agree with into the directory your master includes and reload: `sudo systemctl reload php8.4-fpm`.
2. Run `sudo fpm-tune apply-now`, or press `a` in `fpm-tune top`: the plan is applied once and the daemon stays advisory. See [Applying once](applying-once.md).
3. Switch with `sudo fpm-tune mode apply`, and the daemon writes from then on.
