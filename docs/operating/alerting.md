---
title: Alerting
weight: 3
description: The metrics that tell you whether it is working, and the one distinction (watching versus acting) that is invisible without them.
---

# Alerting

The log reports a persistent condition once, on the transition, rather than every
interval, which is right for reading and useless for alerting. These are the
series to build on instead, served on `/metrics` (default `:9110`; `/healthz`
answers 200 while the process is up).

## Is it working at all

```
fpm_tune_apply_enabled                    # 0 means this process only watches
fpm_tune_apply_blocked{reason}            # asked to apply and cannot
fpm_tune_last_run_timestamp_seconds       # not advancing means the loop has stalled
fpm_tune_last_apply_timestamp_seconds     # not advancing while changes are pending
fpm_tune_applies_failed_total
fpm_tune_rollbacks_total                  # above zero deserves a look at the log
fpm_tune_rollback_failed_total            # worse: a rejected file is still on disk
fpm_tune_repairs_total                    # it had to undo something a run left behind
```

`apply_blocked` is the one people forget. A process that is watching and one that
is acting look identical from outside, and that difference is the whole question
when a host is not being tuned. Its `reason` label is the actionable part:

- `no_master`: nothing to apply to.
- `lock`: another fpm-tune holds the pool directory.
- `unrepaired`: a previous run left something the tool could not resolve.
- `budget_unconfirmed`: php-fpm's own memory limit could not be read, so the
  tool refuses to write from the machine's. Pass `--memory`. See
  [The budget](../how-it-decides/the-budget.md).
- `state_unsaved`: an apply succeeded but its record could not be written, so the
  reload brake is not on disk.

## Is the host full

```
fpm_tune_pool_demand_unmet{pool}   # this pool wanted more workers than it got
fpm_tune_capacity_exhausted        # ...and that is true of at least one pool
```

Either one means no configuration change will help. The machine needs more RAM,
or fewer sites. See
[telling needs-more from machine-full](../how-it-decides/dividing-the-budget.md#telling-needs-more-from-machine-full).

## What it is sizing to

```
fpm_tune_pool_workers_configured{pool}
fpm_tune_pool_workers_recommended{pool}
fpm_tune_pool_worker_rss_bytes{pool,estimate}   # typical_peak, high_water, p50, p95, p99
fpm_tune_pool_baseline_confidence{pool}         # 0 to 1; below 1 the pool will not be cut
fpm_tune_pool_measured{pool}                    # 1 when sized from its own memory, not a guess
fpm_tune_budget_bytes{state}                    # total, reserved, allocated, free
```

The `p50`/`p95`/`p99` estimates are the measured spread, which is what to graph
when a host misbehaves at its busiest minute rather than on average.

## Which resource a pool runs out of first

```
fpm_tune_pool_request_cpu_share{pool,estimate}   # p50, p90: share of a request's wall time on CPU
fpm_tune_pool_request_cpu_readings{pool}         # how many requests that is built on; under 20, no verdict
fpm_tune_pool_cpu_fill_workers{pool}             # busy workers that fill the host's CPU
fpm_tune_pool_cpu_limited{pool}                  # 1 when the pool hits the CPU before its memory ceiling
```

`cpu_limited` at 1 is a pool that queues under load however much RAM the host
has: its ceiling is above the workers that fill the CPU. Compare
`cpu_fill_workers` with `workers_recommended`; the gap is what `--cpu` would
take away. See [CPU per request](../how-it-decides/cpu.md).

## Two you should not see above zero

```
fpm_tune_pools_unreachable   # a pool could not be scraped this round
fpm_tune_pools_ambiguous     # two masters share a pool name; those pools are NOT published
```

`pools_ambiguous` above zero means the daemon is running unscoped on a
multi-master host and two pools share a name. `www` is the default in every
distribution. Those pools are suppressed from the per-pool series rather than
published under a colliding label; name a master with `--drop-in-dir`. See
[The trust boundary](../safety/the-trust-boundary.md).
