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
