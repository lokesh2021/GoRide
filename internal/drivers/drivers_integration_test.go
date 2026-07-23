//go:build integration

// Integration tests for the driver domain: availability transitions, the
// location hot path, release/mark-on-trip mirror maintenance, and the metered
// trip-distance counter. These hit real Postgres + Redis via the shared
// testsupport fixture, so they live in the external test package (testsupport
// imports drivers, which would be an import cycle in-package).
package drivers_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/lokeshbm/goride/internal/drivers"
	"github.com/lokeshbm/goride/internal/testsupport"
)

// ---- Redis key builders (mirror the drivers package's unexported builders;
// they are the SPEC Redis key contract, asserted here by literal shape). ----

func geoKey() string                  { return "geo:drivers:" + testsupport.City }
func lastKey(id string) string        { return "driver:last:" + id }
func statusKey(id string) string      { return "driver:status:" + id }
func rideMKey(id string) string       { return "driver:ride:" + id }
func offerDriverKey(id string) string { return "offer:driver:" + id }
func offerRideKey(id string) string   { return "offer:ride:" + id }
func tripDistKey(id string) string    { return "trip:dist:" + id }

// setFreshLast seeds driver:last:{id} with a fresh (30s TTL) position, the
// freshness signal goAvailable/Release/UpdateLocation key off of.
func setFreshLast(t *testing.T, f *testsupport.Fixture, id string, lat, lng float64) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"lat": lat, "lng": lng, "ts": time.Now().Unix()})
	if err := f.Store.Redis.Set(f.Ctx, lastKey(id), raw, 30*time.Second).Err(); err != nil {
		t.Fatalf("seed driver:last: %v", err)
	}
}

// inGeo reports whether the driver is a member of the city geo set.
func inGeo(t *testing.T, f *testsupport.Fixture, id string) bool {
	t.Helper()
	pos, err := f.Store.Redis.GeoPos(f.Ctx, geoKey(), id).Result()
	if err != nil {
		t.Fatalf("geopos: %v", err)
	}
	return len(pos) == 1 && pos[0] != nil
}

// readMirror returns the parsed status mirror JSON at driver:status:{id}.
func readMirror(t *testing.T, f *testsupport.Fixture, id string) map[string]string {
	t.Helper()
	raw, err := f.Store.Redis.Get(f.Ctx, statusKey(id)).Result()
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal mirror %q: %v", raw, err)
	}
	return m
}

// alignToSecond sleeps until just after the next wall-clock second boundary, so
// a following tight burst of pings shares one rate-limit bucket.
func alignToSecond() {
	now := time.Now()
	time.Sleep(time.Duration(int64(time.Second) - now.UnixNano()%int64(time.Second)))
}

// ---- Availability ----

func TestSetAvailability_OfflineToAvailable_NoFreshFix(t *testing.T) {
	f := testsupport.New(t)
	id, _ := f.InsertDriver("mini", "offline")

	if err := f.Drivers.SetAvailability(f.Ctx, id, true); err != nil {
		t.Fatalf("SetAvailability(true): %v", err)
	}
	if got := f.DriverStatus(id); got != "available" {
		t.Fatalf("driver status = %q, want available", got)
	}
	if m := readMirror(t, f, id); m["status"] != "available" {
		t.Fatalf("mirror status = %q, want available", m["status"])
	}
	// No fresh last fix ⇒ not GEOADDed; the first ping will add it.
	if inGeo(t, f, id) {
		t.Fatalf("driver should not be in geo set without a fresh fix")
	}
}

func TestSetAvailability_OfflineToAvailable_WithFreshFix_GeoAdds(t *testing.T) {
	f := testsupport.New(t)
	id, _ := f.InsertDriver("mini", "offline")
	setFreshLast(t, f, id, 12.9716, 77.5946)

	if err := f.Drivers.SetAvailability(f.Ctx, id, true); err != nil {
		t.Fatalf("SetAvailability(true): %v", err)
	}
	if !inGeo(t, f, id) {
		t.Fatalf("driver with a fresh fix should be GEOADDed on going available")
	}
	if m := readMirror(t, f, id); m["status"] != "available" || m["tier"] != "mini" || m["city"] != testsupport.City {
		t.Fatalf("mirror = %+v, want available/mini/%s", m, testsupport.City)
	}
}

