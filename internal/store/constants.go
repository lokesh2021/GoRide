package store

import "time"

// ---- domain constants ----

const (
	// minPGPoolConns is the floor for the pg pool size regardless of CPU count.
	minPGPoolConns = 4
	// pgConnsPerCPU sizes the pg pool relative to available CPUs.
	pgConnsPerCPU = 4
	// pgMaxConnLifetime bounds how long a pooled connection may live.
	pgMaxConnLifetime = time.Hour
	// pgMaxConnIdleTime bounds how long a pooled connection may sit idle.
	pgMaxConnIdleTime = 30 * time.Minute
	// pgHealthCheckPeriod is how often the pool health-checks idle connections.
	pgHealthCheckPeriod = time.Minute
	// pingTimeout bounds the startup reachability check for Postgres and Redis.
	pingTimeout = 5 * time.Second
)
