---
title: The trust boundary
weight: 2
description: What it trusts, what one tenant on a shared host can and cannot make it do to another, and what bounds it.
---

# The trust boundary

fpm-tune often runs on a host where several tenants own their own pool
configuration (shared hosting). So it's fair to ask exactly what it trusts, what a
tenant can nudge, and what keeps one tenant off another.

## What it trusts

- **The pool configuration**, which is root-owned. A tenant who can edit their
  own pool file can influence the plan for their own pool; the bound on how
  expensive they can make themselves is `php_admin_value[memory_limit]`, which the
  operator sets.
- **The php-fpm binary and config path**, checked before they are ever executed:
  absolute, not a symlink, owned by root or by this process, and (running as root)
  inside root-owned directories. A path read from an attacker-writable state file
  is subjected to the same checks at the point of use, not merely at discovery.

## What one tenant can do to another: nothing it should not

The pools share one budget, so here's the fair question: what does a tenant with
expensive workers take from its neighbours? Not much, and it pays for it itself
first.

Say five pools each want a dozen workers, and one pool's workers start getting
heavier. That heavy pool loses *its own* workers first; its neighbours barely move.
Push it to an order of magnitude heavier than the rest and it's down to a handful,
while everyone else is still close to their dozen. That's the whole trick: cost is
what the budget's divided *by*, so getting expensive buys a pool *fewer* workers, not
more. The squeeze it puts on its neighbours is the same squeeze it puts on itself.
Nobody can get greedy at someone else's expense.

And past the point where a single worker won't fit on the host at all? No arrangement
is valid, so it refuses rather than pick a loser, and names the pool that made it
impossible.

## What is refused rather than trusted

- **A status page that relabels itself.** A tenant who points `pm.status_listen` at
  a socket they control can't make their pool's memory count as another's. The pool
  name comes from the root-owned config; only the numbers come from the response.
- **A pool name that would escape the file.** A section name with a path separator
  or a control character in it can't be written safely. So that one pool is sized
  conservatively and left alone. It doesn't blow up the whole change set and stop
  every other site from being tuned.
- **A truncated config parse.** Say a tenant stuffs thousands of env lines into
  their own file, and `php-fpm -tt` spills more output than the tool will capture.
  It refuses that parse rather than trust it, because a half-read parse would treat
  every pool after the cut as *deleted*, and quietly strip their overrides.

## Multi-master hosts

A host running two PHP versions has two masters, and their pools may share names
(`www` in both). Run one instance per master, scoped with `--drop-in-dir`.
Unscoped:

- `apply` refuses, naming every master and pid.
- `plan` and the metrics are advisory but a mixture: the budget falls back to the
  whole machine's (no single master's limit applies to all the pools), and pools
  sharing a name are suppressed from the per-pool metrics rather than published
  under a colliding label, with `fpm_tune_pools_ambiguous` counting them.

The state file records which master each pool was learned from, so two scoped
daemons sharing one file don't forget each other's pools.
