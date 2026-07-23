package httpapi

// API error codes: the stable SNAKE_CASE vocabulary of the `{"error":
// {"code": ...}}` envelope (SPEC "Errors over HTTP"). This is the single
// authoritative list — handlers never write a code string inline.
const (
	CodeUnauthorized            = "UNAUTHORIZED"
	CodeForbidden               = "FORBIDDEN"
	CodeValidationFailed        = "VALIDATION_FAILED"
	CodeInvalidState            = "INVALID_STATE"
	CodeRideAlreadyActive       = "RIDE_ALREADY_ACTIVE"
	CodeIdempotencyKeyRequired  = "IDEMPOTENCY_KEY_REQUIRED"
	CodeIdempotencyKeyReused    = "IDEMPOTENCY_KEY_REUSED"
	CodeQuoteNotFound           = "QUOTE_NOT_FOUND"
	CodeQuoteExpired            = "QUOTE_EXPIRED"
	CodeInvalidOTP              = "INVALID_OTP"
	CodeOfferExpired            = "OFFER_EXPIRED"
	CodeRateLimited             = "RATE_LIMITED"
	CodeInvalidSignature        = "INVALID_SIGNATURE"
	CodePaymentRetriesExhausted = "PAYMENT_RETRIES_EXHAUSTED"
	CodeNotFound                = "NOT_FOUND"
	CodeInternal                = "INTERNAL"
	CodeDependencyUnavailable   = "DEPENDENCY_UNAVAILABLE"
)
