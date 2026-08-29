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
# An assertion, not a note. This printed either way, which made it the only
# check for "a pool added mid-flight is handled" and no check at all.
#
# The outcome TABLE is the right thing to look at: a pool appears there whether
# or not it has enough history to be overridden, so this is stable while the
# stronger claim — that it is written — is not.
grep -q "news" "$ROOT/a2" \
  || fail "a pool added while the tool was running does not appear in its output at all:
$(cat "$ROOT/a2")"
grep -q "news" "$POOLS/zz-fpm-tune.conf" \
  || echo "  (news is accounted for but not overridden yet; it has no demand history)"

# ---------------------------------------------------------------------------
echo "--- a pool is REMOVED, and php-fpm is reloaded before the tool notices"
#
# The P0. An operator removing a site reloads php-fpm as part of doing so, so
# this ordering is the likely one rather than an exotic one.
# Guarded, because appending to a file that is not there FABRICATES one without
# this tool's marker — and the suite then fails on the repair correctly refusing
# to touch a foreign file, which is a confusing red for the wrong reason.
[ -f "$POOLS/zz-fpm-tune.conf" ] \
  || fail "this tool has written nothing, so there is no override for a removed pool to break"

if ! grep -q "^\[news\]" "$POOLS/zz-fpm-tune.conf"; then
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

# ---------------------------------------------------------------------------
echo "--- a previous run died between writing and being sure the master took it"
#
# The recovery record is the thing that makes a crash survivable, and neither
# suite ever created one — so making writeTransaction a no-op, or making the
# put-back on a rejected leftover do nothing, left both of them green.
#
# The record is hand-written rather than produced by killing a run mid-flight,
# because the window is milliseconds wide and a test that has to hit it is a
# test that fails on a loaded CI machine for no reason. What matters is the
# state on disk, and that is exactly reproducible.
[ -f "$POOLS/zz-fpm-tune.conf" ] || fail "nothing is overridden, so there is nothing to recover"

# A record naming a file this tool did NOT leave: the hash will not match, which
# is the "the rename never happened" case — nothing to undo, and the live file
# must be left exactly as it is.
#
# existed=false because the record's own validation refuses existed=true with no
# saved copy, and rightly: that combination describes a backup that should be
# there and is not, which is a different situation from this one.
KEY="$(printf '%s' "$POOLS" | shasum -a 256 2>/dev/null | cut -c1-8 || printf '%s' "$POOLS" | sha256sum | cut -c1-8)"
mkdir -p "$ROOT/backup"
BEFORE="$(cat "$POOLS/zz-fpm-tune.conf")"
cat > "$ROOT/backup/$KEY-transaction.json" <<EOF
{"drop_in_dir":"$POOLS","binary":"$FPM","config_path":"$ROOT/php-fpm.conf",
 "path":"$POOLS/zz-fpm-tune.conf","existed":false,
 "wrote":"0000000000000000000000000000000000000000000000000000000000000000",
 "phase":"written"}
EOF

"$BIN" apply "${SCOPE[@]}" --backup-dir "$ROOT/backup" > "$ROOT/a8" 2>&1 \
  || fail "a run that found a stale record could not proceed: $(cat "$ROOT/a8")"

[ -f "$POOLS/zz-fpm-tune.conf" ] \
  || fail "recovery removed this tool's file over a record that does not describe it"
"$FPM" -t --fpm-config "$ROOT/php-fpm.conf" 2>/dev/null \
  || fail "the configuration is rejected after recovery ran"
kill -0 "$(cat "$ROOT/fpm.pid")" || fail "the master did not survive recovery"
[ -f "$ROOT/backup/$KEY-transaction.json" ] \
  && fail "the record was left behind, so every future run will reconcile it again"

echo "  confirmed: a record that does not describe the file on disk is resolved and cleared"

# And the other half: a record that DOES describe the file, where php-fpm
# rejects it. That is the crash this whole mechanism exists for — a run that
# wrote something bad and died before it could take it back out — and it is the
# only path that reaches the restore. Without it, making the restore a no-op
# left both suites green.
GOOD="$(cat "$POOLS/zz-fpm-tune.conf")"
printf '%s\n[gone-with-the-site]\npm.max_children = 4\n' "$GOOD" > "$POOLS/zz-fpm-tune.conf"

if "$FPM" -t --fpm-config "$ROOT/php-fpm.conf" >/dev/null 2>&1; then
  echo "  (this php-fpm accepts an override for a pool that does not exist; skipping the restore probe)"
  printf '%s' "$GOOD" > "$POOLS/zz-fpm-tune.conf"
else
  printf '%s' "$GOOD" > "$ROOT/backup/$KEY-zz-fpm-tune.conf.bak"
  HASH="$(shasum -a 256 "$POOLS/zz-fpm-tune.conf" 2>/dev/null | cut -d' ' -f1 \
        || sha256sum "$POOLS/zz-fpm-tune.conf" | cut -d' ' -f1)"
  cat > "$ROOT/backup/$KEY-transaction.json" <<EOF
{"drop_in_dir":"$POOLS","binary":"$FPM","config_path":"$ROOT/php-fpm.conf",
 "path":"$POOLS/zz-fpm-tune.conf","existed":true,
 "saved":"$KEY-zz-fpm-tune.conf.bak",
 "wrote":"$HASH","phase":"written"}
EOF

  "$BIN" apply "${SCOPE[@]}" --backup-dir "$ROOT/backup" > "$ROOT/a9" 2>&1 || true

  "$FPM" -t --fpm-config "$ROOT/php-fpm.conf" 2>/dev/null \
    || fail "the configuration a dead run left is still rejected; recovery did not take it back out:
$(cat "$ROOT/a9")"
  grep -q "gone-with-the-site" "$POOLS/zz-fpm-tune.conf" \
    && fail "the override for a pool that does not exist is still there"
  kill -0 "$(cat "$ROOT/fpm.pid")" || fail "the master did not survive recovery"

  echo "  confirmed: a rejected leftover is taken back out and the previous version restored"
fi

# ---------------------------------------------------------------------------
echo "--- the state file is lost"
#
# What this proves: losing the learned state does not break the host, lose this
# tool's file, or leave a configuration php-fpm rejects.
#
# What it does NOT prove, deliberately: that the individual overrides survive.
# That fault only shows when SOME pools change and others do not, and a harness
# with no load cannot arrange that split reliably — every pool moves together
# here, so ownership never has to be consulted. It is covered exactly, with a
# mutation check, by TestOwnershipSurvivesLosingTheStateFile.
[ -f "$POOLS/zz-fpm-tune.conf" ] || fail "nothing is overridden, so this proves nothing"

rm -f "$STATE/state.json"

"$BIN" apply "${SCOPE[@]}" > "$ROOT/a7" 2>&1 || fail "apply after losing state: $(cat "$ROOT/a7")"

[ -f "$POOLS/zz-fpm-tune.conf" ] || fail "losing the state file took this tool's file with it"
"$FPM" -t --fpm-config "$ROOT/php-fpm.conf" 2>/dev/null \
  || fail "the configuration is rejected after losing state"
kill -0 "$(cat "$ROOT/fpm.pid")" || fail "the master did not survive losing the state file"

echo
echo "chaos: all scenarios survived, against php-fpm $("$FPM" -v | head -1)"
