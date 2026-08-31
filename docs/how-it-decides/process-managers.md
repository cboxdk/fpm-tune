---
title: Static, dynamic, ondemand
weight: 6
description: What fpm-tune does with each pm mode, why it sizes within the mode instead of changing it, and the one suggestion it will make.
---

# Static, dynamic, ondemand

PHP-FPM gives every pool a `pm` mode, and it changes what `pm.max_children`
*means*. fpm-tune sizes that number for all three — but the number lands
differently depending on the mode, so it's worth 60 seconds on what each one does.

- **static** — the pool runs *exactly* `pm.max_children` workers, always. No
  scaling. `max_children` isn't a ceiling here; it's the running count.
- **dynamic** — PHP-FPM keeps a warm floor and scales between it and
  `max_children` as load comes and goes, using `start_servers` and the
  `min/max_spare_servers` band.
- **ondemand** — no workers until a request arrives; each is spawned on demand and
  killed after it goes idle. `max_children` is the ceiling it may spawn up to.

The one thing to hold onto: **for a static pool the number is the memory it
commits right now; for an ondemand pool it's only a limit it might reach.** Sizing
the same integer means two different things.

## What fpm-tune writes, per mode

| mode | what it writes | notes |
|------|----------------|-------|
| **static** | `pm.max_children` only | changing it changes the resident worker count immediately |
| **ondemand** | `pm.max_children` only | sizes the ceiling; PHP-FPM still spawns on demand under it |
| **dynamic** | `pm.max_children` **plus** `start_servers`, `min_spare_servers`, `max_spare_servers` | the spare band is re-derived from the new ceiling (≈25% to start, a 10–50% spare band) so PHP-FPM's own scaling stays coherent |

Two details that matter:

- **Spare settings are dynamic-only.** Writing `pm.start_servers` into a static or
  ondemand pool is a config error PHP-FPM refuses — so fpm-tune writes them for
  dynamic pools and nothing else. (This is exactly the kind of thing its
  [sandbox validation](../safety/how-it-fails-safe.md) catches before a reload, but
  it's cheaper never to write it.)
- **The learner is ondemand-aware.** An ondemand pool sitting visible-but-empty
  isn't evidence its workers are cheap — it's just quiet. fpm-tune won't
  under-size a pool from a scrape that happened to catch it idle. The details are
  in [measuring workers](measuring-workers.md).

## Why it doesn't just pick the mode for you

Because the right mode isn't a memory question, and fpm-tune only measures memory.

static-vs-dynamic-vs-ondemand is a trade between predictable RAM, latency, and
cold-starts — and the tie-breakers (do you have a latency SLO? do you want
memory that never moves? is this one of forty pools that are each hit twice an
hour?) live in your head, not in the numbers on the host. So fpm-tune sizes
*inside* the mode you chose and never rewrites `pm` itself. Its whole stance is to
keep PHP-FPM's own worker management on and get the ceiling right underneath it.

Here's the shape of the trade, so the choice is yours to make well:

| | static | dynamic | ondemand |
|---|---|---|---|
| **idle memory** | highest — every worker resident always | low — scales down to the floor | lowest — nothing when quiet |
| **latency under load** | best — workers always warm | good — warm floor absorbs bursts | worst — each burst pays a cold start |
| **fits** | steady, latency-sensitive, memory to spare | most pools; a safe default | many pools each rarely hit |

## The one suggestion it will make

fpm-tune stays quiet about mode unless the *measured* shape points somewhere
clearly. When it does, `plan` prints a line under the table (and `serve` logs it
once) — a nudge, never a change:

- **A static pool holding idle workers.** If its busiest moment left a lot of
  workers unused, static is paying to keep memory resident that nothing touched.
  dynamic or ondemand would hand that back between requests — *unless* you're
  keeping them warm for latency on purpose, which is a fine reason to ignore it.
- **An ondemand pool that's queuing.** Requests waiting on an ondemand pool means
  bursts are paying cold-start latency. A dynamic pool with a warm floor absorbs
  the burst instead.

It won't push a busy dynamic pool toward static, even though that can be the right
move — telling "steadily maxed out" from "spiked once" needs a sustained-load
signal fpm-tune doesn't keep, and guessing there would just be noise. dynamic is a
safe default; if you want static's predictable memory, that's a deliberate call.

## Mode is not a security boundary

Worth saying plainly, since it comes up: the `pm` mode has nothing to do with
isolating one pool from another. That's `user`/`group`/`chroot`/`open_basedir`,
and it's orthogonal to how workers are scaled. What fpm-tune does guard is covered
in [the trust boundary](../safety/the-trust-boundary.md); mode choice is a
performance-and-memory decision, not a security one.
