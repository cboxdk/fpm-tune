# fpm-tune

**Right-sizes `pm.max_children` across every PHP-FPM pool on a host — from what the
workers actually cost — so you stop guessing and stop OOMing.**

PHP-FPM tuning usually starts with mental arithmetic: *8 GB of RAM, ~100 MB a
worker, so ~80 workers.* Fine for one app. It falls apart when a box runs 10, 50 or
100 pools, because they're all fighting over the **same** 8 GB — and their workers
don't cost the same. One static number can't be right for all of them.

fpm-tune watches what each pool's workers really cost, divides the one pile of RAM
between them, and — if you let it — writes the settings back and reloads.

## The one distinction to get

PHP-FPM already starts and stops workers. fpm-tune doesn't replace that.

- **PHP-FPM** decides how many of a pool's *allowed* workers run right now.
- **fpm-tune** decides how many a pool should be *allowed* in the first place.

FPM manages workers inside a box. fpm-tune sizes the box.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/cboxdk/fpm-tune/main/install.sh | sh
```

or `brew install cboxdk/tap/fpm-tune`, or grab a static binary from
[releases](https://github.com/cboxdk/fpm-tune/releases) (Linux and macOS, amd64 and
arm64; every release is Sigstore-signed —
[how to verify](docs/getting-started/installation.md#verifying-a-release)). The
tuning happens on Linux — reading a host's real memory needs cgroups and
`/proc/meminfo`; the macOS build is for local dev, not for sizing your Mac.

**It's beta.** Run it advisory-first on a real host and read what it recommends
before you let it write anything.

## Try it

```bash
fpm-tune plan     # what it would do, and why. Changes nothing.
fpm-tune serve    # keep watching and recommending. Still changes nothing.
                  # add --apply only when you're ready to let it act.
```

Start with `plan`. Leave `serve` running as an adviser for a while — it watches the
real workload and writes down what it *would* do, touching nothing. Flip on
`--apply` once its decisions look right. On a server, one command sets that up:

```sh
sudo fpm-tune install-service   # runs it under systemd, advisory by default
sudo fpm-tune mode apply        # let it act, when you trust it
```

## Why this beats a fixed number

Point it at a host with a few sites and it measures what each app's workers actually
cost — no profile, no being told anything. The spread is the whole point; on a
typical multi-site box it looks something like:

| pool  | per worker |
|-------|-----------:|
| shop  |    ~100 MiB |
| forum |     ~60 MiB |
| api   |     ~40 MiB |
| blog  |     ~30 MiB |
| docs  |     ~25 MiB |

A `shop` worker there costs four times a `docs` worker. Size every pool from one profile
and you either starve the expensive sites or waste the budget on the cheap ones —
and no hand-written `pm.max_children` knows this ratio until something OOMs. There's
only one pile of RAM; fpm-tune divides it by what each pool actually costs, so an
expensive pool gets *fewer* workers and a busy pool can borrow headroom from an idle
one.

When everyone needs more and the budget's gone, it says so plainly — the host is out
of capacity, and no config change will help.

## Will it break my server?

It writes production config, so it's built to earn trust one step at a time — `plan`
and `serve` change nothing until you add `--apply`. And when it does act, nothing
reaches PHP-FPM until it's been validated against a throwaway copy; changes go in as
one atomic write and a *graceful* reload (no dropped requests), and roll back if the
master doesn't come back. If its own file ever stops php-fpm from starting, it takes
that file back out. The full model is in **[Safety](docs/safety/_index.md)**.

## Docs

New here? **[Start with the overview](docs/index.md)** or jump to the
**[Quickstart](docs/quickstart.md)**.

Going deeper: **[How it decides](docs/how-it-decides/_index.md)**
(the [budget](docs/how-it-decides/the-budget.md), the
[learner](docs/how-it-decides/measuring-workers.md), the
[allocator](docs/how-it-decides/dividing-the-budget.md)),
**[Operating it](docs/operating/_index.md)**, and **[Safety](docs/safety/_index.md)**.

Running Laravel Forge or Ploi? There's a **[recipe for that](docs/cookbook/forge-and-ploi.md)**.

## Is it actually tested?

Yes, and unusually hard — because a tool that writes production config and gets it
wrong is worse than no tool. Beyond the unit tests, CI runs the real binary against
a real php-fpm (a genuine reload, a genuine rollback), throws chaos at it (a site
removed mid-reload, the master restarted underneath it), and then removes each
safety guard one at a time and *requires the suite to fail* — because a test that
passes with the guard deleted was testing nothing. Details in
[Testing](docs/getting-started/testing.md).

## Related

- [phpfpm](https://github.com/cboxdk/phpfpm) — the library it's built on
- [fpm-exporter](https://github.com/cboxdk/fpm-exporter) — Prometheus metrics for PHP-FPM

## License

MIT
