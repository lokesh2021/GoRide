package matching

import "time"

// ---- domain constants ----

const (
	searchRadiusKm = 5.0
	searchLimit    = 10
	offerTTL       = 12 * time.Second
	triedTTL       = 10 * time.Minute
	matchDeadline  = 60 * time.Second
	sweepInterval  = 2 * time.Second
	sweepBatch     = 50
	// uniqueViolation is the Postgres SQLSTATE for a unique constraint breach
	// (the partial one-active-ride-per-driver index).
	uniqueViolation = "23505"
)

// ---- custom metrics (New Relic; see Engine.obs) ----

const (
	// metricOfferLatencyMs is request→offer latency: ride created_at to the
	// first offer claimed for it, recorded in offerNext.
	metricOfferLatencyMs = "Custom/Matching/OfferLatencyMs"
	metricOfferAccepted  = "Custom/Matching/OfferAccepted"
	metricOfferDeclined  = "Custom/Matching/OfferDeclined"
	// metricOfferExpired counts rides whose matching window expired with no
	// driver ever accepting (sweep's matchDeadline branch) — a per-ride
	// outcome, distinct from the per-offer accepted/declined counters above.
	metricOfferExpired = "Custom/Matching/OfferExpired"
)

// ---- event types ----

const (
	eventRideOffer         = "ride.offer"
	eventRideStatusChanged = "ride.status_changed"
	eventRideOTP           = "ride.otp"
)

// ---- Redis key prefixes/builders ----

func geoKey(city string) string    { return "geo:drivers:" + city }
func lastKey(id string) string     { return "driver:last:" + id }
func statusKey(id string) string   { return "driver:status:" + id }
func offerDriver(id string) string { return "offer:driver:" + id }
func offerRide(id string) string   { return "offer:ride:" + id }
func triedKey(id string) string    { return "offered:ride:" + id }

// ---- log messages ----

const (
	logMsgReadCandidateFailed     = "matching: read candidate failed"
	logMsgPublishOfferFailed      = "matching: publish offer failed"
	logMsgOfferedRide             = "matching: offered ride"
	logMsgRequestMatchLoadFailed  = "matching: request match load failed"
	logMsgRequestMatchOfferFailed = "matching: request match offer failed"
	logMsgLoadDriverCardFailed    = "matching: load driver card failed"
	logMsgMarkOnTripFailed        = "matching: mark on_trip failed"
	logMsgCacheInvalidateFailed   = "matching: cache invalidate failed"
	logMsgDelOfferRideFailed      = "matching: del offer:ride failed"
	logMsgPublishStatusFailed     = "matching: publish status failed"
	logMsgPublishOTPFailed        = "matching: publish otp failed"
	logMsgRideAssigned            = "matching: ride assigned"
	logMsgDeclineAdvanceFailed    = "matching: decline advance failed"
	logMsgSweeperStarted          = "matching: sweeper started"
	logMsgSweeperStopped          = "matching: sweeper stopped"
	logMsgSweepClaimFailed        = "matching: sweep claim failed"
	logMsgExpireFailed            = "matching: expire failed"
	logMsgSweepExistsFailed       = "matching: sweep exists failed"
	logMsgSweepOfferFailed        = "matching: sweep offer failed"
)
