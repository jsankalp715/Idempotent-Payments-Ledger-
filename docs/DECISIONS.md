# Decisions

Each entry records a choice made autonomously (the author could not be asked), the
alternatives considered, and why. Newest entries are appended at the end.

## D1. The task prompt was truncated
The prompt ends mid-sentence ("... to detect flakiness. Never"), and its "Definition of done"
section never arrived. The definition of done in `CLAUDE.md` is reconstructed from the visible
requirements. The cut-off sentence is read as a rule against gaming verification: never
skip, disable or weaken a test to get a green run. That rule is in `CLAUDE.md`.

## D2. Go toolchain and dependency versions (revised)
First choice: stay on the VM's Go 1.24.7 and pin pgx v5.8.0 / goose v3.26.0, the newest
versions that still built with Go 1.24, to avoid toolchain downloads.

Reversed after the first CI run: govulncheck found GO-2026-5004 (SQL injection through
placeholder confusion with dollar-quoted strings in pgx's client-side sanitizer, fixed in
pgx v5.9.2) and GO-2026-5970 (infinite loop in golang.org/x/text, fixed in v0.39.0). The
fixed versions need Go 1.25 or newer, and Go 1.24 no longer gets security fixes anyway.
A payments service should not ship with known vulnerabilities to save a download.

Now all dependencies are at their latest releases (pgx v5.11.0, goose v3.28.0,
x/text v0.42.0), which sets `go 1.26.0` in `go.mod`. CI tests on both supported Go
releases (`oldstable` = 1.26, `stable` = 1.27), govulncheck runs on every push, and the
Docker image builds with Go 1.27. Locally, `GOTOOLCHAIN=go1.27.1` downloads the toolchain
once from proxy.golang.org.

## D3. Router: standard library `net/http`
Go 1.22+ `ServeMux` supports method and wildcard patterns (`GET /v1/accounts/{id}`), so chi
would add a dependency without adding capability.

