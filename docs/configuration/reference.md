---
title: Reference
weight: 1
description: Every subcommand and flag with its default, the config file format, the files and paths, the pool markers, and the exit status.
---

# Reference

For looking up a flag, a key or a path. Each entry is one line; the pages under [How it decides](../how-it-decides/_index.md) and [Operating it](../operating/_index.md) say why. Flags are shown as `--flag`; `-flag` is accepted too, and `fpm-tune <command> -h` prints the same list.

## Commands

| command | what it does | root |
|---|---|---|
| `plan` | show what would change, and why; changes no configuration, records what it observed | no |
| `apply` | write the pool settings and reload php-fpm, once | yes |
| `enable-status` | turn on `pm.status_path` for pools that have none, and reload | yes |
| `serve` | keep measuring, with `/metrics`; `--apply` to act | advisory no, `--apply` yes |
| `install-service` | run `serve` under systemd, advisory by default | yes |
| `mode` | show or switch the service between `advisory` and `apply` | switching yes |
| `top` | watch the running service in the terminal | no |
| `apply-now` | ask the running service to apply its plan once | yes |
| `version` | print the version (`--version`, `-v` too) | no |
| `help` | print the command list (`--help`, `-h` too) | no |

`plan`, advisory `serve` and `top` run as the php-fpm user or root, because they read the pool configuration and the status pages. Positional arguments are refused on every command: Go's flag parsing stops at the first one and drops the flags after it. See [Requirements](../requirements.md).

## Flags shared by plan, apply, enable-status and serve

| flag | default | meaning |
|---|---|---|
| `--state PATH` | `/var/lib/fpm-tune/state.json` | the learned baselines |
| `--memory SIZE` | detected | override the memory limit: `8G`, `8GB`, `8Gi`, `8GiB`; units K, M, G, T, all binary |
| `--reserve AMOUNT` | 15% | memory held back from workers: a size (`1G`) or a percentage of the budget (`20%`) |
| `--workload CLASS` | `web` | default class for pools without a marker: `web`, `bursty`, `subprocess-heavy`; see the markers below |
| `--sizing BASIS` | `p95` | per-worker cost basis: `p95`, `p99`, a bare percentile from 50 to 100, or `peak` |
| `--cpu` | off | hold a CPU-limited pool at its CPU ceiling; without it the CPU shape is measured and reported but sizes nothing |
| `--cpu-headroom N` | `2` | with `--cpu`, the factor on the workers that fill the CPU; 1 to 100, never below one per core plus one |
| `--timeout DURATION` | `15s` | budget for scraping all pools |
| `--verbose` | off | log what is being read |
| `--no-learn` | off | do not record this scrape |
| `--confidence-samples N` | `20` | busy samples before a baseline is trusted enough to cut a pool |
| `--confidence-span DURATION` | `30m` | time span of busy evidence before a baseline is trusted enough to cut a pool |

`--sizing p95` sizes on the 95th percentile with a 10% margin, floored by the most recent peak; `peak` sizes on the typical peak itself, which rises in one scrape and decays on a half-life, with the same floor. See [Measuring workers](../how-it-decides/measuring-workers.md).

## plan

| flag | default | meaning |
|---|---|---|
| `--drop-in-dir DIR` | all masters | plan for the master that includes this pool directory |

Without `--drop-in-dir`, a host running several masters is planned as one, against one of their memory limits. When another fpm-tune holds the state lock, `plan` reports without recording; when the state directory is not writable, likewise, and says so.

## apply

| flag | default | meaning |
|---|---|---|
| `--drop-in-dir DIR` | the directory the master includes | where the pool settings are written; also selects the master on a host running several |
| `--backup-dir DIR` | `/var/lib/fpm-tune/backup` | the previous configuration while a change is in flight, the record of the change, and where php-fpm lives |
| `--min-change FRACTION` | `0.15` | smallest relative growth worth a reload; a shrink needs twice that |
| `--min-interval DURATION` | `5m` | shortest time between reloads for growth; a shrink waits four times that |
| `--dry-run` | off | render and validate, write nothing, reload nothing |

