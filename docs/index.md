---
title: fpm-tune
weight: 0
description: Sizes pm.max_children across every PHP-FPM pool on a host, from what the workers actually cost, so the right number isn't a guess you find out the hard way.
---

# fpm-tune

Most PHP-FPM tuning starts with a bit of mental arithmetic:

> I've got 8 GB of RAM. A worker uses about 100 MB. So I can run ~80 workers.

That's a fine starting point when you have **one** app.

It falls apart when a box runs 10, 50, or 100 pools. Because those pools don't
each get 8 GB; they're all fighting over the *same* 8 GB. And their workers don't
cost the same: your shop's workers might sit at 180 MB while the marketing blog's
sit at 40 MB. One static number can't be right for all of them.

And most of us never really tuned this. We copied a number, ran a calculator, or
bumped it when the server felt slow, and that worked *surprisingly* well, because
the easy fix was always to throw more RAM at the box and leave headroom. But headroom
you pay for and don't use isn't free. You don't need to squeeze out every last
megabyte; you do want to balance three things at once: **better use** of the machine
you already pay for, **stability** you can predict, and **enough capacity** for when
the apps actually need it. Lean too conservative and you rent RAM to sit idle while
requests queue for a worker; too aggressive and you get swapping, latency spikes, or
an OOM; too static and today's right answer is next week's wrong one.

**That's what fpm-tune is for.** It watches what each pool's workers really cost,
divides the one pile of RAM between them (keeping a deliberate safety margin), and,
if you let it, writes the `pm.max_children` back and reloads. It's beta; more on that
below.

Memory is the number that OOMs a host, so memory is what it sizes on. It also
measures what each pool's requests cost in CPU, from php-fpm's own per-request
figure, and every plan says which of the two a pool runs out of first: a pool
whose busy workers fill the cores gets nothing from more workers but slower
requests. Pass `--cpu` to let that cap the pool. See
[CPU per request](how-it-decides/cpu.md).

## The one distinction to get

PHP-FPM already starts and stops workers for you. fpm-tune **doesn't replace
that.** They answer different questions:

- **PHP-FPM asks:** how many of my *allowed* workers should be running right now?
- **fpm-tune asks:** how many workers should this pool be *allowed* in the first
  place?

That's it. FPM manages workers inside a box. fpm-tune sizes the box.

## What it actually does

Six steps, on a loop:

1. Work out how much memory PHP-FPM can actually use.
2. Watch the workers.
3. Learn what a worker costs, per pool.
4. See which pools are actually short of capacity.
5. Divide the memory between them.
6. Keep watching, because workloads change.

That's the whole product. Everything else in these docs is how we make those six
steps *safe* and *correct*.

## Why not just… ?

**Set `pm.max_children` by hand?** Sure. Maybe you nailed it. Maybe you're wasting
half the machine. Maybe you'll find out it was wrong when the OOM killer tells you.

**Use a memory calculator?** `8 GB / 100 MB ≈ 80 workers` is useful, until you ask
where the 100 MB came from, and what happens when the shop's workers are 180 MB, the
API's are 70 MB, one app starts shelling out to ImageMagick, and traffic moves from
one site to another overnight. A number frozen at deploy time doesn't move with any
of that.

**Rely on PHP-FPM's `dynamic`/`ondemand`?** Please do, keep it on. But it still
operates *underneath* `pm.max_children`. If that ceiling is wrong, FPM is very
efficiently managing workers inside the wrong box. (fpm-tune sizes the box for all
three modes, and won't touch which one you picked.
[See how it handles each](how-it-decides/process-managers.md).)

**fpm-tune moves the box.** It watches what's actually happening and adjusts the
limits around the real workload.

## The idea worth understanding

Say you've got 4 GB and three sites, and their workers really cost:

```
shop   150 MB
api     60 MB
blog    35 MB
```

Setting `pm.max_children = 30` on all three doesn't mean you have room for 90
workers. And sizing each pool against the full 4 GB *separately* is worse. You'd
promise the same gigabytes three times over. **There's only one pile of RAM.**
fpm-tune treats it that way.

Two things fall out of that, and they're the point:

- **Expensive apps get fewer workers, automatically.** Cost is what the budget is
  divided by, so a pool whose workers are heavy simply gets fewer of them. It
  doesn't get to muscle its neighbours out. The pressure it puts on them is the
  same pressure it puts on itself.
- **A busy pool can borrow from an idle one.** Headroom a quiet site isn't using
  can go to a site that's queueing right now, and come back when the traffic
  moves. That's the bit a per-pool calculator can't do.

## It keeps learning

fpm-tune doesn't measure a worker once and call that its cost forever. Apps change.
Workers warm up. Caches fill. Some requests are cheap and some are monsters. Some
apps spawn *other* processes.

So here's the catch it's careful about: **memory is allowed to jump up fast, but
we're deliberately slow to believe an app got cheaper.** Being a bit too cautious
wastes some RAM. Being too optimistic kills the machine. We spend the caution where
being wrong hurts most. (Want the nerdy version: decay half-lives, why an idle
pool is never trusted, the sawtooth?
[It's all here](how-it-decides/measuring-workers.md).)

## "Autoscaling" that doesn't mean "buy another server"

Scaling up isn't always adding a machine. Sometimes it's using the one you have
better, moving capacity to the pool that needs it from the pools that don't.
fpm-tune does that *inside* one host.

But when everyone needs more and the budget's gone, it won't pretend. It tells you
the host is **out of capacity**. And at that point re-tuning `pm.max_children`
won't save you. You need more RAM, fewer sites, or another box. Being honest about
that moment is the whole reason the [capacity signals](operating/alerting.md) exist.

## Will it break my server?

Fair question: it writes production config. The answer: **you decide how far to
trust it, one step at a time.** `plan` only looks. `serve` watches and recommends,
still touching nothing. Only `--apply`, or an `apply-now` you ask for, lets it
act, and you flip `--apply` on once its decisions have looked right for a day or
a week, not before. (The
[Quickstart](quickstart.md) walks that path command by command.)

When it *does* act, nothing reaches PHP-FPM until it's been validated against a
throwaway copy; the change lands as one atomic write and a *graceful* reload, and
rolls back if the master doesn't come back. The [safety model](safety/_index.md) is
worth two minutes before you trust `--apply`.

**It's beta.** Run it advisory-first and read what it recommends before you let it
write.

## Start here

- **[Quickstart](quickstart.md)**: installed and looking at a real plan in one read.
- **[Getting started](getting-started/_index.md)**: install, and a first run done safely.
- **[On Forge or Ploi?](cookbook/forge-and-ploi.md)**: the whole flow for a shared
  box, start to finish. Probably the fastest path if that's where your sites live.

## Go deeper (only if you want to)

You don't need any of this to run the thing. It's here for when you're deciding
whether to trust it, or something surprised you.

- **[How it decides](how-it-decides/_index.md)**: the budget, the learner, the
  allocator, and when it moves versus holds.
- **[Operating it](operating/_index.md)**: as a daemon, as an adviser, what to
  alert on, and what to do when php-fpm won't start.
- **[Safety](safety/_index.md)**: how it fails safe, and what it trusts on a
  shared host.

## Underneath

- **[phpfpm](https://github.com/cboxdk/phpfpm)**: the library it uses to discover,
  parse, scrape and reload php-fpm.
- **[fpm-exporter](https://github.com/cboxdk/fpm-exporter)**: Prometheus metrics
  for PHP-FPM, if all you want is to watch.
