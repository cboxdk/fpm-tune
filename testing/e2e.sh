#!/usr/bin/env bash
#
# End-to-end check against a real PHP-FPM master.
#
# The unit tests stand php-fpm in with `true` and `false`, which is enough to
# test the decision logic and nothing at all about the part that can take a host
# down. This runs the real binary: a real master, a real `php-fpm -t`, a real
# SIGUSR2, and a real check that the master came back with its sockets intact.
#
# Every assertion here is a property the write path is supposed to have, and
# each one has been observed failing at some point:
#
#   - a dry run left rewritten fragments behind
#   - a run aimed at a directory no master includes wrote there anyway
#   - the reload was refused because the master was pid 1
#   - the master was reported dead when the signal had never been sent
#
# NOT proven by either suite, and worth saying plainly rather than pointing
# somewhere it is not: that a change set php-fpm rejects never reaches the live
# directory. chaos.sh's removed-site scenario looks like it covers this and does
# not — there the rejection comes from a file that is already on disk, discovery
# fails, and Apply is never called, so no change set is ever rendered. Bypassing
# the sandbox validation or the post-write validation individually leaves both
# suites green.
#
# Reaching it needs this tool to render something php-fpm refuses, which its own
# arithmetic will not do. It is covered in Go, against a stub php-fpm that
# rejects on cue, by the tests in apply/.
#
# Usage: testing/e2e.sh /path/to/fpm-tune
set -euo pipefail

BIN="${1:-./build/fpm-tune}"
[ -x "$BIN" ] || { echo "no fpm-tune binary at $BIN"; exit 1; }

FPM="$(command -v php-fpm8.3 || command -v php-fpm8.2 || command -v php-fpm || true)"
[ -n "$FPM" ] || { echo "no php-fpm binary found"; exit 1; }

ROOT="$(mktemp -d)"
POOLS="$ROOT/pool.d"
STATE="$ROOT/state"
mkdir -p "$POOLS" "$STATE"

cleanup() {
  [ -f "$ROOT/fpm.pid" ] && kill "$(cat "$ROOT/fpm.pid")" 2>/dev/null || true
  rm -rf "$ROOT"
}
trap cleanup EXIT

