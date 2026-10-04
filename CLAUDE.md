# CLAUDE.md: Idempotent Payments Ledger

Double-entry ledger HTTP service: exactly-once transfers under client retries and
network failures, no overdrafts or double-spends under concurrency, proven by
stress tests in GitHub Actions. Go 1.26+ (1.27 locally), net/http, pgx/v5, goose, slog, PostgreSQL 16, Docker.

## After a context compaction or VM restart
1. Read `docs/PLAN.md` (milestone checklist, ticked as work completes) and `docs/DECISIONS.md`.
2. `git status && git log --oneline -15`; then continue at the first unticked item in PLAN.md.
3. Postgres and dockerd do not survive a VM restart. Restart them (see Environment).

## Working rules
- Fully autonomous: never end a turn to ask the user anything. Pick the most sensible
  option, record it in `docs/DECISIONS.md`, keep going. Only stop when the Definition of
  done holds, or when options are truly exhausted (explain why in `REPORT.md`).
- Develop on branch `claude/fervent-mayer-hgmyt9`. After every milestone: commit, then
  `git push -u origin claude/fervent-mayer-hgmyt9` (retry with backoff on network errors).
  Never force-push, never delete branches, never push to any other branch.
- Never skip, weaken or delete a test or assertion to get green: fix the code. Never claim
  verification that was not actually run; `REPORT.md` states exactly what was verified.
- Money is BIGINT minor units (`int64`), never floats. No ORM: hand-written SQL via pgx.
- Run long jobs (stress, repeated `-race`) in the background:
  `nohup <cmd> > /tmp/<name>.log 2>&1 &`, then poll with short checks. Keep each stress run
  under about 10 minutes. Foreground commands time out after 2 to 10 minutes.
- Before each push: `gofmt -l .` is empty, `go vet ./...` and staticcheck pass, tests pass.
- Use `export GOTOOLCHAIN=go1.27.1` (go.mod needs Go 1.26+; the VM ships 1.24.7). Keep
  `make vulncheck`-clean dependencies (it runs in CI; vuln.go.dev is blocked on the VM).
- Never put model names or identifiers in commits, code or docs.

## Environment (Claude Code cloud VM; verified)
- Ubuntu 24.04, 4 vCPU, 16 GB RAM, ~30 GB disk. Go 1.24.7 (go1.27.1 auto-downloads via
  GOTOOLCHAIN), Docker 29, PostgreSQL 16.
  Network is allowlisted: proxy.golang.org, GitHub and Docker Hub work.
- Postgres is installed but stopped after a restart. Start it and create the test role/DBs:
  `service postgresql start && scripts/dev-db.sh` (idempotent), then
  `export DATABASE_URL='postgres://ledger:ledger@127.0.0.1:5432/ledger_test?sslmode=disable'`.
- DB tests read `DATABASE_URL` (no testcontainers, no Docker-in-Docker). If it is unset they
  skip, unless `LEDGER_REQUIRE_DB=1` (set in CI), which turns a missing DB into a failure.
  Each test runs in its own throwaway schema, so packages can run in parallel.
- Docker: the daemon is not running by default: `nohup dockerd > /tmp/dockerd.log 2>&1 &`.
  The sandbox intercepts TLS; builds need its CA as a secret:
  `EXTRA_CA_FILE=/root/.ccr/ca-bundle.crt docker compose up --build -d --wait`.
- GitHub: use the `mcp__github__*` tools (load via ToolSearch) to inspect Actions runs and
  open PRs. The remote was empty at start: this branch is the only branch, so a PR may be
  impossible (no base branch). If so, leave the pushed branch and say so in `REPORT.md`.

## Layout
`cmd/ledger` entrypoint · `internal/config` env config · `internal/postgres` pool, migrations,
retrying tx runner · `internal/idempotency` fingerprint, key store, TTL cleanup ·
`internal/ledger` domain logic + SQL · `internal/httpapi` handlers/middleware ·
`internal/testdb` per-test schema helper · `test/integration`, `test/stress` black-box tests.

## Commands (see Makefile)
`make test` unit + integration · `make race` race detector, count=3 · `make verify` full battery · `make stress` CI-sized stress ·
`make stress-large` 10k+ transfers / 200 goroutines / 20 accounts · `make lint` · `make vulncheck` ·
`make run` · `make docker-up` + `make smoke`.

## Definition of done
The task prompt was truncated mid-sentence; this is reconstructed from its visible requirements.
1. Functional requirements 1 to 6 work: accounts with system-account funding, transfers with a
   required Idempotency-Key, double-entry append-only entries, every idempotency case (replay,
   422 mismatch, 409 in flight, key and transfer in one transaction, request hash, TTL cleanup),
   ordered `FOR UPDATE` locking + CHECK constraint + bounded retry, health, env config and
   graceful shutdown.
2. Tests pass on real Postgres: unit (validation, hashing), integration (every idempotency
   case), stress (CI default under 2 min; large mode 10k+ transfers, ~200 goroutines, ~20
   accounts; retries, cancellations and timeouts; invariants a to e), overdraft race, deadlock.
3. The suite passes under `-race` with `-count` greater than 1, repeatedly, with no flakes.
   Results are recorded in `REPORT.md`.
4. gofmt, go vet and staticcheck are clean.
5. GitHub Actions CI (lint, race tests against a Postgres service container, stress, Docker
   build) is green on the pushed branch, checked through the GitHub API.
6. Dockerfile and docker-compose.yml work (`docker compose up --build` verified, or exactly
   what could not be verified is documented).
7. README (API, design, guarantees and how they are proven), `docs/DECISIONS.md`,
   `docs/PLAN.md` (all ticked) and `REPORT.md` are complete and accurate.
8. Everything is committed and pushed; a PR is opened, or `REPORT.md` explains why not.
