---
title: Applying once
weight: 2
description: Watching the daemon with fpm-tune top, and applying the plan it shows once with apply-now.
---

# Applying once

This page is for the person who has read the daemon's plan and wants to act on it once, without switching the daemon to apply mode. `fpm-tune top` shows the plan; `sudo fpm-tune apply-now` (or the `a` key in `top`) applies it.

## fpm-tune top

```bash
fpm-tune top                          # the installed service
fpm-tune top --addr 10.0.0.5:9110     # another host's daemon, for looking
fpm-tune top --refresh 2s
```

`top` reads `/history.json` on the daemon's metrics address, every 5 seconds by default (`--refresh`), and changes nothing. `--addr` defaults to the `metrics` line in `/etc/fpm-tune/config`, with a wildcard host read as `127.0.0.1`, else `127.0.0.1:9110`. It needs a daemon of at least beta.22, which is the version that serves the history; an older one answers 404 and `top` says so.

The screen, top to bottom:

- The title bar: the hostname, a mode badge (`advisory` or `apply`), whether the CPU ceiling is on, and the daemon's version.
- The host line: memory, cores, where the memory figure came from, and the host's CPU busy ratio now and over time.
- The pool table: `POOL`, `BUSY` (workers busy now), `QUEUE` (requests waiting), `MAX` (the configured ceiling), `PLAN` (the ceiling the daemon would set), a sparkline of busy workers over time, `CPU/REQ` (the median CPU share of a request), `CPU MAX` (the CPU ceiling) and `BOUND BY` (`memory`, `cpu`, or `cpu (held)` when `--cpu` holds the pool there).
- The selected pool's panel: busy workers, `MAX`, `PLAN`, the memory ceiling and the CPU ceiling as lines over time, with the queue and the CPU share as sparklines beneath.
- The events since the daemon started: resized, changed outside the daemon, apply failed, rolled back, rollback failed, repaired.

The keys:

| key | what it does |
|---|---|
| `↑` `↓`, `k` `j` | select the previous or next pool |
| `Tab` | cycle through the pools |
| `1` `2` `3` | set the span of the charts: the last hour, six hours, everything the daemon holds |
| `r` | fetch now, rather than at the next refresh |
| `a` | open the apply panel |
| `q`, `Ctrl-C` | quit |

In the apply panel, `Enter` or `y` applies and `Esc` or `n` cancels. The panel lists what would change, pool by pool, from the newest round. `Enter` runs `fpm-tune apply-now` in the terminal, through `sudo` unless you are root already, so a password prompt can appear. When the daemon is in apply mode the panel says so: it would get to the change on its own, and `Enter` only skips its reload damping.

`a` is refused when `--addr` is not a loopback address. `apply-now` talks to the control socket on the host it runs on, so a view of another host's history can only look. The notice says: this view reads that address; apply-now reaches only the daemon on this host, so run `top` there.

## fpm-tune apply-now

```bash
sudo fpm-tune apply-now
sudo fpm-tune apply-now --control /var/lib/fpm-tune/control.sock
```

The daemon holds the state, the plan and the state lock for as long as it runs, so `fpm-tune apply` beside it is refused. `apply-now` is the way to act on a running daemon's plan: it asks the daemon, over a unix socket, to run one round with applying forced on and the [hysteresis](../how-it-decides/hysteresis.md) waived (the smallest change and the shortest interval both reduced to nothing, because you have read the plan and asked for it). The daemon stays in the mode it is in. On an advisory daemon it is the way to apply without switching modes; on an apply-mode daemon it skips the damping for a change you have already read.

The socket is `/var/lib/fpm-tune/control.sock`, created mode `0600` and owned by the daemon's user, which is why `apply-now` needs root. `--control` on both `serve` and `apply-now` moves it. A daemon that could not create the socket says so at startup (`No control socket; fpm-tune apply-now will not reach this daemon`).

It prints one line per pool that changed, `pool  from → to  reason`, or the daemon's message when nothing did:

```
nothing to change: every pool is at its planned ceiling
```

When the round failed, the daemon's error is printed and the exit status is 1. Because every call is a full round with the damping off, do not script it in a loop: that is apply mode with the safeguards removed. Switch to apply mode instead.

One writer per pool directory. An apply-mode daemon holds the directory's lock for as long as it runs; an advisory daemon takes it for the one write and gives it back. So an `apply-now` sent to a second daemon running beside an apply-mode one is refused, and the answer names the cause: it cannot take the pool-directory lock, an `fpm-tune serve --apply` keeps it for as long as it runs, ask that daemon instead. A budget the daemon could not confirm is refused the same way, with the fix (`--memory`, or make the master's cgroup readable).

The socket speaks HTTP: `POST /apply`, answered with JSON. The fields:

- `changed`: a list of the pools resized, each with `at`, `kind` (`resized`), `pool`, `from`, `to` and `detail` (the plan's reason). These are the same objects `/history.json` carries as events.
- `message`: why nothing changed, when nothing did.
- `error`: the apply's failure, in its own words, when it failed.

The daemon answers `503` when it could not take the request within two minutes (a round was already running), and `504` when the round it started did not finish within two minutes. The client waits three.
