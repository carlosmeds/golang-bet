#!/usr/bin/env bash
# Schema tests against a real PostgreSQL (needs psql and a role allowed to CREATE DATABASE).
#
#   WAGERING_TEST_ADMIN_URL=postgres://user:pass@localhost:5432/postgres ./run_tests.sh
#
# The admin URL must end with /<database> and carry no query string. Every run creates
# and drops its own scratch databases; nothing else on the server is touched.
#
# Covers: migration up/down/up (whole chain and step by step), SQL constraints and
# triggers (testdata/constraints.sql), and two-session concurrency behaviour.
set -euo pipefail

ADMIN_URL="${WAGERING_TEST_ADMIN_URL:-postgres://postgres:postgres@localhost:5432/postgres}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MIGRATIONS="$(cd "$HERE/../../../migrations" && pwd)"
BASE_URL="${ADMIN_URL%/*}"
SUFFIX="$(date +%s)_$$"
DBS=()

psqlx() { psql -X -q -v ON_ERROR_STOP=1 "$@"; }

cleanup() {
  for db in "${DBS[@]:-}"; do
    [ -n "$db" ] && psql -X -q "$ADMIN_URL" -c "DROP DATABASE IF EXISTS \"$db\" WITH (FORCE)" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

new_db() {
  local db="wagering_schema_test_${1}_${SUFFIX}"
  psqlx "$ADMIN_URL" -c "CREATE DATABASE \"$db\"" >/dev/null
  DBS+=("$db")
  echo "$db"
}

up()   { psqlx -1 "$BASE_URL/$1" -f "$MIGRATIONS/$2.up.sql"   >/dev/null; }
down() { psqlx -1 "$BASE_URL/$1" -f "$MIGRATIONS/$2.down.sql" >/dev/null; }

# Names of migrations in order (000001_wallets, ...).
mapfile -t NAMES < <(cd "$MIGRATIONS" && ls *.up.sql | sed 's/\.up\.sql$//' | sort)

objects() { # count of user objects left in the public schema
  psql -X -At "$BASE_URL/$1" -c "
    SELECT (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
             WHERE n.nspname = 'public' AND c.relkind IN ('r','v','m','S','i','c','f','p'))
         + (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
             WHERE n.nspname = 'public')
         + (SELECT count(*) FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
             WHERE n.nspname = 'public' AND t.typtype IN ('e','d','r','m'))"
}

echo "== migrations: up, full down, up again"
DB="$(new_db mig)"
for n in "${NAMES[@]}"; do up "$DB" "$n"; done
[ "$(objects "$DB")" != 0 ] || { echo "FAIL: up created nothing"; exit 1; }
for ((i=${#NAMES[@]}-1; i>=0; i--)); do down "$DB" "${NAMES[$i]}"; done
left="$(objects "$DB")"
[ "$left" = 0 ] || { echo "FAIL: $left objects left after full down"; exit 1; }
for n in "${NAMES[@]}"; do up "$DB" "$n"; done

echo "== migrations: stepping down to every version and back up"
for ((k=${#NAMES[@]}-1; k>=0; k--)); do
  for ((i=${#NAMES[@]}-1; i>=k; i--)); do down "$DB" "${NAMES[$i]}"; done
  for ((i=k; i<${#NAMES[@]}; i++)); do up "$DB" "${NAMES[$i]}"; done
done

echo "== migrations: down is safe with data present"
DB="$(new_db data)"
for n in "${NAMES[@]}"; do up "$DB" "$n"; done
psqlx "$BASE_URL/$DB" -o /dev/null -f "$HERE/testdata/constraints.sql" 2>/dev/null
for ((i=${#NAMES[@]}-1; i>=0; i--)); do down "$DB" "${NAMES[$i]}"; done
[ "$(objects "$DB")" = 0 ] || { echo "FAIL: objects left after down with data"; exit 1; }

echo "== SQL constraints and triggers"
DB="$(new_db sql)"
for n in "${NAMES[@]}"; do up "$DB" "$n"; done
psqlx "$BASE_URL/$DB" -o /dev/null -f "$HERE/testdata/constraints.sql" 2>&1 | grep -v '^psql:.*NOTICE' || true
[ "${PIPESTATUS[0]}" = 0 ] || { echo "FAIL: constraint tests"; exit 1; }
psqlx "$BASE_URL/$DB" -At -c "SELECT 'rows: wallets=' || (SELECT count(*) FROM wallets) || ' transactions=' || (SELECT count(*) FROM wager_transactions) || ' ledger=' || (SELECT count(*) FROM ledger_entries)"

echo "== concurrency"
DB="$(new_db conc)"
URL="$BASE_URL/$DB"
for n in "${NAMES[@]}"; do up "$DB" "$n"; done
psqlx "$URL" <<'SQL' >/dev/null
BEGIN;
INSERT INTO wallets (id, player_id, currency, balance_amount) VALUES
  ('00000000-0000-0000-0000-00000000a001', 'c1', 'USD', 100),
  ('00000000-0000-0000-0000-00000000a002', 'c2', 'USD', 100);
INSERT INTO wager_transactions (id, origin, internal_key, wallet_id, player_id, kind, amount, currency,
  status, result_balance_amount, result_balance_currency, result_wallet_version, completed_at) VALUES
  ('00000000-0000-0000-0000-00000000b001', 'INTERNAL', 'opening:00000000-0000-0000-0000-00000000a001',
   '00000000-0000-0000-0000-00000000a001', 'c1', 'OPENING', 100, 'USD', 'PROCESSED', 100, 'USD', 1, now()),
  ('00000000-0000-0000-0000-00000000b002', 'INTERNAL', 'opening:00000000-0000-0000-0000-00000000a002',
   '00000000-0000-0000-0000-00000000a002', 'c2', 'OPENING', 100, 'USD', 'PROCESSED', 100, 'USD', 1, now());
INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency,
  balance_before, balance_after) VALUES
  (gen_random_uuid(), '00000000-0000-0000-0000-00000000a001', '00000000-0000-0000-0000-00000000b001', 1, 'CREDIT', 100, 'USD', 0, 100),
  (gen_random_uuid(), '00000000-0000-0000-0000-00000000a002', '00000000-0000-0000-0000-00000000b002', 1, 'CREDIT', 100, 'USD', 0, 100);
COMMIT;
SQL

# $1 wallet suffix, $2 tx suffix, $3 idempotency key, $4 sleep before commit. A worker that
# read balance 100 / version 1 without locking and writes absolute values (a lost update).
stale_bet() {
  psql -X -q -v ON_ERROR_STOP=1 "$URL" <<SQL
BEGIN;
UPDATE wallets SET balance_amount = 90, version = 2 WHERE id = '00000000-0000-0000-0000-00000000a$1';
INSERT INTO wager_transactions (id, origin, provider_id, external_transaction_id, idempotency_key, request_hash,
  wallet_id, player_id, kind, round_id, game_id, amount, currency, status, result_balance_amount,
  result_balance_currency, result_wallet_version, completed_at)
VALUES ('00000000-0000-0000-0000-00000000c$2', 'EXTERNAL', 'p', 'ext-$3', 'key-$3', repeat('a', 64),
  '00000000-0000-0000-0000-00000000a$1', 'c', 'BET', 'r', 'g', 10, 'USD', 'PROCESSED', 90, 'USD', 2, now());
INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency,
  balance_before, balance_after)
VALUES (gen_random_uuid(), '00000000-0000-0000-0000-00000000a$1', '00000000-0000-0000-0000-00000000c$2', 2, 'DEBIT', 10, 'USD', 100, 90);
SELECT pg_sleep($4);
COMMIT;
SQL
}

stale_bet 001 001 one 2 >/dev/null 2>"$HERE/.conc1.err" &
P1=$!
sleep 0.7
if stale_bet 001 002 two 0 >/dev/null 2>"$HERE/.conc2.err"; then
  wait "$P1" || true
  echo "FAIL: second stale writer on the same wallet committed (lost update)"; exit 1
fi
wait "$P1" || { echo "FAIL: first writer failed"; cat "$HERE/.conc1.err"; exit 1; }
grep -Eq 'wallet-version order|ledger_entries_wallet_version_key|must bump version' "$HERE/.conc2.err" \
  || { echo "FAIL: unexpected error for second writer"; cat "$HERE/.conc2.err"; exit 1; }
rm -f "$HERE/.conc1.err" "$HERE/.conc2.err"
[ "$(psqlx "$URL" -At -c "SELECT balance_amount || ':' || version FROM wallets WHERE id = '00000000-0000-0000-0000-00000000a001'")" = "90:2" ] \
  || { echo "FAIL: wallet 1 must show exactly one debit"; exit 1; }
echo "ok: stale concurrent writer on one wallet is rejected"

# Wallet 2 progresses while another session holds wallet 1 locked.
psql -X -q -v ON_ERROR_STOP=1 "$URL" >/dev/null <<'SQL' &
BEGIN;
SELECT id FROM wallets WHERE id = '00000000-0000-0000-0000-00000000a001' FOR UPDATE;
SELECT pg_sleep(3);
COMMIT;
SQL
P3=$!
sleep 0.7
psqlx "$URL" >/dev/null <<'SQL' || { echo "FAIL: independent wallet blocked by another wallet's lock"; exit 1; }
SET lock_timeout = '1s';
BEGIN;
SELECT id FROM wallets WHERE id = '00000000-0000-0000-0000-00000000a002' FOR UPDATE;
COMMIT;
SQL
echo "ok: independent wallet is not blocked"
if psqlx "$URL" >/dev/null 2>&1 <<'SQL'; then
SET lock_timeout = '1s';
BEGIN;
SELECT id FROM wallets WHERE id = '00000000-0000-0000-0000-00000000a001' FOR UPDATE;
COMMIT;
SQL
  echo "FAIL: same-wallet lock did not serialize"; exit 1
fi
echo "ok: same wallet is serialized by the row lock"
wait "$P3"

# Two publishers claiming with SKIP LOCKED never take the same event.
psqlx "$URL" >/dev/null <<'SQL'
INSERT INTO outbox_events (event_id, aggregate_type, aggregate_id, event_type, correlation_id, payload, occurred_at)
SELECT gen_random_uuid(), 'wallet', 'w', 'E', 'c', '{}', now() FROM generate_series(1, 4);
SQL
psql -X -q -At -v ON_ERROR_STOP=1 "$URL" >"$HERE/.claim1.out" <<'SQL' &
BEGIN;
SELECT event_id FROM outbox_events WHERE published_at IS NULL ORDER BY next_attempt_at, seq LIMIT 2 FOR UPDATE SKIP LOCKED;
SELECT pg_sleep(2);
COMMIT;
SQL
P4=$!
sleep 0.7
C2="$(psqlx "$URL" -At <<'SQL'
BEGIN;
SELECT event_id FROM outbox_events WHERE published_at IS NULL ORDER BY next_attempt_at, seq LIMIT 2 FOR UPDATE SKIP LOCKED;
COMMIT;
SQL
)"
wait "$P4"
C1="$(cat "$HERE/.claim1.out")"; rm -f "$HERE/.claim1.out"
[ "$(echo "$C1" | grep -c .)" = 2 ] && [ "$(echo "$C2" | grep -c .)" = 2 ] \
  || { echo "FAIL: each publisher must claim two events"; exit 1; }
[ -z "$(comm -12 <(echo "$C1" | sort) <(echo "$C2" | sort))" ] \
  || { echo "FAIL: publishers claimed the same event"; exit 1; }
echo "ok: outbox claims do not overlap"

echo "schema tests passed"
