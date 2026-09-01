---
title: First run
weight: 2
description: The plan → advise → apply path, with the guarantee at each step spelled out.
---

# First run

A first run cannot surprise you if you understand what each command does before
you type it. This is that, in order.

## `plan` changes nothing

```bash
fpm-tune plan
```

`plan` reads the host and prints what it would do. It writes no PHP-FPM
configuration and reloads nothing. It *does* record what it observed, to the
state file, so that running it on a schedule builds a real baseline before apply
is ever used. Pass `--no-learn` to turn even that off.

Two things in its output are worth knowing on a first run:

- **Pools are `estimated`, not measured.** With no history, sizing starts from a
  workload profile, the same guess a hand-written config makes. The plan labels
  these plainly. They become measurements once the tool has watched real traffic.
- **The budget line names its source.** `via php-fpm's cgroup` is the number to
  trust; `via /proc/meminfo` on a bare VM means it is sizing against the whole
  machine, which is correct only if php-fpm really has no cap.

## `enable-status` turns on what it measures

fpm-tune sizes each pool from its **live status page**, the `pm.status` php-fpm
exposes over the pool's own socket. A stock php-fpm ships that page off: the
default `www` pool has `pm.status_path` commented out. So on a fresh host `plan`
finds a running master but nothing it can measure, and says exactly that:

```
a php-fpm master is running, but pool www has no pm.status_path, …
  fpm-tune enable-status
```

Turn it on and the pool becomes visible:

```bash
fpm-tune enable-status
```

This writes a small drop-in enabling `pm.status_path` and reloads, through the
same chain `apply` uses: validated against a sandbox first, and rolled back if the
master does not come back. It changes no `pm.max_children`; it only opens the page.
It enables the page on **every** pool that lacks one, not just the default, which
matters on a box where each site has its own pool. `apply` and `serve --apply` do
this for you as their first step, so you need `enable-status` yourself only when you
want to `plan` before you ever apply.

A pool that has no status page cannot be sized, so `serve` **warns** about it by
name each time the set changes ("Pools have no status page, so they are NOT being
sized"), and in `--apply` mode turns the page on itself. If you ever see a pool you
expected tuned sitting untouched, that warning, or a run of `enable-status`, is
where to look: the loop only sizes the pools it can actually measure.

## `apply` acts immediately

```bash
fpm-tune apply
```

There is no confirmation prompt: `apply` writes the file and reloads the master
straight away. That is the command's job, so run `plan` first if you want to
look before you leap. What makes running it safe rather than reckless is the
chain of guarantees underneath it:

1. **Validated against a sandbox first.** The change is rendered and checked with
   `php-fpm -t` against a copy of the pool directory. A configuration php-fpm
   would reject never reaches the directory it globs, not even for the length of
   a fork.
2. **One atomic write.** The whole change set reaches the host in a single
   rename, or not at all: a growth and the reduction that funds it are indivisible.
3. **A reload, not a restart.** SIGUSR2 cycles the workers gracefully and carries
   the listening sockets across, so no request is dropped.
4. **Rolled back if the master does not survive.** If php-fpm accepts the file at
   validation but fails to come back on the reload, the previous configuration is
   restored and you are told.
5. **Recoverable across a crash.** What is about to be written is recorded first,
   so an interrupted run is finished or undone on the next start.

If a single `apply` is interrupted (a Ctrl-C, a timeout) after the signal but
before the master is seen to survive, it says so and exits non-zero rather than
claiming success. Run it again to confirm; the change is in place and recorded.

## `serve` runs the loop

```bash
fpm-tune serve            # watch and publish metrics, change nothing
fpm-tune serve --apply    # close the loop: measure, decide, apply, repair
fpm-tune serve --recommend /var/lib/fpm-tune/recommended.conf   # advise, in a file
```

The recommended way to adopt it is to run `serve` with `--recommend` and no
`--apply` for a while. It changes nothing, and leaves its conclusion where you
can read it. See [Advisory mode](../operating/advisory-mode.md). When you trust
what it recommends, add `--apply`.

## What to try, and what not to

- **Safe to run once out of curiosity:** `plan`, `serve` (without `--apply`),
  `serve --recommend`. None of them touch the host.
- **Acts on the host:** `apply` and `serve --apply`. On a stable host already at
  the right size these are a no-op (nothing is written, nothing is reloaded), but
  they *are* live.
- **On a host running more than one php-fpm master**, name which one with
  `--drop-in-dir`; unscoped, the numbers are a mixture and `apply` refuses. See
  [The trust boundary](../safety/the-trust-boundary.md).
