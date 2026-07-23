package payments

// ---- domain constants ----

// Payment statuses (Postgres CHECK constraint on payments.status).
const (
	StatusPending    = "PENDING"
	StatusProcessing = "PROCESSING"
	StatusSucceeded  = "SUCCEEDED"
	StatusFailed     = "FAILED"
)

// maxRetries caps FAILED → PROCESSING re-triggers (SPEC: max 3).
const maxRetries = 3

// historyLimit caps ride history to the most recent N (SPEC/task: 20).
const historyLimit = 20

// Webhook status literals (the PSP callback vocabulary, distinct from the
// PENDING/PROCESSING/... payment statuses).
const (
	pspSuccess = "success"
	pspFailure = "failure"
)

const (
	// jitterMinMs..jitterMaxMs is the async confirmation delay window (SPEC:
	// 300–800ms).
	jitterMinMs = 300
	jitterMaxMs = 800
	// successPercent is the mock approval rate (SPEC: 90%).
	successPercent = 90
)

// ---- event types ----

const eventPaymentUpdated = "payment.updated"

// ---- log messages ----

const (
	logMsgPublishPaymentUpdatedFailed = "payments: publish payment.updated failed"
	logMsgPostWebhookFailed           = "psp: post webhook failed"
)
