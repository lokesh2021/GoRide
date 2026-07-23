//go:build integration

// Integration tests for the matching engine: the offer loop (candidate search +
// atomic offer:driver claim), Accept (GETDEL ownership → single-tx assignment,
// replay, offer-expired), Decline (advance to next candidate), and the sweeper
// (offer a fresh MATCHING ride / expire a stale one). These hit real Postgres +
// Redis via the shared testsupport fixture, so they live in the external test
// package (testsupport imports matching, an in-package cycle otherwise).
package matching_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/lokeshbm/goride/internal/matching"
	"github.com/lokeshbm/goride/internal/testsupport"
)

// ---- Redis key builders (mirror matching's unexported builders; the SPEC
// Redis key contract, asserted by literal shape). ----

func geoKey() string                  { return "geo:drivers:" + testsupport.City }
func offerDriverKey(id string) string { return "offer:driver:" + id }
func offerRideKey(id string) string   { return "offer:ride:" + id }
func triedKey(id string) string       { return "offered:ride:" + id }

// pickup coordinates InsertRide hardcodes.
const pickupLat, pickupLng = 12.9716, 77.5946

// sweepTickWait is a hair longer than the engine's 2s sweep interval, so a
// Start()ed sweeper is guaranteed to have ticked once.
const sweepTickWait = 2500 * time.Millisecond

// seedCandidate inserts an available driver of the given tier and makes it a
// live matching candidate: fresh driver:last + available mirror + membership in
// the city geo set at the given position (SetAvailability GEOADDs off the fresh
// fix).
func seedCandidate(t *testing.T, f *testsupport.Fixture, tier string, lat, lng float64) string {
	t.Helper()
	id, _ := f.InsertDriver(tier, "available")
	raw, _ := json.Marshal(map[string]any{"lat": lat, "lng": lng, "ts": time.Now().Unix()})
	if err := f.Store.Redis.Set(f.Ctx, "driver:last:"+id, raw, 30*time.Second).Err(); err != nil {
		t.Fatalf("seed driver:last: %v", err)
	}
	if err := f.Drivers.SetAvailability(f.Ctx, id, true); err != nil {
		t.Fatalf("SetAvailability: %v", err)
	}
	return id
}

// matchingRide inserts a rider+quote+MATCHING ride and returns the ride id.
func matchingRide(t *testing.T, f *testsupport.Fixture, tier string) string {
	t.Helper()
	riderID, _ := f.InsertRider()
	quoteID := f.InsertQuote(riderID)
	return f.InsertRide(riderID, quoteID, tier, "MATCHING", nil, nil)
}

func offerHolder(t *testing.T, f *testsupport.Fixture, driverID string) string {
	t.Helper()
	v, err := f.Store.Redis.Get(f.Ctx, offerDriverKey(driverID)).Result()
	if err == redis.Nil {
		return ""
	}
	if err != nil {
		t.Fatalf("read offer:driver: %v", err)
	}
	return v
}

// ---- offer loop ----

func TestRequestMatch_ClaimsOfferForFreshCandidate(t *testing.T) {
	f := testsupport.New(t)
	driverID := seedCandidate(t, f, "mini", pickupLat, pickupLng)
	rideID := matchingRide(t, f, "mini")

	f.Match.RequestMatch(f.Ctx, rideID)

	if got := offerHolder(t, f, driverID); got != rideID {
		t.Fatalf("offer:driver:%s = %q, want %q", driverID, got, rideID)
	}
	// offer:ride marker written for the sweeper/decline path.
	if n, _ := f.Store.Redis.Exists(f.Ctx, offerRideKey(rideID)).Result(); n != 1 {
		t.Fatalf("offer:ride:%s should exist after an offer", rideID)
	}
	// Candidate recorded in the tried-set.
	if !f.Store.Redis.SIsMember(f.Ctx, triedKey(rideID), driverID).Val() {
		t.Fatalf("driver should be in the tried-set after being offered")
	}
}

func TestRequestMatch_TierMismatch_NoOffer(t *testing.T) {
	f := testsupport.New(t)
	// Only a sedan candidate available for a mini ride ⇒ ineligible.
	driverID := seedCandidate(t, f, "sedan", pickupLat, pickupLng)
	rideID := matchingRide(t, f, "mini")

	f.Match.RequestMatch(f.Ctx, rideID)

	if got := offerHolder(t, f, driverID); got != "" {
		t.Fatalf("tier-mismatched driver should not be offered, got %q", got)
	}
}

