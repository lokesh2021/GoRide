// Package config loads GoRide's runtime configuration from environment
// variables, per docs/SPEC.md "Conventions".
package config

import "os"

// Config holds all env-derived settings for the service.
type Config struct {
	Addr            string // GORIDE_ADDR
	PGDSN           string // GORIDE_PG_DSN
	RedisAddr       string // GORIDE_REDIS_ADDR
	Env             string // GORIDE_ENV
	NewRelicLicense string // GORIDE_NEWRELIC_LICENSE (optional; agent disabled when empty)
	PSPSecret       string // GORIDE_PSP_SECRET
}

// Load reads configuration from the environment, applying documented defaults.
func Load() Config {
	return Config{
		Addr:            getenv("GORIDE_ADDR", ":8080"),
		PGDSN:           os.Getenv("GORIDE_PG_DSN"),
		RedisAddr:       getenv("GORIDE_REDIS_ADDR", "localhost:6379"),
		Env:             getenv("GORIDE_ENV", "dev"),
		NewRelicLicense: os.Getenv("GORIDE_NEWRELIC_LICENSE"),
		PSPSecret:       getenv("GORIDE_PSP_SECRET", "dev-psp-secret"),
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
