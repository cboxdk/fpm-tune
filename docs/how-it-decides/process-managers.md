---
title: Static, dynamic, ondemand
weight: 5
description: What fpm-tune writes for each pm mode, why it never changes the mode, and the one suggestion it makes.
---

# Static, dynamic, ondemand

PHP-FPM gives every pool a `pm` mode, and the mode changes what
`pm.max_children` means. This page is what fpm-tune writes for each, and the
two cases where it suggests a different mode.

- **static**: the pool runs exactly `pm.max_children` workers, always. The
  number is the memory the pool commits right now.
- **dynamic**: PHP-FPM keeps a warm floor and scales between it and
  `pm.max_children` as load comes and goes, using `pm.start_servers` and the
  `pm.min_spare_servers` to `pm.max_spare_servers` band.
- **ondemand**: no workers until a request arrives; each is spawned on demand
  and killed when idle. The number is a ceiling the pool may reach.

## What it writes, per mode

| Mode | Written to `zz-fpm-tune.conf` |
|---|---|
| static | `pm.max_children` |
| ondemand | `pm.max_children` |
| dynamic | `pm.max_children`, `pm.start_servers` (25% of it), `pm.min_spare_servers` (10%), `pm.max_spare_servers` (50%), each rounded up |

The spare band is re-derived from the new ceiling so PHP-FPM's own scaling
stays coherent, and PHP-FPM's ordering rule (min spare, then start, then max
spare, then max children, each no larger than the next) is kept. A ceiling of
10 comes out as:

```ini
[www-forge]
pm.max_children = 10
pm.start_servers = 3
pm.min_spare_servers = 1
pm.max_spare_servers = 5
```

Spare settings are written for dynamic pools only. PHP-FPM refuses
`pm.start_servers` in a static or ondemand pool, and the
[sandbox validation](../safety/how-it-fails-safe.md) would catch that before a
reload, but it is cheaper never to write it.

An ondemand pool seen with no workers teaches the learner nothing about its
cost; [measuring workers](measuring-workers.md) has the rules for what counts.

## Why it never changes the mode

The right mode is a trade between idle memory, latency and cold starts, and the
tie-breakers (a latency target, a preference for memory that never moves, forty
pools each hit twice an hour) are not in the numbers on the host. fpm-tune
sizes inside the mode you chose and never writes `pm`.

| | static | dynamic | ondemand |
|---|---|---|---|
| idle memory | every worker resident, always | scales down to the floor | nothing when quiet |
| latency under load | workers always warm | warm floor absorbs bursts | each burst pays a cold start |
| fits | steady, latency-sensitive, memory to spare | most pools | many pools each rarely hit |

## The one suggestion it makes

Two measured shapes get a line under the plan table, and one `Mode suggestion`
line in the daemon's log the first time each is seen. Neither changes anything.

- A static pool holding idle workers: its busiest moment left at least two
  workers unused, and they hold at least 256 MiB between them. dynamic or
  ondemand would hand that memory back between requests, unless you keep them
  warm for latency on purpose.
- An ondemand pool that is queueing, or that the allocator could not fully
  satisfy: its bursts are paying cold-start latency, and a dynamic pool's warm
  floor would absorb them.

It does not push a busy dynamic pool toward static. Telling steady saturation
from one spike needs a sustained-load signal it does not keep.

The `pm` mode is a performance and memory decision and says nothing about
isolating one pool from another; that is `user`, `group`, `chroot` and
`open_basedir`, and what fpm-tune itself guards is in
[the trust boundary](../safety/the-trust-boundary.md).