`--no-learn` is refused unless `--dry-run` is also given: applying has to record what it wrote, or the next run reloads the pool this one just reloaded. `apply` refuses to write from a budget it could not confirm; pass `--memory`. On a host running several masters, `--drop-in-dir` is required. See [How it fails safe](../safety/how-it-fails-safe.md).

## enable-status

| flag | default | meaning |
|---|---|---|
| `--drop-in-dir DIR` | the directory the master includes | which master to act on |
| `--status-path PATH` | `/status` | the `pm.status_path` to set on pools that have none |
| `--backup-dir DIR` | `/var/lib/fpm-tune/backup` | where the previous drop-in is kept while the change is in flight |
| `--dry-run` | off | render and validate, write nothing, reload nothing |

It writes `zz-fpm-tune-status.conf`, validates, reloads, and rolls back if the master does not come back. With every pool already exposing a page it prints `Every pool on this master already exposes a status page` and does nothing.

## serve

| flag | default | meaning |
|---|---|---|
| `--apply` | off | act on the plan; without it the loop observes, learns and publishes |
| `--config PATH` | none | load settings from this file; explicit flags override it |
| `--interval DURATION` | `30s` | how often to sample the pools |
| `--metrics ADDR` | `:9110` | address for `/metrics`, `/healthz` and `/history.json`; empty disables them |
| `--control PATH` | `control.sock` beside the state file | the unix socket `apply-now` asks on |
| `--recommend PATH` | none | write the plan as configuration to this path; nothing reads it |
| `--heartbeat DURATION` | `30m` | re-log the current recommendation this often even when nothing changed; `0` disables |
| `--history DURATION` | `24h` | how far back `/history.json` reaches, in memory |
| `--save-every DURATION` | `5m` | how often the baselines reach disk |
| `--drop-in-dir DIR` | the directory the master includes | as for `apply` |
| `--backup-dir DIR` | `/var/lib/fpm-tune/backup` | as for `apply` |
| `--min-change FRACTION` | `0.15` | as for `apply` |
| `--min-interval DURATION` | `5m` | as for `apply` |

`--apply --no-learn` is refused, for the same reason as on `apply`. `--recommend` inside a directory a master includes is refused. A second daemon whose `--metrics` address is taken does not start.

## install-service

| flag | default | meaning |
|---|---|---|
| `--apply` | off | install in apply mode |
| `--cpu` | off | set `cpu = true` in the config; `--cpu=false` turns it off on a re-run |
| `--metrics ADDR` | `127.0.0.1:9110` | address for `/metrics` |
| `--print` | off | print the config and unit instead of installing them |
| `--log-file PATH` | the journal | append the log to this file through the unit, with a logrotate snippet; `journal` goes back; a re-run keeps what the unit has |

A first run writes the whole config; a re-run keeps it and changes only the keys named. Both rewrite the unit and restart the service. See [Running as a daemon](../operating/running-as-a-daemon.md).

## mode

```bash
fpm-tune mode                  # print the current mode
sudo fpm-tune mode apply       # rewrite the mode line and restart the service
sudo fpm-tune mode advisory
```

No flags. Refused, with the fix, when `/etc/fpm-tune/config` does not exist.

## top

| flag | default | meaning |
|---|---|---|
| `--addr HOST:PORT` | the installed service's metrics address, else `127.0.0.1:9110` | where `/history.json` is served |
| `--refresh DURATION` | `5s` | how often to fetch |

The keys are on [Applying once](../operating/applying-once.md).

## apply-now

| flag | default | meaning |
|---|---|---|
| `--control PATH` | `/var/lib/fpm-tune/control.sock` | the daemon's control socket |

## The config file

`/etc/fpm-tune/config` is what the installed service reads (`serve --config`). One `key = value` per line; blank lines and lines starting with `#` or `;` are ignored. Every `serve` flag name is a key (`interval = 15s`, `reserve = 20%`, `cpu = true`), and an unknown key is refused, so a typo stops the service rather than being ignored. A flag given on the command line overrides the file.

