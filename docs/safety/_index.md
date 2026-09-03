---
title: Safety
weight: 50
description: What happens between a plan and a reloaded master, and what the tool trusts on a shared host.
---

# Safety

fpm-tune writes production configuration and reloads a live master, so this section is about what it does when a step fails and what it refuses to do at all. Read it before switching a host to apply mode.

- **[How it fails safe](how-it-fails-safe.md)**: the chain from plan to reload, the rollback, the transaction record, and the two files it writes.
- **[The trust boundary](the-trust-boundary.md)**: what it trusts, what one tenant can do to another, and what only root can reach.
