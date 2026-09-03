---
title: Quickstart
weight: 1
description: From nothing to a tuned host in one read, without risking anything on the way.
---

# Quickstart

The safe path is the same one this tool wants you to take: look first, advise
for a while, then let it act.

## 1. Get it

```bash
curl -fsSL https://raw.githubusercontent.com/cboxdk/fpm-tune/main/install.sh | sh
```

One static binary, no runtime dependency beyond php-fpm. The installer verifies
the release checksum and refuses to install on a mismatch; it never uses `sudo`.
On macOS, `brew install cboxdk/tap/fpm-tune` does the same. For verifying the
release signature, other platforms, or building from source, see
[Installation](getting-started/installation.md).

## 2. See what it thinks

```bash
fpm-tune plan
```

`plan` writes no configuration and reloads nothing. On a host with a running
php-fpm master it discovers the pools, reads the budget from the master's cgroup,
and prints what it would set and why:

```
container memory 4.0GiB, 12 CPU(s) (via cgroup v2)
  headroom kept:           614.4MiB (15% of 4.0GiB)
  available to workers:    3.4GiB

POOL   MODE     NOW  PLAN  MEMORY    WHY
shop   dynamic  12   14    1.3GiB    peak 11 workers busy; raised to 14, measured 96.0MiB/worker
blog   dynamic  12   8     384.0MiB  peak 6 workers busy; 8 is enough, measured 48.0MiB/worker
```

It keeps ~85% of the budget for workers and holds the rest back as headroom (that's
the 15%, tunable with `--reserve`). On a shared box it also subtracts what MySQL and
friends are actually using; a `used by other services` line shows up then.

Further down, a `CPU per request` table says which of memory and CPU each pool
runs out of first, once it has read enough requests (on a first run it says `too
few readings yet`). The plan reports that on its own. Pass `--cpu` to let it
hold a cpu-limited pool at the workers that fill the CPU. See
[CPU per request](how-it-decides/cpu.md).

On a first run every pool is `estimated`, not measured: the numbers are a
profile's guess until the tool has watched real traffic. That is expected, and
the plan says so.

If instead `plan` says a pool **has no `pm.status_path`**, that is a stock
php-fpm: it sizes each pool from its live status page, and that page ships off.
Turn it on and the pool becomes visible, then run `plan` again:

```bash
fpm-tune enable-status      # validated drop-in + reload; rolled back if it fails
```

(`apply` and `serve --apply` do this for you; you only need it by hand to `plan`
first. See [First run](getting-started/first-run.md).)

## 3. Let it advise, permanently

```bash
fpm-tune serve --recommend /var/lib/fpm-tune/recommended.conf
```

`serve` without `--apply` changes nothing and never will. It measures, publishes
metrics on `:9110`, and (with `--recommend`) writes its conclusion as
PHP-FPM configuration you can read, diff, and paste by hand. The file is
rewritten only when the recommended settings change, so its modification time
tells you when the advice last moved. See [Advisory mode](operating/advisory-mode.md).

Leave it running for a day or two through a real traffic pattern. The estimates
become measurements, and the recommendation settles onto numbers backed by what
the workers actually did.

## 4. Let it act

When you trust the numbers, ask the daemon from step 3 to apply them once:

```bash
sudo fpm-tune apply-now     # or press a in fpm-tune top
```

It stays advisory afterwards. (Without a daemon running, `fpm-tune apply` does
the same from the command line; beside one it is refused, because two writers
of one state file discard each other's learning.) Either way this writes one
file (`zz-fpm-tune.conf`, in the directory your master already includes), validates it against a sandboxed copy of the configuration, and
reloads the master with SIGUSR2. If php-fpm would reject the file, it never
reaches the live directory. If the master does not survive the reload, the
change is rolled back. Deleting the file returns everything to what you
configured.

To let it act on its own from now on:

```bash
fpm-tune serve --apply        # or: sudo fpm-tune mode apply, for the installed service
```

Now it closes the loop: measure, decide, apply when a change is worth a reload,
and repair the host if its own file ever stops php-fpm from starting.

## What to read next

- [First run](getting-started/first-run.md): the same path, with the safety
  guarantees spelled out.
- [How it decides](how-it-decides/_index.md): the part to read before you trust
  `--apply`.
