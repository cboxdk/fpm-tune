---
title: Forge and Ploi
weight: 1
description: From install to apply mode on a Laravel Forge or Ploi host, with a pool per site and MySQL sharing the memory.
---

# Forge and Ploi

This recipe takes a Forge or Ploi host from nothing to a daemon in apply mode. Both lay php-fpm out the same way: Ubuntu, a unit named `php8.4-fpm` (or whichever version was provisioned), and a pool file per site under `/etc/php/8.4/fpm/pool.d`. MySQL, and often Redis, share the host's memory with php-fpm, and no cgroup limit holds php-fpm back.

## 1. Install as root

SSH in as the deploy user and run the installer under `sudo`, so the binary lands in `/usr/local/bin`:

```bash
curl -fsSL https://raw.githubusercontent.com/cboxdk/fpm-tune/main/install.sh | sudo sh
```

Run without `sudo`, the installer cannot write `/usr/local/bin` and falls back to `~/.local/bin`, and `sudo fpm-tune` then fails because `sudo` resets `PATH`. If you download the archive by hand instead, `sudo install -m 0755 fpm-tune /usr/local/bin/fpm-tune` does the same. Check it:

```bash
fpm-tune version
```

```
0.1.0-beta.22
```

## 2. Turn on the status pages

fpm-tune sizes a pool from its status page, and the pool files Forge and Ploi write ship with `pm.status_path` commented out. A pool without one is not sized at all, and on a fresh host that is every pool, so the first `fpm-tune plan` reports no pools found. Turn the pages on:

```bash
sudo fpm-tune enable-status
```

This writes `zz-fpm-tune-status.conf` beside the pool files, validates the whole configuration with `php-fpm -t`, reloads php-fpm gracefully, and rolls back if the master does not come back. Your pool files are not touched. Run it again and it reports that every pool already exposes a status page and there is nothing to do.

## 3. Read a plan

```bash
fpm-tune plan
```

```
host memory 7.5GiB, 4 CPU(s) (via /proc/meminfo)
  used by other services:  3.0GiB (left for them; cap php-fpm's cgroup for a hard limit)
  reserve kept:            1.1GiB (15% of 7.5GiB)
  available to workers:    3.5GiB

POOL       MODE     NOW  PLAN  MEMORY    WHY
www        dynamic  20   20    960.0MiB  peak 2 workers busy, but not yet watched under load; held at its configured 20, estimated 48.0MiB/worker
www-forge  dynamic  10   41    1.7GiB    peak 33 workers busy; raised to 41, measured 41.9MiB/worker + 37.6KiB children

allocated 2.6GiB of 3.5GiB, 869.2MiB free
```

`plan` changes nothing. The first block is the budget: what MySQL and everything else is using is left to them, a 15% reserve is kept, and the rest is what the pools may fill. The table is one row per site. A pool that has not been watched under load is held at its configured value and marked `estimated`; a measured one shows its per-worker cost and the ceiling it gets. [Reading a plan](../getting-started/reading-a-plan.md) explains every line.

## 4. Let it watch

Install the service. It starts in advisory mode: it measures, logs and writes a recommendation, and changes nothing.

```bash
sudo fpm-tune install-service
journalctl -u fpm-tune -f
```

```
time=2026-09-03T17:11:01.747Z level=INFO msg="Pool recommendation" pool=www now=20 recommend=20 why="peak 2 workers busy, but not yet watched under load; held at its configured 20, estimated 48.0MiB/worker"
```

A line like that appears whenever a pool's recommendation moves, and every 30 min as a sign of life. The current recommendation is also written to `/var/lib/fpm-tune/recommended.conf`. Leave it for a day or a week, long enough to cover a traffic peak and a deploy, and check that the numbers have settled and match what you know about the sites: the shop is the heavy one, the docs site is cheap.

## 5. Let it act

```bash
sudo fpm-tune mode apply
```

The same service restarts in apply mode. From here it writes each pool's ceiling to `zz-fpm-tune.conf` and reloads php-fpm when a change is large enough, and has been stable long enough, to be worth a reload ([hysteresis](../how-it-decides/hysteresis.md)). Back to advisory at any time:

```bash
sudo fpm-tune mode advisory
```

To apply the current plan once without waiting for the daemon's damping, see [Applying once](../operating/applying-once.md).

## 6. Put a limit on php-fpm

The `used by other services` line is what MySQL and Redis hold right now, and it is subtracted from the budget on every round. It is not a promise about next Tuesday. For a hard wall, give the php-fpm unit a cgroup memory limit; it takes effect live and survives reboots:

```bash
sudo systemctl set-property php8.4-fpm.service MemoryMax=4G
```

fpm-tune reads the limit from php-fpm's cgroup on its next round and sizes inside it, and the first line of the plan changes to `php-fpm's memory 4.0GiB`. [The budget](../how-it-decides/the-budget.md) explains how the limit and the reserve combine.

## When the host is out of capacity

When every pool wants more and the budget is gone, the plan says so and the daemon stops growing pools. On a Forge or Ploi host that is a sizing question, not a tuning one: a bigger server, or a heavy site moved off. [Metrics and alerting](../operating/metrics-and-alerting.md) has the signal to alert on.
