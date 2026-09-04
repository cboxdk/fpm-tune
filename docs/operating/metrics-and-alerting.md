---
title: Metrics and alerting
weight: 3
description: Every series on /metrics, the three alerts that matter, /healthz, and the day of history on /history.json.
---

# Metrics and alerting

This page is for the person wiring fpm-tune into Prometheus or a dashboard. It lists every series the daemon publishes, the three alerts that matter, and the two other endpoints on the same address.

The daemon serves `/metrics`, `/healthz` and `/history.json` on one address: `127.0.0.1:9110` for the installed service, `:9110` for a hand-run `serve` (`--metrics`, or `metrics` in the config; empty disables all three). None of them has authentication, and `/history.json` carries pool names and a day of per-pool load, so keep the address on loopback or a private network and reach it over an SSH tunnel. `/healthz` answers `200 ok` while the process is up and says nothing more; `fpm_tune_last_run_timestamp_seconds` is what says the loop is running.

The series are named `fpm_tune_*`, disjoint from fpm-exporter's `phpfpm_*`, so both can be scraped side by side.

## The three alerts that matter

**The host is out of capacity.** `fpm_tune_capacity_exhausted` is 1 when a pool wants more workers and there is nowhere to get them: no free budget, no neighbour holding slack. `fpm_tune_pool_demand_unmet{pool}` is the same news per pool. No configuration change will help; the host needs more memory or fewer pools. See [Dividing the budget](../how-it-decides/dividing-the-budget.md).

**It was asked to apply and cannot.** `fpm_tune_apply_enabled` says what the process was asked to do and never changes. `fpm_tune_apply_blocked{reason}` is 1 when a round declined to apply for a reason other than having nothing to do, and the `reason` is the fix:

- `no_master`: no php-fpm master to apply to.
- `lock`: another fpm-tune holds the pool directory.
- `unrepaired`: a previous run left a change this one could not finish or undo. See [Recovering a host](recovering.md).
- `budget_unconfirmed`: php-fpm's own memory limit could not be read, so the daemon refuses to write from the host's. Pass `--memory`, or make the master's cgroup readable. See [The budget](../how-it-decides/the-budget.md).
- `state_unsaved`: a change was applied but the record of it could not be written, so the reload brake is not on disk.

**The last apply is old while a change is pending.** `fpm_tune_last_apply_timestamp_seconds` advances only when a change was written and adopted. A daemon in apply mode whose `fpm_tune_pool_workers_recommended` has differed from `fpm_tune_pool_workers_configured` for longer than the hysteresis holds a change (20 minutes for a shrink) and whose last apply is older than that is not acting. `fpm_tune_last_run_timestamp_seconds` not advancing is a different fault: the loop has stalled.

Beside these, alert on any increase of `fpm_tune_rollback_failed_total`: a rejected file is still on disk, and the next reload from any source adopts it.

## Every series

Per pool, labelled `pool`:

- `fpm_tune_pool_workers_configured`: `pm.max_children` as php-fpm is running it.
- `fpm_tune_pool_workers_recommended`: the ceiling fpm-tune would set. Differs from configured while a change is pending or held back.
- `fpm_tune_pool_worker_rss_bytes{estimate}`: per-worker memory, PSS where the kernel reports it. `typical_peak` is what sizing follows, `high_water` the largest worker ever seen, `p50`, `p95` and `p99` the measured spread.
- `fpm_tune_pool_subtree_rss_bytes`: the high-water of a worker and everything it spawned. The gap to `high_water` is what children cost.
- `fpm_tune_pool_child_rss_bytes`: the child memory folded into each worker's cost. Zero for a plain web pool.
- `fpm_tune_pool_baseline_confidence`: 0 to 1. Below 1 the pool is not cut below its configured ceiling.
- `fpm_tune_pool_measured`: 1 when the pool is sized from its own memory rather than an estimate.
- `fpm_tune_pool_demand_unmet`: 1 when the pool wanted more workers than it was given.
- `fpm_tune_pool_cpu_ratio{estimate}`: CPU seconds over wall seconds per request, `p50` and `p90`. Absent until the pool has enough readings.
- `fpm_tune_pool_cpu_readings`: how many requests the CPU share is built on. Under 20, no shape is called.
- `fpm_tune_pool_cpu_fill_workers`: how many busy workers of this pool fill the host's CPU.
- `fpm_tune_pool_cpu_box_millicores_per_worker`: what one busy worker costs the whole host, MySQL, nginx and the kernel included. Absent until the fit has enough spread.
- `fpm_tune_pool_cpu_ceiling`: the workers `--cpu` holds the pool at, published whether or not `--cpu` is on.
- `fpm_tune_pool_cpu_starved_rounds`: scrapes that found requests queued while the host's CPU was full.
- `fpm_tune_pool_cpu_limited`: 1 when the pool runs out of CPU before its memory ceiling.

The host:

