---
title: fpm-tune
weight: 0
description: Sizes pm.max_children for every PHP-FPM pool on a host from what its workers cost.
---

# fpm-tune

This page says what fpm-tune is for and what it does, for anyone deciding whether to run it. The [Quickstart](quickstart.md) is where to go to run it.

## The arithmetic everyone does

PHP-FPM tuning starts with a sum: 8 GB of RAM, about 100 MB a worker, so about 80 workers. The 100 MB is a guess, and `pm.max_children` is only as good as it. Set the ceiling too high and a traffic spike spawns more workers than the host can feed, and the OOM killer takes something down, often MySQL. Set it too low and requests queue behind a ceiling pinned below what the memory could serve, while memory you pay for sits idle.

A host with several pools makes the sum harder. Every pool draws on the same 8 GB, and their workers cost different amounts: a shop's workers can cost four times a blog's. One number cannot be right for all of them, and sizing each pool against the full 8 GB on its own promises the same gigabytes several times over.

## The one distinction

PHP-FPM already starts and stops workers. fpm-tune does not replace that. PHP-FPM decides how many of a pool's allowed workers run right now; fpm-tune decides how many the pool should be allowed at all. That number is the ceiling, `pm.max_children`, and it is the only sizing fpm-tune does.

## What it does

fpm-tune works out how much memory php-fpm may use (the cgroup limit where there is one, otherwise the host's memory less what other services hold), keeps 15% of it as a reserve, measures what each pool's workers cost from `/proc`, and divides the rest between the pools by that cost and by how many workers each has needed. An expensive pool gets fewer workers, and a busy pool may use the slack of a quiet one. It reports what each pool's requests cost in CPU as well, and with `--cpu` holds a cpu-bound pool at the workers that fill the cores. It keeps watching, because workloads change. When every pool wants more and the budget is gone, it says the host is out of capacity rather than pretending a configuration change would help.

In advisory mode it does all of that and changes nothing: the plan is printed, published as metrics, and written to a recommendation file. In apply mode it writes `zz-fpm-tune.conf` (and, when it turns a status page on, `zz-fpm-tune-status.conf`) into the directory the master includes, and reloads php-fpm. It never changes which process manager a pool runs.

## Start here

Follow the [Quickstart](quickstart.md): install, read a plan, run it advisory for a day, then let it apply. On Laravel Forge or Ploi, the [Forge and Ploi](cookbook/forge-and-ploi.md) recipe is the same path with those hosts' details filled in.

fpm-tune is beta. Run it in advisory mode on a real host and read what it recommends before you let it write.

When it does write, the change is validated against a copy of the configuration, written atomically, reloaded with SIGUSR2 rather than a restart, and rolled back if the master does not come back; see [How it fails safe](safety/how-it-fails-safe.md).

## Going deeper

- [How it decides](how-it-decides/_index.md): the budget, the measurement, the division, and when it holds still.
- [Operating](operating/_index.md): as a daemon, applying once, metrics, recovering a host.
- [Configuration](configuration/_index.md): every command, flag, key and path.
- [Safety](safety/_index.md): how it fails safe, and what it trusts on a shared host.

Built on [phpfpm](https://github.com/cboxdk/phpfpm), the library that discovers, scrapes and reloads php-fpm.
