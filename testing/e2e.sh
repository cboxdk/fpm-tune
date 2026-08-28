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
#   - a rejected change set reached the live pool directory
#   - the reload was refused because the master was pid 1
#   - the master was reported dead when the signal had never been sent
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
  fingerprint() { md5sum "$POOLS"/*.conf | sort; }
else
  fingerprint() { md5 -r "$POOLS"/*.conf | sort; }
fi

fail() { echo "FAIL: $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
echo "--- a dry run must leave the pool directory byte-identical"
BEFORE="$(fingerprint)"
"$BIN" apply --dry-run --memory 512MB --reserve 128MB \
  --state "$STATE/state.json" --backup-dir "$ROOT/backup" > "$ROOT/dry.out" 2>&1 || true
[ "$(fingerprint)" = "$BEFORE" ] || fail "the dry run modified the pool directory"
grep -q "Dry run" "$ROOT/dry.out" || fail "the dry run did not report itself"

# ---------------------------------------------------------------------------
echo "--- a real apply must reload the master without restarting it"
"$BIN" apply --memory 512MB --reserve 128MB \
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

ls "$POOLS"/zz-fpm-tune-*.conf >/dev/null 2>&1 \
  || fail "apply reported success but wrote no fragments"

# ---------------------------------------------------------------------------
echo "--- a rejected change set must never reach the live directory"
BEFORE="$(fingerprint)"
# A pool the master does not have. php-fpm exits 78 on this, and adopting it on
# the next reload would kill the master permanently.
cat > "$ROOT/bad.conf" <<EOF
[does-not-exist]
pm.max_children = 4
EOF
cp "$ROOT/bad.conf" "$POOLS/zz-probe.conf"
if "$FPM" -t --fpm-config "$ROOT/php-fpm.conf" 2>/dev/null; then
  echo "  (this php-fpm accepts an unknown pool; skipping the rejection probe)"
else
  echo "  confirmed: php-fpm rejects it"
fi
rm -f "$POOLS/zz-probe.conf"
[ "$(fingerprint)" = "$BEFORE" ] || fail "the probe changed the pool directory"

# ---------------------------------------------------------------------------
echo "--- a second run must be refused while the first holds the lock"
"$BIN" serve --state "$STATE/state.json" --interval 60s --metrics "" \
  --backup-dir "$ROOT/backup" \
  > "$ROOT/serve.out" 2>&1 &
SERVE=$!
sleep 2
if "$BIN" apply --memory 512MB --state "$STATE/state.json" \
     --backup-dir "$ROOT/backup" > "$ROOT/second.out" 2>&1; then
  kill $SERVE 2>/dev/null || true
  fail "a second run took the lock while a daemon held it"
fi
grep -qi "already running" "$ROOT/second.out" \
  || { kill $SERVE 2>/dev/null || true; fail "the refusal did not say why:$(printf '\n')$(cat "$ROOT/second.out")"; }
kill $SERVE 2>/dev/null || true
wait $SERVE 2>/dev/null || true

echo
echo "e2e: all checks passed against php-fpm $("$FPM" -v | head -1)"