func TestRequestMatch_NotMatching_NoOp(t *testing.T) {
	f := testsupport.New(t)
	driverID := seedCandidate(t, f, "mini", pickupLat, pickupLng)
	riderID, _ := f.InsertRider()
	quoteID := f.InsertQuote(riderID)
	// Ride already past MATCHING ⇒ RequestMatch is a no-op.
	rideID := f.InsertRide(riderID, quoteID, "mini", "DRIVER_ARRIVING", &driverID, nil)

	f.Match.RequestMatch(f.Ctx, rideID)

	if got := offerHolder(t, f, driverID); got != "" {
		t.Fatalf("non-MATCHING ride should not produce an offer, got %q", got)
	}
}

// ---- Accept ----

func TestAccept_AssignsRideAndDriver(t *testing.T) {
	f := testsupport.New(t)
	driverID := seedCandidate(t, f, "mini", pickupLat, pickupLng)
	rideID := matchingRide(t, f, "mini")
	f.Match.RequestMatch(f.Ctx, rideID)
	if offerHolder(t, f, driverID) != rideID {
		t.Fatalf("precondition: driver must hold the offer")
	}

	v, err := f.Match.Accept(f.Ctx, driverID, rideID)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if v.Status != "DRIVER_ASSIGNED" {
		t.Fatalf("view status = %q, want DRIVER_ASSIGNED", v.Status)
	}
	if v.DriverID == nil || *v.DriverID != driverID {
		t.Fatalf("view driver = %v, want %s", v.DriverID, driverID)
	}
	if got := f.RideStatus(rideID); got != "DRIVER_ASSIGNED" {
		t.Fatalf("ride status = %q, want DRIVER_ASSIGNED", got)
	}
	if d := f.RideDriver(rideID); d == nil || *d != driverID {
		t.Fatalf("ride driver = %v, want %s", d, driverID)
	}
	if got := f.DriverStatus(driverID); got != "on_trip" {
		t.Fatalf("driver status = %q, want on_trip", got)
	}
	// OTP hash was written.
	if n := f.Count(`SELECT count(*) FROM rides WHERE id=$1 AND otp_hash IS NOT NULL`, rideID); n != 1 {
		t.Fatalf("otp_hash should be set on the ride")
	}
	// Offer consumed (GETDEL) and driver removed from the geo pool.
	if offerHolder(t, f, driverID) != "" {
		t.Fatalf("offer:driver should be consumed by Accept")
	}
	pos, _ := f.Store.Redis.GeoPos(f.Ctx, geoKey(), driverID).Result()
	if len(pos) == 1 && pos[0] != nil {
		t.Fatalf("assigned driver should be removed from the geo set")
	}
	// on_trip mirror written by MarkOnTrip.
	if got, _ := f.Store.Redis.Get(f.Ctx, "driver:ride:"+driverID).Result(); got != rideID {
		t.Fatalf("driver:ride = %q, want %q", got, rideID)
	}
}

func TestAccept_ReplayReturnsCurrentView(t *testing.T) {
	f := testsupport.New(t)
	driverID := seedCandidate(t, f, "mini", pickupLat, pickupLng)
	rideID := matchingRide(t, f, "mini")
	f.Match.RequestMatch(f.Ctx, rideID)
	if _, err := f.Match.Accept(f.Ctx, driverID, rideID); err != nil {
		t.Fatalf("first Accept: %v", err)
	}

	// Offer already consumed; a replayed accept by the assigned driver returns
	// the current assigned view rather than an error.
	v, err := f.Match.Accept(f.Ctx, driverID, rideID)
	if err != nil {
		t.Fatalf("replayed Accept: %v", err)
	}
	if v.Status != "DRIVER_ASSIGNED" || v.DriverID == nil || *v.DriverID != driverID {
		t.Fatalf("replay view = %+v, want DRIVER_ASSIGNED for %s", v, driverID)
	}
}

func TestAccept_OfferExpired(t *testing.T) {
	f := testsupport.New(t)
	driverID, _ := f.InsertDriver("mini", "available")
	rideID := matchingRide(t, f, "mini")

	// Driver holds no offer for this ride ⇒ ErrOfferExpired.
	_, err := f.Match.Accept(f.Ctx, driverID, rideID)
	if err != matching.ErrOfferExpired {
		t.Fatalf("Accept without a held offer = %v, want ErrOfferExpired", err)
	}
}

func TestAccept_RideNoLongerMatching_ErrRideGone(t *testing.T) {
	f := testsupport.New(t)
	driverID := seedCandidate(t, f, "mini", pickupLat, pickupLng)
	rideID := matchingRide(t, f, "mini")
	f.Match.RequestMatch(f.Ctx, rideID)
	if offerHolder(t, f, driverID) != rideID {
		t.Fatalf("precondition: driver must hold the offer")
	}
	// Ride leaves MATCHING out from under the accept (e.g. rider cancelled) ⇒
	// the guarded assignment finds zero rows.
	if _, err := f.Store.PG.Exec(f.Ctx, `UPDATE rides SET status='CANCELLED_BY_RIDER' WHERE id=$1`, rideID); err != nil {
		t.Fatalf("flip ride: %v", err)
	}
	if _, err := f.Match.Accept(f.Ctx, driverID, rideID); err != matching.ErrRideGone {
		t.Fatalf("Accept on non-MATCHING ride = %v, want ErrRideGone", err)
	}
}

