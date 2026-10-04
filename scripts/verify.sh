#!/usr/bin/env bash
# Full local verification battery: the race suite five times, large stress under
# both isolation levels, and a 30k-transfer run. Takes about 3 minutes.
set -uo pipefail
failed=0
cd "$(dirname "$0")/.."
export DATABASE_URL="${DATABASE_URL:-postgres://ledger:ledger@127.0.0.1:5432/ledger_test?sslmode=disable}" LEDGER_REQUIRE_DB=1
echo "=== race x5 shuffled $(date -u +%T)"
time go test -race -count=5 -shuffle=on -timeout 40m ./... 2>&1 | tail -12 || failed=1
echo "=== large stress, read committed, race $(date -u +%T)"
time env STRESS_MODE=large go test -race -count=1 -run TestStress -v -timeout 15m ./test/stress 2>&1 | grep -E "config|outcomes|invariants|transfers in|requests sent|retries|PASS|FAIL|ok" || failed=1
echo "=== large stress, serializable, race $(date -u +%T)"
time env STRESS_MODE=large TX_ISOLATION=serializable go test -race -count=1 -run TestStress -v -timeout 15m ./test/stress 2>&1 | grep -E "config|outcomes|invariants|transfers in|requests sent|retries|PASS|FAIL|ok" || failed=1
echo "=== 30k stress, 300 goroutines, no race $(date -u +%T)"
time env STRESS_TRANSFERS=30000 STRESS_WORKERS=300 go test -count=1 -run TestStress -v -timeout 15m ./test/stress 2>&1 | grep -E "config|outcomes|invariants|transfers in|requests sent|retries|PASS|FAIL|ok" || failed=1
echo "=== done $(date -u +%T)"
if [ "$failed" -ne 0 ]; then echo "VERIFICATION FAILED"; exit 1; fi
echo "all verification steps passed"
