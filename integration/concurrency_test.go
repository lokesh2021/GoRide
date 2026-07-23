//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/lokeshbm/goride/internal/payments"
)

// ---- HTTP helpers ----

func (e *env) post(path, token, idemKey, body string) (int, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.server.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (e *env) postSigned(path, body string) (int, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.server.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-PSP-Signature", payments.Sign(testPSPSecret, []byte(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// ---- §5(a): parallel accepts ----

// TestParallelAccepts fires Accept for two drivers, both holding a live offer for
// the same MATCHING ride, from 10 goroutines. The invariant: exactly one driver
// is assigned, the ride has exactly one driver_id, and exactly one driver ends
// up on_trip — never two.
func TestParallelAccepts(t *testing.T) {
	e := newEnv(t)
	riderID, _ := e.insertRider()
	quoteID := e.insertQuote(riderID)
	driverA, _ := e.insertDriver("mini", "available")
	driverB, _ := e.insertDriver("mini", "available")
	rideID := e.insertRide(riderID, quoteID, "mini", "MATCHING", nil, nil)

	// Both drivers hold the offer for this ride (a genuine two-way race).
	set := func(driverID string) {
		if err := e.st.Redis.Set(e.ctx, "offer:driver:"+driverID, rideID, 12*time.Second).Err(); err != nil {
			t.Fatalf("seed offer: %v", err)
		}
	}
	set(driverA)
	set(driverB)

	const goroutines = 10
	var wg sync.WaitGroup
	results := make([]*string, goroutines) // assigned driver id per non-error accept
	for i := 0; i < goroutines; i++ {
		driverID := driverA
		if i%2 == 1 {
			driverID = driverB
		}
		wg.Add(1)
		go func(idx int, did string) {
			defer wg.Done()
			view, err := e.match.Accept(e.ctx, did, rideID)
			if err == nil && view != nil {
				results[idx] = view.DriverID
			}
		}(i, driverID)
	}
	wg.Wait()

	// Ride assigned to exactly one driver.
	if got := e.rideStatus(rideID); got != "DRIVER_ASSIGNED" {
		t.Fatalf("ride status = %s, want DRIVER_ASSIGNED", got)
	}
	winner := e.rideDriver(rideID)
	if winner == nil {
		t.Fatal("ride has no driver_id after accepts")
	}
	if *winner != driverA && *winner != driverB {
		t.Fatalf("ride driver_id %s is neither test driver", *winner)
	}

	// Exactly one driver on_trip.
	onTrip := 0
	if e.driverStatus(driverA) == "on_trip" {
		onTrip++
	}
	if e.driverStatus(driverB) == "on_trip" {
		onTrip++
	}
	if onTrip != 1 {
		t.Fatalf("drivers on_trip = %d, want exactly 1", onTrip)
	}
	// The on_trip driver must be the assigned one.
	if e.driverStatus(*winner) != "on_trip" {
		t.Fatalf("assigned driver %s is not on_trip", *winner)
	}

	// Every non-error accept must agree on the single winner (real assignment or
	// a replay of it — never a second, conflicting assignment).
	for i, got := range results {
		if got != nil && *got != *winner {
			t.Fatalf("goroutine %d saw driver %s, winner is %s", i, *got, *winner)
		}
	}
}

// ---- §5(b): double booking ----

// TestDoubleBooking fires two concurrent ride creations for one rider (distinct
// idempotency keys, both valid) and asserts the partial unique index — not app
// logic — yields exactly one 201 and one 409 RIDE_ALREADY_ACTIVE, one ride row.
func TestDoubleBooking(t *testing.T) {
	e := newEnv(t)
	riderID, token := e.insertRider()
	quoteID := e.insertQuote(riderID)
	body := `{"quote_id":"` + quoteID + `","tier":"mini","payment_method":"upi"}`

	var wg sync.WaitGroup
	statuses := make([]int, 2)
	bodies := make([][]byte, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			statuses[idx], bodies[idx] = e.post("/v1/rides", token, uuid.NewString(), body)
		}(i)
	}
	wg.Wait()

	created, conflict := 0, 0
	for i, st := range statuses {
		switch st {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
			if code := errCode(bodies[i]); code != "RIDE_ALREADY_ACTIVE" {
				t.Errorf("conflict body code = %q, want RIDE_ALREADY_ACTIVE (%s)", code, bodies[i])
			}
		default:
			t.Errorf("unexpected status %d: %s", st, bodies[i])
		}
	}
	if created != 1 || conflict != 1 {
		t.Fatalf("got created=%d conflict=%d, want exactly 1 each (statuses=%v)", created, conflict, statuses)
	}
	if n := e.count(`SELECT count(*) FROM rides WHERE rider_id = $1`, riderID); n != 1 {
		t.Fatalf("rides for rider = %d, want 1", n)
	}
}

// ---- §5(c): idempotency ----

// TestIdempotentReplaySequential is the clean idempotency contract: the same key
// replayed sequentially returns the identical stored response (same ride id,
// same status) and creates exactly one ride.
func TestIdempotentReplaySequential(t *testing.T) {
	e := newEnv(t)
	riderID, token := e.insertRider()
	quoteID := e.insertQuote(riderID)
	body := `{"quote_id":"` + quoteID + `","tier":"mini","payment_method":"upi"}`
	key := uuid.NewString()

	st1, b1 := e.post("/v1/rides", token, key, body)
	st2, b2 := e.post("/v1/rides", token, key, body)

	if st1 != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201 (%s)", st1, b1)
	}
	// Bodies are compared as parsed JSON: the stored response lives in a jsonb
	// column, so a replay is semantically identical but re-serialized (keys
	// reordered, whitespace added) — never byte-identical to the original.
	if st2 != st1 || !jsonEqual(b1, b2) {
		t.Fatalf("replay differs: st1=%d st2=%d\n b1=%s\n b2=%s", st1, st2, b1, b2)
	}
	if id1, id2 := rideIDOf(b1), rideIDOf(b2); id1 == "" || id1 != id2 {
		t.Fatalf("ride ids differ across replay: %q vs %q", id1, id2)
	}
	if n := e.count(`SELECT count(*) FROM rides WHERE rider_id = $1`, riderID); n != 1 {
		t.Fatalf("rides for rider = %d, want 1", n)
	}
}

