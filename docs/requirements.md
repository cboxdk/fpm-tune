---
title: Requirements
weight: 2
description: What fpm-tune needs to run, and where.
---

# Requirements

- **A host running PHP-FPM.** fpm-tune discovers masters by scanning the process
  table, parses their effective configuration with `php-fpm -tt`, scrapes pool
  status over FastCGI, validates with `php-fpm -t`, and reloads with SIGUSR2. It
  runs where php-fpm runs, which in practice means Linux. On macOS it builds and
  the allocator's logic runs, but the cgroup budget detection and the process
  scan have no equivalent.

- **Permission to see and signal the master.** Discovery reads other processes'
  details, and applying signals the master — so it wants to run as root or as the
  php-fpm user. Running as a user that cannot read the master's cgroup is
  detected and refused rather than sized against the whole machine; see
  [The budget](how-it-decides/the-budget.md).

- **The status page enabled**, on the pools you want measured. Worker memory is
  read from the pids the status page reports, so a pool with no
  `pm.status_path` is sized from a profile guess rather than from measurement.

- **A writable state directory**, `/var/lib/fpm-tune` by default, for the learned
  baselines and — during a change — the backup and recovery record. It is created
  on first run; under systemd, `StateDirectory=fpm-tune` gives it sensible
  ownership.

- **Go 1.26 or newer** to build from source. Released binaries have no runtime
  dependency beyond php-fpm itself.

The tool holds no long-running connection to php-fpm and needs no agent inside
it. It reads, decides, optionally writes one file and sends one signal, and
otherwise stays out of the way.
