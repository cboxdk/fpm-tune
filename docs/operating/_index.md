---
title: Operating it
weight: 30
description: Running it as a service, applying once, the metrics, advisory mode, recovery, and the lifecycle of an install.
---

# Operating it

These pages are for the person who runs fpm-tune on a host: installing it as a service, acting on its plan once, alerting on it, and undoing, removing or upgrading it.

- **[Running as a daemon](running-as-a-daemon.md)**: the systemd install, the two modes, what a round does, and what it logs.
- **[Applying once](applying-once.md)**: `fpm-tune top` and `sudo fpm-tune apply-now`, for acting on a plan you have read.
- **[Metrics and alerting](metrics-and-alerting.md)**: every series on `/metrics`, the three alerts that matter, `/healthz` and `/history.json`.
- **[Advisory mode](advisory-mode.md)**: the recommendation file, and the workflow of deciding by hand.
- **[Recovering a host](recovering.md)**: what happens, and what to do, when php-fpm will not start.
- **[Lifecycle](lifecycle.md)**: undoing a change, removing it, upgrading, what it costs to run, and resetting the baseline.