// TestIdempotencyUnderConcurrency fires the same Idempotency-Key 10× concurrently
// at POST /v1/rides. The guaranteed invariants: exactly one ride row exists, and
// every caller observes a byte-identical response (the idempotency contract:
// same key ⇒ same response).
func TestIdempotencyUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	riderID, token := e.insertRider()
	quoteID := e.insertQuote(riderID)
	body := `{"quote_id":"` + quoteID + `","tier":"mini","payment_method":"upi"}`
	key := uuid.NewString()

	const goroutines = 10
	var wg sync.WaitGroup
	statuses := make([]int, goroutines)
	bodies := make([][]byte, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			statuses[idx], bodies[idx] = e.post("/v1/rides", token, key, body)
		}(i)
	}
	wg.Wait()

	// Exactly one ride row — no double-booking under a concurrent idempotent retry.
	if n := e.count(`SELECT count(*) FROM rides WHERE rider_id = $1`, riderID); n != 1 {
		t.Fatalf("rides for rider = %d, want exactly 1", n)
	}
	// All callers see a semantically identical status + body (idempotency: same
	// key ⇒ same response). Bodies are compared as parsed JSON because the winner's
	// response is replayed from the jsonb column, re-serialized rather than echoed
	// byte-for-byte.
	for i := 1; i < goroutines; i++ {
		if statuses[i] != statuses[0] || !jsonEqual(bodies[i], bodies[0]) {
			t.Fatalf("caller %d response differs from caller 0:\n [%d] %s\n [%d] %s",
				i, statuses[0], bodies[0], statuses[i], bodies[i])
		}
	}
	t.Logf("concurrent idempotent create settled on status=%d body=%s", statuses[0], bodies[0])
}

// ---- §5(d): duplicate webhook ----

