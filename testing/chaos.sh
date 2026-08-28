#!/usr/bin/env bash
#
# The perturbations a real host experiences, against a real PHP-FPM master.
#
# e2e.sh checks the happy path and the guards around it. This checks what happens
# when the world moves underneath the tool — and it exists because a soak on a VM
# found a P0 that every static review, every unit test and every container test
# had missed:
#
#   a site was removed, php-fpm was reloaded (which is the same action), and this
#   tool's file still declared that pool. A pool defined only here has no listen
#   and no user, so php-fpm refused to start at all. The master died and stayed
#   dead, with fpm-tune running alongside it, having caused it.
#
# Every scenario below has broken this tool at some point.
#
# Usage: testing/chaos.sh /path/to/fpm-tune
set -uo pipefail

BIN="${1:-./build/fpm-tune}"
[ -x "$BIN" ] || { echo "no fpm-tune binary at $BIN"; exit 1; }

FPM="$(command -v php-fpm8.5 || command -v php-fpm8.4 || command -v php-fpm8.3 || command -v php-fpm || true)"
[ -n "$FPM" ] || { echo "no php-fpm binary found"; exit 1; }

ROOT="$(mktemp -d)"
POOLS="$ROOT/pool.d"
STATE="$ROOT/state"
mkdir -p "$POOLS" "$STATE"

cleanup() {
  [ -f "$ROOT/fpm.pid" ] && kill "$(cat "$ROOT/fpm.pid")" 2>/dev/null
  rm -rf "$ROOT"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

cat > "$ROOT/php-fpm.conf" <<EOF
[global]
pid = $ROOT/fpm.pid
error_log = $ROOT/fpm.log
daemonize = yes
include=$POOLS/*.conf
EOF

BASE=$((25000 + (RANDOM % 15000)))

as_user=""
if [ "$(id -u)" = "0" ]; then
  as_user="user = www-data"$'\n'"group = www-data"
fi

make_pool() { # name port children
  cat > "$POOLS/$1.conf" <<EOF
[$1]
$as_user
listen = 127.0.0.1:$2
pm = dynamic
pm.max_children = $3
pm.start_servers = 2
pm.min_spare_servers = 1
pm.max_spare_servers = 2
pm.status_path = /status
EOF
}

start_fpm() {
  "$FPM" --fpm-config "$ROOT/php-fpm.conf" || return 1
  for _ in $(seq 1 50); do
    [ -s "$ROOT/fpm.pid" ] && return 0
    sleep 0.1
  done

  return 1
}

make_pool shop "$BASE" 8
make_pool blog "$((BASE + 1))" 8
make_pool docs "$((BASE + 2))" 8

start_fpm || { echo "php-fpm did not start:"; cat "$ROOT/fpm.log"; exit 1; }
echo "php-fpm running as $(cat "$ROOT/fpm.pid")"

# Flags follow the subcommand, so the shared ones are kept separately.
# A budget tight enough that every step genuinely proposes a change. With room
# to spare the tool correctly does nothing, and the scenarios below would then
# assert against a file that was never written.
SCOPE=(--drop-in-dir "$POOLS" --state "$STATE/state.json" --backup-dir "$ROOT/backup"
       --memory 400MB --reserve 100MB --min-interval 1s)

# ---------------------------------------------------------------------------
echo "--- a pool is added while the tool is managing the host"
"$BIN" apply "${SCOPE[@]}" > "$ROOT/a1" 2>&1 || fail "first apply: $(cat "$ROOT/a1")"

make_pool news "$((BASE + 3))" 8
kill -USR2 "$(cat "$ROOT/fpm.pid")"
sleep 1

"$BIN" apply "${SCOPE[@]}" > "$ROOT/a2" 2>&1 || fail "apply after a pool appeared: $(cat "$ROOT/a2")"
grep -q "news" "$POOLS/zz-fpm-tune.conf" || echo "  (news not overridden yet; it has no demand history)"

# ---------------------------------------------------------------------------
echo "--- a pool is REMOVED, and php-fpm is reloaded before the tool notices"
#
# The P0. An operator removing a site reloads php-fpm as part of doing so, so
# this ordering is the likely one rather than an exotic one.
if ! grep -q "^\[news\]" "$POOLS/zz-fpm-tune.conf" 2>/dev/null; then
  # Force the situation the bug needs: our file must name the pool being removed.
  printf '\n[news]\npm.max_children = 4\n' >> "$POOLS/zz-fpm-tune.conf"
fi

rm -f "$POOLS/news.conf"

if "$FPM" -t --fpm-config "$ROOT/php-fpm.conf" 2>/dev/null; then
  fail "php-fpm accepts a pool defined only by an override; the scenario is not set up"
fi
echo "  confirmed: the configuration is now rejected"

# The master dies on the next reload from any source, exactly as it did on the VM.
kill -USR2 "$(cat "$ROOT/fpm.pid")" 2>/dev/null
sleep 2

"$BIN" apply "${SCOPE[@]}" > "$ROOT/a3" 2>&1
if ! "$FPM" -t --fpm-config "$ROOT/php-fpm.conf" 2>/dev/null; then
  fail "the tool did not repair a configuration its own file was breaking:$(printf '\n')$(cat "$ROOT/a3")"
fi
echo "  repaired: the configuration validates again"

# ---------------------------------------------------------------------------
echo "--- the master can be started again, and is re-tuned"
rm -f "$ROOT/fpm.pid"
start_fpm || { echo "php-fpm will not start after the repair:"; cat "$ROOT/fpm.log"; exit 1; }

"$BIN" apply "${SCOPE[@]}" > "$ROOT/a4" 2>&1 || fail "apply after recovery: $(cat "$ROOT/a4")"
[ -f "$POOLS/zz-fpm-tune.conf" ] || fail "the tool wrote nothing after recovering"
grep -q "^\[news\]" "$POOLS/zz-fpm-tune.conf" 2>/dev/null \
  && fail "the override for the removed pool came back"
"$FPM" -t --fpm-config "$ROOT/php-fpm.conf" 2>/dev/null \
  || fail "the configuration is rejected after re-tuning"

# ---------------------------------------------------------------------------
echo "--- php-fpm is restarted underneath the tool"
kill "$(cat "$ROOT/fpm.pid")" 2>/dev/null
sleep 1
rm -f "$ROOT/fpm.pid"
start_fpm || fail "php-fpm did not restart"

"$BIN" apply "${SCOPE[@]}" > "$ROOT/a5" 2>&1 || fail "apply after a restart: $(cat "$ROOT/a5")"
kill -0 "$(cat "$ROOT/fpm.pid")" || fail "the master is not running"

# ---------------------------------------------------------------------------
echo "--- an unrelated part of the configuration is broken"
#
# The tool must NOT delete its own work for a breakage it did not cause.
[ -f "$POOLS/zz-fpm-tune.conf" ] \
  || fail "this tool has written nothing, so the check below would pass vacuously"

printf '[broken]\n; no listen, no user\n' > "$POOLS/zz-broken.conf"
BEFORE="$(cat "$POOLS/zz-fpm-tune.conf")"

"$BIN" apply "${SCOPE[@]}" > "$ROOT/a6" 2>&1

[ -f "$POOLS/zz-fpm-tune.conf" ] \
  || fail "the tool deleted its own configuration for a breakage it did not cause"
[ "$(cat "$POOLS/zz-fpm-tune.conf")" = "$BEFORE" ] \
  || fail "the tool rewrote its own configuration for a breakage elsewhere"
rm -f "$POOLS/zz-broken.conf"

echo
echo "chaos: all scenarios survived, against php-fpm $("$FPM" -v | head -1)"
