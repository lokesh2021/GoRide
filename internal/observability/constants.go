package observability

// ---- domain constants ----

// sseRoutePrefix is the path prefix for SSE event streams (GET /v1/events and
// GET /v1/events/driver/{id}). These are deliberately excluded from
// transaction tracing in Middleware: they are long-lived (potentially
// hours-long) connections, and a "transaction" spanning that long reports a
// meaningless duration and skews every latency percentile/apdex/throughput
// metric the agent computes. See Middleware's doc comment.
const sseRoutePrefix = "/v1/events"

// ---- log messages ----

const (
	logMsgMonitoringDisabled = "observability: monitoring disabled (GORIDE_NEWRELIC_LICENSE not set)"
	logMsgAppInitFailed      = "observability: failed to start New Relic application"
	logMsgAppStarted         = "observability: New Relic application started"
)
