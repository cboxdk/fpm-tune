---
title: Running as a daemon
weight: 1
description: Installing the service under systemd, switching its mode, what each round does, and what it logs.
---

# Running as a daemon

This page is for the person installing fpm-tune as a service and reading its log afterwards. It covers the install, the two modes, the round, and the log.

## Installing the service

```bash
sudo fpm-tune install-service            # advisory: watch, learn, recommend
sudo fpm-tune install-service --apply    # apply: also act on the plan
sudo fpm-tune install-service --cpu      # let the CPU measurement hold a pool at its CPU ceiling
sudo fpm-tune install-service --print    # show the config and unit; install nothing
sudo fpm-tune install-service --log-file /var/log/fpm-tune.log   # log to a file instead of the journal
```

It writes `/etc/fpm-tune/config` and `/etc/systemd/system/fpm-tune.service`, runs `daemon-reload`, enables the unit and starts it. The unit runs `fpm-tune serve --config /etc/fpm-tune/config`, so every setting lives in the config file and the unit never needs editing. The relevant lines of `--print`:

```
# /etc/fpm-tune/config
mode = advisory

# Where /metrics is served. Empty disables it.
metrics = 127.0.0.1:9110

# /etc/systemd/system/fpm-tune.service
ExecStart=/usr/local/bin/fpm-tune serve --config /etc/fpm-tune/config
Restart=on-failure
RestartSec=5
StateDirectory=fpm-tune
StateDirectoryMode=0700
```

The installed service binds `/metrics` to loopback (`127.0.0.1:9110`); a hand-run `fpm-tune serve` binds `:9110`, every interface. `--metrics` changes the address, and the install warns when it is reachable off the host, because the endpoint has no authentication. The unit says `Wants=php-fpm.service`; on Debian and Ubuntu the unit is `php8.4-fpm.service`, so that ordering does nothing there. It does not matter: the daemon looks for the master every round.

Re-running `install-service` keeps the config and changes only the keys you name. `--apply` sets `mode`, `--metrics` sets `metrics`, `--cpu` or `--cpu=false` sets `cpu`; everything else, including a mode set with `fpm-tune mode` and any hand edit, is left as it was. The unit is rewritten and the service restarted, which is also how an [upgrade](lifecycle.md) takes effect.

### Logging to a file

The log goes to the journal unless the unit says otherwise. `--log-file /var/log/fpm-tune.log` makes systemd append the daemon's output to that file instead (`StandardOutput=append:`, systemd 240 or later), and writes `/etc/logrotate.d/fpm-tune` beside it: weekly, eight kept, compressed, `copytruncate` because systemd opens the file once at start. Nothing in the daemon changes; it writes the same lines to stdout. A re-run keeps the file the unit has; `--log-file journal` goes back to the journal and removes the logrotate snippet.

## The mode

The daemon is in one of two modes. In advisory mode it watches, learns, publishes metrics and writes [the recommendation file](advisory-mode.md) at `/var/lib/fpm-tune/recommended.conf`; it changes nothing. In apply mode it also writes `zz-fpm-tune.conf` and reloads the master when a change clears the [hysteresis](../how-it-decides/hysteresis.md). The self-repair belongs to apply mode: an advisory daemon will not remove its own file from a host whose master that file stops from starting. See [Recovering a host](recovering.md).

`mode` is one line in the config, and `fpm-tune mode` rewrites it and restarts the service:

```bash
sudo fpm-tune mode apply
sudo fpm-tune mode advisory
fpm-tune mode
```

```
mode = apply (/etc/fpm-tune/config)
```

The other keys are documented in the [configuration reference](../configuration/reference.md).

## What a round does

Every 30 seconds (`interval`), in this order:

1. **Reconcile**, in apply mode, when the host has not been reconciled since this daemon last wrote: finish or undo a change a previous run left in flight, before discovery, because a broken configuration stops the master from parsing and nothing is discoverable after that. This step also turns the status page on for pools that lack one.
2. **Discover** the masters and their pools, re-reading the effective configuration, so a `pm.max_children` someone changed by hand is seen.
3. **Warn** about pools with no status page. In advisory mode they are not sized; in apply mode the next reconcile turns the page on.
4. **Scrape** each pool's status page and its workers' memory and CPU.
5. **Learn**: fold the readings into the baselines.
6. **Forget** pools that are no longer configured.
7. **Budget and CPU**: read the master's memory limit and the host's CPU.
8. **Plan**: divide the budget.
9. **Record** the counters the next round compares against.
10. **Publish**: metrics, the history ring, the log, and the recommendation file.
11. **Apply**, in apply mode or for the one round an [`apply-now`](applying-once.md) asks for, when a change is worth a reload.
12. **Save** the baselines, every 5 minutes, at once after a resize, and on shutdown.

## What it logs

The log is the daemon's output, at info level. Follow it with `sudo journalctl -u fpm-tune -f`, or `tail -f` the file when the service was installed with `--log-file`. The line to watch is the recommendation:

```
time=2026-09-03T17:11:01.747Z level=INFO msg="Pool recommendation" pool=www now=20 recommend=20 why="peak 2 workers busy, but not yet watched under load; held at its configured 20, estimated 48.0MiB/worker"
```

It is logged the first time a pool is seen and whenever the recommended ceiling changes, in both modes. As a heartbeat it is repeated every 30 minutes even when nothing changed (`heartbeat` in the config, `--heartbeat` on `serve`, `0` disables it), so a quiet host still shows a sign of life and the `why` firming up.

The other lines, by their `msg`:

- `Pool resized`, with `pool`, `from`, `to` and `reason`: a change reached the master. Apply mode only.
- `Mode suggestion`, with `pool`, `mode`, `consider` and `why`: this pool might fit a different `pm` better. Logged once per pool, and again only if the suggestion changes; fpm-tune never changes `pm` itself. See [Process managers](../how-it-decides/process-managers.md).
- `Forgot pools that are no longer configured`, with `pools`: their baselines were dropped.
- `Capacity exhausted`, a warning, once when the host becomes [out of capacity](../how-it-decides/dividing-the-budget.md), and `No longer at capacity` when it stops being.
- `Pools have no status page`, a warning, when the set of unsized pools changes.
- `Budget`, once at start with the budget line the plan is made against, and `Budget changed` with `from` and `to` when the source or the size moves: a `MemoryMax=` set on the unit, a container resized, or a cgroup that became unreadable. Every plan number moves with it, and this is the line that says why.
- `Pool bound by CPU rather than memory`, with `pool`, `cpu_ceiling`, `memory_ceiling`, `fill_workers`, `cpu_share` and whether the pool is held there or only would be with `--cpu`, the first round the pool runs out of CPU before memory; `Pool bound by memory again` when it stops. See [CPU per request](../how-it-decides/cpu.md).
- `Pool queued while the host's CPU was full`, a warning with `pool`, `queue`, `busy`, `configured`, `cpu_ceiling` and `host_busy`, the first round that finds requests waiting while the host is at 95% CPU or more, and `No longer queued while the host's CPU was full` when it stops. Another worker would find no core to run on, so this queue is the CPU's; see [CPU per request](../how-it-decides/cpu.md).
- `The recommendation changed`, with `path`: the recommendation file was rewritten.

`--verbose` adds the per-scrape detail. Persistent conditions are logged on the transition, not every round; `/metrics` is the continuous view. See [Metrics and alerting](metrics-and-alerting.md).
