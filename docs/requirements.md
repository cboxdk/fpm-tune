---
title: Requirements
weight: 2
description: What fpm-tune needs from the host, and what it falls back to when a kernel feature is missing.
---

# Requirements

This page lists what the code checks for or reads. Anything not here is not required.

## Linux

fpm-tune reads the memory budget from the cgroup php-fpm runs in (v2 or v1) or from `/proc/meminfo`, finds masters in the process table, and reads each worker's memory under `/proc/<pid>`. All of that is Linux. The macOS build compiles and is for development; it cannot size a host.

## php-fpm with a status page

A pool is sized from its status page, `pm.status_path`, scraped over the pool's own listen socket. A pool without one is not sized: `plan` leaves it out, and `serve` warns about it by name. `sudo fpm-tune enable-status` turns the page on for every pool that lacks one (as do `apply` and `serve --apply`), by writing `zz-fpm-tune-status.conf` and reloading.

fpm-tune also runs `php-fpm -tt` to read the effective configuration and `php-fpm -t` to validate a change, and reloads with SIGUSR2. The drop-ins go into the pool directory the master includes; a master that includes no directory cannot be written.

## Root, for the commands that write

`apply`, `serve --apply`, `enable-status`, `install-service`, `mode` and `apply-now` need root: they write to the pool directory or `/etc/fpm-tune`, talk to systemd, or use the control socket, which is root's. `plan`, `serve` (advisory) and `top` run as the php-fpm user or as root; they read the pool configuration, the status pages and the workers' `/proc` entries. Run as any other user, `plan` still prints the plan and says it cannot write `/var/lib/fpm-tune`, so no baseline is recorded.

## Kernel features, and what happens without them

| feature | since | used for | without it |
|---|---|---|---|
| `/proc/<pid>/smaps_rollup` | 4.14 | a worker's proportional set size, so shared pages are not counted once per worker | the worker's RSS |
| `MemAvailable` in `/proc/meminfo` | 3.14 | the `used by other services` line on a host without a cgroup limit | no neighbour subtraction; only the reserve is held back |
| cgroup v2 `memory.peak` | 5.19 | the cgroup's high-water mark, in the recommendation file and on `/metrics` | a running maximum of `memory.current`, kept by the daemon |

A cgroup v1 host reads `memory.limit_in_bytes`, `memory.usage_in_bytes` and `memory.max_usage_in_bytes` instead.

## systemd, for the service

`install-service` and `mode` write a unit under `/etc/systemd/system` and call `systemctl`. Without systemd, run `fpm-tune serve` under whatever supervises processes on the host; nothing else depends on systemd.

## A state directory

`/var/lib/fpm-tune` holds the learned baselines, the backup and transaction record of a change in flight, the recommendation file, the control socket and the state lock. It is created on first run by a user who may; under systemd, `StateDirectory=fpm-tune` in the unit creates it. The per-pool-directory locks go under `/run/fpm-tune`.
