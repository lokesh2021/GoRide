// Package store owns connections to Postgres (pgx pool) and Redis, shared
// by all domain packages. It holds no domain logic.
package store

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/lokeshbm/goride/internal/config"
)

// Store bundles the shared Postgres pool and Redis client.
type Store struct {
	PG    *pgxpool.Pool
	Redis *redis.Client
}

// New connects to Postgres and Redis using cfg, sizing the pg pool relative
// to available CPUs, and verifies both are reachable before returning.
func New(ctx context.Context, cfg config.Config) (*Store, error) {
	pgCfg, err := pgxpool.ParseConfig(cfg.PGDSN)
	if err != nil {
		return nil, fmt.Errorf("store: parse pg dsn: %w", err)
	}

	maxConns := int32(runtime.NumCPU() * 4)
	if maxConns < 4 {
		maxConns = 4
	}
	pgCfg.MaxConns = maxConns
	pgCfg.MinConns = 1
	pgCfg.MaxConnLifetime = time.Hour
	pgCfg.MaxConnIdleTime = 30 * time.Minute
	pgCfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, pgCfg)
	if err != nil {
		return nil, fmt.Errorf("store: create pg pool: %w", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})

	s := &Store{PG: pool, Redis: rdb}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.PingPostgres(pingCtx); err != nil {
		pool.Close()
		_ = rdb.Close()
		return nil, fmt.Errorf("store: postgres unreachable: %w", err)
	}
	if err := s.PingRedis(pingCtx); err != nil {
		pool.Close()
		_ = rdb.Close()
		return nil, fmt.Errorf("store: redis unreachable: %w", err)
	}

	return s, nil
}

// PingPostgres runs SELECT 1 against the pg pool.
func (s *Store) PingPostgres(ctx context.Context) error {
	var one int
	return s.PG.QueryRow(ctx, "SELECT 1").Scan(&one)
}

// PingRedis issues a PING against the redis client.
func (s *Store) PingRedis(ctx context.Context) error {
	return s.Redis.Ping(ctx).Err()
}

// Close releases the Postgres pool and Redis client.
func (s *Store) Close() {
	s.PG.Close()
	_ = s.Redis.Close()
}
