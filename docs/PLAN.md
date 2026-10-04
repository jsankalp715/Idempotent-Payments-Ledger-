# Plan

Milestone checklist. Tick items (`[x]`) as they are finished and pushed; this file is the
recovery point after a context compaction. Decisions and their rationale live in
`docs/DECISIONS.md`.

## M0: Bootstrap
- [x] CLAUDE.md (rules, environment, definition of done)
- [x] docs/PLAN.md and docs/DECISIONS.md
- [x] First commit pushed

## M1: Skeleton
- [x] go.mod (Go 1.24, pgx v5.8.0, goose v3.26.0), .gitignore, Makefile
- [x] scripts/dev-db.sh (start-agnostic, idempotent role + database creation)
- [x] internal/config: env parsing with defaults and validation, plus unit tests

## M2: Schema and migrations
- [x] Tables: accounts, transfers, entries, idempotency_keys
- [x] Constraints: CHECK balance >= 0 for non-system accounts, composite (id, currency) FKs,
      one system account per currency, UNIQUE transfers.idempotency_key
- [x] Triggers: entries and transfers append-only (UPDATE/DELETE/TRUNCATE rejected);
      deferred check that each transfer has exactly two entries summing to zero;
      running-balance chain (balance_after) and cached balance == latest balance_after
- [x] goose migrations embedded in the binary; migrate on start
- [x] internal/testdb: throwaway schema per test, migrated, dropped on cleanup
- [x] Schema integration tests (append-only, CHECK, balanced-transfer trigger, currency FK)

## M3: Transaction runner
- [x] RunInTx with configurable isolation and bounded exponential backoff with jitter
- [x] Retry on 40001 serialization_failure, 40P01 deadlock_detected, idempotency unique races
- [x] Retry counters (by reason) for tests and logs; unit tests for classification and backoff

## M4: Ledger domain
- [x] Validation (amounts, currency, ids, same-account) with unit tests
- [x] CreateAccount with optional funding transfer from the per-currency system account
- [x] Transfer: lock both accounts FOR UPDATE in id order, check funds, 1 transfer + 2 entries,
      update cached balances
- [x] GetAccount, GetTransfer, ListEntries (cursor pagination)

## M5: Idempotency
- [x] Request fingerprint (SHA-256 over canonical request) with unit tests
- [x] Key header validation
- [x] Store.Do: advisory try-lock (409 in flight), lookup (replay or 422), run business fn and
      persist key + response in the same transaction
- [x] Permanent guard: transfers.idempotency_key UNIQUE; response rebuilt after TTL expiry
- [x] TTL cleanup job (batched, SKIP LOCKED)

## M6: HTTP service
- [x] Routes: POST/GET accounts, GET entries, POST/GET transfers, GET /healthz
- [x] Error mapping and JSON envelope; strict JSON decoding; body size limit
- [x] Middleware: request id, access log (slog), panic recovery, request timeout
- [x] cmd/ledger: config, migrations, server, cleanup job, graceful shutdown

## M7: Integration tests (real Postgres)
- [x] Replay: same key + same payload returns identical status and body, executes once
- [x] Mismatch: same key + different payload is 422
- [x] In flight: same key during an in-flight request is 409
- [x] Atomicity: a failure injected after the transfer write leaves neither key nor transfer
- [x] Transient failures are retried and never cached; definitive errors are cached
- [x] TTL cleanup and post-expiry behaviour
- [x] Concurrent same-key storm executes exactly once
- [x] HTTP API behaviour (validation, 404s, pagination, health, graceful shutdown)

## M8: Concurrency proofs
- [x] Invariant checker: (a) money conserved, (b) no negative balances, (c) balance equals sum of
      entries, (d) entries sum to zero, (e) each idempotency key applied at most once
- [x] Overdraft race: exactly floor(balance / amount) of N concurrent drains succeed
- [x] Deadlock test: opposing A->B and B->A in parallel, no deadlock, no drift; plus a real
      deadlock that the retry loop recovers from
- [x] Stress test: env-configurable; CI default under 2 min; large mode 10k+ transfers,
      200 goroutines, 20 accounts; retries with the same key, cancelled contexts, timeouts

## M9: Verification runs
- [x] `go test -race -count=3 ./...` passes repeatedly
- [x] Large stress passes (read committed and serializable)
- [ ] Results recorded in REPORT.md

## M10: CI
- [ ] GitHub Actions: lint (gofmt, vet, staticcheck), race tests with a Postgres service,
      stress under both isolation levels, Docker build and compose smoke test
- [ ] CI green on the pushed branch, checked through the GitHub API

## M11: Docker
- [x] Multi-stage Dockerfile (static binary, non-root, healthcheck subcommand)
- [x] docker-compose.yml (Postgres 16 + service), smoke script
- [x] `docker compose up --build` verified locally (or limits documented)

## M12: Docs and wrap-up
- [ ] README: overview, API, design, guarantees and how each is proven, how to run and test
- [ ] docs/DECISIONS.md complete
- [ ] REPORT.md: what was built, verification evidence, what was not verified, limitations
- [ ] Final self-review of the diff; PR opened or reason documented