// TestDuplicateWebhook delivers the same signed success webhook 5× concurrently
// for one PROCESSING payment. The guarded PROCESSING→SUCCEEDED update plus the
// unique receipt index mean the payment goes terminal exactly once and exactly
// one receipt is written.
func TestDuplicateWebhook(t *testing.T) {
	e := newEnv(t)
	riderID, _ := e.insertRider()
	quoteID := e.insertQuote(riderID)
	driverID, _ := e.insertDriver("mini", "available")
	rideID := e.insertRide(riderID, quoteID, "mini", "COMPLETED", &driverID, nil)

	// Trip ENDED with a fare breakdown (the receipt copies it).
	fare := `{"base":3000,"distance_component":9900,"time_component":3675,"surge_x100":100,"total":16600}`
	if _, err := e.st.PG.Exec(e.ctx, `
		INSERT INTO trips (id, ride_id, status, started_at, ended_at, paused_seconds, distance_m, fare)
		VALUES ($1,$2,'ENDED', now()-interval '20 minutes', now(), 0, 9000, $3)`,
		uuid.NewString(), rideID, fare); err != nil {
		t.Fatalf("insert trip: %v", err)
	}
	// PROCESSING payment with a known psp_ref.
	pspRef := uuid.NewString()
	if _, err := e.st.PG.Exec(e.ctx, `
		INSERT INTO payments (id, ride_id, amount, method, status, psp_ref)
		VALUES ($1,$2,$3,'upi','PROCESSING',$4)`,
		uuid.NewString(), rideID, 16600, pspRef); err != nil {
		t.Fatalf("insert payment: %v", err)
	}

	body := `{"psp_ref":"` + pspRef + `","status":"success"}`
	const goroutines = 5
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, b := e.postSigned("/v1/webhooks/psp", body)
			if st != http.StatusOK {
				t.Errorf("webhook status = %d, want 200 (%s)", st, b)
			}
		}()
	}
	wg.Wait()

	var status string
	var retry int
	if err := e.st.PG.QueryRow(e.ctx,
		`SELECT status, retry_count FROM payments WHERE psp_ref = $1`, pspRef).Scan(&status, &retry); err != nil {
		t.Fatalf("read payment: %v", err)
	}
	if status != payments.StatusSucceeded {
		t.Fatalf("payment status = %s, want SUCCEEDED", status)
	}
	if retry != 0 {
		t.Errorf("retry_count = %d, want 0 (no double-processing)", retry)
	}
	if n := e.count(`SELECT count(*) FROM receipts WHERE ride_id = $1`, rideID); n != 1 {
		t.Fatalf("receipts for ride = %d, want exactly 1", n)
	}
}

// ---- §5(e): offer claim exclusivity ----

