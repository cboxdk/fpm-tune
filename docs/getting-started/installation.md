---
title: Installation
weight: 1
description: Get the binary, and know where it keeps its state.
---

# Installation

## From source

```bash
git clone https://github.com/cboxdk/fpm-tune
cd fpm-tune
make build          # → build/fpm-tune
```

Or straight from the module path:

```bash
go install github.com/cboxdk/fpm-tune/cmd/fpm-tune@latest
```

The result is a single static binary with no runtime dependency beyond php-fpm
itself. It cross-compiles for `linux/amd64` and `linux/arm64` (and builds on
darwin, where the sizing logic runs but the host-reading does not).

## Where it lives on a host

- **The binary** goes wherever you keep operational tools — `/usr/local/bin` is
  conventional.
- **`/var/lib/fpm-tune`** holds the learned baselines (`state.json`) and, while a
  change is in flight, the previous configuration, the recovery record, and a
  note of where php-fpm lives. This directory is not scratch space: cleaning it
  removes the ability to undo a change and to repair a host whose master will not
  start. See [Recovering a host](../operating/recovering.md).
- **`zz-fpm-tune.conf`** is the one file it writes, into the directory your
  master already includes (`/etc/php-fpm.d` on RHEL, `/etc/php/*/fpm/pool.d` on
  Debian). It contains only `pm.*` keys; your pool configuration is not touched.

## As a systemd service

```ini
[Unit]
Description=fpm-tune
# Wants, not Requires: a supervisor that dies with the thing it supervises
# cannot repair it.
Wants=php-fpm.service
After=php-fpm.service

[Service]
ExecStart=/usr/local/bin/fpm-tune serve --apply --metrics 127.0.0.1:9110
Restart=on-failure
RestartSec=5

# systemd owns /var/lib/fpm-tune with sensible permissions, rather than the
# tool creating it under whatever umask it inherited.
StateDirectory=fpm-tune
StateDirectoryMode=0700

[Install]
WantedBy=multi-user.target
```

Bind it with `Wants=`, not `Requires=` — a supervisor that dies with the thing
it supervises cannot repair it.

## Checking it runs

```bash
fpm-tune version
fpm-tune plan       # reads the host, writes nothing
```

If `plan` reports no pools found, either no php-fpm master is running or the tool
cannot see it — discovery reads the process table, and inspecting another user's
processes needs root. The error says which.
