---
title: How it fails safe
weight: 1
description: The chain from plan to reloaded master, in order, and the path back from every step that can fail.
---

# How it fails safe

This page is the safety chain: what happens, in order, when fpm-tune writes a pool ceiling, and what it does when a step fails. Read it before running `apply` or `mode apply` on a host you care about.

## What it writes

Two files, both in the directory your master already includes (`/etc/php/8.4/fpm/pool.d` on Debian and Ubuntu):

- `zz-fpm-tune.conf` holds the sizes. For each pool it manages there is a section with `pm.max_children` and, for a dynamic pool, `pm.start_servers`, `pm.min_spare_servers` and `pm.max_spare_servers`. Nothing else.
- `zz-fpm-tune-status.conf` turns on `pm.status_path` for pools that had none. `enable-status` writes it, and so does `apply`, for a pool it could not otherwise measure.

PHP-FPM merges a section defined across several files, so a section header with only these keys overrides them and leaves `listen`, `user` and the rest as you wrote them. The `zz-` prefix sorts last in the include glob, so these files win over anything already there. Both open with a marker line naming the tool; a file under either name without it is refused, never overwritten.

Delete `zz-fpm-tune.conf` and reload, and every pool is back at its own configured values. Delete `zz-fpm-tune-status.conf` and reload, and the status pages are off again. A daemon in apply mode writes the sizes back on its next round, so switch it to advisory or stop it first. [Lifecycle](../operating/lifecycle.md) has the commands.

## The chain

**1. Refuse before writing.** Nothing is written when the master does not include the path the file would take (the change would validate, reload and do nothing), when a file under that name was not written by this tool, when no running master was found, or when `apply` could not read the memory limit. [The trust boundary](the-trust-boundary.md) lists what else is refused.

**2. Validate a copy.** The pool directory is copied to a temporary directory, the new file is placed in the copy, and `php-fpm -t` runs against a master configuration rewritten to include the copy. A configuration php-fpm rejects never reaches the directory it globs, not even for the length of a fork. `apply --dry-run` stops here.

**3. Write once, atomically.** The previous file is saved under `/var/lib/fpm-tune/backup` with a transaction record beside it. The new file is then written under a temporary name and renamed into place, and the directory is fsynced. A reader sees the old file or the new one, never half of either, and because the file holds every pool the tool overrides, a growth and the reduction that pays for it land together or not at all.

**4. Validate in place.** `php-fpm -t` runs again against the real tree. If it fails, the previous file goes back, and the master has not been signalled.

**5. Record, then reload.** The transaction record is rewritten to say the master is about to be signalled, and made durable before the signal goes out; if that write fails, the previous file goes back and there is no reload. The master then gets SIGUSR2, php-fpm's graceful reload: it re-reads the configuration and cycles workers without stopping the service. A daemonized master comes back under a new pid, which fpm-tune follows through the pid file. It then watches the master for 2 s.

**6. Roll back if the master does not come back.** The previous file is restored and the master is signalled once more so it reloads onto it. If the restore itself fails (a full or read-only directory), the error says which state the host is in, because the next move depends on it: a rejected file that was never signalled is armed for the next reload from any source, and a master that is down and cannot be put back needs `sudo systemctl start php8.4-fpm` now.

**7. Close the record.** Once the master has survived the settle window, the transaction record is removed, then the saved copy. A crash between the two leaves a backup nothing reads, which the next start sweeps.

## After a crash

A transaction record found at startup means a process died between steps 3 and 7: the OOM killer, a reboot, Ctrl-C during the reload. The next `apply`, and the first round of a daemon in apply mode, resolves it before writing anything. The record carries the phase (written or signalled), a hash of what was written, and where php-fpm lives, so recovery works with no master running and no state file.

If the file on disk does not match the hash and the master was never signalled, the rename never happened and there is nothing to undo. If it matches and php-fpm accepts it, the reload is finished; a master that already adopted it re-reads the same file. If php-fpm rejects it, the rollback is rehearsed in a sandbox first, because a configuration can be broken by something other than this file, and undoing a change that would not fix it only loses the change. A check that is interrupted leaves the record for the next start.

With no record, the same start checks whether php-fpm accepts its configuration at all. If it does not, and removing `zz-fpm-tune.conf` makes it valid (rehearsed in a sandbox, then confirmed in place), the file is removed and the next round writes it again for the pools that remain. The usual cause is a site removed while its section was still in the file: a pool defined only there has no `listen` and no `user`, so php-fpm refuses to start. The repair is part of applying, so a daemon in advisory mode does not perform it. `fpm_tune_repairs_total` counts repairs, and [Recovering a host](../operating/recovering.md) covers the cases it cannot fix.

## One writer per pool directory

An `flock` under `/run/fpm-tune`, keyed on the pool directory at a fixed path, stops two processes writing the same file. A second run pointed at its own state file and backup directory is still refused, and a crashed process cannot leave a stale lock, because the kernel drops it on exit. The state file has its own lock beside it, held by a running daemon for its lifetime: a `plan` beside the daemon reports without recording, `apply` beside it is refused, and `apply-now` asks the daemon to act instead ([Applying once](../operating/applying-once.md)).

## The reload and a 502

On reload php-fpm recreates a pool's listen socket, so under concurrent load a request arriving in the sub-second window before the new socket is accepting can fail to connect and get a 502. That is php-fpm's behaviour, and it is rare because a resize only reloads when the change clears the [hysteresis](../how-it-decides/hysteresis.md) threshold. If even a resize-time blip is unacceptable, a TCP pool (`listen = 127.0.0.1:9000`) with `SO_REUSEPORT` hands the socket over without the recreate window, where a unix-socket pool cannot. Sizing is identical either way.