// ---- Decline ----

func TestDecline_AdvancesToNextCandidate(t *testing.T) {
	f := testsupport.New(t)
	// d1 at pickup (closest ⇒ offered first); d2 ~150m away.
	d1 := seedCandidate(t, f, "mini", pickupLat, pickupLng)
	d2 := seedCandidate(t, f, "mini", pickupLat+0.0014, pickupLng)
	rideID := matchingRide(t, f, "mini")

	f.Match.RequestMatch(f.Ctx, rideID)
	if offerHolder(t, f, d1) != rideID {
		t.Fatalf("precondition: nearest driver d1 should hold the first offer")
	}

	if err := f.Match.Decline(f.Ctx, d1, rideID); err != nil {
		t.Fatalf("Decline: %v", err)
	}
	// d1's offer released; d2 now holds the advanced offer.
	if offerHolder(t, f, d1) != "" {
		t.Fatalf("d1 offer should be released on decline")
	}
	if got := offerHolder(t, f, d2); got != rideID {
		t.Fatalf("offer:driver:%s = %q, want %q (advanced to next candidate)", d2, got, rideID)
	}
	// d1 remains in the tried-set (not re-offered).
	if !f.Store.Redis.SIsMember(f.Ctx, triedKey(rideID), d1).Val() {
		t.Fatalf("declining driver should stay in the tried-set")
	}
}

func TestDecline_NoOfferHeld_AdvancesOffer(t *testing.T) {
	f := testsupport.New(t)
	driverID := seedCandidate(t, f, "mini", pickupLat, pickupLng)
	rideID := matchingRide(t, f, "mini")

	// No offer was ever made to this driver (held == "" path); Decline still
	// advances the offer loop for a still-MATCHING ride.
	if err := f.Match.Decline(f.Ctx, driverID, rideID); err != nil {
		t.Fatalf("Decline with no held offer: %v", err)
	}
	// The advance offered the (only) candidate.
	if got := offerHolder(t, f, driverID); got != rideID {
		t.Fatalf("offer:driver:%s = %q, want %q after advance", driverID, got, rideID)
	}
}

func TestDecline_MissingRide(t *testing.T) {
	f := testsupport.New(t)
	driverID, _ := f.InsertDriver("mini", "available")
	err := f.Match.Decline(f.Ctx, driverID, "00000000-0000-0000-0000-000000000000")
	if err != matching.ErrNotFound {
		t.Fatalf("Decline on missing ride = %v, want ErrNotFound", err)
	}
}

func TestRequestMatch_MissingRide_NoPanic(t *testing.T) {
	f := testsupport.New(t)
	// loadRideCtx returns ErrNotFound; RequestMatch logs and returns cleanly.
	f.Match.RequestMatch(f.Ctx, "00000000-0000-0000-0000-000000000000")
}

// ---- sweeper lifecycle ----

// TestSweeper_StartTicksAndStops covers the Start goroutine end to end: it ticks
// (invoking sweep at least once) and then exits cleanly on context cancel. The
// sweep offer/expire branches themselves are covered deterministically by the
// white-box tests in sweep_internal_test.go (the shared datastore, driven by the
// long-running server's own sweeper, makes ticker-timed outcome assertions here
// unreliable).
func TestSweeper_StartTicksAndStops(t *testing.T) {
	f := testsupport.New(t)
	// A stale ride gives the tick real work to do (it may be expired by either
	// this sweeper or the running server — we assert the lifecycle, not who wins).
	rideID := matchingRide(t, f, "mini")
	if _, err := f.Store.PG.Exec(f.Ctx,
		`UPDATE rides SET created_at = now() - interval '61 seconds' WHERE id=$1`, rideID); err != nil {
		t.Fatalf("age ride: %v", err)
	}

	ctx, cancel := context.WithCancel(f.Ctx)
	f.Match.Start(ctx)
	// Sleep past one tick so sweep() runs, then cancel and let the goroutine
	// observe ctx.Done and log its stop.
	time.Sleep(sweepTickWait)
	cancel()
	time.Sleep(150 * time.Millisecond)

	if got := f.RideStatus(rideID); got != "EXPIRED" {
		t.Fatalf("aged ride should be EXPIRED after a sweep tick, got %q", got)
	}
}