- `fpm_tune_budget_bytes{state}`: `total`, `reserved`, `reserved_children`, `allocated`, `free`.
- `fpm_tune_cgroup_memory_bytes{state}`: what the master's cgroup used, `current` and `peak`, workers and children together. Absent on a host with no cgroup.
- `fpm_tune_capacity_exhausted`: 1 when the host is out of capacity.
- `fpm_tune_pools_unreachable`: pools that could not be scraped this round. Their allocation is left alone.
- `fpm_tune_pools_ambiguous`: pool names shared by more than one master. Those pools are not published at all; name a master with `--drop-in-dir`. See [The trust boundary](../safety/the-trust-boundary.md).
- `fpm_tune_last_run_timestamp_seconds`: when the last round completed.

Acting:

- `fpm_tune_apply_enabled`: 1 when this process acts on its plan.
- `fpm_tune_apply_blocked{reason}`: 1 when it could not, with the reason above.
- `fpm_tune_last_apply_timestamp_seconds`: when a change was last written and adopted.
- `fpm_tune_applies_failed_total`: applies that ended in an error.
- `fpm_tune_rollbacks_total`: changes taken back out because php-fpm rejected them or the master did not survive the reload.
- `fpm_tune_rollback_failed_total`: rejected changes that could not be taken back out.
- `fpm_tune_repairs_total`: times a previous run's leftovers had to be undone or completed, or the tool's own file removed to let php-fpm start.

A slice of a real scrape, from an apply-mode daemon:

```
fpm_tune_apply_enabled 1
fpm_tune_budget_bytes{state="allocated"} 1.44595331e+09
fpm_tune_budget_bytes{state="free"} 2.304453244e+09
fpm_tune_budget_bytes{state="reserved"} 4.355561062e+09
fpm_tune_budget_bytes{state="reserved_children"} 385500
fpm_tune_budget_bytes{state="total"} 8.105967616e+09
fpm_tune_capacity_exhausted 0
fpm_tune_pool_cpu_ceiling{pool="www-forge"} 10
fpm_tune_pool_cpu_limited{pool="www-forge"} 1
fpm_tune_pool_workers_configured{pool="www-forge"} 10
fpm_tune_pool_workers_recommended{pool="www-forge"} 10
```

## /history.json

Every round leaves one sample in a ring in the daemon's memory, and every apply and every change it notices leaves an event. `/history.json` hands them out, and is what `fpm-tune top` draws from:

```bash
curl -s 127.0.0.1:9110/history.json | jq '.rounds[-1]'
curl -s '127.0.0.1:9110/history.json?last=120' | jq '.rounds[].pools[] | select(.pool=="www") | .active'
```

`?last=N` limits the rounds to the newest N; the events always come whole. The ring holds a day at the default interval (`history` in the config, `--history` on `serve`; 24 hours divided by the interval, and a thousand events). It starts empty at every daemon start and is never written to disk; Prometheus is the place for anything that must outlive a restart.

The shape, with one round from a real host:

```json
{
    "interval_seconds": 30,
    "capacity": 2880,
    "host": {
        "hostname": "cbox-web",
        "version": "1.0.0",
        "apply": true,
        "cpu_ceiling": true,
        "cpu_headroom": 2,
        "memory_bytes": 8105967616,
        "cpu_millicores": 4000,
        "memory_source": "/proc/meminfo"
    },
    "rounds": [
        {
            "at": "2026-09-03T19:54:24.257136064Z",
            "host_busy_ratio": 0.07414469906368805,
            "host_busy_known": true,
            "pools": [
                {
                    "pool": "www-forge",
                    "active": 1,
                    "queue": 0,
                    "configured": 10,
                    "recommended": 10,
                    "demand_unmet": false,
                    "worker_bytes": 43932035,
                    "memory_ceiling": 41,
                    "cpu_ratio_p50": 0.9,
                    "cpu_readings": 1464,
                    "cpu_fill_workers": 5,
                    "cpu_ceiling": 10,
                    "cpu_limited": true,
                    "cpu_bound": true
                }
            ]
        }
    ],
    "events": []
}
```

`host` is what the title bar of `top` shows: the mode (`apply`), whether the CPU ceiling is on, and where the budget came from. A round carries the host's CPU busy ratio over the interval (`host_busy_known` is false on the first round) and, per pool, what was observed (`active`, `queue`, `configured`), what was planned (`recommended`, `demand_unmet`, `worker_bytes`, and `memory_ceiling`, what memory alone would have set, so a CPU-held pool shows the gap) and the CPU side.

Events carry `at`, `kind`, and where they apply `pool`, `from`, `to` and `detail`. The kinds: `resized` (the daemon changed a ceiling), `apply_failed`, `rolled_back`, `rollback_failed`, `repaired`, and `changed`, a ceiling that moved without the daemon moving it: a hand edit, a deploy, or an `fpm-tune apply` run beside it.
