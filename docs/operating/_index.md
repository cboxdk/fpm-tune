---
title: Operating it
weight: 30
description: Running it as a daemon, advising instead of acting, what to alert on, and recovering a host whose master will not start.
---

# Operating it

Once you understand [how it decides](../how-it-decides/_index.md), the operating
concerns are small and few:

- **[Running as a daemon](running-as-a-daemon.md)** — watch-only versus
  `--apply`, and what the loop does each round.
- **[Advisory mode](advisory-mode.md)** — running it permanently as an adviser
  that writes its conclusion to a file you paste by hand, and never touches the
  host.
- **[Alerting](alerting.md)** — the metrics that tell you whether it is working,
  and the one distinction (watching versus acting) that is invisible without them.
- **[Recovering a host](recovering.md)** — what happens, and what to do, when
  php-fpm will not start.
