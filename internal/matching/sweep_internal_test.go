//go:build integration

// Internal (white-box) integration test for the sweeper. It drives e.sweep()
// synchronously so the offer/expire branches are exercised deterministically —
// the fixture-based, Start()-driven tests race the long-running :8080 server's
// own sweeper on the shared datastore, which makes the ticker path unreliable
// for coverage. This file lives in package matching (not testsupport, which
// would be an import cycle) and wires a minimal engine against the same infra.
package matching

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lokeshbm/goride/internal/config"
	"github.com/lokeshbm/goride/internal/drivers"
	"github.com/lokeshbm/goride/internal/quotes"
	"github.com/lokeshbm/goride/internal/rides"
	"github.com/lokeshbm/goride/internal/store"
)

const sweepCity = "BLR"

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// sweepEnv is a minimal engine + store, plus the ids it created for cleanup.
type sweepEnv struct {
	t       *testing.T
	st      *store.Store
	drivers *drivers.Service
	engine  *Engine
	ctx     context.Context

	riderIDs  []string
	driverIDs []string
	rideIDs   []string
}

func newSweepEnv(t *testing.T) *sweepEnv {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{
		PGDSN:     getenv("GORIDE_PG_DSN", "postgres://lokesh@localhost:5432/goride?sslmode=disable"),
		RedisAddr: getenv("GORIDE_REDIS_ADDR", "localhost:6379"),
		Env:       "test",
	}
	ctx := context.Background()
	st, err := store.New(ctx, cfg)
	if err != nil {
		t.Fatalf("infra unavailable: %v", err)
	}
	q := quotes.NewService(st, log)
	r := rides.NewService(st, q, log)
	d := drivers.NewService(st, log)
	e := NewEngine(st, r, d, log)
	env := &sweepEnv{t: t, st: st, drivers: d, engine: e, ctx: ctx}
	t.Cleanup(env.cleanup)
	return env
}

func (s *sweepEnv) cleanup() {
	ctx := context.Background()
	for _, rid := range s.rideIDs {
		s.st.Redis.Del(ctx, "offer:ride:"+rid, "offered:ride:"+rid)
	}
	for _, did := range s.driverIDs {
		s.st.Redis.Del(ctx, "offer:driver:"+did, "driver:status:"+did, "driver:last:"+did, "driver:ride:"+did)
		s.st.Redis.ZRem(ctx, "geo:drivers:"+sweepCity, did)
	}
	if len(s.riderIDs) > 0 {
		s.st.PG.Exec(ctx, `DELETE FROM rides WHERE rider_id = ANY($1)`, s.riderIDs)
		s.st.PG.Exec(ctx, `DELETE FROM quotes WHERE rider_id = ANY($1)`, s.riderIDs)
		s.st.PG.Exec(ctx, `DELETE FROM riders WHERE id = ANY($1)`, s.riderIDs)
	}
	if len(s.driverIDs) > 0 {
		s.st.PG.Exec(ctx, `DELETE FROM drivers WHERE id = ANY($1)`, s.driverIDs)
	}
	s.st.Close()
}

func (s *sweepEnv) insertRider() string {
	id := uuid.NewString()
	if _, err := s.st.PG.Exec(s.ctx,
		`INSERT INTO riders (id, name, phone, api_token) VALUES ($1,$2,$3,$4)`,
		id, "Sweep Rider", "+9197"+randDigits(), "sw-rider-"+uuid.NewString()); err != nil {
		s.t.Fatalf("insert rider: %v", err)
	}
	s.riderIDs = append(s.riderIDs, id)
	return id
}