`mode` is the one key that is not a flag: `advisory` (the default) or `apply`. In advisory mode, when no `recommend` key is set, the recommendation is written to `/var/lib/fpm-tune/recommended.conf`. The file `install-service` writes carries `mode` and `metrics` as active lines and the other common keys as commented defaults:

```
mode = advisory

# Where /metrics is served. Empty disables it.
metrics = 127.0.0.1:9110

# heartbeat = 30m
# sizing = p95
# cpu = true
# cpu-headroom = 2
# drop-in-dir =
# history = 24h
# control = /var/lib/fpm-tune/control.sock
# recommend = /var/lib/fpm-tune/recommended.conf
```

`fpm-tune mode` and a re-run of `install-service` rewrite single lines and leave the rest, including your comments, as they were. After editing by hand: `sudo systemctl restart fpm-tune`.

## Files and paths

| path | what it is |
|---|---|
| `/usr/local/bin/fpm-tune` | the binary; install.sh falls back to `~/.local/bin`, then `~/bin` |
| `/etc/fpm-tune/config` | the service settings |
| `/etc/systemd/system/fpm-tune.service` | the unit `install-service` writes |
| `/etc/logrotate.d/fpm-tune` | the logrotate snippet, when installed with `--log-file` |
| `/var/lib/fpm-tune/state.json` | the learned baselines, and where php-fpm was last seen |
| `/var/lib/fpm-tune/fpm-tune.lock` | the state lock; one process learns at a time |
| `/var/lib/fpm-tune/control.sock` | the control socket, mode `0600`, root's |
| `/var/lib/fpm-tune/recommended.conf` | the recommendation file, advisory mode |
| `/var/lib/fpm-tune/backup/` | `<id>-transaction.json` (a change in flight), the previous drop-in it saved, and `<id>-master.json` (where php-fpm lives); not scratch space |
| `/run/fpm-tune/<id>-apply.lock` | the pool-directory lock; one writer per directory (`/tmp/fpm-tune-locks` on a host without `/run`) |
| `<pool directory>/zz-fpm-tune.conf` | the sizes it wrote |
| `<pool directory>/zz-fpm-tune-status.conf` | the status pages it turned on |
| `/metrics`, `/healthz`, `/history.json` | on the metrics address |

`<id>` is the first eight hex digits of a SHA-256 of the pool directory, so two masters sharing a backup directory keep separate records. The pool directory is the one the master includes, `/etc/php/8.4/fpm/pool.d` on Debian and Ubuntu.

## Pool markers

A pool declares its own class and headroom in its config, as `env[...]` lines that php-fpm accepts and fpm-tune reads:

```ini
[media]
env[FPM_TUNE_WORKLOAD] = subprocess-heavy
env[FPM_TUNE_CPU_HEADROOM] = 3
```

`FPM_TUNE_WORKLOAD` names the class, overriding `--workload` for this pool: `web` (aliases `api`, `simple`; spawns nothing, the default), `bursty` (a child now and then), `subprocess-heavy` (aliases `subprocess`, `media`, `children`; a child on most requests). Case does not matter. An unknown name is a warning in the plan, and nothing is reserved for that pool's children. Measurement refines the class once a baseline exists. See [Spawned children](../how-it-decides/spawned-children.md).

`FPM_TUNE_CPU_HEADROOM` is the pool's own headroom factor, 1 to 100, overriding `--cpu-headroom`. A value that is not a number in that range is a warning in the plan and the host's headroom is used, so one pool's typo does not stop the host being planned. See [CPU](../how-it-decides/cpu.md).

## Exit status

`0` on success, including `-h`, `help` and `version`. `1` on any error, printed to stderr as `fpm-tune: ...`: an unknown command, no command, a positional argument, a flag that does not parse, a refusal, or a failed run. `apply` exits `1` when the reload was signalled but the run ended before the master was seen to survive (the change stands; run `apply` again to confirm it), and when the change was applied but its record could not be saved. `apply-now` exits `1` when the daemon reports an error.
