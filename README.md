# fpm-tune

**Sizes `pm.max_children` across every PHP-FPM pool on a host from what the workers
actually cost, so the right number isn't a guess you find out the hard way.**

PHP-FPM tuning usually starts with mental arithmetic: *8 GB of RAM, ~100 MB a worker,
so ~80 workers.* Even on **one pool** (what most boxes run), that single number has
two ways to bite:

- **Too high** and a traffic spike spawns more workers than the box can feed. Memory
  runs out, and the OOM killer takes something down, often MySQL or php-fpm itself.
- **Too low** and requests queue behind a ceiling you pinned below what the RAM could
  actually serve. The site is slow under load *and* you're paying for memory that
  sits idle.

The right `pm.max_children` is the one your workers' *real* cost supports: high enough
to use the RAM you have, low enough not to blow past it. And ~100 MB was a guess. It
only gets harder with more pools. A box running 10, 50 or 100 of them has every pool
fighting over the **same** 8 GB, with workers that don't cost the same, so one static
number can't be right for all of them at once.

Most of us just guess (copy a number, run a calculator, bump it when the box feels
slow), and honestly that worked fine while the cheap fix was always *more RAM*. But
headroom you pay for and don't use isn't free. The goal isn't to squeeze out every
last megabyte; it's to balance three things at once: using the box you already pay
for, staying stable, and keeping workers free for when traffic actually shows up.

fpm-tune watches what a pool's workers really cost and sizes its `pm.max_children` to
fit the memory that's actually there. Across several pools, it divides the one shared
budget between them by what each really costs. It keeps a deliberate safety margin
and, if you let it, writes the numbers back and reloads php-fpm.

## The one distinction to get

PHP-FPM already starts and stops workers. fpm-tune doesn't replace that.

- **PHP-FPM** decides how many of a pool's *allowed* workers run right now.
- **fpm-tune** decides how many a pool should be *allowed* in the first place.

FPM manages workers inside a box. fpm-tune sizes the box.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/cboxdk/fpm-tune/main/install.sh | sh
```

or `brew install cboxdk/tap/fpm-tune`, or a static binary from
[releases](https://github.com/cboxdk/fpm-tune/releases) (every release is
Sigstore-signed; see [how to verify](docs/getting-started/installation.md#verifying-a-release)).
The tuning runs on Linux: reading a host's real memory needs cgroups and
`/proc/meminfo`. The macOS build is for local dev.

**It's beta.** Run it advisory-first on a real host and read what it recommends
before you let it write anything.

## Try it

```bash
fpm-tune plan     # what it would do, and why. Changes nothing.
fpm-tune serve    # keep watching and recommending. Still changes nothing.
                  # add --apply only when you're ready to let it act.
```

Start with `plan`. Leave `serve` running as an adviser for a while, and flip on
`--apply` once its decisions look right. On a server, one command sets that up under
systemd:

```sh
sudo fpm-tune install-service   # advisory by default
sudo fpm-tune mode apply        # let it act, when you trust it
fpm-tune top                    # watch it: workers, queues, CPU, every resize
```

## Why this beats a fixed number

Point it at a host with a few sites and it measures what each app's workers actually
cost, with no profile and nothing to tell it. The spread is the whole point; on a
typical multi-site box it looks something like:

| pool  | per worker |
|-------|-----------:|
| shop  |    ~100 MiB |
| forum |     ~60 MiB |
| api   |     ~40 MiB |
| blog  |     ~30 MiB |
| docs  |     ~25 MiB |

A `shop` worker there costs four times a `docs` worker. Size every pool from one
profile and you either starve the expensive sites or waste budget on the cheap ones.
There's only one pile of RAM; fpm-tune divides it by what each pool actually costs, so
an expensive pool gets *fewer* workers and a busy one can borrow headroom from an idle
neighbour. When the budget's gone, it says so plainly instead of pretending a config
change will help.

## And the other dimension

Memory is what OOMs a host, so memory is what fpm-tune sizes on. For a cpu-bound
pool that is the wrong number. Take uncached WordPress: once its busy workers
fill the cores, every extra worker only lengthens each request, and the queue
stops draining. So every plan also reports what each pool's requests cost in
CPU, from php-fpm's own per-request figure. Pass `--cpu` to let that cap the
pool. See [CPU per request](docs/how-it-decides/cpu.md).

## Safe to try

It writes production config, so it earns trust one step at a time: `plan` and `serve`
change nothing until you add `--apply`. When it does act, every change is validated
against a throwaway copy, written atomically, reloaded *gracefully* (SIGUSR2, not a
restart), and rolled back if the master doesn't come back. And if its own file ever
stops php-fpm from starting, it takes that file back out. It reloads only when a
change is worth it, because a reload is not perfectly free (php-fpm recreates a
pool's socket, so a rare request can blip). The full model is in
[Safety](docs/safety/_index.md).

## Docs

**[Start with the overview](docs/index.md)**, or jump to the
**[Quickstart](docs/quickstart.md)**. On Laravel Forge or Ploi? There's a
**[recipe for that](docs/cookbook/forge-and-ploi.md)**.

Going deeper: **[How it decides](docs/how-it-decides/_index.md)**,
**[Operating it](docs/operating/_index.md)**, and **[Safety](docs/safety/_index.md)**.
It's [tested hard](docs/getting-started/testing.md): CI runs the real binary against
a real php-fpm, then deletes each safety guard in turn and requires the suite to fail.

## Related

- [phpfpm](https://github.com/cboxdk/phpfpm): the library it's built on
- [fpm-exporter](https://github.com/cboxdk/fpm-exporter): Prometheus metrics for PHP-FPM

## License

MIT
