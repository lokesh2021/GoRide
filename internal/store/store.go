// Package store owns connections to Postgres (pgx pool) and Redis, shared
// by all domain packages. It holds no domain logic.
package store

import (
	"context"
	"fmt"
	"runtime"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/newrelic/go-agent/v3/integrations/nrpgx5"
	nrredis "github.com/newrelic/go-agent/v3/integrations/nrredis-v9"
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
//
// The pg pool always gets the nrpgx5 tracer and the Redis client always gets
// the nrredis-v9 hook, unconditionally — neither needs an *newrelic.Application
// reference here. Both integrations instead look up the transaction from each
// call's own context via newrelic.FromContext at query time and no-op when
// none is found there (no txn in context — e.g. background/sweeper work — or
// monitoring disabled so no txn was ever started). That is what makes this
// safe regardless of whether GORIDE_NEWRELIC_LICENSE is set: the request's
// txn (attached by observability.Middleware) rides along on ctx, and queries
// made without one are simply untraced, never broken.
func New(ctx context.Context, cfg config.Config) (*Store, error) {
	pgCfg, err := pgxpool.ParseConfig(cfg.PGDSN)
	if err != nil {
		return nil, fmt.Errorf("store: parse pg dsn: %w", err)
	}

	maxConns := int32(runtime.NumCPU() * pgConnsPerCPU)
	if maxConns < minPGPoolConns {
		maxConns = minPGPoolConns
	}
	pgCfg.MaxConns = maxConns
	pgCfg.MinConns = 1
	pgCfg.MaxConnLifetime = pgMaxConnLifetime
	pgCfg.MaxConnIdleTime = pgMaxConnIdleTime
	pgCfg.HealthCheckPeriod = pgHealthCheckPeriod
	pgCfg.ConnConfig.Tracer = nrpgx5.NewTracer()

	pool, err := pgxpool.NewWithConfig(ctx, pgCfg)
	if err != nil {
		return nil, fmt.Errorf("store: create pg pool: %w", err)
	}

	redisOpts := &redis.Options{Addr: cfg.RedisAddr}
	rdb := redis.NewClient(redisOpts)
	rdb.AddHook(nrredis.NewHook(redisOpts))

	s := &Store{PG: pool, Redis: rdb}

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
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
