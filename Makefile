GORIDE_PG_DSN ?= postgres://goride:goride@localhost:5432/goride?sslmode=disable

.PHONY: run migrate migrate-down test test-integration vet tidy

run:
	GORIDE_PG_DSN=$(GORIDE_PG_DSN) go run ./cmd/server

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