func TestSetAvailability_AvailableToOffline_ClearsGeoAndOffer(t *testing.T) {
	f := testsupport.New(t)
	id, _ := f.InsertDriver("mini", "available")
	setFreshLast(t, f, id, 12.9716, 77.5946)
	// Put the driver in the pool and give them an outstanding offer.
	if err := f.Store.Redis.GeoAdd(f.Ctx, geoKey(), &redis.GeoLocation{Name: id, Longitude: 77.5946, Latitude: 12.9716}).Err(); err != nil {
		t.Fatalf("seed geo: %v", err)
	}
	rideID := "ride-" + id
	f.TrackRedisKey(offerRideKey(rideID))
	if err := f.Store.Redis.Set(f.Ctx, offerDriverKey(id), rideID, 0).Err(); err != nil {
		t.Fatalf("seed offer:driver: %v", err)
	}
	if err := f.Store.Redis.Set(f.Ctx, offerRideKey(rideID), `{"driver_id":"`+id+`"}`, 0).Err(); err != nil {
		t.Fatalf("seed offer:ride: %v", err)
	}

	if err := f.Drivers.SetAvailability(f.Ctx, id, false); err != nil {
		t.Fatalf("SetAvailability(false): %v", err)
	}
	if got := f.DriverStatus(id); got != "offline" {
		t.Fatalf("driver status = %q, want offline", got)
	}
	if m := readMirror(t, f, id); m["status"] != "offline" {
		t.Fatalf("mirror status = %q, want offline", m["status"])
	}
	if inGeo(t, f, id) {
		t.Fatalf("driver should be ZREMd from geo set on going offline")
	}
	if _, err := f.Store.Redis.Get(f.Ctx, offerDriverKey(id)).Result(); err != redis.Nil {
		t.Fatalf("offer:driver should be cleared, err=%v", err)
	}
	if _, err := f.Store.Redis.Get(f.Ctx, offerRideKey(rideID)).Result(); err != redis.Nil {
		t.Fatalf("offer:ride should be cleared, err=%v", err)
	}
}

func TestSetAvailability_OnTripWithActiveRide_Rejected(t *testing.T) {
	f := testsupport.New(t)
	riderID, _ := f.InsertRider()
	quoteID := f.InsertQuote(riderID)
	id, _ := f.InsertDriver("mini", "on_trip")
	// An active ride pins the driver on_trip: availability must 409.
	f.InsertRide(riderID, quoteID, "mini", "IN_PROGRESS", &id, nil)

	err := f.Drivers.SetAvailability(f.Ctx, id, true)
	if err != drivers.ErrInvalidState {
		t.Fatalf("SetAvailability on active-ride driver = %v, want ErrInvalidState", err)
	}
	if got := f.DriverStatus(id); got != "on_trip" {
		t.Fatalf("driver status = %q, want on_trip (unchanged)", got)
	}
}

func TestSetAvailability_OrphanedOnTrip_SelfHeals(t *testing.T) {
	f := testsupport.New(t)
	// on_trip in Postgres but with NO active ride: the NOT EXISTS arm heals it.
	id, _ := f.InsertDriver("mini", "on_trip")

	if err := f.Drivers.SetAvailability(f.Ctx, id, true); err != nil {
		t.Fatalf("SetAvailability(true) on orphaned on_trip: %v", err)
	}
	if got := f.DriverStatus(id); got != "available" {
		t.Fatalf("driver status = %q, want available (self-healed)", got)
	}
	if m := readMirror(t, f, id); m["status"] != "available" {
		t.Fatalf("mirror status = %q, want available", m["status"])
	}
}

func TestSetAvailability_MalformedLastFix_NoGeoAdd(t *testing.T) {
	f := testsupport.New(t)
	id, _ := f.InsertDriver("mini", "offline")
	// A corrupt last-fix must not crash going-available; it simply skips the
	// GEOADD (the next well-formed ping will add the driver).
	if err := f.Store.Redis.Set(f.Ctx, lastKey(id), "not-json", 30*time.Second).Err(); err != nil {
		t.Fatalf("seed bad last: %v", err)
	}
	if err := f.Drivers.SetAvailability(f.Ctx, id, true); err != nil {
		t.Fatalf("SetAvailability(true): %v", err)
	}
	if got := f.DriverStatus(id); got != "available" {
		t.Fatalf("driver status = %q, want available", got)
	}
	if inGeo(t, f, id) {
		t.Fatalf("driver should not be GEOADDed from a malformed fix")
	}
}

func TestSetAvailability_NotFound(t *testing.T) {
	f := testsupport.New(t)
	if err := f.Drivers.SetAvailability(f.Ctx, "00000000-0000-0000-0000-000000000000", true); err != drivers.ErrNotFound {
		t.Fatalf("SetAvailability on missing driver = %v, want ErrNotFound", err)
	}
}

// ---- Location hot path ----

