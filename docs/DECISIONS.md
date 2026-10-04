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
