---
title: Lifecycle
weight: 6
description: Undoing a change, removing fpm-tune entirely, upgrading it, what it costs to run, and resetting the baseline.
---

# Lifecycle

This page is for the person who wants fpm-tune's change gone, fpm-tune itself gone, or a newer version in place. The unit is `php8.4-fpm` on Debian and Ubuntu (Forge, Ploi) and `php-fpm` elsewhere; the pool directory is the one the master includes, `/etc/php/8.4/fpm/pool.d` on those distributions.

## Undoing a change

fpm-tune writes two files into the pool directory: `zz-fpm-tune.conf` holds the sizes, and `zz-fpm-tune-status.conf` turns the status pages on. Each overrides only the settings it names; every pool's own configuration is untouched. Undoing is deleting the file and reloading:

```bash
sudo rm /etc/php/8.4/fpm/pool.d/zz-fpm-tune.conf
sudo systemctl reload php8.4-fpm
```

A daemon in apply mode writes the file back on its next round, so switch it first (`sudo fpm-tune mode advisory`) or stop it (`sudo systemctl stop fpm-tune`). Deleting `zz-fpm-tune-status.conf` turns the status pages off again, after which nothing can be sized until they are back on.

## Removing it entirely

```bash
sudo systemctl disable --now fpm-tune
sudo rm /etc/systemd/system/fpm-tune.service /etc/logrotate.d/fpm-tune
sudo systemctl daemon-reload
sudo rm -r /etc/fpm-tune /var/lib/fpm-tune /run/fpm-tune
sudo rm /etc/php/8.4/fpm/pool.d/zz-fpm-tune.conf /etc/php/8.4/fpm/pool.d/zz-fpm-tune-status.conf
sudo systemctl reload php8.4-fpm
sudo rm /usr/local/bin/fpm-tune
```

`/var/lib/fpm-tune` holds the state, the recommendation, the control socket and the backups; `/run/fpm-tune` holds the pool-directory locks. The logrotate snippet exists only when the service was installed with `--log-file`, and the log file it names stays until you remove it. Deleting both drop-ins and reloading once returns every pool to its own configuration. If the binary went somewhere else (`~/.local/bin`, `~/bin`), remove it from there.

## Upgrading

Install the new binary over the old one, then restart the service so it runs the new one:

```bash
curl -fsSL https://raw.githubusercontent.com/cboxdk/fpm-tune/main/install.sh | sudo sh
sudo systemctl restart fpm-tune
```

`sudo fpm-tune install-service` does the restart as well, and rewrites the unit, which matters when a release changes it. It keeps `/etc/fpm-tune/config` and changes only the keys you pass. The state file is kept across upgrades; a version that has to adjust what an older one wrote says so at startup (`State file adjusted on load`). See [Installation](../getting-started/installation.md) for the install methods.

## What it costs to run

Each round, every 30 seconds: one `php-fpm -tt` parse per master, to read the effective configuration; one status-page request per pool over its own socket; and a read of `/proc` for each worker, `smaps_rollup` where the kernel has it. On a host with forty pools that is a few hundred milliseconds of work per round, and the daemon holds a few MB of RSS, including the day of history. The baselines reach disk every 5 minutes, and at once after a resize.

Reloads are the only cost that touches your traffic, and only apply mode causes them: a change has to clear the [hysteresis](../how-it-decides/hysteresis.md) first, and a reload is graceful, so workers finish their request before they are replaced.

## Resetting the baseline

The baselines live in `/var/lib/fpm-tune/state.json`. Deleting it while the daemon runs does nothing: the daemon holds them in memory and writes them back at the next save, and on the way out. Stop it first:

```bash
sudo systemctl stop fpm-tune
sudo rm /var/lib/fpm-tune/state.json
sudo systemctl start fpm-tune
```

Every pool then starts from an estimate again, and will not be cut below its configured ceiling until it has 20 busy samples over 30 minutes. Leave the backup directory alone; it is what [recovery](recovering.md) reads.