## D4. Identifiers
Accounts and transfers use UUIDv7, generated in Go: not guessable from outside, sortable by
creation time (good B-tree locality), and known before the INSERT, so the transfer and its
entries go to the server in one batch. Entries use a `bigint` identity: they are internal,
strictly increasing per account (inserted under the account's row lock), and make a
simple, stable pagination cursor. The API accepts only the canonical 36-character UUID
form.

## D5. Idempotency protocol: one transaction plus an advisory try-lock
The requirement is that the key record and the transfer commit in the same transaction,
and that a duplicate arriving while the original is in flight gets 409.

* Rejected: a "pending" key row committed first, then the work in a second transaction
  (Stripe's recovery-point design). The key and the work would no longer be atomic, and a
  crash between the two leaves stuck pending rows that need lease expiry and recovery.
* Rejected: `INSERT … ON CONFLICT DO NOTHING` on the key as the first statement. A
  concurrent duplicate then blocks on the uncommitted row instead of getting 409, unless
  `lock_timeout` turns the wait into an error, which makes "in flight" a timing window.
* Chosen: `pg_try_advisory_xact_lock(hash(key))` as the first statement of the
  transaction. It never waits, so an in-flight duplicate gets 409 deterministically, and it
  is released automatically at COMMIT or ROLLBACK, including when a connection dies. Under
  the lock the key is looked up (replay or 422), the work runs, and the key record with the
  exact response is inserted, all in the same transaction. The key record can only be seen
  once the work has committed, so there is no pending state to recover.

The lock id is the first 8 bytes of SHA-256 over the key. Two keys colliding (2^-64 per
pair) only causes a spurious 409, which the client retries. The primary key on
`idempotency_keys` stays as a safety net, and a violation of it is retried as a race.

## D6. What is stored and replayed
* Stored: 201 results and business rejections (insufficient funds, unknown account,
  currency mismatch, system account). These are definitive outcomes of executing the
  request. Replaying a rejection means a client that retries later cannot accidentally turn
  a declined payment into an executed one.
* Not stored: 400 validation errors (nothing executed, so the key stays free), 409 (the key
  is busy), and transient failures (503 after exhausted retries, timeouts, database errors).
  Those roll the transaction back, so there is nothing to store, and the client's retry with
  the same key must be able to execute.

## D7. Request hash
The hash covers method, path and a canonical JSON encoding of the parsed, validated request
(fixed field order, canonical lower-case UUIDs), with every part length-prefixed. The same
request sent with different whitespace or field order is the same request; changing any
value, the endpoint or the method is a different request (422). Hashing raw bytes was
rejected because harmless re-serialization by a client library would turn a legitimate
retry into a 422.

Keys are global because the service has no authentication. A multi-tenant version would
use `PRIMARY KEY (client_id, key)`.

## D8. A key never moves money twice, even after its TTL
`idempotency_keys` is a response cache with a TTL (24 h, cleaned up in batches). The
permanent guarantee is `UNIQUE (transfers.idempotency_key)` together with the request hash
stored on the transfer row. When a key's record has expired and the same request arrives,
the original 201 is rebuilt from the transfer and its entries. Those rows are immutable and
rendered deterministically, so the bytes are identical (tested). A different request with
that key gets 422. Rejected keys have no transfer row, so after expiry they may execute,
which is safe because they never moved money.

## D9. Isolation level and locking
Read committed plus `SELECT … FOR UPDATE` is the default. Correctness comes from the
explicit locks: under read committed a locking read returns the latest committed version of
the row, and the balance check runs under the lock. Both rows are locked in a single
`… WHERE id IN ($1, $2) ORDER BY id FOR UPDATE`. PostgreSQL sorts before taking row locks,
so the lock order is deterministic and opposing transfers cannot deadlock.

`SERIALIZABLE` (and repeatable read) is supported through `TX_ISOLATION`. Under contention
it rejects most attempts, because a row updated after the transaction's snapshot cannot be
locked, so throughput drops about 5x. It is still useful as a test mode: CI runs the large
stress test and the integration suite with it, which drives tens of thousands of real
serialization failures through the retry path and the client-side idempotent retries, and
the ledger stays exact.

## D10. Which errors are retried
The transaction runner retries `40001` (serialization failure), `40P01` (deadlock
detected), unique violations on constraints declared as races (`idempotency_keys_pkey`,
`transfers_idempotency_key_key`; these only occur under snapshot isolation when a
concurrent duplicate won), and pgconn's `SafeToRetry` errors (nothing reached the server).
These all guarantee that nothing was committed. A broken connection during COMMIT is
ambiguous, so it is returned to the client as a 5xx; the client's retry with the same key is
safe either way. Retries use full-jitter exponential backoff, 8 attempts by default, and stop
when the request context ends. Exhausted retries become 503 with `Retry-After`.

## D11. 409 for an in-flight duplicate
The alternative, waiting for the original to finish and then replaying, gives the client a
nicer answer but holds a connection and a database session behind a lock that the client,
typically retrying after a timeout, already gave up on. Under load those waiters pile up. A
409 with `Retry-After: 1` costs nothing and the next retry gets the stored result.

## D12. The database enforces the ledger rules
Append-only `entries` and `transfers` (triggers reject UPDATE, DELETE and TRUNCATE), a
deferred double-entry check, the per-account `balance_after` chain, cached balance equal to
the latest `balance_after` at commit, `CHECK (is_system OR balance >= 0)`, composite
`(id, currency)` foreign keys, and immutable account columns. Triggers were preferred over
revoked privileges as the primary mechanism because they hold for every role, including
the table owner that runs migrations and tests. Privilege separation is listed as a
deployment hardening step. The checks cost a few indexed lookups per transfer.

## D13. Cached balance with a running balance per entry
A balance column is needed for the `CHECK (balance >= 0)` defence and for O(1) balance
reads. `balance_after` on each entry makes every balance derivable and auditable at any
point in history, and it lets the database verify the cache cheaply (latest entry instead
of a full sum).

## D14. System accounts
Money enters only through one system account per currency, created lazily
(`INSERT … ON CONFLICT` on a partial unique index). It is the only account allowed to go
negative; its balance is minus the sum of all user balances in that currency. Client
transfers to or from system accounts are rejected (422), otherwise any client could mint
money. Funding locks the system account, a hot row that is acceptable because funding
happens once per account.

## D15. HTTP status codes
* Unknown account referenced in a transfer: 422 `account_not_found`. The request is
  well-formed but semantically invalid. 404 is reserved for the resource in the URL
  (`GET /v1/accounts/{id}`).
* Transfer validation failures: 400 with every field problem listed at once.
* Non-JSON `Content-Type`: 415. Oversized body (over 64 KiB): 413. Unknown JSON fields and
  trailing data: 400 (strict decoding protects the request hash from silent field drops).
* Client disconnects are logged as 499.

## D16. Account creation idempotency is optional
Only transfers require a key, per the specification. Account creation moves money (initial
funding), so it accepts an optional `Idempotency-Key` handled by the same store. Without a
key, a retried creation creates a second account. After the TTL a retry creates a new
account, because there is no permanent guard as there is for transfers.

## D17. Transfer responses omit `balance_after`
A transfer response is returned to the sender. Including the recipient's running balance
would leak it. Account entry listings include `balance_after`.

## D18. Cancelling statements server-side on context cancellation
pgx's default reaction to a cancelled context is to close the socket, but a PostgreSQL
backend blocked on a lock does not notice until it next writes to the client. Until then it
keeps every lock it holds, including the key's advisory lock, so the client's retry would
see 409. The pool is configured with `CancelRequestContextWatcherHandler`, which sends a
cancel request, so the statement fails at once, the transaction rolls back and the connection
returns to the pool.

## D19. Test isolation and gating
Each database test creates its own schema, migrates it and drops it, so all packages run in
parallel against one database with exact global invariants per test. Tests skip when
`DATABASE_URL` is unset, so `go test ./...` works without a database, but CI sets
`LEDGER_REQUIRE_DB=1`, which turns a missing database into a failure. That rules out a
green CI run that silently skipped the database tests. Faults are injected with triggers
created in the test's own schema, for example "raise 40001 for the first two key inserts",
counted with a sequence because sequences are not rolled back.

## D20. Stress test design
The stress test goes through real HTTP over loopback, not through service calls, so
cancellations and lost responses behave as in production. Per transfer it randomly injects:
response lost after the server processed the request (10%), a client deadline of 0.5 to
15 ms that cancels at a random stage (10%), a context cancelled before sending (3%), 2 to 4
concurrent identical submissions (10%), and a late duplicate after the definitive answer
(10%). 2% of transfers use unaffordable amounts. The client keeps its own ledger of what it
was told and the test requires it to match the database exactly. A seed makes the job list
reproducible (`STRESS_SEED`). The CI default runs 3,000 transfers in about 2 s; large mode
runs 12,000 transfers on 200 goroutines.

## D21. Container image
Multi-stage build: Go 1.27 builder, static binary (`CGO_ENABLED=0`, `-trimpath`), distroless
`static-debian12:nonroot` runtime (no shell, CA certificates included, uid 65532). The
binary has its own `healthcheck` subcommand because the image has no curl. An optional
BuildKit secret (`extra_ca`) adds a CA for building behind TLS-intercepting proxies,
including the sandbox this was built in; without it the build uses only the system trust
store, as in CI.

## D22. Limits on amounts
Amounts and initial balances are capped at 10^15 minor units: below 2^53, so values survive
JavaScript clients, and far below int64 overflow. Additions are also checked against
int64 bounds before writing (`balance_overflow`).

## D23. No pull request
The repository was empty at the start, so the working branch is its only branch, and
therefore its default branch. GitHub rejects a PR without a base (`PullRequest.base
(invalid)`). Pushing a new `main` would break the instruction to push only to the
working branch, so the pushed branch is left as the deliverable. To review as a PR, create
`main` from the first commit (`37fd30c`) and open a PR from `claude/fervent-mayer-hgmyt9`.
