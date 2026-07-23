package drivers

import (
	"fmt"
	"time"
)

// ---- domain constants ----

// Driver status values (Postgres CHECK constraint + Redis mirror).
const (
	StatusOffline   = "offline"
	StatusAvailable = "available"
	StatusOnTrip    = "on_trip"
)

const (
	// maxPingsPerSec caps location pings per driver (SPEC: 3/sec).
	maxPingsPerSec = 3
	// lastTTL is the freshness window for a driver's last known position.
	lastTTL = 30 * time.Second
	// locPubTTL guards the per-second ride-location publish throttle key.
	locPubTTL = 2 * time.Second
	// rateBucketTTL keeps the per-second rate-limit key just long enough to
	// span the one-second window it counts (a little slack for clock skew).
	rateBucketTTL = 2 * time.Second
	// tripDistTTL bounds the lifetime of the per-ride metered-distance counter
	// so an abandoned trip's key cannot linger forever (trip end DELs it).
	tripDistTTL = 2 * time.Hour
	// maxPingDeltaM is the teleport filter: a single ping cannot legitimately
	// move a city driver more than ~200m within its (sub-)second cadence, so a
	// larger jump is a spurious GPS fix and is excluded from metered distance.
	maxPingDeltaM = 200.0
)

// ---- event types ----

const eventRideDriverLocation = "ride.driver_location"

// ---- Redis key prefixes/builders ----

func geoKey(city string) string    { return "geo:drivers:" + city }
func lastKey(id string) string     { return "driver:last:" + id }
func statusKey(id string) string   { return "driver:status:" + id }
func rideKey(id string) string     { return "driver:ride:" + id }
func offerDriver(id string) string { return "offer:driver:" + id }
func offerRide(id string) string   { return "offer:ride:" + id }

func rateKey(id string, sec int64) string {
	return fmt.Sprintf("ratelimit:loc:%s:%d", id, sec)
}

func locPubKey(rideID string, sec int64) string {
	return fmt.Sprintf("loc:pub:%s:%d", rideID, sec)
}

func tripDistKey(rideID string) string { return "trip:dist:" + rideID }

// ---- log messages ----

const (
	logMsgClearOfferOnOfflineFailed = "drivers: clear offer on offline failed"
	logMsgClearRideMarkerFailed     = "drivers: clear stale ride marker failed"
	logMsgClearTripDistanceFailed   = "drivers: clear trip distance failed"
	logMsgPublishLocationFailed     = "drivers: publish location failed"
)
