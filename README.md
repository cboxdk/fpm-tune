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
fpm-tune serve    # keep measuring; add --apply to act on what it finds
```

`serve` observes and publishes metrics without touching anything until you pass
`--apply`. That is a reasonable way to run it permanently — but note that the
self-repair below is part of applying, so a watch-only daemon will not fix a host
either.

## Running it as an adviser

Watching without acting is a first-class way to use this, and not only a step on
the way to `--apply`. A daemon with no `--apply` changes nothing and never will;
`--recommend` gives it somewhere to put its conclusion.

```bash
fpm-tune serve --recommend /var/lib/fpm-tune/recommended.conf
```

The file is PHP-FPM configuration you can read, diff and paste. Nothing loads
it — a path inside a directory your master includes is refused, because what it
writes carries this tool's own marker and php-fpm would pick it up. It is
rewritten only when the recommended SETTINGS change, so its modification time
answers "when did the advice last move" rather than "is the daemon up".

Each pool comes with the evidence for its number:

```ini
; shop: peak 34 workers busy; raised to 42, measured 96.0MiB/worker
;   measured per worker: median 88.0MiB, p95 137.0MiB, p99 194.0MiB, worst 512.0MiB (4096 readings)

[shop]
pm.max_children = 42
```

The spread is there because one number cannot answer the question you are
actually asking. Sizing follows the typical peak — it rises fast and falls on a
half-life, which is what makes it safe to divide a budget by — but a pool whose
p99 is twice its median has a tail, and a tail is what fills a host at the wrong
moment. A pool that sits flat wants a different decision from one that spikes,
and only the distribution tells them apart. The same three are on `/metrics` as
`estimate="p50"`, `"p95"` and `"p99"`, and `plan` prints them too.

A unit to start from:

```ini
[Unit]
Description=fpm-tune
# Wants, not Requires: a supervisor that dies with the thing it supervises
# cannot repair it.
Wants=php-fpm.service
After=php-fpm.service

[Service]
ExecStart=/usr/local/bin/fpm-tune serve --apply --metrics 127.0.0.1:9110
Restart=on-failure
RestartSec=5

# systemd creates and owns /var/lib/fpm-tune. The tool will create it itself,
# but then its permissions are whatever the umask happened to be — and what
# lives there is the record of what was changed and how to undo it.
StateDirectory=fpm-tune
StateDirectoryMode=0700

[Install]
WantedBy=multi-user.target
```

On a host with five sites under load, it measured what each one's workers cost
without being told anything:

| pool  | what the app holds | measured per worker |
|-------|-------------------:|--------------------:|
| shop  |              80 MB |            98.7 MiB |
| forum |              45 MB |            62.1 MiB |
| api   |              26 MB |            42.3 MiB |
| blog  |              14 MB |            29.5 MiB |
| docs  |               9 MB |            24.5 MiB |

A worker on `shop` costs four times one on `docs`. Sizing every pool from one
profile either starves the expensive sites or wastes the budget on the cheap
ones, and no hand-written `pm.max_children` knows this ratio until something
OOMs. That is the argument for dividing one budget across pools rather than
sizing each alone.

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
- **Never learns from an idle pool.** A quiet pool's workers give their memory
  back to the operating system, so they read small — and that is a lull, not a
  cheaper application. A pool must be serving at least a request a second before
  a smaller reading counts as evidence. The threshold is a real cliff and it is
  deliberate: below it the wrong answer wastes memory, and above it the wrong
  answer loses the host.
- **Time only counts if it was watched.** The estimate falls on a half-life, so
  elapsed time is the weight — which is right while the looking is regular and
  wrong the moment it stops. Restart this tool while php-fpm keeps serving and
  the first reading back is hours old; each pool remembers how often it is
  actually looked at, so a gap can never move the estimate further than one
  ordinary scrape.
- **Small pools are measured too.** A pool that never runs two workers at once,
  or that recycles them before they warm up, was invisible to a stricter version
  of this and got a table's guess for ever. Its readings count toward what it
  COSTS; they still do not count toward permission to shrink it.
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

When a site slows down, the question is whether a configuration change can help:

```
fpm_tune_pool_demand_unmet{pool}   # this pool wanted more workers than it got
fpm_tune_capacity_exhausted        # ...and that is true of at least one pool
```

These are the same news at two granularities — which pool, and whether any — and
either one means no configuration change will help: the machine needs more RAM,
or fewer sites. By the time a plan is published the headroom has already been
taken from the idle pools and given to the busy ones, so a pool still short in
it is short because the budget ran out, not because the next run might rearrange
something. In that state the tool stops rearranging and says so, since moving a
shortfall between pools costs a reload of each and does not make it smaller.

The warning can fire with headroom still showing, and that is not a
contradiction: 300MiB free is nothing to a pool whose workers cost 700MiB each.
It carries both numbers so you can see which it is.

## Alerting

The log reports a persistent condition on the transition rather than every
interval, which is right for reading and useless for alerting. These are the
series to build on instead:

```
fpm_tune_apply_enabled                    # 0 means this process only watches
fpm_tune_apply_blocked{reason}            # asked to apply and cannot: lock, no_master,
                                          # unrepaired, budget_unconfirmed, state_unsaved
