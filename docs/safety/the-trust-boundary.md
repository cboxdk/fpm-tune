---
title: The trust boundary
weight: 2
description: What it trusts, what one tenant on a shared host can and cannot make it do to another, and what bounds it.
---

# The trust boundary

fpm-tune is often run on a host where several tenants own their own pool
configuration — shared hosting. It is worth being precise about what it trusts,
what a tenant can influence, and what stops one tenant from reaching another.

## What it trusts

- **The pool configuration**, which is root-owned. A tenant who can edit their
  own pool file can influence the plan for their own pool; the bound on how
  expensive they can make themselves is `php_admin_value[memory_limit]`, which the
  operator sets.
- **The php-fpm binary and config path**, checked before they are ever executed:
  absolute, not a symlink, owned by root or by this process, and — running as root
  — inside root-owned directories. A path read from an attacker-writable state file
  is subjected to the same checks at the point of use, not merely at discovery.

## What one tenant can do to another: nothing it should not

Because the pools share a budget, it is fair to ask what a tenant with expensive
workers takes from its neighbours. Measured, on a 4GiB host with five pools
configured for twelve workers each:

| the expensive pool's workers | it gets | a neighbour gets |
|-----------------------------:|--------:|-----------------:|
| 48 MiB (same as the rest)    |      12 |               12 |
| 200 MiB                      |       9 |               10 |
| 500 MiB                      |       5 |                6 |
| 1 GiB                        |       3 |                3 |

Growing more expensive costs a pool its own workers first, and does not buy them:
cost is what the budget is divided *by*, so an expensive pool gets fewer workers,
not more, and the pressure it puts on its neighbours is the same pressure it puts
on itself. That is the property to want from a shared budget.

Past the point where a single worker will not fit on the host at all, no
arrangement is valid and the tool refuses rather than choosing a loser — naming
the pool that made it impossible.

## What is refused rather than trusted

- **A status page that relabels itself.** A tenant who sets `pm.status_listen` to
  a socket they control cannot make their pool's memory be measured as another's:
  the view keeps the name discovery read from the root-owned configuration, and
  takes only numbers from the response.
- **A pool name that would escape the file.** A section name with a path
  separator or a control character in it cannot be written safely, so that one
  pool is reserved conservatively and left alone — it does not abort the whole
  change set and stop every other site being tuned.
- **A truncated configuration parse.** If `php-fpm -tt` produces more output than
  the tool will capture — a tenant with thousands of env lines in their own file —
  the parse is refused rather than treated as complete, because a partial parse
  would read every pool after the cut as removed and take their overrides out.

## Multi-master hosts

A host running two PHP versions has two masters, and their pools may share names
(`www` in both). Run one instance per master, scoped with `--drop-in-dir`.
Unscoped:

- `apply` refuses, naming every master and pid.
- `plan` and the metrics are advisory but a mixture: the budget falls back to the
  whole machine's (no single master's limit applies to all the pools), and pools
  sharing a name are suppressed from the per-pool metrics rather than published
  under a colliding label — with `fpm_tune_pools_ambiguous` counting them.

The state file records which master each pool was learned from, so two scoped
daemons sharing one file do not forget each other's pools.