func TestUpdateLocation_AvailableGeoAddsAndSetsLast(t *testing.T) {
	f := testsupport.New(t)
	id, _ := f.InsertDriver("mini", "available")
	// Establish the available mirror (no fresh fix yet ⇒ not in geo).
	if err := f.Drivers.SetAvailability(f.Ctx, id, true); err != nil {
		t.Fatalf("SetAvailability: %v", err)
	}

	if err := f.Drivers.UpdateLocation(f.Ctx, id, 12.9720, 77.5950); err != nil {
		t.Fatalf("UpdateLocation: %v", err)
	}
	// driver:last written.
	raw, err := f.Store.Redis.Get(f.Ctx, lastKey(id)).Result()
	if err != nil {
		t.Fatalf("read driver:last: %v", err)
	}
	var pos map[string]any
	if err := json.Unmarshal([]byte(raw), &pos); err != nil {
		t.Fatalf("unmarshal last: %v", err)
	}
	if pos["lat"].(float64) != 12.9720 {
		t.Fatalf("last lat = %v, want 12.9720", pos["lat"])
	}
	// Conditional GEOADD fired because the mirror says available.
	if !inGeo(t, f, id) {
		t.Fatalf("available driver should be GEOADDed by the ping")
	}
}

func TestUpdateLocation_RateLimitedAfterThreePerSecond(t *testing.T) {
	f := testsupport.New(t)
	id, _ := f.InsertDriver("mini", "offline")

	alignToSecond()
	var errs [4]error
	for i := 0; i < 4; i++ {
		errs[i] = f.Drivers.UpdateLocation(f.Ctx, id, 12.9716, 77.5946)
	}
	for i := 0; i < 3; i++ {
		if errs[i] != nil {
			t.Fatalf("ping %d = %v, want nil (within 3/sec budget)", i+1, errs[i])
		}
	}
	if errs[3] != drivers.ErrRateLimited {
		t.Fatalf("4th ping = %v, want ErrRateLimited", errs[3])
	}
}

func TestUpdateLocation_MetersTripDistanceAndFiltersTeleport(t *testing.T) {
	f := testsupport.New(t)
	riderID, _ := f.InsertRider()
	quoteID := f.InsertQuote(riderID)
	id, _ := f.InsertDriver("mini", "on_trip")
	rideID := f.InsertRide(riderID, quoteID, "mini", "IN_PROGRESS", &id, nil)

	// Mark the driver as on this ride so the hot path meters distance.
	if err := f.Store.Redis.Set(f.Ctx, rideMKey(id), rideID, 0).Err(); err != nil {
		t.Fatalf("seed driver:ride: %v", err)
	}

	alignToSecond()
	// Ping 1: establishes the previous fix (no prior ⇒ nothing metered).
	if err := f.Drivers.UpdateLocation(f.Ctx, id, 12.9716, 77.5946); err != nil {
		t.Fatalf("ping1: %v", err)
	}
	// Ping 2: ~15m step ⇒ counted.
	if err := f.Drivers.UpdateLocation(f.Ctx, id, 12.97173, 77.5946); err != nil {
		t.Fatalf("ping2: %v", err)
	}
	// Ping 3: ~3km jump ⇒ teleport, excluded from metered distance.
	if err := f.Drivers.UpdateLocation(f.Ctx, id, 12.9916, 77.6146); err != nil {
		t.Fatalf("ping3: %v", err)
	}

	dist, err := f.Store.Redis.Get(f.Ctx, tripDistKey(rideID)).Float64()
	if err != nil {
		t.Fatalf("read trip:dist: %v", err)
	}
	// Only the ~15m step should be accumulated; the teleport is filtered out.
	if dist <= 0 || dist > 60 {
		t.Fatalf("metered distance = %.2fm, want a small positive value (teleport filtered)", dist)
	}
}

// ---- ReadAndClearTripDistance ----

func TestReadAndClearTripDistance(t *testing.T) {
	f := testsupport.New(t)
	riderID, _ := f.InsertRider()
	quoteID := f.InsertQuote(riderID)
	rideID := f.InsertRide(riderID, quoteID, "mini", "IN_PROGRESS", nil, nil)

	// Missing counter ⇒ fall back.
	if d, ok := f.Drivers.ReadAndClearTripDistance(f.Ctx, rideID); ok || d != 0 {
		t.Fatalf("missing counter: got (%d,%v), want (0,false)", d, ok)
	}

	if err := f.Store.Redis.Set(f.Ctx, tripDistKey(rideID), "1234.6", 0).Err(); err != nil {
		t.Fatalf("seed trip:dist: %v", err)
	}
	d, ok := f.Drivers.ReadAndClearTripDistance(f.Ctx, rideID)
	if !ok || d != 1235 { // rounded
		t.Fatalf("ReadAndClearTripDistance = (%d,%v), want (1235,true)", d, ok)
	}
	// Counter is deleted after read.
	if _, err := f.Store.Redis.Get(f.Ctx, tripDistKey(rideID)).Result(); err != redis.Nil {
		t.Fatalf("trip:dist should be deleted after read, err=%v", err)
	}

	// A non-positive counter falls back too (and is still cleared).
	if err := f.Store.Redis.Set(f.Ctx, tripDistKey(rideID), "0", 0).Err(); err != nil {
		t.Fatalf("seed zero trip:dist: %v", err)
	}
	if d, ok := f.Drivers.ReadAndClearTripDistance(f.Ctx, rideID); ok || d != 0 {
		t.Fatalf("non-positive counter: got (%d,%v), want (0,false)", d, ok)
	}
}

