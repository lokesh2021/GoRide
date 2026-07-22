GORIDE_PG_DSN ?= postgres://goride:goride@localhost:5432/goride?sslmode=disable

.PHONY: run migrate migrate-down test vet tidy

run:
	GORIDE_PG_DSN=$(GORIDE_PG_DSN) go run ./cmd/server

migrate:
	GORIDE_PG_DSN=$(GORIDE_PG_DSN) go run ./cmd/migrate up

migrate-down:
	GORIDE_PG_DSN=$(GORIDE_PG_DSN) go run ./cmd/migrate down $(N)

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy
