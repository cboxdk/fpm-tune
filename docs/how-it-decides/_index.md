---
title: How it decides
weight: 20
description: The budget, the learner, the allocator, and when it moves versus holds — the part worth reading before you trust it.
---

# How it decides

A tool that writes production configuration earns trust by being legible, not by
being clever. This section is the whole of what it does with a host's memory,
and every non-obvious choice here was made because the obvious one is wrong in a
way that only shows up under load.

Read it in order:

1. **[The budget](the-budget.md)** — where the number it divides comes from, and
   why reading the machine's memory is the wrong answer on a VM.
2. **[Measuring workers](measuring-workers.md)** — the learner: how it decides
   what one worker costs, why it separates "what it costs" from "may I shrink
   it", and why it will believe an expensive reading instantly but a cheap one
   only slowly.
3. **[Dividing the budget](dividing-the-budget.md)** — the allocator: floors
   first, then demand to the pools a shortage is actually hurting, cheapest fix
   first; and what it does when the floors themselves do not fit.
4. **[Hysteresis](hysteresis.md)** — when a change is worth a reload and when it
   is not, and why growing and shrinking are not held to the same caution.

The allocator ([dividing the budget](dividing-the-budget.md)) is pure
computation with no I/O and no dependencies, which is what makes it exhaustively
testable — a randomised sweep over hundreds of thousands of generated plans
checks the one invariant that matters: a plan never commits more memory than the
budget.
