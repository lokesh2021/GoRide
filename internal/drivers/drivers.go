// Package drivers owns the driver domain: availability transitions and the
// location-ingestion hot path, plus the Redis "mirrors" that let the matching
// engine and the location path avoid Postgres entirely.
//
// Source of truth: Postgres `drivers.status` is authoritative. The Redis
// mirrors (`driver:status:{id}`, `driver:ride:{id}`) are a rebuildable cache
// maintained at every place driver status changes (availability here,
// assignment/release in the matching engine). If Redis is flushed the mirrors
// are lost but the system self-heals: a driver simply re-goes-available (which
// rewrites the mirror) and the next ping re-GEOADDs.
package drivers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/lokeshbm/goride/internal/pricing"
	"github.com/lokeshbm/goride/internal/store"
)

// Domain errors, mapped to HTTP codes by the handler layer.
var (
	ErrNotFound     = errors.New("drivers: not found")
	ErrInvalidState = errors.New("drivers: invalid state")
	ErrRateLimited  = errors.New("drivers: rate limited")
)

// RidePublisher is the seam onto the rides/SSE event bus. The location hot path
// republishes a driver's position onto the ride channel while on an active
// ride. Defined locally (structural interface) so drivers need not import rides.
type RidePublisher interface {
	PublishRideEvent(ctx context.Context, rideID, eventType string, data any) error
}

// noopPublisher discards events; used until a real publisher is wired.
type noopPublisher struct{}

func (noopPublisher) PublishRideEvent(context.Context, string, string, any) error { return nil }

// statusMirror is the JSON stored at driver:status:{id}. It carries everything
// the matching search loop and the location path need about a driver without a
// Postgres round-trip: current status, tier (for tier-match filtering) and city
// (for the GEO key).
type statusMirror struct {
	Status string `json:"status"`
	Tier   string `json:"tier"`
	City   string `json:"city"`
}

// lastPosition is the JSON stored at driver:last:{id} (TTL 30s).
type lastPosition struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
	Ts  int64   `json:"ts"`
}

// Service is the driver domain service.
type Service struct {
	st  *store.Store
	log *slog.Logger
	pub RidePublisher
}

// NewService constructs a driver Service with a no-op ride publisher.
func NewService(st *store.Store, log *slog.Logger) *Service {
	return &Service{st: st, log: log, pub: noopPublisher{}}
}

// SetPublisher overrides the no-op ride publisher (used for ride-location
// republishing; M5 wires the real SSE hub).
func (s *Service) SetPublisher(p RidePublisher) { s.pub = p }

// meteredPingDelta returns the haversine distance (metres) between the previous
// and current fix and whether it should count toward metered trip distance. A
// delta above maxPingDeltaM is a teleport / spurious GPS fix and is rejected
// (count=false) so it does not corrupt the metered distance.
func meteredPingDelta(prevLat, prevLng, lat, lng float64) (float64, bool) {
	d := pricing.Haversine(prevLat, prevLng, lat, lng)
	return d, d <= maxPingDeltaM
}

// ---- Availability ----

// SetAvailability transitions a driver between offline and available. on_trip is
// rejected (ErrInvalidState → 409). The Postgres UPDATE is optimistic and
// idempotent: offline⇄available is allowed (re-asserting the same state is a
// no-op), on_trip fails the WHERE clause and yields zero rows.
//
// Going available: refresh the status mirror, then GEOADD at the last known
// position if it is still fresh; otherwise just mark available and let the first
// ping GEOADD. Going offline: ZREM from the geo set and clear any outstanding
// offer (decline semantics).
func (s *Service) SetAvailability(ctx context.Context, driverID string, available bool) error {
	target := StatusOffline
	if available {
		target = StatusAvailable
	}

	// Optimistic guarded update: offline/available are freely mutable; on_trip
	// is locked out ONLY while an active ride actually exists. The NOT EXISTS
	// arm self-heals orphaned on_trip drivers (crash/cleanup between a ride
	// reaching a terminal state and the driver release) instead of dead-ending
	// them behind a 409 forever. RETURNING gives us tier+city for the Redis
	// ops without a second query.
	var tier, city string
	err := s.st.PG.QueryRow(ctx,
		`UPDATE drivers SET status = $1
		   WHERE id = $2
		     AND (status IN ('offline','available')
		          OR (status = 'on_trip' AND NOT EXISTS (
		                SELECT 1 FROM rides
		                WHERE driver_id = $2
		                  AND status IN ('REQUESTED','MATCHING','DRIVER_ASSIGNED',
		                                 'DRIVER_ARRIVING','ARRIVED','IN_PROGRESS'))))
		 RETURNING tier, city`,
		target, driverID,
	).Scan(&tier, &city)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.availabilityRejectReason(ctx, driverID)
	}
	if err != nil {
		return fmt.Errorf("drivers: set availability: %w", err)
	}

	if available {
		return s.goAvailable(ctx, driverID, tier, city)
	}
	return s.goOffline(ctx, driverID, tier, city)
}

