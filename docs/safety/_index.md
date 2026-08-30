---
title: Safety
weight: 40
description: How it fails safe when it writes production configuration, and what it trusts on a shared host.
---

# Safety

It writes production configuration and reloads a live master, so correctness and
fail-safety matter more than anything else it does. Two things to read:

- **[How it fails safe](how-it-fails-safe.md)** — the guarantees around every
  write: sandbox validation, atomic replacement, graceful reload, rollback, and
  crash recovery.
- **[The trust boundary](the-trust-boundary.md)** — what it trusts, what one
  tenant on a shared host can and cannot make it do to another, and what bounds
  it.
