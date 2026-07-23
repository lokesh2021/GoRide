package config

// ---- domain constants ----

// Default values applied when the corresponding GORIDE_* env var is unset.
const (
	defaultAddr            = ":8080"
	defaultRedisAddr       = "localhost:6379"
	defaultEnv             = "dev"
	defaultPSPSecret       = "dev-psp-secret"
	defaultPSPWebhookURL   = "http://localhost:8080/v1/webhooks/psp"
	defaultNewRelicAppName = "goride"
)