fpm_tune_last_run_timestamp_seconds       # not advancing means the loop has stalled
fpm_tune_last_apply_timestamp_seconds     # not advancing while changes are pending
fpm_tune_applies_failed_total
fpm_tune_rollbacks_total                  # above zero deserves a look at the log
fpm_tune_rollback_failed_total            # worse: a rejected file is still on disk
fpm_tune_repairs_total                    # it had to undo something a run left behind
fpm_tune_pools_ambiguous                  # pools NOT published, because two masters
                                          # share their name — see --drop-in-dir
```

`apply_blocked` is the one people forget. A process that is watching and one
that is acting look identical from outside, and the difference is the whole
question when a host is not being tuned.

`/metrics` defaults to `:9110`; `/healthz` answers 200 while the process is up.

## What one site can do to another

The pools share a budget, so it is fair to ask what a site with expensive
workers takes from its neighbours. Measured, on a 4GiB host with five pools
configured for twelve workers each:

| the expensive pool's workers | it gets | a neighbour gets |
|-----------------------------:|--------:|-----------------:|
| 48 MiB (same as the rest)    |      12 |               12 |
| 200 MiB                      |       9 |               10 |
| 500 MiB                      |       5 |                6 |
| 1 GiB                        |       3 |                3 |

Growing more expensive costs a pool its own workers first. It does not buy
them: cost is what the budget is divided by, so an expensive pool gets fewer
workers, not more, and the pressure it puts on its neighbours is the same
pressure it puts on itself. That is the property to want from a shared budget,
and it is worth stating because the opposite would be easy to build by accident.

Past the point where a single worker will not fit on the host at all, no
arrangement is valid and the tool refuses rather than choosing a loser — naming
the pool that made it impossible, since on a host with many sites that is the
only actionable thing in the message.

What the tool trusts is the pool configuration, which is root-owned. A tenant
who can edit their own pool file can influence the plan, and
`php_admin_value[memory_limit]` is what bounds how expensive they can become.

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

- **Only `pm.*` keys**, in one file of its own — `zz-fpm-tune.conf`, in the
  directory your master already includes. Your pool config is not edited, and
  deleting that file returns everything to what you configured — the next run
  writes a fresh one from what it can see, and does not put the old overrides
  back.
- **`/var/lib/fpm-tune/backup` is not scratch space.** The previous version of
  the file lives there while a change is in flight, alongside a record of what
  was in flight and a note of where php-fpm lives. That note is how the tool
  repairs a host whose master will not start — the one situation where nothing
  can be discovered because there is nothing running to discover. A tmpfiles
  rule that cleans this directory takes away the ability to undo a change and to
  fix that host.
- **One file, one atomic rename.** The change set is indivisible: a growth and the
  reduction that funds it reach the host together or not at all.
- **Validated against a sandboxed copy first**, so a configuration php-fpm would
  reject never reaches the directory it globs — not even for the length of a fork.
  `--dry-run` writes no PHP-FPM configuration and reloads nothing; it does record
  what it observed, unless you pass `--no-learn`.
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
- **Hysteresis, asymmetric.** Growing needs a 15% change and 5 minutes since the
  last one; shrinking needs 30% and 20 minutes. Growing too eagerly costs unused
  memory; shrinking too eagerly costs queued requests, so the two are not worth
  the same caution. If a pool is not moving when you think it should, this is
  usually why — `plan` shows the proposal, `apply` shows which rule held it back.
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

And a third thing, which is what stopped several of the tests above from being
decoration: `testing/mutations.py` removes each safety guard in turn and
requires the suite to fail. A test that passes against a deleted guard is not
testing it — this repo has shipped an end-to-end reload test that passed with
the reload deleted outright, a rollback test that only ever ran the success
path, and a shell scenario asserting on a directory the run never touched.
Coverage does not catch those.

```bash
make check         # what CI's unit jobs run: fmt, tidy, vet, lint, race, vulncheck
make integration   # both suites against a real php-fpm on this machine
make mutations     # every guard removed in turn; the suite must fail each time
```

`testing/loadgen` is the fourth thing, and it is not wired into any of them. It
puts real, sustained concurrency on a pool over persistent FastCGI connections,
which is what a soak on a VM needs and what a shell loop cannot provide — six
"concurrent" `cgi-fcgi` processes serialise on process creation and produce a
measured concurrency of two, so the saturation path never runs and the tool
looks like it is cutting a busy pool when it is reading an idle one.

It stays out of the suites on purpose: an assertion that depends on load
arriving in time is an assertion that fails on a loaded CI runner for no reason.
Reach for it by hand when a change touches saturation, growth, or the difference
between a quiet pool and a cheap one.

```bash
go run ./testing/loadgen --socket 127.0.0.1:9000 --concurrency 12 --duration 5m \
  --script /var/www/work.php --query 'mb=8&hold=0.2'
```

The script it requests has to allocate and hold, or the pool is busy without its
workers costing anything and the measurement under test never happens.

## Related

- [phpfpm](https://github.com/cboxdk/phpfpm) — the shared library underneath
- [fpm-exporter](https://github.com/cboxdk/fpm-exporter) — Prometheus metrics for PHP-FPM

## License

MIT
