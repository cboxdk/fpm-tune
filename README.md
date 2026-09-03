# fpm-tune

Sizes `pm.max_children` for every PHP-FPM pool on a host from what its workers cost, and reloads php-fpm when the number is worth changing.

PHP-FPM decides how many of a pool's allowed workers run right now. fpm-tune decides how many should be allowed at all: the ceiling. It measures what each pool's workers cost, divides the host's memory between the pools by that cost, keeps a 15% reserve, and says so when the host is out of capacity.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/cboxdk/fpm-tune/main/install.sh | sh
```

Or `brew install cboxdk/tap/fpm-tune`, or a signed [release archive](https://github.com/cboxdk/fpm-tune/releases). It runs on Linux; see [Installation](docs/getting-started/installation.md).

## Try it

```bash
fpm-tune plan                    # what it would set, and why; writes nothing
sudo fpm-tune install-service    # keep measuring under systemd, in advisory mode
sudo fpm-tune mode apply         # let it act, once the plan has looked right for a day
```

It is beta. Run it advisory first and read what it recommends. When it does act, a change is validated against a copy of the configuration, written atomically, reloaded with SIGUSR2, and rolled back if the master does not come back ([how it fails safe](docs/safety/how-it-fails-safe.md)).

## Docs

- [Overview](docs/index.md): why, and what it does
- [Quickstart](docs/quickstart.md): from install to a sized host in one read
- [Reading a plan](docs/getting-started/reading-a-plan.md): every line of the output
- [Forge and Ploi](docs/cookbook/forge-and-ploi.md): the recipe for those hosts
- [How it decides](docs/how-it-decides/_index.md), [Operating](docs/operating/_index.md), [Safety](docs/safety/_index.md)

Built on [phpfpm](https://github.com/cboxdk/phpfpm). MIT.
