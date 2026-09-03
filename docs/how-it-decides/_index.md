---
title: How it decides
weight: 20
description: The budget, the learner, the allocator, the damping and the CPU side, for anyone deciding whether to let it write.
---

# How it decides

This section is what fpm-tune does with a host's memory and CPU between reading
the status pages and writing `pm.max_children`. Read it before switching a
daemon to apply mode, or when a plan's number looks wrong and you want to know
where it came from.

- **[The budget](the-budget.md)**: where the memory it divides comes from, what
  it holds back, and how to give php-fpm a hard limit.
- **[Dividing the budget](dividing-the-budget.md)**: the allocator: floors,
  demand, and what happens when the host is out of capacity.
- **[Measuring workers](measuring-workers.md)**: what one worker costs, which
  readings count, and when a pool may be shrunk.
- **[Spawned children](spawned-children.md)**: the ffmpeg behind a worker, how
  it is measured, and how to declare it before it is.
- **[Static, dynamic, ondemand](process-managers.md)**: what it writes for each
  `pm` mode, and the one suggestion it makes.
- **[Hysteresis](hysteresis.md)**: when a change is worth a reload.
- **[CPU per request](cpu.md)**: which of memory and CPU a pool runs out of
  first, and the ceiling `--cpu` holds it at.
- **[CPU measurement](cpu-measurement.md)**: appendix: how the CPU figures are
  measured.
