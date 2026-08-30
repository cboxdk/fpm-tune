---
title: The budget
weight: 1
description: Where the number it divides comes from, and why the machine's memory is the wrong answer on a VM.
---

# The budget

Everything downstream divides one number: how much memory there is for PHP-FPM
workers. Getting it wrong is the single worst thing this tool can do. Too large
and the host OOMs; too small and every site is throttled.

## It reads the master's cgroup, not the machine

The budget is read from the cgroup of the php-fpm master being managed — walking
up the hierarchy and taking the tightest limit, because a cap on any ancestor
binds everything below it.

That distinction is the difference between a container and a VM, and it is not a
detail:

- **Inside a container**, `/sys/fs/cgroup/memory.max` is the container's own
  limit. Reading it is exactly right.
- **On a VM**, that same path is the *machine*, and the machine is usually not
  limited. But php-fpm may well be capped — by a systemd `MemoryMax=3G` on its
  own slice. Sizing against the machine's 20GiB would grow the pools straight
  into a 3GiB ceiling they never see, and the host would look fine right up until
  the OOM killer arrives.

So the tool finds the master's pid, reads *its* cgroup path from
`/proc/<pid>/cgroup`, and walks the memory-limit files up the tree. On the VM,
that reaches the slice's `MemoryMax` rather than the machine's non-limit.

## Soft limits count too

`MemoryHigh=` is systemd's documented way to say "keep this service under N".
Above it the cgroup is not killed — it is throttled into aggressive reclaim,
which from outside looks like a host that has simply gone slow. The tool reads
`memory.high` alongside `memory.max` and takes the tighter of the two, because a
pool sized past the soft ceiling thrashes rather than serves.

## When it cannot read the limit, it refuses to write

"Found no limit" and "could not read the limit" produce the same fallback — the
machine's memory — and they are opposite situations. The second is the one that
sizes a 3GiB service against 32GiB.

So the two are distinguished. If the master's own limit *could not be read* — a
`/proc` mounted `hidepid=2` while php-fpm runs as root and the tool does not, a
hardened host, or the plain race of the master restarting during the scrape — the
tool records that, and **refuses to write** rather than sizing against a budget
nobody confirmed. The message names the file it could not read, and tells you to
either make it readable or pass `--memory` with the real number.

A host with genuinely no cgroup limit anywhere — a bare VM, or a platform with no
cgroups at all — is not a failed lookup. There the machine's memory is the honest
answer, and the tool uses it.

## Overriding it

`--memory 8G` replaces the detection entirely, for when php-fpm is not the only
tenant of its cgroup, or when the detection cannot see the real limit and you
know it. `--reserve 1G` sets how much to hold back from workers for the operating
system and everything else; without it, a sensible fraction (with a floor on
small hosts) is kept back automatically.

## Reading the budget line

Every plan and every recommendation states where the number came from:

```
host memory 4.0GiB, 12 CPU(s) (via php-fpm's cgroup)
```

- `via php-fpm's cgroup` — read from the master's own slice. Trust it.
- `via /proc/meminfo` — the machine's memory, because no cap was found. Correct
  on a bare VM or in a container that really is unlimited; a red flag if you
  expected php-fpm to be capped.
- A `WARNING` that the limit could not be read — the number is the machine's and
  the tool will not apply from it. Pass `--memory`.
