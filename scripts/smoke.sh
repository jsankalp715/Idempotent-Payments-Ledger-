#!/usr/bin/env bash
# End-to-end smoke test against a running ledger (default http://localhost:8080):
# health, account funding, a transfer, its idempotent replay, key reuse with a
# different payload, an overdraft, and final balances.
#
#   scripts/smoke.sh [base-url]
set -euo pipefail

BASE="${1:-${LEDGER_URL:-http://localhost:8080}}"
KEY="smoke-$(date +%s)-$$-${RANDOM}"
JSON='Content-Type: application/json'

fail() { echo "FAIL: $*" >&2; exit 1; }

# request METHOD PATH [BODY] [IDEMPOTENCY-KEY] -> sets STATUS, BODY, HEADERS
request() {
	local method=$1 path=$2 body=${3:-} key=${4:-}
	local args=(-sS -X "$method" -D /tmp/smoke-headers -o /tmp/smoke-body -w '%{http_code}' "$BASE$path")
	[ -n "$body" ] && args+=(-H "$JSON" --data "$body")
	[ -n "$key" ] && args+=(-H "Idempotency-Key: $key")
	STATUS=$(curl "${args[@]}")
	BODY=$(cat /tmp/smoke-body)
	HEADERS=$(cat /tmp/smoke-headers)
}

field() { # field NAME: extract a top-level string or number field from BODY
	echo "$BODY" | sed -n "s/.*\"$1\":\"\{0,1\}\([^\",}]*\)\"\{0,1\}[,}].*/\1/p" | head -n1
}

expect() { # expect STATUS DESCRIPTION
	[ "$STATUS" = "$1" ] || fail "$2: expected HTTP $1, got $STATUS: $BODY"
	echo "ok   $2 ($STATUS)"
}

for _ in $(seq 1 30); do
	if curl -fsS "$BASE/healthz" >/dev/null 2>&1; then break; fi
	sleep 1
done
request GET /healthz
expect 200 "health check"

request POST /v1/accounts '{"currency":"USD","initial_balance":10000}'
expect 201 "create funded account"
ALICE=$(field id)
request POST /v1/accounts '{"currency":"USD"}'
expect 201 "create empty account"
BOB=$(field id)

TRANSFER="{\"from_account\":\"$ALICE\",\"to_account\":\"$BOB\",\"amount\":2500,\"currency\":\"USD\"}"
request POST /v1/transfers "$TRANSFER" "$KEY"
expect 201 "transfer 2500"
FIRST="$BODY"

request POST /v1/transfers "$TRANSFER" "$KEY"
expect 201 "retry with the same key"
[ "$BODY" = "$FIRST" ] || fail "replayed body differs from the original"
echo "$HEADERS" | grep -qi '^Idempotent-Replayed: true' || fail "replay not marked Idempotent-Replayed"
echo "ok   replay is byte-identical and marked Idempotent-Replayed"

request POST /v1/transfers "{\"from_account\":\"$ALICE\",\"to_account\":\"$BOB\",\"amount\":9999,\"currency\":\"USD\"}" "$KEY"
expect 422 "same key, different payload"

request POST /v1/transfers "{\"from_account\":\"$ALICE\",\"to_account\":\"$BOB\",\"amount\":100000,\"currency\":\"USD\"}" "$KEY-overdraft"
expect 422 "overdraft rejected"

request POST /v1/transfers "$TRANSFER"
expect 400 "missing Idempotency-Key"

request GET "/v1/accounts/$ALICE"
expect 200 "get source account"
[ "$(field balance)" = "7500" ] || fail "source balance $(field balance), want 7500"
request GET "/v1/accounts/$BOB"
expect 200 "get destination account"
[ "$(field balance)" = "2500" ] || fail "destination balance $(field balance), want 2500"
echo "ok   balances 7500 / 2500: the transfer applied exactly once"

request GET "/v1/accounts/$ALICE/entries?limit=10"
expect 200 "list entries"

echo "smoke test passed against $BASE"
