package rides

import "time"

// ---- domain constants ----

// Roles used for authorization.
const (
	RoleRider  = "rider"
	RoleDriver = "driver"
)

const (
	cacheTTL = 60 * time.Second
	// uniqueViolation is the Postgres SQLSTATE for a unique constraint breach.
	uniqueViolation = "23505"
)

// ---- event types ----

const eventRideStatusChanged = "ride.status_changed"

// ---- Redis key prefixes/builders ----

const cacheKeyPrefix = "ride:cache:"

func cacheKey(id string) string { return cacheKeyPrefix + id }

// ---- log messages ----

const (
	logMsgCacheInvalidateFailed = "rides: cache invalidate failed"
	logMsgPublishEventFailed    = "rides: publish event failed"
	logMsgCacheSetFailed        = "rides: cache set failed"
)
