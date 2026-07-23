package config

import "testing"

// clearEnv unsets (via empty-string t.Setenv, indistinguishable from unset to
// os.Getenv/getenv's own "" check) every GORIDE_* var Load reads, so each test
// starts from a known-default state regardless of the ambient environment.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GORIDE_ADDR", "GORIDE_PG_DSN", "GORIDE_REDIS_ADDR", "GORIDE_ENV",
		"GORIDE_NEWRELIC_LICENSE", "GORIDE_NEWRELIC_APP_NAME", "GORIDE_PSP_SECRET",
		"GORIDE_PSP_WEBHOOK_URL", "GORIDE_LOG_LEVEL", "GORIDE_SLOW_REQUEST_MS",
	} {
		t.Setenv(k, "")
	}
}

func TestGetenv(t *testing.T) {
	tests := []struct {
		name     string
		set      bool
		value    string
		fallback string
		want     string
	}{
		{"unset uses fallback", false, "", "fallback", "fallback"},
		{"empty uses fallback", true, "", "fallback", "fallback"},
		{"set overrides fallback", true, "custom-value", "fallback", "custom-value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const key = "GORIDE_TEST_GETENV_KEY"
			if tt.set {
				t.Setenv(key, tt.value)
			} else {
				t.Setenv(key, "")
			}
			if got := getenv(key, tt.fallback); got != tt.want {
				t.Fatalf("getenv(%q, %q) = %q, want %q", key, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestGetenvInt(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		set      bool
		fallback int
		want     int
	}{
		{"unset uses fallback", "", false, 42, 42},
		{"empty uses fallback", "", true, 42, 42},
		{"valid int parses", "100", true, 42, 100},
		{"negative int parses", "-5", true, 42, -5},
		{"zero parses", "0", true, 42, 0},
		{"non-numeric uses fallback", "not-a-number", true, 42, 42},
		{"float-looking string uses fallback", "3.14", true, 42, 42},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const key = "GORIDE_TEST_GETENVINT_KEY"
			if tt.set {
				t.Setenv(key, tt.value)
			} else {
				t.Setenv(key, "")
			}
			if got := getenvInt(key, tt.fallback); got != tt.want {
				t.Fatalf("getenvInt(%q, %d) = %d, want %d", key, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)

	got := Load()
	want := Config{
		Addr:            defaultAddr,
		PGDSN:           "",
		RedisAddr:       defaultRedisAddr,
		Env:             defaultEnv,
		NewRelicLicense: "",
		NewRelicAppName: defaultNewRelicAppName,
		PSPSecret:       defaultPSPSecret,
		PSPWebhookURL:   defaultPSPWebhookURL,
		LogLevel:        defaultLogLevel,
		SlowRequestMs:   defaultSlowRequestMs,
	}
	if got != want {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}
}

func TestLoadFromEnv(t *testing.T) {
	clearEnv(t)

	t.Setenv("GORIDE_ADDR", ":9090")
	t.Setenv("GORIDE_PG_DSN", "postgres://u@h:5432/db")
	t.Setenv("GORIDE_REDIS_ADDR", "redis-host:6379")
	t.Setenv("GORIDE_ENV", "prod")
	t.Setenv("GORIDE_NEWRELIC_LICENSE", "test-license-key")
	t.Setenv("GORIDE_NEWRELIC_APP_NAME", "goride-prod")
	t.Setenv("GORIDE_PSP_SECRET", "super-secret")
	t.Setenv("GORIDE_PSP_WEBHOOK_URL", "https://example.test/webhook")
	t.Setenv("GORIDE_LOG_LEVEL", "debug")
	t.Setenv("GORIDE_SLOW_REQUEST_MS", "500")

	got := Load()
	want := Config{
		Addr:            ":9090",
		PGDSN:           "postgres://u@h:5432/db",
		RedisAddr:       "redis-host:6379",
		Env:             "prod",
		NewRelicLicense: "test-license-key",
		NewRelicAppName: "goride-prod",
		PSPSecret:       "super-secret",
		PSPWebhookURL:   "https://example.test/webhook",
		LogLevel:        "debug",
		SlowRequestMs:   500,
	}
	if got != want {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}
}

func TestLoadSlowRequestMsFallsBackOnBadValue(t *testing.T) {
	clearEnv(t)
	t.Setenv("GORIDE_SLOW_REQUEST_MS", "not-an-int")

	got := Load()
	if got.SlowRequestMs != defaultSlowRequestMs {
		t.Fatalf("SlowRequestMs = %d, want default %d", got.SlowRequestMs, defaultSlowRequestMs)
	}
}
