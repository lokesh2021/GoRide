package trips

import "time"

// ---- domain constants ----

// Trip statuses (Postgres CHECK constraint on trips.status).
const (
	StatusStarted = "STARTED"
	StatusPaused  = "PAUSED"
	StatusEnded   = "ENDED"
)

// uniqueViolation is the Postgres SQLSTATE for a unique constraint breach
// (trips.ride_id is unique — one trip per ride).
const uniqueViolation = "23505"

// pausedAtTTL bounds the lifetime of the trip:paused_at mirror so an abandoned
// paused trip cannot leave a stale key forever (resume/end delete it).
const pausedAtTTL = 2 * time.Hour

// ---- event types ----

const eventRideStatusChanged = "ride.status_changed"

// ---- fare-event enrichment keys (M13) ----

// Added to the "fare" object of the ride.status_changed event published on trip
// end, so the rider's receipt view has the trip metrics (distance/duration/time
// span) immediately — before any payment/receipt row exists. The pricing
// components are copied from the fare breakdown; these are the additive extras.
const (
	fareKeyDistanceM = "distance_m"
	fareKeyDurationS = "duration_s"
	fareKeyStartedAt = "started_at"
	fareKeyEndedAt   = "ended_at"
)

// ---- Redis key prefixes/builders ----

func pausedAtKey(rideID string) string { return "trip:paused_at:" + rideID }

// ---- log messages ----

const (
	logMsgSetPausedAtFailed     = "trips: set paused_at failed"
	logMsgDriverReleaseFailed   = "trips: driver release failed"
	logMsgCacheInvalidateFailed = "trips: cache invalidate failed"
	logMsgPublishStatusFailed   = "trips: publish status failed"
	logMsgReadPausedAtFailed    = "trips: read paused_at failed"
)
