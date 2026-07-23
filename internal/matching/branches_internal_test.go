//go:build integration

// White-box branch coverage for the matching engine. Being in package matching
// lets these call the unexported helpers (loadRideCtx, offerNext, readCandidate,
// recordOffer, assignTx, claimMatchingBatch) directly, and drive error branches
// deterministically with a pre-cancelled context (every Redis/PG op then fails
// immediately) rather than depending on real infra failures.
package matching

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// canceledCtx returns a context that is already cancelled, so the first
// datastore round-trip in any code path returns an error.
func canceledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// insertRideStatus inserts a ride in an arbitrary status with an optional
// driver, for constructing assignTx pre-conditions.
func (s *sweepEnv) insertRideStatus(riderID, quoteID, status string, driverID *string) string {
	id := uuid.NewString()
	if _, err := s.st.PG.Exec(s.ctx, `
		INSERT INTO rides (id, rider_id, driver_id, quote_id, tier, status,
			pickup_lat, pickup_lng, drop_lat, drop_lng, payment_method)
		VALUES ($1,$2,$3,$4,'mini',$5,$6,$7,$8,$9,'upi')`,
		id, riderID, driverID, quoteID, status, 12.9716, 77.5946, 12.9352, 77.6245); err != nil {
		s.t.Fatalf("insert ride status=%s: %v", status, err)
	}
	s.rideIDs = append(s.rideIDs, id)
	return id
}

// insertDriverStatus inserts a driver in the given status (no Redis mirror).
func (s *sweepEnv) insertDriverStatus(status string) string {
	id := uuid.NewString()
	if _, err := s.st.PG.Exec(s.ctx,
		`INSERT INTO drivers (id, name, phone, city, tier, vehicle_model, plate, rating, status, api_token)
		 VALUES ($1,$2,$3,$4,'mini',$5,$6,$7,$8,$9)`,
		id, "Br Driver", "+9195"+randDigits(), sweepCity, "Model", "KA-95-"+randDigits(), 4.5,
		status, "br-driver-"+uuid.NewString()); err != nil {
		s.t.Fatalf("insert driver status=%s: %v", status, err)
	}
	s.driverIDs = append(s.driverIDs, id)
	return id
}

// ---- loadRideCtx ----

func TestLoadRideCtx_NotFound(t *testing.T) {
	env := newSweepEnv(t)
	if _, err := env.engine.loadRideCtx(env.ctx, uuid.NewString()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("loadRideCtx(missing) = %v, want ErrNotFound", err)
	}
}

func TestLoadRideCtx_QueryError(t *testing.T) {
	env := newSweepEnv(t)
	rider := env.insertRider()
	quote := env.insertQuote(rider)
	rideID := env.insertRide(rider, quote, 0)
	_, err := env.engine.loadRideCtx(canceledCtx(), rideID)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("loadRideCtx(canceled) = %v, want a non-nil, non-ErrNotFound error", err)
	}
}

// ---- offerNext ----

func TestOfferNext_GeoSearchError(t *testing.T) {
	env := newSweepEnv(t)
	r := &rideCtx{ID: uuid.NewString(), Tier: "mini", City: sweepCity, PickupLat: 12.9716, PickupLng: 77.5946}
	if _, err := env.engine.offerNext(canceledCtx(), r); err == nil {
		t.Fatal("offerNext(canceled) should error on GeoSearch")
	}
}

func TestOfferNext_CandidateAlreadyClaimed_Skips(t *testing.T) {
	env := newSweepEnv(t)
	// Remote location ⇒ our candidate is the only one in range.
	driverID := env.seedCandidate(remoteLat, remoteLng)
	rider := env.insertRider()
	quote := env.insertQuote(rider)
	rideID := env.insertRideAt(rider, quote, 0, remoteLat, remoteLng)
	// The only candidate already holds another ride's offer ⇒ SET NX fails ⇒
	// offerNext skips it and makes no offer.
	env.st.Redis.Set(env.ctx, "offer:driver:"+driverID, "some-other-ride", offerTTL)

	r, err := env.engine.loadRideCtx(env.ctx, rideID)
	if err != nil {
		t.Fatalf("loadRideCtx: %v", err)
	}
	made, err := env.engine.offerNext(env.ctx, r)
	if err != nil {
		t.Fatalf("offerNext: %v", err)
	}
	if made {
		t.Fatal("offerNext should not claim an already-held candidate")
	}
	if v, _ := env.st.Redis.Get(env.ctx, "offer:driver:"+driverID).Result(); v != "some-other-ride" {
		t.Fatalf("offer:driver = %q, want unchanged 'some-other-ride' (skipped, not stomped)", v)
	}
}

// ---- readCandidate / recordOffer ----

func TestReadCandidate_PipelineError(t *testing.T) {
	env := newSweepEnv(t)
	if _, _, err := env.engine.readCandidate(canceledCtx(), uuid.NewString()); err == nil {
		t.Fatal("readCandidate(canceled) should error")
	}
}

