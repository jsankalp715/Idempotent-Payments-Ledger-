# Report

Final state of the autonomous build of the Idempotent Payments Ledger: what was built,
what was verified and how, and what was not verified. Every number below comes from a
command that was actually run. Commands are listed so each result can be reproduced.

## Outcome

All functional requirements are implemented. All verification is in place, passes
locally against native PostgreSQL 16, and passes in GitHub Actions on every push. Docker
and docker-compose were verified end to end in the build VM and in CI.

| Definition of done item | Status |
|---|---|
| 1. Functional requirements 1 to 6 | Done |
| 2. Unit, integration, stress, overdraft-race and deadlock tests on real Postgres | Done, all passing |
| 3. `-race` with `-count` > 1, repeatedly, no flakes | Done (see below) |
| 4. gofmt, go vet, staticcheck clean | Done (plus govulncheck in CI) |
| 5. GitHub Actions CI green, checked through the GitHub API | Done |
| 6. Dockerfile and docker-compose verified | Done (locally and in CI) |
| 7. README, DECISIONS, PLAN, REPORT | Done |
| 8. Committed, pushed, PR | Pushed; **no PR possible** (see "Not done") |

The task prompt was cut off mid-sentence ("...to detect flakiness. Never") and its
"Definition of done" section never arrived. The definition used here was reconstructed from
the visible requirements (`CLAUDE.md`, `docs/DECISIONS.md` D1).

## What was built

* **Service** (`cmd/ledger`, `internal/...`): accounts funded from a per-currency system
  account; transfers with a required `Idempotency-Key`; entry listing with cursor
  pagination; health; env configuration; graceful shutdown with a drain phase; embedded
  goose migrations; batched TTL cleanup of idempotency records; structured logs.
* **Idempotency**: key record and transfer committed in one transaction, advisory try-lock
  for 409-in-flight, SHA-256 request hash over the canonical request (422 on mismatch),
  byte-identical replay, definitive rejections cached while transient failures are not, and
  a permanent `UNIQUE (transfers.idempotency_key)` guard that keeps a key from moving money
  twice even after its record expires.
* **Concurrency**: `SELECT … ORDER BY id FOR UPDATE` on both accounts, balance checked under
  the lock, `CHECK (balance >= 0)` as the second line of defence, bounded full-jitter retry
  on 40001/40P01, server-side statement cancellation when a request is cancelled.
* **Database-enforced ledger rules**: append-only triggers, a deferred double-entry check,
  the running-balance chain, cached-balance consistency at commit, composite currency FKs.
* **Tests**: about 3,300 lines covering unit, schema, store, ledger, integration, shutdown
  and stress. 86.2% statement coverage of `internal/` (`make cover`).
* **Delivery**: multi-stage Dockerfile (distroless, non-root, 22 MB), docker-compose with
  health checks, smoke script, and a six-job GitHub Actions workflow.

## Verification evidence

### GitHub Actions

Workflow: `.github/workflows/ci.yml`. Jobs: lint (gofmt, `go mod tidy` diff, vet,
staticcheck, build), govulncheck, test on `oldstable` (Go 1.26) and `stable` (Go 1.27)
running `go test -race -count=3 -shuffle=on ./...` against a `postgres:16` service with
`LEDGER_REQUIRE_DB=1`, stress (large mode under read committed and serializable plus the
integration suite under serializable, all with `-race`), and docker (compose up, smoke
test, graceful shutdown check).

