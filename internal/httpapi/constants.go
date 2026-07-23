package httpapi

// ---- domain constants ----

// maxIdempotentBody caps the request body we buffer for hashing/replay.
const maxIdempotentBody = 1 << 20 // 1 MiB

// maxWebhookBody caps the PSP webhook body we buffer for HMAC verification.
const maxWebhookBody = 1 << 16 // 64 KiB

// sseRoutePathPrefix is the path prefix for the long-lived SSE event streams
// (GET /v1/events and GET /v1/events/driver/{id}). requestLogger never
// slow-warns on these: they are open for the life of the connection, so their
// duration_ms is expected to be large and is not a latency signal. Mirrors
// observability.sseRoutePrefix (kept local — constants live with their owner).
const sseRoutePathPrefix = "/v1/events"

// ---- log messages ----

const (
	logMsgAuthResolveTokenFailed = "auth: resolve token failed"
	logMsgCreateQuoteFailed      = "createQuote failed"
	logMsgIdempotencyLoadFailed  = "idempotency: load failed"
	logMsgIdempotencyStoreFailed = "idempotency: store failed"
	logMsgStreamRideEventsFailed = "streamRideEvents load failed"
	logMsgEventsStreamEnded      = "events: stream ended"
	logMsgUpdateLocationFailed   = "updateLocation failed"
	logMsgSetAvailabilityFailed  = "setAvailability failed"
	logMsgAcceptOfferFailed      = "acceptOffer failed"
	logMsgDeclineOfferFailed     = "declineOffer failed"
	logMsgPspWebhookFailed       = "pspWebhook failed"
	logMsgRiderHistoryFailed     = "riderHistory failed"
	logMsgStateLookupFailed      = "state lookup failed"
	logMsgOTPRegenFailed         = "otp regeneration failed"
	logMsgHTTPRequest            = "http_request"
)