func (s *sweepEnv) insertQuote(riderID string) string {
	id := uuid.NewString()
	prices, _ := json.Marshal(map[string]int{"mini": 15000, "sedan": 22000, "xl": 30000})
	if _, err := s.st.PG.Exec(s.ctx, `
		INSERT INTO quotes (id, rider_id, city, pickup_lat, pickup_lng, drop_lat, drop_lng,
			distance_m, duration_s, surge_x100, prices, expires_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		id, riderID, sweepCity, 12.9716, 77.5946, 12.9352, 77.6245,
		9000, 1470, 100, prices, time.Now().UTC().Add(3*time.Minute), time.Now().UTC()); err != nil {
		s.t.Fatalf("insert quote: %v", err)
	}
	return id
}

// remotePickup is a location far from central Bengaluru (~120km) but still in
// the BLR shard, used to isolate a candidate from the shared geo set that the
// long-running server and other tests populate around the city centre.
const remoteLat, remoteLng = 13.6000, 78.6000

// insertRide inserts a MATCHING ride aged by ageSeconds (0 = now) at the central
// pickup.
func (s *sweepEnv) insertRide(riderID, quoteID string, ageSeconds int) string {
	return s.insertRideAt(riderID, quoteID, ageSeconds, 12.9716, 77.5946)
}

// insertRideAt inserts a MATCHING ride at the given pickup coordinates.
func (s *sweepEnv) insertRideAt(riderID, quoteID string, ageSeconds int, lat, lng float64) string {
	id := uuid.NewString()
	if _, err := s.st.PG.Exec(s.ctx, `
		INSERT INTO rides (id, rider_id, quote_id, tier, status,
			pickup_lat, pickup_lng, drop_lat, drop_lng, payment_method, created_at, updated_at)
		VALUES ($1,$2,$3,$4,'MATCHING',$5,$6,$7,$8,'upi', now() - ($9 * interval '1 second'), now())`,
		id, riderID, quoteID, "mini", lat, lng, 12.9352, 77.6245, ageSeconds); err != nil {
		s.t.Fatalf("insert ride: %v", err)
	}
	s.rideIDs = append(s.rideIDs, id)
	return id
}

func (s *sweepEnv) seedCandidate(lat, lng float64) string {
	id := uuid.NewString()
	if _, err := s.st.PG.Exec(s.ctx,
		`INSERT INTO drivers (id, name, phone, city, tier, vehicle_model, plate, rating, status, api_token)
		 VALUES ($1,$2,$3,$4,'mini',$5,$6,$7,'available',$8)`,
		id, "Sweep Driver", "+9196"+randDigits(), sweepCity, "Model", "KA-96-"+randDigits(), 4.7,
		"sw-driver-"+uuid.NewString()); err != nil {
		s.t.Fatalf("insert driver: %v", err)
	}
	s.driverIDs = append(s.driverIDs, id)
	raw, _ := json.Marshal(map[string]any{"lat": lat, "lng": lng, "ts": time.Now().Unix()})
	s.st.Redis.Set(s.ctx, "driver:last:"+id, raw, 30*time.Second)
	if err := s.drivers.SetAvailability(s.ctx, id, true); err != nil {
		s.t.Fatalf("set availability: %v", err)
	}
	return id
}

func randDigits() string { return uuid.NewString()[:8] }

// TestSweep_ExpiresStaleMatchingRide drives sweep() directly on a ride aged past
// the 60s deadline: it must be EXPIRED via the rides funnel.
func TestSweep_ExpiresStaleMatchingRide(t *testing.T) {
	env := newSweepEnv(t)
	rider := env.insertRider()
	quote := env.insertQuote(rider)
	// Insert aged and sweep immediately, beating the external server's cadence.
	rideID := env.insertRide(rider, quote, 61)

	env.engine.sweep(env.ctx)

	var status string
	if err := env.st.PG.QueryRow(env.ctx, `SELECT status FROM rides WHERE id=$1`, rideID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "EXPIRED" {
		t.Fatalf("ride status = %q, want EXPIRED", status)
	}
}

// TestSweep_OffersFreshMatchingRide drives sweep() on a fresh ride with no live
// offer: it must claim an offer for the eligible candidate.
func TestSweep_OffersFreshMatchingRide(t *testing.T) {
	env := newSweepEnv(t)
	// Remote location so the only candidate in range is ours (isolated from the
	// shared geo set the running server populates near the city centre).
	driverID := env.seedCandidate(remoteLat, remoteLng)
	rider := env.insertRider()
	quote := env.insertQuote(rider)
	rideID := env.insertRideAt(rider, quote, 0, remoteLat, remoteLng)
	env.st.Redis.Del(env.ctx, "offer:ride:"+rideID)

	env.engine.sweep(env.ctx)

	held, err := env.st.Redis.Get(env.ctx, "offer:driver:"+driverID).Result()
	if err != nil {
		t.Fatalf("read offer:driver (want claimed by sweep): %v", err)
	}
	if held != rideID {
		t.Fatalf("offer:driver:%s = %q, want %q", driverID, held, rideID)
	}
}

// TestSweep_SkipsRideWithLiveOffer covers the branch where a MATCHING ride
// already has a live offer:ride marker: sweep must leave it untouched.
func TestSweep_SkipsRideWithLiveOffer(t *testing.T) {
	env := newSweepEnv(t)
	rider := env.insertRider()
	quote := env.insertQuote(rider)
	rideID := env.insertRide(rider, quote, 0)
	// Simulate an outstanding offer so sweep skips the offer step.
	os := offerState{DriverID: uuid.NewString(), ExpiresAt: time.Now().Add(offerTTL).Unix()}
	raw, _ := json.Marshal(os)
	env.st.Redis.Set(env.ctx, "offer:ride:"+rideID, raw, offerTTL)

	env.engine.sweep(env.ctx)

	// Ride stays MATCHING (fresh, offer live ⇒ neither expired nor re-offered).
	var status string
	env.st.PG.QueryRow(env.ctx, `SELECT status FROM rides WHERE id=$1`, rideID).Scan(&status)
	if status != "MATCHING" {
		t.Fatalf("ride status = %q, want MATCHING (offer live)", status)
	}
}