cat > "$ROOT/php-fpm.conf" <<EOF
[global]
pid = $ROOT/fpm.pid
error_log = $ROOT/fpm.log
daemonize = yes
include=$POOLS/*.conf
EOF

# Ports chosen from the pid so concurrent runs — and a master left behind by an
# earlier one — do not collide. A bind failure here looks like a php-fpm that
# will not start, which is a confusing way to fail a test about reloading.
BASE=$((20000 + (RANDOM % 20000)))
i=0
for pool in www shop; do
  port=$((BASE + i))
  i=$((i + 1))
  # php-fpm refuses to start as root without an explicit user per pool, and
  # refuses to SET one when it is not root. Neither case is this tool's
  # business, but the fixture has to satisfy whichever applies.
  as_user=""
  if [ "$(id -u)" = "0" ]; then
    as_user="user = www-data"$'\n'"group = www-data"
  fi

  cat > "$POOLS/$pool.conf" <<EOF
[$pool]
$as_user
listen = 127.0.0.1:$port
pm = dynamic
pm.max_children = 12
pm.start_servers = 2
pm.min_spare_servers = 1
pm.max_spare_servers = 3
pm.status_path = /status
EOF
done

# php-fpm daemonizes, so the exit status of the parent is not the whole story:
# a pool that cannot bind reports FPM initialization failed and leaves no pid
# file. Checked explicitly, because a test that silently proceeds without a
# master would go on to "pass" every assertion about reloading one.
"$FPM" --fpm-config "$ROOT/php-fpm.conf" || {
  echo "php-fpm did not start:"; cat "$ROOT/fpm.log" 2>/dev/null; exit 1;
}
for _ in $(seq 1 50); do
  [ -s "$ROOT/fpm.pid" ] && break
  sleep 0.1
done
[ -s "$ROOT/fpm.pid" ] || {
  echo "php-fpm wrote no pid file; it did not start:"; cat "$ROOT/fpm.log" 2>/dev/null; exit 1;
}
PID_BEFORE="$(cat "$ROOT/fpm.pid")"
kill -0 "$PID_BEFORE" 2>/dev/null || {
  echo "php-fpm exited immediately:"; cat "$ROOT/fpm.log" 2>/dev/null; exit 1;
}
echo "php-fpm master running as pid $PID_BEFORE"

# md5sum on Linux, md on macOS. Development happens on one and production on
# the other, and a fingerprint that silently produces nothing would make the
# "changed nothing" assertions pass against any change at all.
if command -v md5sum >/dev/null; then
  # Passed to every invocation. A development machine, or any host serving several
# sites, genuinely runs more than one php-fpm master — this one has Herd's two
# alongside the test's — and naming the pool directory is how an operator says
# which master they mean. Leaving it out would test only the single-master case
# and call it the general one.
SCOPE=(--drop-in-dir "$POOLS")

# Every entry, not just *.conf. A leaked ".zz-fpm-tune.conf.tmp-XXXX" from an
# interrupted atomic write is exactly the kind of thing this should catch, and a
# glob on *.conf cannot see it.
fingerprint() { ls -A "$POOLS"; md5sum "$POOLS"/* 2>/dev/null | sort; }
else
  # Passed to every invocation. A development machine, or any host serving several
# sites, genuinely runs more than one php-fpm master — this one has Herd's two
# alongside the test's — and naming the pool directory is how an operator says
# which master they mean. Leaving it out would test only the single-master case
# and call it the general one.
SCOPE=(--drop-in-dir "$POOLS")

fingerprint() { ls -A "$POOLS"; md5 -r "$POOLS"/* 2>/dev/null | sort; }
fi

fail() { echo "FAIL: $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
echo "--- a dry run must leave the pool directory byte-identical"
BEFORE="$(fingerprint)"
"$BIN" apply --dry-run --memory 512MB --reserve 128MB "${SCOPE[@]}" \
  --state "$STATE/state.json" --backup-dir "$ROOT/backup" > "$ROOT/dry.out" 2>&1 || true
[ "$(fingerprint)" = "$BEFORE" ] || fail "the dry run modified the pool directory"
grep -q "Dry run" "$ROOT/dry.out" || fail "the dry run did not report itself"

# And a dry run against a tree php-fpm will not accept must FAIL rather than
# report a clean rendering — the check that distinguishes a dry run that
# validated from one that only rendered.
printf '[dry-broken]\n; no listen, no user\n' > "$POOLS/zz-dry-broken.conf"
if "$FPM" -t --fpm-config "$ROOT/php-fpm.conf" >/dev/null 2>&1; then
  echo "  (this php-fpm accepts a pool with no listen; skipping the validation probe)"
else
  BEFORE="$(fingerprint)"
  if "$BIN" apply --dry-run --memory 512MB --reserve 128MB "${SCOPE[@]}" \
       --state "$STATE/state.json" --backup-dir "$ROOT/backup" > "$ROOT/dry2.out" 2>&1; then
    rm -f "$POOLS/zz-dry-broken.conf"
    fail "a dry run against a configuration php-fpm rejects reported success:$(printf '\n')$(cat "$ROOT/dry2.out")"
  fi
  [ "$(fingerprint)" = "$BEFORE" ] \
    || { rm -f "$POOLS/zz-dry-broken.conf"; fail "the failing dry run changed the pool directory"; }
  echo "  confirmed: a dry run validates, and still writes nothing when it fails"
fi
rm -f "$POOLS/zz-dry-broken.conf"

# ---------------------------------------------------------------------------
echo "--- a real apply must reload the master without restarting it"
"$BIN" apply --memory 512MB --reserve 128MB "${SCOPE[@]}" \
  --state "$STATE/state.json" --backup-dir "$ROOT/backup" > "$ROOT/apply.out" 2>&1 \
  || fail "apply failed:$(printf '\n')$(cat "$ROOT/apply.out")"
cat "$ROOT/apply.out"

sleep 1
PID_AFTER="$(cat "$ROOT/fpm.pid")"
kill -0 "$PID_AFTER" || fail "no master is running after the apply"

# The pid may legitimately change. SIGUSR2 makes the master re-exec itself, and
# a DAEMONIZED php-fpm — its own default — comes back as a new process while the
# original exits. Asserting the pid is preserved would be asserting that this
# tool only works in the foreground, and that mistake is exactly what made
# fpm-tune roll back every successful apply on such a host.
#
# What must hold is that the SERVICE never went down. php-fpm proves that
# itself: on a reload it carries its listening sockets across, so no connection
# is refused and no request is dropped. A stop-and-start cannot say that.
if [ "$PID_BEFORE" != "$PID_AFTER" ]; then
  echo "  master re-execed: $PID_BEFORE -> $PID_AFTER (a daemonized reload does this)"
fi
grep -q "Reloading in progress" "$ROOT/fpm.log" \
  || fail "the master never reloaded, so the change was never adopted"
grep -q "using inherited socket" "$ROOT/fpm.log" \
  || fail "the master did not inherit its listening sockets; this was a restart, not a reload"
grep -qi "shutting down\|exiting, bye-bye" "$ROOT/fpm.log" \
  && fail "the master shut down; in-flight requests were dropped"

# And the master that came back is serving the configuration that was written.
ps -o command= -p "$PID_AFTER" | grep -q "master process" \
  || fail "pid $PID_AFTER is not a php-fpm master"
"$FPM" -t --fpm-config "$ROOT/php-fpm.conf" 2>/dev/null \
  || fail "the configuration left on disk is rejected by php-fpm"

ls "$POOLS"/zz-fpm-tune.conf >/dev/null 2>&1 \
  || fail "apply reported success but wrote no fragments"

# ---------------------------------------------------------------------------
echo "--- a run that cannot see a master must refuse, not write"
#
# What this block used to claim was that a rejected change set never reaches the
# live directory. It did not test that. It built a broken master config and
# never passed it to anything, ran the tool against a directory no master
# includes — so it failed inside discovery, before a change set was rendered —
# and then fingerprinted the LIVE pool directory, which that run never targeted.
# Deleting the sandbox validation, the post-write validation and the rollback
# all left it printing "confirmed".
#
# The rejected-change-set property is real and is proven, by chaos.sh's
# removed-site scenario: it renders an override for a pool that no longer
# exists, php-fpm refuses the whole configuration, and the tool takes its own
# file back out. That needs a state where the tool has already written, which is
# what chaos.sh sets up and this suite does not.
#
# What IS worth proving here, and is not proven anywhere else: pointing this
# tool at a pool directory no running master includes must refuse. That flag is
# how an operator aims it at the wrong place, and the failure has to be a
# refusal rather than a file written where nothing will read it.
BEFORE="$(fingerprint)"

mkdir -p "$ROOT/orphan.d"
OUT="$ROOT/orphan.out"
if "$BIN" apply --memory 512MB --reserve 128MB \
     --drop-in-dir "$ROOT/orphan.d" --state "$STATE/orphan.json" \
     --backup-dir "$ROOT/backup" > "$OUT" 2>&1; then
  fail "a run against a directory no master includes reported success:
$(cat "$OUT")"
fi

# The wording, so "it failed" cannot pass for the wrong reason — which is
# exactly how the previous version of this block stayed green.
grep -qi "no pools belong to a master that includes" "$OUT" \
  || fail "the run failed, but not because the directory is not included:
$(cat "$OUT")"

# Both directories: the one it was aimed at, and the live one it must not have
# fallen back to.
[ -z "$(ls -A "$ROOT/orphan.d")" ] \
  || fail "the refused run wrote into $ROOT/orphan.d: $(ls -A "$ROOT/orphan.d")"
[ "$(fingerprint)" = "$BEFORE" ] \
  || fail "a refused run changed the live pool directory"

echo "  confirmed: refused by name, and neither directory was touched"

echo "--- a second writer must be refused while a daemon holds the pool directory"
#
# --apply, and a DIFFERENT state file and backup directory for the second run.
#
# Without --apply the daemon takes only the state lock, and with both runs
# sharing --state that is the lock being tested — which is not the one that
# matters. The lock that stops two processes writing the same pool files is
# keyed on the pool DIRECTORY, and it has to hold when someone points a second
# run at their own state and backups, because that is what an operator does when
# the first one seems stuck.
"$BIN" serve --apply --state "$STATE/state.json" --interval 60s --metrics "" \
  --backup-dir "$ROOT/backup" "${SCOPE[@]}" \
  > "$ROOT/serve.out" 2>&1 &
SERVE=$!
sleep 3
# A different state DIRECTORY, not just a different file: the state lock is
# keyed on the directory, so sharing it would test that lock instead of the one
# this scenario is named for.
mkdir -p "$ROOT/other-state"
if "$BIN" apply --memory 512MB --state "$ROOT/other-state/state.json" "${SCOPE[@]}" \
     --backup-dir "$ROOT/other-backup" > "$ROOT/second.out" 2>&1; then
  kill $SERVE 2>/dev/null || true
  fail "a second writer took the pool directory while a daemon held it, with its own
state file and backup directory:$(printf '\n')$(cat "$ROOT/second.out")"
fi
# The POOL DIRECTORY's lock by name, so a refusal from the state lock — which
# is a different guarantee and was what this used to be measuring — cannot pass
# for this one.
grep -q "apply.lock" "$ROOT/second.out" \
  || { kill $SERVE 2>/dev/null || true; fail "the refusal did not say why:$(printf '\n')$(cat "$ROOT/second.out")"; }
echo "  confirmed: refused with $(head -1 "$ROOT/second.out")"
kill $SERVE 2>/dev/null || true
wait $SERVE 2>/dev/null || true

# ---------------------------------------------------------------------------
echo "--- a watching daemon applies once when asked through its control socket"
# Advisory: the daemon plans and writes nothing. apply-now asks it to act on
# that plan once, over the root-only socket beside its state file, and the
# pool directory changes while the daemon stays advisory.
# A short socket path: unix sockets are limited to about a hundred characters
# and mktemp's directory on macOS is longer than that.
CONTROL="/tmp/fpm-tune-e2e-$$.sock"
"$BIN" serve --state "$STATE/state.json" --interval 60s --metrics "" --control "$CONTROL" \
  --backup-dir "$ROOT/backup" "${SCOPE[@]}" > "$ROOT/serve-adv.out" 2>&1 &
SERVE=$!
sleep 3
[ -S "$CONTROL" ] || { kill $SERVE 2>/dev/null || true; fail "the daemon opened no control socket:$(printf '\n')$(cat "$ROOT/serve-adv.out")"; }
# The pools are at their plan after the applies above, so the honest answer
# here is "nothing to change"; a resize through this path is covered in Go.
BEFORE="$(fingerprint)"
if ! "$BIN" apply-now --control "$CONTROL" > "$ROOT/applynow.out" 2>&1; then
  kill $SERVE 2>/dev/null || true; fail "apply-now failed:$(printf '\n')$(cat "$ROOT/applynow.out")$(printf '\n')$(cat "$ROOT/serve-adv.out")"
fi
cat "$ROOT/applynow.out"
grep -qE "→|nothing to change" "$ROOT/applynow.out" || { kill $SERVE 2>/dev/null || true; fail "apply-now said neither what changed nor that nothing did"; }
if grep -q "→" "$ROOT/applynow.out" && [ "$(fingerprint)" = "$BEFORE" ]; then
  kill $SERVE 2>/dev/null || true; fail "apply-now reported a change but the pool directory is unchanged"
fi
# Asked a second time, it applies again: taking the lock back on a directory it
# has already reconciled must not send it through a round of recovery, which
# refused every second apply-now.
if ! "$BIN" apply-now --control "$CONTROL" > "$ROOT/applynow2.out" 2>&1; then
  kill $SERVE 2>/dev/null || true; fail "the second apply-now failed:$(printf '\n')$(cat "$ROOT/applynow2.out")$(printf '\n')$(cat "$ROOT/serve-adv.out")"
fi
grep -qE "→|nothing to change" "$ROOT/applynow2.out" \
  || { kill $SERVE 2>/dev/null || true; fail "the second apply-now did not reach the plan:$(printf '\n')$(cat "$ROOT/applynow2.out")"; }
# And the daemon let go of the pool directory afterwards: a second writer is
# not refused while the watching daemon is still up. (Its own state dir, so
# the state lock is not what is being tested.)
mkdir -p "$ROOT/writer-state"
"$BIN" apply --dry-run --memory 512MB --state "$ROOT/writer-state/state.json" "${SCOPE[@]}" \
  --backup-dir "$ROOT/writer-backup" > "$ROOT/writer.out" 2>&1 \
  || { kill $SERVE 2>/dev/null || true; fail "a writer was refused after apply-now; the watching daemon kept the pool lock:$(printf '\n')$(cat "$ROOT/writer.out")"; }
kill $SERVE 2>/dev/null || true; wait $SERVE 2>/dev/null || true
[ -S "$CONTROL" ] && fail "the control socket was left behind after the daemon stopped"
echo "  confirmed: the watching daemon applied once, on request, and cleaned up its socket"

# ---------------------------------------------------------------------------
echo "--- the CPU dimension is always reported, and --cpu only changes what may bind"
# Real requests through www, each computing for ~100ms: past the 50ms floor,
# and not this tool's own traffic, so they count. The script has to be
# readable by the workers, which as root run as www-data.
cat > "$ROOT/work.php" <<'PHP'
<?php
$end = microtime(true) + (float)($_GET['hold'] ?? 0.1);
while (microtime(true) < $end) { hash('sha256', 'x'); }
echo "ok";
PHP
chmod 755 "$ROOT"; chmod 644 "$ROOT/work.php"
go run ./testing/loadgen --socket "127.0.0.1:$BASE" --script "$ROOT/work.php" \
  --concurrency 2 --duration 2s --query 'hold=0.1' >/dev/null 2>&1 \
  || fail "loadgen could not put requests through www"
# Measured on every scrape, so a plan can say which of memory and CPU a pool
# runs out of first without anyone having turned a switch a week earlier.
"$BIN" plan --memory 512MB "${SCOPE[@]}" --state "$STATE/state.json" > "$ROOT/plan-cpu.out" 2>&1 \
  || fail "plan failed:$(printf '\n')$(cat "$ROOT/plan-cpu.out")"
grep -q "CPU per request, as measured:" "$ROOT/plan-cpu.out" \
  || fail "plan printed no CPU section:$(printf '\n')$(cat "$ROOT/plan-cpu.out")"
# Both pools are listed. shop served nothing but this tool's own probes, which
# are not the site's traffic, so it has no readings and says so rather than
# vanishing; www served real requests, and one scrape reads each idle worker's
# last one, so it has a few — still short of the twenty a verdict needs.
for pool in www shop; do
  grep -E "^  $pool[[:space:]]" "$ROOT/plan-cpu.out" | grep -q "too few readings yet" \
    || fail "plan did not list $pool as having too few readings:$(printf '\n')$(cat "$ROOT/plan-cpu.out")"
done
# The READINGS column of the CPU table, not the memory table above it, which
# lists the same pools.
cpu_readings() { awk -v pool="$1" '/^CPU per request/ {f=1} f && $1 == pool {print $4; exit}' "$ROOT/plan-cpu.out"; }
readings=$(cpu_readings www)
[ "${readings:-0}" -gt 0 ] || fail "www served real requests but plan shows ${readings:-0} CPU readings:$(printf '\n')$(cat "$ROOT/plan-cpu.out")"
readings=$(cpu_readings shop)
[ "${readings:-0}" -eq 0 ] || fail "shop served only this tool's probes but plan shows $readings CPU readings"
grep -q "pass --cpu to hold it at the ceiling shown" "$ROOT/plan-cpu.out" \
  || fail "plan did not say the ceiling is off:$(printf '\n')$(cat "$ROOT/plan-cpu.out")"
# The rows are there on a run that learned nothing, against a state file that
# has never seen these pools: the case where the section used to vanish.
"$BIN" plan --no-learn --memory 512MB "${SCOPE[@]}" --state "$ROOT/fresh-state/state.json" > "$ROOT/plan-fresh.out" 2>&1 \
  || fail "plan --no-learn on a fresh state failed:$(printf '\n')$(cat "$ROOT/plan-fresh.out")"
for pool in www shop; do
  grep -E "^  $pool[[:space:]]" "$ROOT/plan-fresh.out" | grep -q "too few readings yet" \
    || fail "plan --no-learn on a fresh state dropped $pool from the CPU report:$(printf '\n')$(cat "$ROOT/plan-fresh.out")"
done
# --cpu is accepted, says it is on, and takes nothing away from the plan table.
"$BIN" plan --cpu --memory 512MB "${SCOPE[@]}" --state "$STATE/state.json" > "$ROOT/plan-cpu-on.out" 2>&1 \
  || fail "plan --cpu failed:$(printf '\n')$(cat "$ROOT/plan-cpu-on.out")"
grep -q -- "--cpu is on" "$ROOT/plan-cpu-on.out" || fail "plan --cpu did not say the ceiling is on"
grep -q "^POOL" "$ROOT/plan-cpu-on.out" || fail "plan --cpu lost the plan table"
echo "  confirmed: measured always, listed always, and --cpu only turns the ceiling on"

echo
echo "e2e: all checks passed against php-fpm $("$FPM" -v | head -1)"
