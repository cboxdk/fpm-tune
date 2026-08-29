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
fpm-tune apply    # write the settings and reload once
fpm-tune serve    # keep measuring and adjusting, with its own /metrics
```

On a host with five sites under load, capped at 3GiB, it measured this without
being told anything:

| pool  | measured per worker | workers it allocated |
|-------|--------------------:|---------------------:|
| shop  |            98.7 MiB |                    7 |
| forum |            62.1 MiB |                    8 |
| api   |            42.3 MiB |                   20 |
| blog  |            29.5 MiB |                    8 |
| docs  |            24.5 MiB |                    6 |

`api` costs a third of what `shop` costs per worker, so it got nearly three times
as many. That is the whole argument for dividing one budget across pools rather
than sizing each one alone.

## How it decides

**Bootstrap → learned.** With no history it starts from a workload profile, the
same guess a hand-written config makes. As it accumulates trustworthy
measurements it switches to sizing each pool on its observed worker memory.
Baselines persist to `/var/lib/fpm-tune/state.json`, so a restart does not begin
from zero.

Three things make the measurement honest:

- **Adjusts, rather than pinning.** The estimate rises quickly when a pool gets
  more expensive and falls on a half-life measured in time, not in samples. Sizing
  to a percentile of the day pins the host to its busiest hour; a per-sample decay
  means the scrape interval silently changes the behaviour.
- **Never learns from an idle pool.** A worker that has served three requests is
  much smaller than one that has served five hundred. Learning from a quiet pool
  produces a number that fails the moment traffic arrives.
- **Follows the peak of the sawtooth.** `pm.max_requests` recycles workers, so
  memory climbs and resets. The peak is the number that has to fit — and PHP-FPM
  resets its own high-water marks on reload, so the peak is remembered here.

## The budget

Read from the cgroup of the php-fpm master being managed, walking up and taking
the tightest limit, because a cap on any ancestor binds everything below it.

That distinction is the difference between a container and a VM. Inside a
container, `/sys/fs/cgroup/memory.max` is the container's own limit. On a VM it
is the machine, and the machine is never limited — so a php-fpm under
`MemoryMax=3G` would be sized against the host's 20GiB and grown straight into a
ceiling it never sees. `--memory` overrides the detection when php-fpm is not the
only tenant.

## Telling "needs more" from "machine is full"

The distinction that matters when a site slows down:

```
fpm_tune_pool_demand_unmet{pool}   # this pool wants more workers
fpm_tune_capacity_exhausted        # ...and there is nowhere left to get them
```

Demand alone is routine — fpm-tune takes headroom from an idle pool and gives it
to a busy one. Both together is the signal that no configuration change will
help: the machine needs more RAM, or fewer sites. On a host in that state it
stops rearranging and says so, because moving the shortfall between pools costs a
reload of each and does not make it smaller.

## Operating it

**Resetting the baseline.** `rm /var/lib/fpm-tune/state.json` does nothing while
`serve` is running: the daemon holds its baselines in memory and writes them back
on the next save, including on the way out. Stop it first.

```bash
systemctl stop fpm-tune && rm /var/lib/fpm-tune/state.json && systemctl start fpm-tune
```

**If php-fpm will not start.** The most likely cause is a site removed while this
tool still overrides its pool: a pool defined only in the drop-in has no `listen`
and no `user`, so php-fpm refuses the whole configuration. fpm-tune detects that
and takes its own file out — but it cannot bring the service back, because
systemd exhausts its restart burst in seconds, long before any polling supervisor
can land a fix. `systemctl reset-failed php-fpm && systemctl start php-fpm` once
you have read the log line explaining what happened.

**Bind it with `Wants=`, not `Requires=`.** A supervisor that dies with the thing
it supervises cannot repair it.

## Safety

It writes production configuration, so it is built to fail safe.

- **Only `pm.*` keys**, in one file of its own. Your pool config is not edited,
  and deleting that file returns everything to what you configured.
- **One file, one atomic rename.** The change set is indivisible: a growth and the
  reduction that funds it reach the host together or not at all.
- **Validated against a sandboxed copy first**, so a configuration php-fpm would
  reject never reaches the directory it globs — not even for the length of a fork.
  `--dry-run` therefore changes nothing at all, mtimes included.
- **SIGUSR2, never a restart.** PHP-FPM cycles its own workers gracefully and
  carries its listening sockets across, so no request is dropped. A daemonized
  master comes back under a new pid, which is followed rather than mistaken for a
  death.
- **A crash is recoverable.** What is about to be written is recorded first, with
  a phase, so an interrupted run is finished or undone on the next start — and a
  rollback is rehearsed before it is performed, because a configuration can be
  broken by something that is not this tool.
- **It repairs the host if its own file is the problem.** Remove a site and reload
  php-fpm and the override for the pool that no longer exists would stop the master
  from starting; fpm-tune takes its file out so it can.
- **Hysteresis, asymmetric.** Shrinking needs a larger change held for longer than
  growing, because growing too eagerly costs unused memory and shrinking too
  eagerly costs queued requests.
- **One writer at a time**, enforced with a lock on the pool directory as well as
  on the state file.

## Testing

Beyond the unit tests, CI runs two suites against a real php-fpm:

- `testing/e2e.sh` — the happy path and its guards: a dry run that provably
  changes nothing, a reload the master survives with its sockets intact, a second
  run refused while the first holds the lock.
- `testing/chaos.sh` — what happens when the world moves: a site added, a site
  removed and php-fpm reloaded before the tool notices, the master restarted
  underneath it, and a breakage elsewhere that the tool must not respond to by
  deleting its own work.

The chaos suite exists because a soak on a VM found a fault that every unit test,
every container test and five rounds of review had walked past.

## Related

- [phpfpm](https://github.com/cboxdk/phpfpm) — the shared library underneath
- [fpm-exporter](https://github.com/cboxdk/fpm-exporter) — Prometheus metrics for PHP-FPM

## License

MIT
