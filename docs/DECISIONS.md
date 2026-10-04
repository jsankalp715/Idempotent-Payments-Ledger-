# Decisions

Each entry records a choice made autonomously (the author could not be asked), the
alternatives considered, and why. Newest entries are appended at the end.

## D1. The task prompt was truncated
The prompt ends mid-sentence ("... to detect flakiness. Never"), and its "Definition of done"
section never arrived. The definition of done in `CLAUDE.md` is reconstructed from the visible
requirements. The cut-off sentence is read as a rule against gaming verification: never
skip, disable or weaken a test to get a green run. That rule is in `CLAUDE.md`.

## D2. Go 1.24 toolchain and pinned dependency versions
The VM has Go 1.24.7. The latest pgx (v5.9+) needs Go 1.25 and the latest goose (v3.28) needs
Go 1.26, which would force a toolchain download in every environment. Pinned instead:
`github.com/jackc/pgx/v5 v5.8.0` (go 1.24) and `github.com/pressly/goose/v3 v3.26.0`
(go 1.23). `go.mod` declares `go 1.24.0`. CI also builds and tests on the latest stable Go to
show forward compatibility.

## D3. Router: standard library `net/http`
Go 1.22+ `ServeMux` supports method and wildcard patterns (`GET /v1/accounts/{id}`), so chi
would add a dependency without adding capability.
