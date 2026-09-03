---
title: Two PHP versions
weight: 2
description: Two php-fpm masters on one host, one daemon each, with the memory budget split between them.
---

# Two PHP versions

This recipe is for a host running two php-fpm masters, here `php8.3-fpm` and `php8.4-fpm` on Debian or Ubuntu, with pool directories `/etc/php/8.3/fpm/pool.d` and `/etc/php/8.4/fpm/pool.d`. It ends with two daemons, one per master, each with its own budget.

## Why one daemon must not see both

Unscoped, `fpm-tune plan` plans both masters' pools as one host and `sudo fpm-tune apply` refuses, naming every master and pid and asking for `--drop-in-dir`. Pools that share a name (`www` in both) cannot be told apart in the metrics either.

Scoping fixes the writing, and creates a budget problem. Each daemon reads its own master's memory limit, and with no cgroup limit that is the whole host from `/proc/meminfo`. Two daemons that each budget 85% of the same memory can commit more than the host has. So the budget is split first, by giving each master its own cgroup limit, and each daemon then sizes inside its master's limit.

## 1. Split the budget

Decide how much of the host each PHP version may use, and set it on the units. This takes effect live and survives reboots:

```bash
sudo systemctl set-property php8.3-fpm.service MemoryMax=2G
sudo systemctl set-property php8.4-fpm.service MemoryMax=4G
```

A scoped daemon reads the limit from its master's cgroup, so each budget starts from its own number. The alternative is `--memory 2G` on one daemon and `--memory 4G` on the other; that sizes against the number you gave without subtracting other services, and the kernel enforces nothing. [The budget](../how-it-decides/the-budget.md) explains the difference.

## 2. Turn on the status pages, per master

```bash
sudo fpm-tune enable-status --drop-in-dir /etc/php/8.3/fpm/pool.d
sudo fpm-tune enable-status --drop-in-dir /etc/php/8.4/fpm/pool.d
```

Each writes `zz-fpm-tune-status.conf` into its own pool directory and reloads its own master.

## 3. Plan each master

```bash
fpm-tune plan --drop-in-dir /etc/php/8.4/fpm/pool.d
```

The first line of the budget block reads `php-fpm's memory 4.0GiB` rather than `host memory`, which confirms the daemon is sizing inside the cgroup limit. Repeat for 8.3.

## 4. Install the first daemon

`install-service` installs exactly one unit, `fpm-tune.service`, reading `/etc/fpm-tune/config`. Install it, then scope it to 8.4 by setting `drop-in-dir` in the config (the key is present, commented out) and restarting:

```bash
sudo fpm-tune install-service
sudo sed -i 's|^# drop-in-dir =.*|drop-in-dir = /etc/php/8.4/fpm/pool.d|' /etc/fpm-tune/config
sudo systemctl restart fpm-tune
```

This daemon keeps the defaults: state at `/var/lib/fpm-tune/state.json`, control socket beside it, backups under `/var/lib/fpm-tune/backup`, metrics on `127.0.0.1:9110`.

## 5. Write the second daemon's config and unit

The second daemon needs its own state file (a daemon holds the state lock for its lifetime, so two cannot share one), its own control socket, backup directory, metrics port and recommendation file. Any `serve` flag is a config key:

```bash
sudo tee /etc/fpm-tune/config-8.3 >/dev/null <<'CONF'
mode = advisory
metrics = 127.0.0.1:9111
drop-in-dir = /etc/php/8.3/fpm/pool.d
state = /var/lib/fpm-tune-8.3/state.json
control = /var/lib/fpm-tune-8.3/control.sock
backup-dir = /var/lib/fpm-tune-8.3/backup
recommend = /var/lib/fpm-tune-8.3/recommended.conf
CONF
```

The unit is the one `install-service --print` shows, with the paths and the php-fpm unit changed:

```bash
sudo tee /etc/systemd/system/fpm-tune-8.3.service >/dev/null <<'UNIT'
[Unit]
Description=fpm-tune for php8.3-fpm
Wants=php8.3-fpm.service
After=php8.3-fpm.service

[Service]
ExecStart=/usr/local/bin/fpm-tune serve --config /etc/fpm-tune/config-8.3
Restart=on-failure
RestartSec=5
StateDirectory=fpm-tune-8.3
StateDirectoryMode=0700

[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl daemon-reload
sudo systemctl enable --now fpm-tune-8.3
journalctl -u fpm-tune-8.3 -f
```

The lock that serialises writers lives under `/run/fpm-tune` and is keyed on the pool directory, so the two daemons never block each other. A stray `sudo fpm-tune apply` beside them is refused by the state lock the first daemon holds; ask a daemon with `apply-now` instead.

## 6. Operate them

`mode`, `top` and `apply-now` default to the installed service. For the second daemon, name its endpoints:

```bash
fpm-tune top --addr 127.0.0.1:9111
sudo fpm-tune apply-now --control /var/lib/fpm-tune-8.3/control.sock
```

`fpm-tune mode` only rewrites `/etc/fpm-tune/config`, so switch the second daemon by editing `mode` in `/etc/fpm-tune/config-8.3` and running `sudo systemctl restart fpm-tune-8.3`. Point Prometheus at both ports; the series are the same, one set per daemon.
