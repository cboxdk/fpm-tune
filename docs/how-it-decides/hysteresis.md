---
title: Hysteresis
weight: 5
description: When a change is worth a reload and when it is not, and why growing and shrinking are held to different caution.
---

# Hysteresis

Every reload cycles a pool's workers. On a busy multi-site host that is not free,
so the tool does not chase every wobble in the numbers. A change is applied only
when it is large enough and old enough to be worth the reload — and growing and
shrinking are not held to the same bar, because the two failures they guard
against are not the same size.

## The thresholds

| | change needed | time since the last one |
|---|---:|---:|
| **Growing** a pool | 15% | 5 minutes |
| **Shrinking** a pool | 30% | 20 minutes |

Shrinking needs a larger change held for longer. Growing too eagerly costs
unused memory for a few minutes; shrinking too eagerly costs queued requests when
the load comes back and the workers are gone. The two are not worth the same
caution, so they do not get it.

These are the defaults; `--min-change` and `--min-interval` set the growth
thresholds, and the shrink thresholds follow at twice the change and four times
the interval. To force an immediate reload during testing, pass a small non-zero
value (`--min-interval 1s`) rather than `0` — zero is read as "unset" and falls
back to the default.

## Why a pool is not moving

If a pool looks like it should change and does not, this is usually why, and both
`plan` and `apply` will tell you which rule held it back:

- **Too soon.** The pool changed recently and the interval has not elapsed.
- **Too small.** The proposed change is below the threshold — not worth a reload.
- **Not yet trusted.** The pool has plenty of headroom but has not been watched
  under load long enough to justify cutting it. From outside this is
  indistinguishable from the tool ignoring the pool, so the reason says so
  explicitly. See [cost versus permission](measuring-workers.md#cost-and-permission-are-different-questions).

## The change set is indivisible

When a plan grows one pool and shrinks another to fund it, the two are not two
changes — they are one, written in a single atomic rename and reloaded once.
Either both reach the host or neither does. A growth that landed while its
funding reduction did not would commit the host past its budget for the length of
a reload, which is exactly the window an OOM finds.
