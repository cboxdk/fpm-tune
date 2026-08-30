---
title: How it fails safe
weight: 1
description: The guarantees around every write — sandbox validation, atomic replacement, graceful reload, rollback, and crash recovery.
---

# How it fails safe

A change reaches a host through a sequence designed so that every failure has a
path back. None of these is decoration; each exists because the obvious approach
takes a host down, and each is tested against a real master.

## Only `pm.*` keys, in one file

The tool writes one file, `zz-fpm-tune.conf`, into the directory your master
already includes. It contains only `pm.*` settings; your pool configuration —
`listen`, `user`, everything else — is not touched. PHP-FPM merges a section
defined across several included files, so repeating each section header with just
these keys overrides them and leaves the rest as you wrote it.

Deleting that file returns everything to what you configured. The next run writes
a fresh one from what it can see; it does not put the old overrides back.

## Validated against a sandbox first

Before anything reaches the live directory, the change is rendered and checked
with `php-fpm -t` against a *copy* of the pool directory. A configuration php-fpm
would reject never reaches the directory it globs — not even for the length of a
fork. `--dry-run` stops there: it renders and validates and writes nothing.

## One atomic write

The change set is indivisible. A growth and the reduction that funds it reach the
host together, in a single rename, or not at all. A file php-fpm might read
mid-write is never on disk: a reader sees the old file or the new one, never half
of either.

## A reload, never a restart

The master is reloaded with SIGUSR2, which cycles its workers gracefully and
carries its listening sockets across, so no request is dropped. A daemonized
master comes back under a new pid — php-fpm's own default — which is followed
rather than mistaken for a death.

## Rolled back if the master does not survive

Validation forks a separate process with no sockets to bind; a live master can
still fail to initialise on a configuration that validated. If the master does
not come back from the reload, the previous file is restored and the master
reloaded onto it. If even the restore cannot be written — a full or read-only
directory — you are told exactly which state the host is in, because the operator's
next move depends on it: a rejected file that was never signalled is not yet
dangerous, but a master that is *down* and cannot be put back needs a `systemctl
start` now.

## Recoverable across a crash

What is about to be written is recorded first — path, phase, and a hash of the
intended content — and the record is made durable before the master is signalled.
An interrupted run is finished or undone on the next start. The rollback itself is
rehearsed against a sandbox before it is performed, because a configuration can be
broken by something that is not this tool, and reverting a change that would not
have fixed it makes things worse. See [Recovering a host](../operating/recovering.md).

## One writer at a time

A lock on the pool directory — not just the state file — stops two processes
writing the same file. It is keyed on the directory itself, at a fixed path, so a
second run pointed at its own state file and backups is still refused. A crashed
process cannot leave a stale lock: it is an `flock` the kernel drops on exit.
