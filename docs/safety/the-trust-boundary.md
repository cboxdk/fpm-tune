---
title: The trust boundary
weight: 2
description: What it trusts, what one tenant on a shared host can make it do to another, and what only root can reach.
---

# The trust boundary

fpm-tune often runs on a host where several tenants own their own pool configuration. This page says what it trusts, what a tenant can influence, what it refuses, and which of its own surfaces need protecting.

## What it trusts

- **The pool configuration**, which is root-owned. A tenant who can edit their own pool file influences the plan for their own pool. How expensive they can make one worker is bounded by `php_admin_value[memory_limit]`, which the operator sets.
- **The php-fpm binary and config path**, checked before either is executed: absolute, not a symlink, owned by root or by this process, and (running as root) inside root-owned directories. A path read back from the state file gets the same checks at the point of use.

## What one tenant can do to another

The pools share one budget, and cost is what the budget is divided by, so a pool whose workers get heavier is allocated fewer of them: whatever it takes from its neighbours it has first taken from itself. When one worker will not fit on the host at all, no arrangement is valid, so the run refuses rather than pick a loser, and names the pool that made it impossible. [Dividing the budget](../how-it-decides/dividing-the-budget.md) has the arithmetic.

## What is refused rather than trusted

- **A status page that relabels itself.** A tenant who points `pm.status_listen` at a socket they control cannot make their pool's memory count as another's. The pool name comes from the root-owned configuration; only the numbers come from the response.
- **A pool name that would escape the file.** A section name with a path separator or a control character cannot be written safely, so that pool is sized conservatively and left alone. The rest of the change set still goes through.
- **A truncated config parse.** If a tenant stuffs thousands of `env` lines into their own file and `php-fpm -tt` prints more than the tool will capture, the parse is refused. A half-read parse would treat every pool after the cut as deleted and strip their overrides.

## The control socket: root only

`/var/lib/fpm-tune/control.sock` is how `apply-now` (and the `a` key in `top`) asks a running daemon to apply its plan once. Whoever can write to it can reconfigure php-fpm, so it is created mode 0600 in a 0700 directory and answers root alone. A stale socket file from a dead daemon is replaced; a file at that path that is not a socket is refused.

## /history.json: no authentication

`/history.json` is served on the metrics address beside `/metrics`, read-only and without authentication. It carries pool names, ceilings, busy counts and the daemon's apply events for the last 24 h. The installed service binds `127.0.0.1:9110`; a hand-run `serve` binds `:9110` on every interface, so pass `--metrics 127.0.0.1:9110` or keep the port behind a firewall. [Metrics and alerting](../operating/metrics-and-alerting.md) describes both endpoints.

## Multi-master hosts

A host running two PHP versions has two masters, and their pools may share names (`www` in both). Run one daemon per master, scoped with `--drop-in-dir`; [Two PHP versions](../cookbook/two-php-versions.md) is the recipe. Unscoped:

- `apply` refuses, naming every master and pid.
- `plan` and the metrics are advisory but mixed: the budget falls back to the whole host, since no single master's limit binds all the pools, and pools sharing a name are left out of the per-pool metrics rather than published under a colliding label. `fpm_tune_pools_ambiguous` counts them.

The state file records which master each pool was learned from: `www` on one master and `www` on the other are two pools, with two baselines.
