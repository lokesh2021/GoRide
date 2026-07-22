// Command migrate applies or reverts golang-migrate SQL migrations in
// migrations/ against GORIDE_PG_DSN.
//
// Usage:
//
//	go run ./cmd/migrate up
//	go run ./cmd/migrate down [N]   # N defaults to 1
package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/lokeshbm/goride/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: migrate <up|down> [N]")
	}
	cmd := os.Args[1]

	cfg := config.Load()
	if cfg.PGDSN == "" {
		log.Fatal("GORIDE_PG_DSN is required")
	}

	m, err := migrate.New("file://migrations", cfg.PGDSN)
	if err != nil {
		log.Fatalf("migrate: init failed: %v", err)
	}
	defer func() {
		_, _ = m.Close()
	}()

	switch cmd {
	case "up":
		err = m.Up()
	case "down":
		n := 1
		if len(os.Args) > 2 {
			n, err = strconv.Atoi(os.Args[2])
			if err != nil {
				log.Fatalf("migrate: invalid N: %v", err)
			}
		}
		err = m.Steps(-n)
	default:
		log.Fatalf("migrate: unknown command %q (want up|down)", cmd)
	}

	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		log.Fatalf("migrate: %s failed: %v", cmd, err)
	}

	fmt.Printf("migrate: %s complete\n", cmd)
}
