GORIDE_PG_DSN ?= postgres://goride:goride@localhost:5432/goride?sslmode=disable

.PHONY: run migrate migrate-down test test-integration vet tidy

# Loads .env (gitignored) if present, so secrets like GORIDE_NEWRELIC_LICENSE
# never live in tracked files or the shell history. Falls back to the default
# DSN when .env is absent.
run:
	@set -a; [ -f .env ] && . ./.env; set +a; \
		GORIDE_PG_DSN=$${GORIDE_PG_DSN:-$(GORIDE_PG_DSN)} go run ./cmd/server

migrate:
	GORIDE_PG_DSN=$(GORIDE_PG_DSN) go run ./cmd/migrate up

migrate-down:
	GORIDE_PG_DSN=$(GORIDE_PG_DSN) go run ./cmd/migrate down $(N)

test:
	go test ./... -race

# Integration + concurrency tests (build tag `integration`) hit real Postgres
# and Redis from GORIDE_PG_DSN / GORIDE_REDIS_ADDR. -count=1 disables the test
# cache so they always run against live infra.
test-integration:
	go test -tags integration ./... -count=1 -race

vet:
	go vet ./...

tidy:
	go mod tidy

# Combined statement coverage across internal/* production code (unit +
# integration tests). testsupport is excluded — it is the test fixture, not
# production code. Needs live Postgres + Redis.
COVERPKG = $(shell go list ./internal/... | grep -v /internal/testsupport | paste -sd, -)
cover:
	go test -tags integration ./... -coverpkg=$(COVERPKG) -covermode=atomic -coverprofile=coverage.out -count=1
	@go tool cover -func=coverage.out | tail -1