// availabilityRejectReason distinguishes a missing driver (404) from an on_trip
// driver (409) after the guarded UPDATE affected zero rows.
func (s *Service) availabilityRejectReason(ctx context.Context, driverID string) error {
	var status string
	err := s.st.PG.QueryRow(ctx, `SELECT status FROM drivers WHERE id = $1`, driverID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("drivers: availability reason: %w", err)
	}
	// status must be on_trip (offline/available would have matched the UPDATE).
	return ErrInvalidState
}

func (s *Service) goAvailable(ctx context.Context, driverID, tier, city string) error {
	// Drop any stale active-ride marker: reaching here means Postgres agreed
	// the driver has no active ride (incl. the orphaned-on_trip heal path),
	// so a lingering driver:ride mirror is by definition stale.
	if err := s.st.Redis.Del(ctx, rideKey(driverID)).Err(); err != nil {
		s.log.Warn(logMsgClearRideMarkerFailed, "error", err, "driver_id", driverID)
	}
	// Refresh the mirror first so a ping racing in immediately sees available.
	if err := s.writeMirror(ctx, driverID, statusMirror{Status: StatusAvailable, Tier: tier, City: city}); err != nil {
		return err
	}
	// GEOADD only if we have a fresh last position; existence of the 30s-TTL key
	// is the freshness signal.
	raw, err := s.st.Redis.Get(ctx, lastKey(driverID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil // no fresh fix; first ping will GEOADD
	}
	if err != nil {
		return fmt.Errorf("drivers: read last pos: %w", err)
	}
	var pos lastPosition
	if json.Unmarshal([]byte(raw), &pos) != nil {
		return nil
	}
	if err := s.st.Redis.GeoAdd(ctx, geoKey(city), &redis.GeoLocation{
		Name: driverID, Longitude: pos.Lng, Latitude: pos.Lat,
	}).Err(); err != nil {
		return fmt.Errorf("drivers: geoadd on available: %w", err)
	}
	return nil
}

func (s *Service) goOffline(ctx context.Context, driverID, tier, city string) error {
	// Clear any outstanding offer (decline semantics) and remove from geo set.
	if err := s.clearOutstandingOffer(ctx, driverID); err != nil {
		s.log.Warn(logMsgClearOfferOnOfflineFailed, "error", err, "driver_id", driverID)
	}
	pipe := s.st.Redis.Pipeline()
	pipe.ZRem(ctx, geoKey(city), driverID)
	// Same staleness argument as goAvailable: no active ride at this point.
	pipe.Del(ctx, rideKey(driverID))
	// Keep the mirror (now offline) so the location path knows not to GEOADD.
	m, _ := json.Marshal(statusMirror{Status: StatusOffline, Tier: tier, City: city})
	pipe.Set(ctx, statusKey(driverID), m, 0)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("drivers: go offline redis: %w", err)
	}
	return nil
}

// clearOutstandingOffer removes this driver's offer keys if one is held,
// mirroring decline semantics when the driver goes offline.
func (s *Service) clearOutstandingOffer(ctx context.Context, driverID string) error {
	rideID, err := s.st.Redis.GetDel(ctx, offerDriver(driverID)).Result()
	if errors.Is(err, redis.Nil) || rideID == "" {
		return nil
	}
	if err != nil {
		return err
	}
	return s.st.Redis.Del(ctx, offerRide(rideID)).Err()
}

