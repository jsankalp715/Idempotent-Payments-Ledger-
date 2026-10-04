# Developer entry points. Database tests read DATABASE_URL; the default points
# at the local role/database created by scripts/dev-db.sh.
DATABASE_URL ?= postgres://ledger:ledger@127.0.0.1:5432/ledger_test?sslmode=disable
export DATABASE_URL

STATICCHECK_VERSION ?= v0.8.1
GOVULNCHECK_VERSION ?= latest
GO_PACKAGES := ./...

.PHONY: all build run test race stress stress-large stress-serializable lint fmt vet staticcheck vulncheck db docker-up docker-down smoke cover clean

all: lint test

build: ## Build the service binary into bin/
	CGO_ENABLED=0 go build -trimpath -o bin/ledger ./cmd/ledger

run: ## Run the service against DATABASE_URL (dev database by default)
	DATABASE_URL=$${LEDGER_RUN_DATABASE_URL:-postgres://ledger:ledger@127.0.0.1:5432/ledger?sslmode=disable} LOG_FORMAT=text go run ./cmd/ledger

test: ## Unit + integration tests (CI-sized stress included)
	go test -count=1 $(GO_PACKAGES)

race: ## Whole suite under the race detector, three times, shuffled
	go test -race -count=3 -shuffle=on -timeout 30m $(GO_PACKAGES)

stress: ## CI-sized concurrent stress test with the race detector
	go test -race -count=1 -run 'TestStress' -v -timeout 15m ./test/stress

stress-large: ## Large mode: 12k transfers, 200 goroutines, 20 accounts
	STRESS_MODE=large go test -count=1 -run 'TestStress' -v -timeout 20m ./test/stress

stress-serializable: ## Stress test with SERIALIZABLE write transactions
	TX_ISOLATION=serializable go test -race -count=1 -run 'TestStress' -v -timeout 15m ./test/stress

cover: ## Coverage profile across packages
	go test -count=1 -coverpkg=./internal/... -coverprofile=coverage.out $(GO_PACKAGES)
	go tool cover -func=coverage.out | tail -1

lint: fmt vet staticcheck ## gofmt, go vet, staticcheck

fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet $(GO_PACKAGES)

staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) $(GO_PACKAGES)

vulncheck: ## Known vulnerabilities in dependencies and the standard library
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) $(GO_PACKAGES)

db: ## Create the local role and databases (Postgres must be running)
	scripts/dev-db.sh

docker-up: ## Build and start Postgres + ledger with Docker Compose
	docker compose up --build -d

docker-down:
	docker compose down -v

smoke: ## End-to-end smoke test against a running instance (default http://localhost:8080)
	scripts/smoke.sh

clean:
	rm -rf bin coverage.out
