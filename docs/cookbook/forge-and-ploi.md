---
title: Forge & Ploi
weight: 1
description: The full fpm-tune flow on a Laravel Forge or Ploi box, with a pool per site, MySQL sharing the RAM, and no cgroup cap holding php-fpm back.
---

# Forge & Ploi

Forge and Ploi boxes are the case fpm-tune was built for. A handful of sites, each
with its own PHP-FPM pool, all sharing one Ubuntu VM, and MySQL (and often Redis)
sitting on that same box, eating into the RAM php-fpm thinks it owns. There's no
container limit holding php-fpm back, so a bad `pm.max_children` on one busy site can
take the whole machine down with it.

Here's the whole flow, start to finish. It's the same on both. They lay php-fpm out
the same way (`php8.x-fpm`, a pool file per site under `pool.d/`).

## 1. Install it

SSH in as your deploy user and grab the binary:

```sh
curl -fsSL https://raw.githubusercontent.com/cboxdk/fpm-tune/main/install.sh | sh
```

Everything below wants root, because it reads php-fpm's config and (later) reloads
the master, so run the commands with `sudo`.

## 2. Turn on the status page

Try a plan first:

```sh
sudo fpm-tune plan
```

On a fresh Forge or Ploi box this will tell you it found your pools but has nothing
to measure. The stock pool files ship with `pm.status_path` commented out, and
that's the page fpm-tune reads to learn what a worker costs. Turn it on:

```sh
sudo fpm-tune enable-status
```

This doesn't touch your pool files. It writes a separate `zz-fpm-tune-status.conf`
drop-in, validates the whole config with `php-fpm -t` before anything moves, reloads
gracefully, and rolls back if the master doesn't come back. Now plan again:

```sh
sudo fpm-tune plan
```

This time you get the real thing: per-pool worker cost and what it'd set each
`pm.max_children` to. **Changes nothing.** Read it. Does the ranking match what you
know about these sites? The shop being the heavy one, the docs site being cheap?

## 3. Let it watch for a while

Don't jump to apply. Install it as a background service. It starts in advisory mode,
so it watches and recommends but writes nothing:

```sh
sudo fpm-tune install-service
```

Then watch it think:

```sh
journalctl -u fpm-tune -f
```

Leave that for a day, or a week. It logs a heartbeat so you can see it's alive, and a
fresh line whenever its recommendation for a pool actually moves. What you're waiting
to see is that its numbers settle and stop surprising you: a real traffic peak, a
deploy, a quiet Sunday, all folded in. That's the beta talking: trust it on *your*
workload before you let it act.

## 4. Let it act

Once the recommendations look right, flip the switch:

```sh
sudo fpm-tune mode apply
```

Same service, no reinstall. It just restarts in apply mode. From here it writes each
pool's `pm.max_children` and reloads php-fpm gracefully when the right size drifts far
enough from the current one to be worth a reload. Back to watch-only any time:

```sh
sudo fpm-tune mode advisory
```

## 5. The MySQL-on-the-box catch

This is the part that bites on a shared Forge/Ploi box. php-fpm doesn't have the
machine to itself. MySQL might be holding 3-4 GB, Redis another few hundred meg. If
fpm-tune sized pools against *all* the RAM, it'd hand php-fpm memory MySQL is already
using, and the OOM killer would settle the argument.

It doesn't. On a bare VM it reads `/proc/meminfo` and leaves what the other services
are *actually* using out of php-fpm's budget, on top of a headroom margin, so the
whole host stays under target, not just php-fpm. You'll see it in the plan:

```
budget:    7.6 GiB total
           4.2 GiB used by other services
           512 MiB headroom kept
```

That's it being a good neighbour on its own. But "what MySQL is using right now" isn't
a promise about next Tuesday. If you want a *hard* wall (MySQL can never be starved
by php-fpm, full stop), cap php-fpm's memory at the OS level so the kernel enforces it
no matter what fpm-tune thinks:

```sh
sudo systemctl set-property php8.4-fpm.service MemoryMax=4G
```

(Match the version to your box: `php8.3-fpm`, `php8.4-fpm`, whatever Forge/Ploi
installed.) That takes effect live, no restart, and survives reboots. fpm-tune then
sizes pools *inside* that ceiling, and you've got a guarantee instead of a courtesy.
The reasoning behind both is in [the budget](../how-it-decides/the-budget.md).

## When it says the host is full

If a pool is queueing and fpm-tune can't fix it (no free budget, nothing to borrow
from an idle neighbour), it tells you the host is **out of capacity** and stops. That
isn't a bug to tune around. On a Forge/Ploi box it usually means one honest question:
is it time for a bigger droplet, or does a heavy site need to move off? See
[alerting](../operating/alerting.md) for the signal to watch.