func TestRecordOffer_PipelineError(t *testing.T) {
	env := newSweepEnv(t)
	if err := env.engine.recordOffer(canceledCtx(), uuid.NewString(), uuid.NewString()); err == nil {
		t.Fatal("recordOffer(canceled) should error")
	}
}

// ---- assignTx ----

func TestAssignTx_BeginError(t *testing.T) {
	env := newSweepEnv(t)
	if err := env.engine.assignTx(canceledCtx(), uuid.NewString(), uuid.NewString(), "hash"); err == nil {
		t.Fatal("assignTx(canceled) should error on Begin")
	}
}

func TestAssignTx_RideNotMatching(t *testing.T) {
	env := newSweepEnv(t)
	rider := env.insertRider()
	quote := env.insertQuote(rider)
	driverID := env.seedCandidate(12.9716, 77.5946)
	// Ride already past MATCHING ⇒ guarded UPDATE affects zero rows.
	rideID := env.insertRideStatus(rider, quote, "CANCELLED_BY_RIDER", nil)
	if err := env.engine.assignTx(env.ctx, rideID, driverID, "hash"); !errors.Is(err, ErrRideGone) {
		t.Fatalf("assignTx on non-MATCHING ride = %v, want ErrRideGone", err)
	}
}

func TestAssignTx_DriverNotAvailable(t *testing.T) {
	env := newSweepEnv(t)
	rider := env.insertRider()
	quote := env.insertQuote(rider)
	rideID := env.insertRide(rider, quote, 0) // MATCHING
	offlineDriver := env.insertDriverStatus("offline")
	// Ride flips fine but the driver guard (WHERE status='available') fails.
	if err := env.engine.assignTx(env.ctx, rideID, offlineDriver, "hash"); !errors.Is(err, ErrRideGone) {
		t.Fatalf("assignTx with unavailable driver = %v, want ErrRideGone", err)
	}
	// Rolled back: ride stays MATCHING.
	var status string
	env.st.PG.QueryRow(env.ctx, `SELECT status FROM rides WHERE id=$1`, rideID).Scan(&status)
	if status != "MATCHING" {
		t.Fatalf("ride status = %q, want MATCHING (assignTx rolled back)", status)
	}
}

func TestAssignTx_UniqueViolation(t *testing.T) {
	env := newSweepEnv(t)
	driverID := env.seedCandidate(12.9716, 77.5946)
	// Driver already holds an active ride ⇒ the partial unique index on
	// driver_id makes a second assignment a 23505 unique violation.
	riderA := env.insertRider()
	quoteA := env.insertQuote(riderA)
	env.insertRideStatus(riderA, quoteA, "DRIVER_ASSIGNED", &driverID)

	riderB := env.insertRider()
	quoteB := env.insertQuote(riderB)
	rideB := env.insertRide(riderB, quoteB, 0) // MATCHING

	if err := env.engine.assignTx(env.ctx, rideB, driverID, "hash"); !errors.Is(err, ErrRideGone) {
		t.Fatalf("assignTx with driver already active = %v, want ErrRideGone (unique violation)", err)
	}
}

// ---- claimMatchingBatch / sweep error branches ----

func TestClaimMatchingBatch_BeginError(t *testing.T) {
	env := newSweepEnv(t)
	if _, err := env.engine.claimMatchingBatch(canceledCtx()); err == nil {
		t.Fatal("claimMatchingBatch(canceled) should error")
	}
}

func TestSweep_ClaimErrorIsLogged(t *testing.T) {
	env := newSweepEnv(t)
	// A cancelled context makes claimMatchingBatch fail; sweep must log & return
	// without panicking.
	env.engine.sweep(canceledCtx())
}

// ---- Accept / Decline error branches ----

func TestAccept_GetDelError(t *testing.T) {
	env := newSweepEnv(t)
	if _, err := env.engine.Accept(canceledCtx(), uuid.NewString(), uuid.NewString()); err == nil {
		t.Fatal("Accept(canceled) should error on GETDEL")
	}
}

func TestDecline_GetDelError(t *testing.T) {
	env := newSweepEnv(t)
	if err := env.engine.Decline(canceledCtx(), uuid.NewString(), uuid.NewString()); err == nil {
		t.Fatal("Decline(canceled) should error on GETDEL")
	}
}

// ---- SetObservability ----

func TestSetObservability_NilIsSafe(t *testing.T) {
	env := newSweepEnv(t)
	env.engine.SetObservability(nil) // must not panic; nil app is a documented no-op
}

// ---- offerState round-trip via the wire the sweeper reads (extra guard) ----

func TestOfferStateWireShape(t *testing.T) {
	raw, _ := json.Marshal(offerState{DriverID: "d", ExpiresAt: time.Now().Unix()})
	var back offerState
	if err := json.Unmarshal(raw, &back); err != nil || back.DriverID != "d" {
		t.Fatalf("offerState round-trip failed: %v (%s)", err, raw)
	}
}
