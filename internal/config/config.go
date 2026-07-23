// Package config loads GoRide's runtime configuration from environment
// variables, per docs/SPEC.md "Conventions".
package config

import (
	"os"
	"strconv"
)

// Config holds all env-derived settings for the service.
type Config struct {
	Addr            string // GORIDE_ADDR
	PGDSN           string // GORIDE_PG_DSN
	RedisAddr       string // GORIDE_REDIS_ADDR
	Env             string // GORIDE_ENV
	NewRelicLicense string // GORIDE_NEWRELIC_LICENSE (optional; agent disabled when empty)
	NewRelicAppName string // GORIDE_NEWRELIC_APP_NAME (optional; defaults to "goride")
	PSPSecret       string // GORIDE_PSP_SECRET
	PSPWebhookURL   string // GORIDE_PSP_WEBHOOK_URL (mock PSP posts confirmations here)
	LogLevel        string // GORIDE_LOG_LEVEL (debug|info|warn|error; default info)
	SlowRequestMs   int    // GORIDE_SLOW_REQUEST_MS (request-log slow-warn threshold; default 250)
}

// Load reads configuration from the environment, applying documented defaults.
func Load() Config {
	return Config{
		Addr:            getenv("GORIDE_ADDR", defaultAddr),
		PGDSN:           os.Getenv("GORIDE_PG_DSN"),
		RedisAddr:       getenv("GORIDE_REDIS_ADDR", defaultRedisAddr),
		Env:             getenv("GORIDE_ENV", defaultEnv),
		NewRelicLicense: os.Getenv("GORIDE_NEWRELIC_LICENSE"),
		NewRelicAppName: getenv("GORIDE_NEWRELIC_APP_NAME", defaultNewRelicAppName),
		PSPSecret:       getenv("GORIDE_PSP_SECRET", defaultPSPSecret),
		PSPWebhookURL:   getenv("GORIDE_PSP_WEBHOOK_URL", defaultPSPWebhookURL),
		LogLevel:        getenv("GORIDE_LOG_LEVEL", defaultLogLevel),
		SlowRequestMs:   getenvInt("GORIDE_SLOW_REQUEST_MS", defaultSlowRequestMs),
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getenvInt reads an integer env var, falling back to the default when unset,
// empty, or unparseable.
func getenvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
