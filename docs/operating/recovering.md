---
title: Recovering a host
weight: 4
description: What happens, and what to do, when php-fpm will not start, including when this tool's own file is the cause.
---

# Recovering a host

The situation this tool works hardest to survive is a php-fpm master that will
not start. Here is what happens in each case, and what you do.

## If this tool's own file is the problem

The commonest cause is a site removed while the tool still overrides its pool. A
pool defined only in the drop-in has no `listen` and no `user`, so php-fpm
refuses the whole configuration and the master will not come back.

A daemon running `serve --apply` detects this and takes its own file out. But it
cannot bring the service back on its own, because systemd exhausts its restart
burst in seconds, long before any polling supervisor can land a fix. Once you
have read the log line explaining what happened:

```bash
systemctl reset-failed php-fpm && systemctl start php-fpm
```

The repair works even when nothing is running to discover, because on every
successful apply the tool records where php-fpm lives (its binary, config, and
pool directory) beside the backups. That note is how it finds the master to
repair when there is no live process to scan for. It is why
`/var/lib/fpm-tune/backup` is [not scratch space](../getting-started/installation.md#where-it-lives-on-a-host):
a rule that cleans it takes away the ability to undo a change *and* the ability
to fix that host.

## If a run was interrupted mid-change

What is about to be written is recorded first, with a phase (written, or
signalled) and a SHA-256 of the intended content. On the next start the tool
reads that record and finishes or undoes the change:

- **Written but never signalled**: the file is in place for whenever php-fpm
  starts; nothing was reloaded.
- **Signalled and the master came back**: the change is confirmed and the record
  cleared.
- **Signalled and the master is gone**: the previous configuration is restored,
  and you are told it needs starting.
- **Signalled, and the file has been edited since** by something other than this
  tool: php-fpm is asked whether it accepts what is on disk now. If it does, the
  current file stands; if it does not, you are told the host is broken.

A one-shot `apply` interrupted after the signal but before the master is seen to
survive exits non-zero and says so. The change is in place and recorded; run
`apply` again to confirm it.

## If php-fpm is broken by something that is not this tool

Removing this tool's file achieves nothing when the configuration is broken for
some other reason, and would leave the host both broken *and* untuned. So the
tool rehearses the removal against a sandbox first: only if taking its file out
would actually make the configuration valid does it remove it. When the breakage
is elsewhere, it leaves its file alone and says so, distinguishing "removing this
does not fix it" from "the binary could not even be run".

## Resetting the baseline

`rm /var/lib/fpm-tune/state.json` does nothing while `serve` is running: the
daemon holds its baselines in memory and writes them back on the next save,
including on the way out. Stop it first.

```bash
systemctl stop fpm-tune && rm /var/lib/fpm-tune/state.json && systemctl start fpm-tune
```
