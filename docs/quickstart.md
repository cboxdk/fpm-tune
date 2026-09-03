---
title: Quickstart
weight: 1
description: From install to a host sized by fpm-tune, in one read.
---

# Quickstart

This page takes you from install to a host sized by fpm-tune: read a plan, run it advisory for a day, then let it apply. It is written for the person who runs the host.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/cboxdk/fpm-tune/main/install.sh | sh
```

The installer verifies the release checksum, and the Sigstore signature when `cosign` is installed, and refuses to install on a mismatch. It never runs `sudo`: run it as root on a server so the binary lands in `/usr/local/bin`, where `sudo fpm-tune` can find it. The other methods and the details are in [Installation](getting-started/installation.md).

## Read a plan

```bash
fpm-tune plan
```

`plan` writes no configuration and reloads nothing. It records what it observed to the state file, so a plan run before anything is applied is already building a baseline. It prints the memory budget, one row per pool, and what it measured:

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

The WHY column is the evidence for the number. `estimated` means the pool has not been watched under load yet and is held where you configured it; `measured` means the per-worker cost is the pool's own. Every line is explained in [Reading a plan](getting-started/reading-a-plan.md).

### If it finds no pools

fpm-tune sizes a pool from its status page (`pm.status_path`), and a stock php-fpm ships that page off. On such a host `plan` says a master is running but its pools have no `pm.status_path`. Turn the page on:

```bash
sudo fpm-tune enable-status
```

This writes `zz-fpm-tune-status.conf` into the pool directory, validated first and rolled back if the master does not come back, and reloads. It changes no ceiling. `apply` and `serve --apply` do this on their own; `enable-status` is for reading a plan before anything is applied. A pool with no status page is not sized at all: `plan` leaves it out, and `serve` warns about it by name.

## Let it advise for a day

Run it in the background in advisory mode. On a systemd host:

```bash
sudo fpm-tune install-service
```

This writes `/etc/fpm-tune/config` and a unit, and starts the service. Every 30 s it measures, publishes metrics on `127.0.0.1:9110`, and writes its recommendation to `/var/lib/fpm-tune/recommended.conf`, logging it whenever it changes. It changes nothing. Without systemd, `fpm-tune serve` in a terminal does the same. `fpm-tune top` shows what it sees: busy workers, queues, the CPU side and every resize.

Give it a day through a real traffic pattern. The estimates become measurements, and the recommendation settles.

## Let it act

To apply the plan once, now:

```bash
sudo fpm-tune apply-now
```

The daemon writes `zz-fpm-tune.conf` into the pool directory and reloads, then stays in advisory mode. When there is nothing to change it says so:

```
nothing to change: every pool is at its planned ceiling
```

To let it keep the host sized from now on:

```bash
sudo fpm-tune mode apply
```

From here the daemon writes a new ceiling whenever a change is worth a reload (a growth of 15% held for 5 minutes, a shrink of 30% held for 20 minutes; see [Hysteresis](how-it-decides/hysteresis.md)), and repairs the host if its own file ever stops php-fpm from starting. `sudo fpm-tune mode advisory` turns it back. Every write is validated against a copy of the configuration, written atomically, reloaded with SIGUSR2, and rolled back if the master does not come back; see [How it fails safe](safety/how-it-fails-safe.md).

## Things to know on day one

- `fpm-tune help` lists the commands. `fpm-tune version` (or `--version`) prints the version.
- `fpm-tune plan --no-learn` reports without recording anything to the state file.
- `sudo fpm-tune apply --dry-run` renders and validates the change and writes nothing. `apply --no-learn` is refused: a run that writes has to record it, or the next run reloads the pool this one just reloaded.
- fpm-tune takes no positional arguments. `fpm-tune plan www` is refused, because a flag after a stray word would be silently ignored.
- Beside a running daemon, a hand-run `plan` prints `another fpm-tune is running, so this will report without recording what it observes` and carries on; a hand-run `apply` is refused. Use `apply-now`.
