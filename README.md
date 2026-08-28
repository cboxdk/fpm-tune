# fpm-tune

Sizes PHP-FPM pools against the memory a machine actually has, using the memory
its workers actually use.

A server running many sites has many pools competing for one pool of RAM. Setting
`pm.max_children` per pool by hand means either leaving capacity unused or
discovering the ceiling through an OOM kill. fpm-tune measures what each pool's
workers really cost, divides the budget accordingly, and writes it back.

Runs standalone. It does its own discovery, its own measurement, its own budget
detection, and serves its own metrics — no other process required.

## What it does

```bash
fpm-tune plan     # what it would change, and why — writes nothing
fpm-tune apply    # write the drop-ins and reload once
fpm-tune serve    # keep measuring and adjusting, with its own /metrics
```

## How it decides

**Bootstrap → learned.** With no history it starts from a workload profile, the
same guess a hand-written config makes. As it accumulates trustworthy
measurements it switches to sizing each pool on its observed worker memory.
Baselines persist to `/var/lib/fpm-tune/state.json`, so a restart does not begin
from zero.

Three things make the measurement honest:

- **Sizes on a high percentile, not the mean.** Mean worker RSS systematically
  under-provisions; the tail is what OOMs.
- **Never learns from an idle pool.** A worker that has served three requests is
  much smaller than one that has served five hundred. Learning from a quiet pool
  produces a number that fails the moment traffic arrives.
- **Follows the peak of the sawtooth.** `pm.max_requests` recycles workers, so
  memory climbs and resets. The peak is the number that has to fit.

## Telling "needs more" from "machine is full"

The distinction that matters when a site slows down:

```
fpm_tune_pool_demand_unmet{pool}   # this pool wants more workers
fpm_tune_capacity_exhausted        # ...and there is nowhere left to get them
```

Demand alone is routine — fpm-tune takes headroom from an idle pool and gives it
to a busy one. Both together is the signal that no configuration change will
help: the machine needs more RAM, or fewer sites.

## Safety

It writes production configuration, so it is built to fail safe:

- Only `pm.*` keys are written, to a separate drop-in. Your pool config is not edited.
- Every change is validated with `php-fpm -t` **before** anything is moved into place.
- Reload via SIGUSR2 — never a restart. PHP-FPM cycles its own workers gracefully.
- The previous drop-in is kept; a master that does not come back is rolled back.
- Hysteresis and a minimum interval, so a busy server is not reloaded constantly.

## Related

- [phpfpm](https://github.com/cboxdk/phpfpm) — the shared library underneath
- [fpm-exporter](https://github.com/cboxdk/fpm-exporter) — Prometheus metrics for PHP-FPM

## License

MIT
