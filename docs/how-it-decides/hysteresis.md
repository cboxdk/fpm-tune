---
title: Hysteresis
weight: 6
description: When a change is worth a reload, why shrinking is damped harder than growing, and where to see what held a pool back.
---

# Hysteresis

Every reload cycles a pool's workers, so fpm-tune does not chase every wobble
in the numbers. A change is applied only when it is large enough and the last
one is old enough, and the two directions are held to different bars. This
page is the thresholds, and where to look when a pool is not moving.

## The thresholds

| | change needed | time since the last change |
|---|---:|---:|
| growing a pool | 15% | 5 minutes |
| shrinking a pool | 30% | 20 minutes |

Growing too eagerly costs some unused memory for a few minutes. Shrinking too
eagerly costs queued requests when the load comes back and the workers are
gone, and a symmetric threshold lets a pool cross it in both directions on
adjacent rounds, growing and shrinking every few minutes with each reload
individually justified. The larger, longer bar on the way down is what breaks
that cycle.

`--min-change` and `--min-interval` set the growth thresholds; the shrink
thresholds follow at twice the change and four times the interval. Zero means
the default, so to force a reload while testing pass a small value,
`--min-interval 1s --min-change 0.001`, rather than `0`.

## What held a pool back

Hysteresis is evaluated only when something is about to be written. `plan`
never evaluates it: its number is what the allocator reaches now, and the
recommendation file says the same of itself. `fpm-tune apply` prints the rule
in its DETAIL column:

- **too soon**: the pool changed less than the interval ago.
- **too small**: the change is below the threshold.
- **held back**: the pool would grow, but the memory would have to come from a
  shrink that is still inside its own interval, so the growth waits.

The daemon logs `Pool resized` when a change goes through; between changes its
`Pool recommendation` line shows the number it wants beside the one in effect.
`apply-now` asks the daemon for one round with the damping waived (see
[applying once](../operating/applying-once.md)).

The commonest reason a pool with plenty of slack keeps its configured ceiling
is a different rule, the confidence gate: it has not been watched under load
long enough to be shrunk, and the plan's WHY says so with
`not yet watched under load; held at its configured N`. See
[cost and permission](measuring-workers.md#cost-and-permission).

## One change set, one reload

When a plan grows one pool and shrinks another to fund it, the two are one
change: written in a single rename of `zz-fpm-tune.conf` and reloaded once, so
either both reach the host or neither does. A growth that landed while its
funding shrink stayed behind would commit the host past its budget for the
length of a reload, which is the window an OOM finds. The shrink is pulled
through the size threshold when the arithmetic needs it, but never through the
time brake: a pool reloaded a minute ago is not reloaded again because a
neighbour wants to grow, and the growth waits instead.