// ---- Release / MarkOnTrip mirrors ----

func TestRelease_AvailableGeoAdds(t *testing.T) {
	f := testsupport.New(t)
	id, _ := f.InsertDriver("mini", "available")
	setFreshLast(t, f, id, 12.9716, 77.5946)
	// Simulate a lingering on-trip marker that Release must clear.
	if err := f.Store.Redis.Set(f.Ctx, rideMKey(id), "stale-ride", 0).Err(); err != nil {
		t.Fatalf("seed driver:ride: %v", err)
	}

	if err := f.Drivers.Release(f.Ctx, id); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if m := readMirror(t, f, id); m["status"] != "available" {
		t.Fatalf("mirror status = %q, want available", m["status"])
	}
	if _, err := f.Store.Redis.Get(f.Ctx, rideMKey(id)).Result(); err != redis.Nil {
		t.Fatalf("driver:ride should be cleared by Release, err=%v", err)
	}
	if !inGeo(t, f, id) {
		t.Fatalf("available driver with a fresh fix should be GEOADDed by Release")
	}
}

func TestRelease_AvailableNoFreshFix_NoGeoAdd(t *testing.T) {
	f := testsupport.New(t)
	id, _ := f.InsertDriver("mini", "available")
	// Available but no fresh fix: Release refreshes the mirror but cannot
	// GEOADD (nothing to add).
	if err := f.Drivers.Release(f.Ctx, id); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if m := readMirror(t, f, id); m["status"] != "available" {
		t.Fatalf("mirror status = %q, want available", m["status"])
	}
	if inGeo(t, f, id) {
		t.Fatalf("driver without a fresh fix must not be GEOADDed")
	}
}

func TestRelease_AvailableMalformedFix_NoGeoAdd(t *testing.T) {
	f := testsupport.New(t)
	id, _ := f.InsertDriver("mini", "available")
	if err := f.Store.Redis.Set(f.Ctx, lastKey(id), "not-json", 30*time.Second).Err(); err != nil {
		t.Fatalf("seed bad last: %v", err)
	}
	if err := f.Drivers.Release(f.Ctx, id); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if inGeo(t, f, id) {
		t.Fatalf("driver with a malformed fix must not be GEOADDed")
	}
}

func TestRelease_OfflineDriverNotGeoAdded(t *testing.T) {
	f := testsupport.New(t)
	id, _ := f.InsertDriver("mini", "offline")
	setFreshLast(t, f, id, 12.9716, 77.5946)

	if err := f.Drivers.Release(f.Ctx, id); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if m := readMirror(t, f, id); m["status"] != "offline" {
		t.Fatalf("mirror status = %q, want offline", m["status"])
	}
	if inGeo(t, f, id) {
		t.Fatalf("offline driver must not be GEOADDed by Release")
	}
}

func TestRelease_NotFound(t *testing.T) {
	f := testsupport.New(t)
	if err := f.Drivers.Release(f.Ctx, "00000000-0000-0000-0000-000000000000"); err != drivers.ErrNotFound {
		t.Fatalf("Release on missing driver = %v, want ErrNotFound", err)
	}
}

func TestMarkOnTrip_RemovesFromGeoAndSetsMirror(t *testing.T) {
	f := testsupport.New(t)
	id, _ := f.InsertDriver("sedan", "available")
	if err := f.Store.Redis.GeoAdd(f.Ctx, geoKey(), &redis.GeoLocation{Name: id, Longitude: 77.5946, Latitude: 12.9716}).Err(); err != nil {
		t.Fatalf("seed geo: %v", err)
	}
	rideID := "ride-" + id
	f.TrackRedisKey(rideMKey(id))

	if err := f.Drivers.MarkOnTrip(f.Ctx, id, rideID, testsupport.City, "sedan"); err != nil {
		t.Fatalf("MarkOnTrip: %v", err)
	}
	if inGeo(t, f, id) {
		t.Fatalf("driver should be ZREMd from geo set on assignment")
	}
	if got, _ := f.Store.Redis.Get(f.Ctx, rideMKey(id)).Result(); got != rideID {
		t.Fatalf("driver:ride = %q, want %q", got, rideID)
	}
	if m := readMirror(t, f, id); m["status"] != "on_trip" || m["tier"] != "sedan" {
		t.Fatalf("mirror = %+v, want on_trip/sedan", m)
	}
}
