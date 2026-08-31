---
title: Is it tested?
weight: 3
description: How fpm-tune is tested, with real php-fpm, chaos, and mutation testing that removes each safety guard and demands the suite fail.
---

# Is it tested?

Short answer: yes, and harder than most tools this size, because one that writes
production config and gets it wrong is worse than no tool at all. If you're deciding
whether to trust it near a real host, here's what stands behind it.

## Against a real php-fpm, not a mock

Unit tests cover the decision logic. But the part that can take a host down (the
reload) is only worth trusting if it's tested against the real thing. So CI also
runs the actual binary against a real php-fpm master:

- **`testing/e2e.sh`**: the happy path *and* its guards: a dry run that provably
  changes nothing, a reload the master survives with its sockets intact, a second
  run correctly refused while the first holds the lock.
- **`testing/chaos.sh`**: what happens when the world moves under it: a site added,
  a site removed and php-fpm reloaded *before* the tool notices, the master
  restarted underneath it, and a breakage elsewhere that the tool must not react to
  by deleting its own work.

The chaos suite exists for a reason: a soak on a VM found a fault that every unit
test, every container test, and five rounds of review had walked straight past.

## The guards are tested by removing them

Here's the part that keeps the *other* tests honest. `testing/mutations.py` takes
each safety guard, removes it, and **requires the suite to fail.** A test that still
passes with the guard deleted was testing nothing, and this repo has shipped
exactly those: an end-to-end reload test that passed with the reload deleted
outright, a rollback test that only ever ran the success path, a shell scenario
asserting on a directory the run never touched. Coverage reports all three as
covered. Mutation testing is what caught them.

```bash
make check         # what CI's unit jobs run: fmt, tidy, vet, lint, race, vulncheck
make integration   # e2e + chaos, against a real php-fpm on this machine
make mutations      # every guard removed in turn; the suite must fail each time
```

## The load generator (by hand)

`testing/loadgen` puts real, sustained concurrency on a pool over persistent
FastCGI connections, which is what a soak needs and a shell loop can't fake (six
"concurrent" `cgi-fcgi` processes serialise on process creation and give you a real
concurrency of two, so the saturation path never runs).

It's deliberately *not* in CI: an assertion that depends on load arriving in time is
one that fails on a busy runner for no real reason. Reach for it by hand when a
change touches saturation, growth, or telling a busy pool from a cheap one.

```bash
go run ./testing/loadgen --socket 127.0.0.1:9000 --concurrency 12 --duration 5m \
  --script /var/www/work.php --query 'mb=8&hold=0.2'
```

The script it hits has to actually allocate and hold the memory, or the pool is
busy without its workers costing anything, and the thing you're trying to measure
never happens.