// TestOfferClaimExclusivity is a direct Redis-level test of the offer claim
// helper: N concurrent SET NX for one driver key must have exactly one winner.
func TestOfferClaimExclusivity(t *testing.T) {
	e := newEnv(t)
	driverID := uuid.NewString()
	rideID := uuid.NewString()
	key := "offer:driver:" + driverID
	e.trackRedisKey(key)

	const goroutines = 10
	var wg sync.WaitGroup
	won := make([]bool, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ok, err := e.st.Redis.SetNX(e.ctx, key, rideID, 12*time.Second).Result()
			if err != nil {
				t.Errorf("setnx: %v", err)
			}
			won[idx] = ok
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, w := range won {
		if w {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("SET NX winners = %d, want exactly 1", winners)
	}
	if v, _ := e.st.Redis.Get(e.ctx, key).Result(); v != rideID {
		t.Fatalf("claimed key holds %q, want %q", v, rideID)
	}
}

// ---- §5(f): state machine under race ----

// TestCancelAcceptRace runs concurrent rider-cancel + driver-accept on the same
// MATCHING ride, many times. The invariant: the final state is exactly one of
// DRIVER_ASSIGNED or CANCELLED_BY_RIDER (never both effects), and the driver is
// freed if and only if the ride was cancelled.
func TestCancelAcceptRace(t *testing.T) {
	e := newEnv(t)
	riderID, _ := e.insertRider()
	quoteID := e.insertQuote(riderID)
	driverID, _ := e.insertDriver("mini", "available")

	const iterations = 30
	for i := 0; i < iterations; i++ {
		rideID := e.insertRide(riderID, quoteID, "mini", "MATCHING", nil, nil)
		if err := e.st.Redis.Set(e.ctx, "offer:driver:"+driverID, rideID, 12*time.Second).Err(); err != nil {
			t.Fatalf("seed offer: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = e.rides.Cancel(e.ctx, rideID, riderID, "rider", "race")
		}()
		go func() {
			defer wg.Done()
			_, _ = e.match.Accept(e.ctx, driverID, rideID)
		}()
		wg.Wait()

		status := e.rideStatus(rideID)
		dstatus := e.driverStatus(driverID)
		switch status {
		case "DRIVER_ASSIGNED":
			if d := e.rideDriver(rideID); d == nil || *d != driverID {
				t.Fatalf("iter %d: assigned ride driver_id=%v, want %s", i, d, driverID)
			}
			if dstatus != "on_trip" {
				t.Fatalf("iter %d: ride DRIVER_ASSIGNED but driver status=%s, want on_trip", i, dstatus)
			}
		case "CANCELLED_BY_RIDER":
			if dstatus != "available" {
				t.Fatalf("iter %d: ride CANCELLED_BY_RIDER but driver status=%s, want available (driver orphaned)", i, dstatus)
			}
		default:
			t.Fatalf("iter %d: final status %s, want DRIVER_ASSIGNED or CANCELLED_BY_RIDER", i, status)
		}

		// Reset ride to a terminal, non-active state and free the driver so both
		// unique indexes are clear for the next iteration's fresh ride.
		if _, err := e.st.PG.Exec(e.ctx,
			`UPDATE rides SET status='EXPIRED', driver_id=NULL WHERE id=$1`, rideID); err != nil {
			t.Fatalf("reset ride: %v", err)
		}
		if _, err := e.st.PG.Exec(e.ctx,
			`UPDATE drivers SET status='available' WHERE id=$1`, driverID); err != nil {
			t.Fatalf("reset driver: %v", err)
		}
		_ = e.st.Redis.Del(e.ctx,
			"offer:driver:"+driverID, "offer:ride:"+rideID, "offered:ride:"+rideID,
			"driver:ride:"+driverID, "driver:status:"+driverID, "ride:cache:"+rideID).Err()
	}
}

// ---- §5(g): trip end idempotent replay ----

// TestTripEndIdempotentReplay ends a trip twice with the same Idempotency-Key.
// The first call finalizes the fare and creates the payment; the second replays
// the stored response verbatim. Result: identical bodies, exactly one payment.
func TestTripEndIdempotentReplay(t *testing.T) {
	e := newEnv(t)
	riderID, _ := e.insertRider()
	quoteID := e.insertQuote(riderID)
	driverID, driverToken := e.insertDriver("mini", "on_trip")

	// A bcrypt OTP hash so the ride row is well-formed (end does not check OTP).
	otpHash, err := bcrypt.GenerateFromPassword([]byte("1234"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash otp: %v", err)
	}
	hs := string(otpHash)
	rideID := e.insertRide(riderID, quoteID, "mini", "IN_PROGRESS", &driverID, &hs)

	// Trip STARTED 20 minutes ago (positive duration; distance falls back to quote).
	if _, err := e.st.PG.Exec(e.ctx, `
		INSERT INTO trips (id, ride_id, status, started_at, paused_seconds)
		VALUES ($1,$2,'STARTED', now()-interval '20 minutes', 0)`,
		uuid.NewString(), rideID); err != nil {
		t.Fatalf("insert trip: %v", err)
	}

	key := uuid.NewString()
	path := "/v1/trips/" + rideID + "/end"

	st1, b1 := e.post(path, driverToken, key, "")
	st2, b2 := e.post(path, driverToken, key, "")

	if st1 != http.StatusOK {
		t.Fatalf("first end status = %d, want 200 (%s)", st1, b1)
	}
	// Semantic (parsed) comparison: the replay is served from the jsonb store.
	if st2 != st1 || !jsonEqual(b1, b2) {
		t.Fatalf("end replay differs: st1=%d st2=%d\n b1=%s\n b2=%s", st1, st2, b1, b2)
	}
	if n := e.count(`SELECT count(*) FROM payments WHERE ride_id = $1`, rideID); n != 1 {
		t.Fatalf("payments for ride = %d, want exactly 1", n)
	}
	if got := e.rideStatus(rideID); got != "COMPLETED" {
		t.Fatalf("ride status = %s, want COMPLETED", got)
	}
}

// ---- small body helpers ----

func errCode(b []byte) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &env)
	return env.Error.Code
}

func rideIDOf(b []byte) string {
	var v struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(b, &v)
	return v.ID
}

// jsonEqual reports whether two response bodies decode to the same JSON value,
// ignoring key order and whitespace. Needed because idempotent replays are
// served from a jsonb column and are re-serialized, not echoed verbatim.
func jsonEqual(a, b []byte) bool {
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}
