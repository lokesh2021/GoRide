//go:build integration

package store

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/lokeshbm/goride/internal/config"
)

const (
	defaultTestPGDSN     = "postgres://lokesh@localhost:5432/goride?sslmode=disable"
	defaultTestRedisAddr = "localhost:6379"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	pgDSN := os.Getenv("GORIDE_PG_DSN")
	if pgDSN == "" {
		pgDSN = defaultTestPGDSN
	}
	redisAddr := os.Getenv("GORIDE_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = defaultTestRedisAddr
	}
	return config.Config{PGDSN: pgDSN, RedisAddr: redisAddr}
}

func TestNewConnectsAndPings(t *testing.T) {
	ctx := context.Background()
	st, err := New(ctx, testConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()

	if err := st.PingPostgres(ctx); err != nil {
		t.Errorf("PingPostgres: %v", err)
	}
	if err := st.PingRedis(ctx); err != nil {
		t.Errorf("PingRedis: %v", err)
	}
}

func TestNewBadPGDSNParseError(t *testing.T) {
	cfg := testConfig(t)
	cfg.PGDSN = "not a dsn ::: bad"

	_, err := New(context.Background(), cfg)
	if err == nil {
		t.Fatal("New with malformed PGDSN: want error, got nil")
	}
	if !strings.Contains(err.Error(), "parse pg dsn") {
		t.Errorf("New error = %q, want it to mention parse pg dsn", err.Error())
	}
}

func TestNewPostgresUnreachable(t *testing.T) {
	cfg := testConfig(t)
	// Nothing listens on port 1: connection refused, fast and deterministic.
	cfg.PGDSN = "postgres://user@localhost:1/db?sslmode=disable"

	_, err := New(context.Background(), cfg)
	if err == nil {
		t.Fatal("New with unreachable postgres: want error, got nil")
	}
	if !strings.Contains(err.Error(), "postgres unreachable") {
		t.Errorf("New error = %q, want it to mention postgres unreachable", err.Error())
	}
}

func TestNewRedisUnreachable(t *testing.T) {
	cfg := testConfig(t)
	// Postgres must be reachable so New gets past that check and exercises the
	// redis-unreachable branch.
	cfg.RedisAddr = "localhost:1"

	_, err := New(context.Background(), cfg)
	if err == nil {
		t.Fatal("New with unreachable redis: want error, got nil")
	}
	if !strings.Contains(err.Error(), "redis unreachable") {
		t.Errorf("New error = %q, want it to mention redis unreachable", err.Error())
	}
}

func TestClose(t *testing.T) {
	st, err := New(context.Background(), testConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	st.Close()

	// Postgres pool is closed; a query against it should now fail.
	if err := st.PingPostgres(context.Background()); err == nil {
		t.Error("PingPostgres after Close: want error, got nil")
	}
}
