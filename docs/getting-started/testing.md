---
title: Testing
weight: 3
description: What stands behind fpm-tune, and how to try it on a host without risk.
---

# Testing

This page is for anyone deciding whether to trust fpm-tune near a host: what the suite covers, and which commands are safe to try.

## What stands behind it

Unit tests cover the decision logic: the budget, the measurement, the allocator (a randomised sweep of generated plans holds the never-over-budget invariant) and hysteresis. `make check` runs `fmt-check`, `tidy-check`, `vet`, `lint`, `test`, `vulncheck` and `license-check`.

The part that can take a host down is tested against a real php-fpm. CI installs one and runs:

- `testing/e2e.sh`: the end-to-end path with the real binary. A dry run that changes nothing, a reload the master survives with its sockets intact, and a second run refused while the first holds the lock.
- `testing/chaos.sh`: the perturbations a host has. A site added, a site removed and php-fpm reloaded before the tool notices, the master restarted underneath it, and a breakage elsewhere the tool must not react to by deleting its own work. It exists because a soak on a VM found a removed pool still declared in `zz-fpm-tune.conf`, which kept the master from starting.
- `testing/mutations.py`: every named safety guard removed from the source in turn, with the suite required to fail each time. A test that passes with its guard deleted is testing nothing.

CI runs these as separate jobs: `test`, `lint`, `vulncheck`, `sbom` (with the license check), `mutations`, `integration`, and cross-compiled builds for linux and darwin on amd64 and arm64. On a machine with php-fpm installed, `make integration` runs e2e and chaos, and `make mutations` runs the sweep.

## Trying it without risk

- `fpm-tune plan` writes no configuration and reloads nothing. It records what it observed to the state file; `--no-learn` turns that off.
- `sudo fpm-tune apply --dry-run` renders and validates the change against a copy of the pool directory and writes nothing.
- `fpm-tune serve` and `sudo fpm-tune install-service` run in advisory mode: they measure, publish metrics and write the recommendation file, and change nothing.
- `apply --no-learn` and `serve --apply --no-learn` are refused, because a run that writes has to record it, or the next one reloads the pool this one just reloaded.
