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

// ---- receipt breakdown keys (M13) ----

// Keys stamped onto the immutable receipt breakdown jsonb at creation. The
// pricing components (base/distance_component/...) are copied verbatim from the
// trip's fare; these add the payment identity plus the trip metrics needed for
// a detailed, itemised receipt. All additive — older receipts may lack the
// M13 metric keys, which the frontend handles gracefully.
const (
	receiptKeyMethod    = "method"
	receiptKeyRideID    = "ride_id"
	receiptKeyPaymentID = "payment_id"
	receiptKeyDistanceM = "distance_m"
	receiptKeyDurationS = "duration_s"
	receiptKeyStartedAt = "started_at"
	receiptKeyEndedAt   = "ended_at"
)

// ---- event types ----

const eventPaymentUpdated = "payment.updated"

// ---- custom metrics (New Relic; see Service.obs) ----

const (
	metricPaymentSucceeded = "Custom/Payments/Succeeded"
	metricPaymentFailed    = "Custom/Payments/Failed"
)

// ---- log messages ----

const (
	logMsgPublishPaymentUpdatedFailed = "payments: publish payment.updated failed"
	logMsgPostWebhookFailed           = "psp: post webhook failed"
)