// ---- Mirrors used by the matching engine ----

// MarkOnTrip records assignment in Redis: remove the driver from the geo set,
// point driver:ride at the ride, and flip the status mirror to on_trip. Called
// by the matching engine after the assignment transaction commits.
func (s *Service) MarkOnTrip(ctx context.Context, driverID, rideID, city, tier string) error {
	pipe := s.st.Redis.Pipeline()
	pipe.ZRem(ctx, geoKey(city), driverID)
	pipe.Set(ctx, rideKey(driverID), rideID, 0)
	m, _ := json.Marshal(statusMirror{Status: StatusOnTrip, Tier: tier, City: city})
	pipe.Set(ctx, statusKey(driverID), m, 0)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("drivers: mark on_trip: %w", err)
	}
	return nil
}

// Release re-adds a driver to the available pool after their ride ends or is
// cancelled (the rides service has already set Postgres status='available').
// It reads tier/city from Postgres (not a hot path), refreshes the mirror,
// clears driver:ride, and GEOADDs the last known position if still fresh.
func (s *Service) Release(ctx context.Context, driverID string) error {
	var tier, city, status string
	err := s.st.PG.QueryRow(ctx,
		`SELECT tier, city, status FROM drivers WHERE id = $1`, driverID,
	).Scan(&tier, &city, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("drivers: release load: %w", err)
	}

	pipe := s.st.Redis.Pipeline()
	pipe.Del(ctx, rideKey(driverID))
	m, _ := json.Marshal(statusMirror{Status: status, Tier: tier, City: city})
	pipe.Set(ctx, statusKey(driverID), m, 0)
	lastCmd := pipe.Get(ctx, lastKey(driverID))
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("drivers: release redis: %w", err)
	}

	// GEOADD only when available and we still have a fresh fix.
	if status != StatusAvailable {
		return nil
	}
	raw, err := lastCmd.Result()
	if errors.Is(err, redis.Nil) {
		return nil
	}
	if err != nil {
		return nil
	}
	var pos lastPosition
	if json.Unmarshal([]byte(raw), &pos) != nil {
		return nil
	}
	if err := s.st.Redis.GeoAdd(ctx, geoKey(city), &redis.GeoLocation{
		Name: driverID, Longitude: pos.Lng, Latitude: pos.Lat,
	}).Err(); err != nil {
		return fmt.Errorf("drivers: release geoadd: %w", err)
	}
	return nil
}

// ReadAndClearTripDistance returns the metered distance (metres, rounded)
// accumulated in trip:dist:{ride_id} by the location hot path, then deletes the
// counter. ok is false when the counter is missing or non-positive, signalling
// the caller to fall back to the quote's estimated distance. Called by trips.End
// (not a hot path).
func (s *Service) ReadAndClearTripDistance(ctx context.Context, rideID string) (int, bool) {
	raw, err := s.st.Redis.Get(ctx, tripDistKey(rideID)).Float64()
	if err != nil {
		// redis.Nil (never accumulated) or a parse/read error: fall back.
		return 0, false
	}
	if err := s.st.Redis.Del(ctx, tripDistKey(rideID)).Err(); err != nil {
		s.log.Warn(logMsgClearTripDistanceFailed, "error", err, "ride_id", rideID)
	}
	if raw <= 0 {
		return 0, false
	}
	return int(raw + 0.5), true
}

func (s *Service) writeMirror(ctx context.Context, driverID string, m statusMirror) error {
	raw, _ := json.Marshal(m)
	if err := s.st.Redis.Set(ctx, statusKey(driverID), raw, 0).Err(); err != nil {
		return fmt.Errorf("drivers: write status mirror: %w", err)
	}
	return nil
}

// ---- Location ingestion (hot path) ----

