// Package observability wires New Relic APM into GoRide: application
// startup, HTTP transaction naming/tracing, and the seams datastore
// integrations (nrpgx5, nrredis-v9) and domain packages use to attach
// segments/custom metrics to the in-flight transaction.
//
// Nil-safety is the whole design: every exported *newrelic.Application and
// *newrelic.Transaction method the go-agent v3 API exposes is documented
// nil-safe (a nil receiver makes the call a no-op), so New returns a plain
// nil *newrelic.Application when monitoring is disabled (GORIDE_NEWRELIC_LICENSE
// unset) or fails to start, and every other piece of this package — and every
// call site elsewhere in the codebase that holds an *newrelic.Application —
// works unchanged against that nil value. There is no separate "disabled"
// code path to keep in sync.
package observability

import (
	"log/slog"

	"github.com/newrelic/go-agent/v3/newrelic"

	"github.com/lokeshbm/goride/internal/config"
)

// New starts the New Relic Go agent from cfg. When GORIDE_NEWRELIC_LICENSE is
// empty, monitoring is disabled per SPEC: this logs that fact and returns nil.
// A nil *newrelic.Application is safe to use everywhere an app is expected —
// StartTransaction, RecordCustomMetric, RecordCustomEvent, and Shutdown all
// no-op on a nil receiver — so the rest of the service runs identically with
// or without a license key.
//
// A malformed config (e.g. NewApplication rejects it outright) is treated the
// same way: logged and disabled, rather than failing startup. An
// accepted-but-invalid license key (right shape, wrong value) is NOT caught
// here — the agent connects and harvests asynchronously in the background, so
// that failure surfaces only as harvest errors in the agent's own logs, never
// as a startup failure or a behavioral change.
func New(cfg config.Config, log *slog.Logger) *newrelic.Application {
	if cfg.NewRelicLicense == "" {
		log.Info(logMsgMonitoringDisabled)
		return nil
	}

	app, err := newrelic.NewApplication(
		newrelic.ConfigAppName(cfg.NewRelicAppName),
		newrelic.ConfigLicense(cfg.NewRelicLicense),
		newrelic.ConfigDistributedTracerEnabled(true),
	)
	if err != nil {
		log.Warn(logMsgAppInitFailed, "error", err)
		return nil
	}

	log.Info(logMsgAppStarted, "app_name", cfg.NewRelicAppName)
	return app
}
