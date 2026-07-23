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
	NewRelicAppName string // GORIDE_NEWRELIC_APP_NAME (optional; defaults to "goride")
	PSPSecret       string // GORIDE_PSP_SECRET
	PSPWebhookURL   string // GORIDE_PSP_WEBHOOK_URL (mock PSP posts confirmations here)
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
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