// UpdateLocation is the location-ingestion hot path: zero Postgres, two Redis
// round-trips.
//
// Round-trip 1 (pipeline): token-bucket rate limit + read the status/ride
// mirrors + the previous fix (driver:last). Rate limiting uses a per-second
// INCR key (INCR + EXPIRE) rather than a sliding token bucket — it is the
// simplest correct form: the Nth ping within a given wall-clock second
// increments the counter to N, and anything past maxPingsPerSec is rejected.
// Bucket boundaries reset each second, which is exactly the "max 3/sec"
// contract. Reading driver:last here (before it is overwritten in rt2) lets us
// meter the inter-ping distance without adding a round-trip.
//
// Round-trip 2 (pipeline): SET driver:last (EX 30); GEOADD to the city set iff
// the mirror says available; iff on an active ride, claim a 1/sec publish token
// so the position is republished onto the ride channel at most once per second
// for rider tracking; and, also iff on an active ride, accumulate the haversine
// delta from the previous fix into trip:dist:{ride_id} (teleport-filtered) for
// actual-distance fare finalization.
func (s *Service) UpdateLocation(ctx context.Context, driverID string, lat, lng float64) error {
	now := time.Now()
	sec := now.Unix()

	// --- round-trip 1: rate limit + mirror reads + previous position ---
	p1 := s.st.Redis.Pipeline()
	incr := p1.Incr(ctx, rateKey(driverID, sec))
	p1.Expire(ctx, rateKey(driverID, sec), rateBucketTTL)
	statusCmd := p1.Get(ctx, statusKey(driverID))
	rideCmd := p1.Get(ctx, rideKey(driverID))
	lastCmd := p1.Get(ctx, lastKey(driverID))
	if _, err := p1.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("drivers: location rt1: %w", err)
	}
	if incr.Val() > maxPingsPerSec {
		return ErrRateLimited
	}

	var mirror statusMirror
	if raw, err := statusCmd.Result(); err == nil {
		_ = json.Unmarshal([]byte(raw), &mirror)
	}
	rideID := rideCmd.Val() // "" if not on a ride
	onRide := rideID != ""

	// Inter-ping distance delta from the previous fix, teleport-filtered. Only
	// meaningful while on an active ride; computed before rt2 overwrites last.
	var prev lastPosition
	havePrev := false
	if raw, err := lastCmd.Result(); err == nil {
		havePrev = json.Unmarshal([]byte(raw), &prev) == nil
	}

	// --- round-trip 2: persist position + conditional GEOADD + publish token + metered distance ---
	posJSON, _ := json.Marshal(lastPosition{Lat: lat, Lng: lng, Ts: sec})
	p2 := s.st.Redis.Pipeline()
	p2.Set(ctx, lastKey(driverID), posJSON, lastTTL)
	if mirror.Status == StatusAvailable && mirror.City != "" {
		p2.GeoAdd(ctx, geoKey(mirror.City), &redis.GeoLocation{
			Name: driverID, Longitude: lng, Latitude: lat,
		})
	}
	var pubTokenCmd *redis.BoolCmd
	if onRide {
		pubTokenCmd = p2.SetNX(ctx, locPubKey(rideID, sec), "1", locPubTTL)
		if havePrev {
			// Teleport (GPS jump) deltas are excluded from metered distance.
			if d, count := meteredPingDelta(prev.Lat, prev.Lng, lat, lng); count {
				p2.IncrByFloat(ctx, tripDistKey(rideID), d)
				p2.Expire(ctx, tripDistKey(rideID), tripDistTTL)
			}
		}
	}
	if _, err := p2.Exec(ctx); err != nil {
		return fmt.Errorf("drivers: location rt2: %w", err)
	}

	// Publish at most once per second onto the ride channel (default no-op).
	if onRide && pubTokenCmd != nil && pubTokenCmd.Val() {
		if err := s.pub.PublishRideEvent(ctx, rideID, eventRideDriverLocation, map[string]any{
			"driver_id": driverID,
			"lat":       lat,
			"lng":       lng,
		}); err != nil {
			s.log.Warn(logMsgPublishLocationFailed, "error", err, "ride_id", rideID)
		}
	}
	return nil
}