| Run | Commit | Result |
|---|---|---|
| [#1](https://github.com/jsankalp715/Idempotent-Payments-Ledger-/actions/runs/37174196915) | `2af6847` | test ×2, stress, docker green; **lint and govulncheck failed**, see below |
| [#2](https://github.com/jsankalp715/Idempotent-Payments-Ledger-/actions/runs/37174389455) | `9e97395` | **all 6 jobs green** |
| [#3](https://github.com/jsankalp715/Idempotent-Payments-Ledger-/actions/runs/37174677057) | `3d17d92` | **all 6 jobs green** (on the Node 24 action majors) |

Stress results from run #3's job log (GitHub-hosted runner, `postgres:16` service, `-race`):

```
large, read committed: 12000 transfers in 18.517s by 200 goroutines over 20 accounts
  requests sent 19398: responses lost 1139, client timeouts 1201, pre-cancelled 371,
  409 in flight 2255, 503 transient 0, replays 3571
  outcomes: 10856 executed, 1144 rejected for insufficient funds
  invariants hold over 21 accounts, 10876 transfers, 21752 entries, 12000 idempotency records
large, serializable: 12000 transfers in 1m31.957s
  server-side transaction retries: Serialization 89679, Deadlock 0, Exhausted 6933
  (503 transient 6030, all retried by clients with the same key)
  invariants hold over 21 accounts, 10442 transfers, 20884 entries, 12000 idempotency records
integration suite, serializable: ok (19.9s)
```

Run #1 found two real problems, and both were fixed rather than suppressed:
* staticcheck SA4000: a tautological comparison in a test.
* govulncheck: GO-2026-5004 (SQL injection via placeholder confusion in pgx v5.8.0) and
  GO-2026-5970 (x/text v0.29.0). Their fixes require Go 1.25+, which reversed the original
  decision to stay on the VM's end-of-life Go 1.24 (DECISIONS D2). All dependencies are now
  at their latest releases.

### Local runs (4 vCPU / 16 GB VM, native PostgreSQL 16.14, Go 1.27.1)

Final battery on commit `3d17d92`, run by [`scripts/verify.sh`](scripts/verify.sh) (`make verify`), with
`LEDGER_REQUIRE_DB=1` so nothing could skip:

| Run | Command | Result |
|---|---|---|
| Whole suite under the race detector, 5 shuffled repetitions | `go test -race -count=5 -shuffle=on ./...` | all 6 test packages pass, no data races, 74 s |
| Large stress, read committed, race | `STRESS_MODE=large go test -race -run TestStress ./test/stress` | 12,000 transfers / 200 goroutines / 20 accounts in 15.1 s; 10,749 executed, 1,251 rejected; 0 retries needed; invariants hold |
| Large stress, serializable, race | `… TX_ISOLATION=serializable …` | 12,000 transfers in 67 s; 89,511 serialization failures retried, 7,273 exhausted to 503 and retried by clients; 0 deadlocks; invariants hold |
| 30k stress, 300 goroutines | `STRESS_TRANSFERS=30000 STRESS_WORKERS=300 go test -run TestStress ./test/stress` | 30,000 transfers in 22.0 s (1,362/s); 3,011 lost responses, 3,038 client timeouts, 5,840 in-flight 409s, 9,252 replays; invariants hold |

Earlier runs during development (all passing): `-race -count=3 -shuffle=on` on Go 1.24 with
the original dependencies; `-race -count=2` after the upgrade to Go 1.27; the integration
suite under `TX_ISOLATION=serializable` with `-race` (in the opposing-transfers test:
7,605 serialization retries, 0 deadlocks); CI-sized stress under serializable (21,196
retries); and the review fixes (`-race -count=3` on the shutdown and config tests).

Other checks: `make lint` (gofmt, go vet, staticcheck v0.8.1) is clean, and `make cover`
reports 86.2% statement coverage of `internal/`.

### Stress test outcomes in detail

What one large run injects and checks: about 1,200 responses lost after the server
committed, about 1,200 client timeouts at random points mid-request, about 350 requests
cancelled before sending, about 2,300 409s from racing duplicates, and about 3,600 replays.
Afterwards: every key has exactly one definitive answer, and every answer seen for a key is
byte-identical. The database holds exactly the transfers the client was told about, with
the same ids. Every balance equals the client's own bookkeeping, user balances still sum to
the funded total, and invariants (a) to (e) hold under independent SQL.

Under `SERIALIZABLE` the same test drives tens of thousands of real serialization failures
through the retry loop. Thousands of requests exhaust their retries and return 503, and
the clients retry them with the same key. The ledger is still exact.

### Docker (in the build VM)

* `docker build` and `EXTRA_CA_FILE=/root/.ccr/ca-bundle.crt docker compose up --build -d --wait`:
  both containers healthy; image 22.1 MB.
* `scripts/smoke.sh`: health, funding, transfer, byte-identical replay with
  `Idempotent-Replayed`, 422 on key reuse, 422 overdraft, 400 without a key, balances
  7500/2500. All passed.
* `docker compose stop ledger`: logs show `shutdown started` (drain 2s) then
  `shutdown complete`, 2.3 s in total.
* `docker compose build` without the extra CA also succeeds (warm module cache), which
  validates the default `/dev/null` secret path that CI uses.

## What was not verified, and caveats

* **No pull request.** The GitHub repository was empty at the start. The work branch
  `claude/fervent-mayer-hgmyt9` is its only branch, and therefore its default branch, so
  there is no base to open a PR against. The attempt (head `claude/fervent-mayer-hgmyt9`,
  base `main`) failed with `Validation Failed: PullRequest.base (invalid)`. Creating another
  branch was ruled out by the instruction to push only to the working branch. To review as a PR, create `main` from the
  first commit (`37fd30c`) and open a PR from the work branch.
* **govulncheck cannot run in the build VM**, because `vuln.go.dev` is blocked by its egress
  policy (HTTP 403). It runs, and passes, in CI.
* **CI artifacts could not be downloaded** from the VM (the artifact storage host is blocked).
  CI results were read through the GitHub API (job status and logs) instead.
* **The Docker build in the VM needs the sandbox's TLS-interception CA** as a build secret.
  That is a property of the sandbox, not of the project: CI builds without it.
* **CI raises Postgres `max_connections`** to 300 with `ALTER SYSTEM` plus a restart,
  because test packages run in parallel, each with its own pool. Local runs fit in the
  default 100.
* **Throughput numbers** are from a shared 4-vCPU VM with Postgres on the same machine,
  with `fsync` on. They illustrate behaviour under contention and are not a benchmark.
* **Not built** (listed in the README under next steps): authentication and tenancy (keys
  are global), multi-currency transfers, a metrics endpoint, privilege-separated database
  roles.

## How to reproduce

```sh
service postgresql start && scripts/dev-db.sh
export DATABASE_URL='postgres://ledger:ledger@127.0.0.1:5432/ledger_test?sslmode=disable' LEDGER_REQUIRE_DB=1
make lint
go test -race -count=5 -shuffle=on ./...
STRESS_MODE=large go test -race -count=1 -run TestStress -v ./test/stress
STRESS_MODE=large TX_ISOLATION=serializable go test -race -count=1 -run TestStress -v ./test/stress
docker compose up --build -d --wait && scripts/smoke.sh && docker compose down -v
```
