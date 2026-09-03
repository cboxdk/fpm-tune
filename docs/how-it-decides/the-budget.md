---
title: The budget
weight: 1
description: Where the memory it divides comes from, what it holds back, and how to give php-fpm a hard limit.
---

# The budget

Everything else in this section divides one number: the memory available to
PHP-FPM workers. This page is where that number comes from, and it is the page
to read when a plan's first lines look wrong.

## Where the number comes from

fpm-tune finds the php-fpm master, reads its cgroup path from
`/proc/<pid>/cgroup`, and walks up the cgroup tree taking the tightest limit it
finds. It reads `memory.high` as well as `memory.max`, because a service above
its `MemoryHigh=` is throttled into reclaim rather than killed, and a pool sized
past that line thrashes instead of serving.

Reading the master's own cgroup, rather than the machine, is what makes the same
code right in both places. Inside a container, `/sys/fs/cgroup/memory.max` is
the container's limit. On a VM that path is the machine, which is usually
unlimited, while php-fpm may be capped by a systemd `MemoryMax=` on its own
service. Sizing against the machine's 20 GiB would grow the pools into a 3 GiB
limit they never see.

A host with no limit anywhere (a bare VM, or a platform without cgroups) is
sized against the machine's memory, read from `/proc/meminfo`. The plan's first
line says which source it used:

```
host memory 7.5GiB, 4 CPU(s) (via /proc/meminfo)
```

With a cgroup limit the line begins `php-fpm's memory` and ends
`(via php-fpm's cgroup)`.

## When the limit cannot be read

"Found no limit" and "could not read the limit" fall back to the same number,
the machine's memory, and they are opposite situations. The second is the one
that sizes a 3 GiB service against 32 GiB: a `/proc` mounted `hidepid=2` while
php-fpm runs as root and fpm-tune does not, or the master restarting during the
scrape.

So the two are kept apart. When the master's own limit could not be read, the
plan prints a `WARNING:` under the first line saying so, and `apply`,
`serve --apply` and `apply-now` refuse to write from that budget. Make the file
readable, or pass `--memory` with the real number.

## The shared host

On the `/proc/meminfo` path php-fpm is rarely alone: MySQL, Redis and the OS
want their share, and there is no cgroup limit to exclude them. So the budget
reads `MemAvailable` and holds back `MemTotal - MemAvailable - php-fpm's own`
for them, on top of the reserve below. The host as a whole stays under the
target, and on a dedicated host, where almost everything is free, this holds
back nothing extra.

```
host memory 7.5GiB, 4 CPU(s) (via /proc/meminfo)
  used by other services:  3.0GiB (left for them; cap php-fpm's cgroup for a hard limit)
  reserve kept:            1.1GiB (15% of 7.5GiB)
  available to workers:    3.5GiB
```

This is what the neighbours use now. A MySQL still filling its buffer pool will
use more later, which is why the line says to cap php-fpm's cgroup. The
neighbour term is the safe default; the limit is the guarantee.

## The reserve

The reserve is what is held back from workers for the OS, the web server and
opcache's shared segment. The default is 15% of the budget (85% for workers),
with a floor of 256 MiB: on a host where 15% would be less than that, the floor
is kept instead, and on a host smaller than 256 MiB half of it is. The plan
prints the reserve and its reason on the `reserve kept:` line.

`--reserve` changes it. A percentage (`--reserve 20%`) sets the fraction and
keeps the neighbour term. A fixed amount (`--reserve 1G`) replaces the whole
reserve, neighbours included, so it is the total you are choosing. In the
service config the key is `reserve`.

## Giving php-fpm a hard limit

On a shared host the clean answer is a cgroup limit on php-fpm itself: neither
it nor its neighbours can then starve the other, and the budget stops moving
when MySQL does. On systemd (the unit is `php8.4-fpm` on Debian and Ubuntu,
including Forge and Ploi hosts, and `php-fpm` elsewhere):

```bash
sudo systemctl set-property php8.4-fpm.service MemoryMax=4G
```

This takes effect immediately and persists across reboots. The next plan reads
`via php-fpm's cgroup` and sizes to the 4 GiB.

## Overriding the detection

`--memory 8G` replaces the detected budget, for a php-fpm that shares its
cgroup with something else, or a host where the detection cannot see the real
limit and you can. In the service config it is `memory`.
