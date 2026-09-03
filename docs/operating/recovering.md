---
title: Recovering a host
weight: 5
description: What happens, and what to do, when php-fpm will not start, including when this tool's own file is the cause.
---

# Recovering a host

This page is for the person whose php-fpm master will not start. It says what fpm-tune does in each case, and what is left for you to do. The unit is `php8.4-fpm` on Debian and Ubuntu (Forge, Ploi) and `php-fpm` elsewhere.

## If this tool's own file is the problem

The commonest cause is a site removed while `zz-fpm-tune.conf` still overrides its pool. A pool defined only in the drop-in has no `listen` and no `user`, so php-fpm refuses the whole configuration and the master does not come back.

A daemon in apply mode detects this and takes its own file out. It cannot bring the service back on its own, because systemd exhausts its restart burst in seconds, long before a polling supervisor can land a fix. Once the log line explaining what happened has appeared:

```bash
sudo systemctl reset-failed php8.4-fpm && sudo systemctl start php8.4-fpm
```

The repair works when nothing is running to discover, because every successful apply records where php-fpm lives (its binary, config and pool directory) beside the backups. That note is how the master is found when there is no process to scan for. It is why `/var/lib/fpm-tune/backup` is not scratch space: a rule that cleans it takes away the ability to undo a change and the ability to fix the host.

An advisory daemon does not repair. The self-repair is part of applying; see [Running as a daemon](running-as-a-daemon.md).

## If a run was interrupted mid-change

What is about to be written is recorded first, with a phase (written, or signalled) and a SHA-256 of the intended content. On the next start the record is read and the change finished or undone:

- **Written but never signalled**: the file is in place for whenever php-fpm starts; nothing was reloaded.
- **Signalled and the master came back**: the change is confirmed and the record cleared.
- **Signalled and the master is gone**: the previous configuration is restored, and you are told it needs starting.
- **Signalled, and the file has been edited since** by something other than this tool: php-fpm is asked whether it accepts what is on disk now. If it does, the current file stands; if not, you are told the host is broken.

A one-shot `apply` interrupted after the signal but before the master was seen to survive exits 1 and says so. The change is in place and recorded; run `apply` again to confirm it. A daemon in the same position reconciles before its next write.

## If php-fpm is broken by something else

Removing this tool's file achieves nothing when the configuration is broken for another reason, and would leave the host both broken and untuned. So the removal is rehearsed against a sandbox first: only if taking the file out makes the configuration valid is it removed. When the breakage is elsewhere, the file is left alone and the log says so, distinguishing "removing this does not fix it" from "the binary could not be run".

Resetting the baseline, which is a different job, is on the [lifecycle](lifecycle.md) page.
