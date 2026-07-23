//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/lokeshbm/goride/internal/payments"
	"github.com/lokeshbm/goride/internal/testsupport"
)

// mustHash returns a bcrypt hash of code for planting a known OTP on a ride.
func mustHash(t *testing.T, code string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return string(h)
}

// client is a thin HTTP helper bound to the fixture's in-process server. It
// issues real requests through the router (middleware + auth + services), which
// is the whole point: one call exercises the full stack end to end.
type client struct {
	t   *testing.T
	f   *testsupport.Fixture
	url string
}

type reqOpts struct {
	token   string
	idemKey string
	sig     string // X-PSP-Signature for the webhook
	body    string
}

// do issues method+path with the given options and returns (status, rawBody).
func (c *client) do(method, path string, o reqOpts) (int, []byte) {
	c.t.Helper()
	var bodyR io.Reader
	if o.body != "" {
		bodyR = bytes.NewReader([]byte(o.body))
	}
	req, err := http.NewRequest(method, c.url+path, bodyR)
	if err != nil {
		c.t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if o.token != "" {
		req.Header.Set("Authorization", "Bearer "+o.token)
	}
	if o.idemKey != "" {
		req.Header.Set("Idempotency-Key", o.idemKey)
	}
	if o.sig != "" {
		req.Header.Set("X-PSP-Signature", o.sig)
	}
	if o.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// decodeMap unmarshals a JSON object body into a map for field probing.
func decodeMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode body %q: %v", raw, err)
	}
	return m
}

// wantStatus fails the test if got != want, printing the body for context.
func wantStatus(t *testing.T, got, want int, raw []byte) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %d, want %d (body: %s)", got, want, raw)
	}
}

// wantCode asserts the error envelope carries the expected SNAKE_CASE code.
func wantCode(t *testing.T, raw []byte, code string) {
	t.Helper()
	m := decodeMap(t, raw)
	errObj, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("body is not an error envelope: %s", raw)
	}
	if errObj["code"] != code {
		t.Errorf("error.code = %v, want %q (body: %s)", errObj["code"], code, raw)
	}
}

// TestHappyPathOverHTTP drives the full ride lifecycle through the real HTTP
// server: driver goes online + pings, rider quotes and books, driver accepts,
// rider regenerates the OTP, driver progresses to arrived, starts with the OTP,
// ends the trip, rider pays, the PSP webhook settles it, and history/state read
// back the completed ride.
func TestHappyPathOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}

	riderID, riderTok := f.InsertRider()
	driverID, driverTok := f.InsertDriver("mini", "offline")

	// Driver goes available, then pings a location at the quote pickup.
	st, raw := c.do("POST", "/v1/drivers/"+driverID+"/availability", reqOpts{token: driverTok, body: `{"available":true}`})
	wantStatus(t, st, http.StatusOK, raw)
	st, raw = c.do("POST", "/v1/drivers/"+driverID+"/location", reqOpts{token: driverTok, body: `{"lat":12.9716,"lng":77.5946}`})
	wantStatus(t, st, http.StatusOK, raw)

	// Rider creates a quote.
	st, raw = c.do("POST", "/v1/quotes", reqOpts{token: riderTok, body: `{"pickup":{"lat":12.9716,"lng":77.5946},"drop":{"lat":12.9352,"lng":77.6245}}`})
	wantStatus(t, st, http.StatusOK, raw)
	quoteID, _ := decodeMap(t, raw)["quote_id"].(string)
	if quoteID == "" {
		t.Fatalf("no quote_id in %s", raw)
	}

	// Rider books the ride (idempotent).
	st, raw = c.do("POST", "/v1/rides", reqOpts{token: riderTok, idemKey: "book-1", body: `{"quote_id":"` + quoteID + `","tier":"mini","payment_method":"upi"}`})
	wantStatus(t, st, http.StatusCreated, raw)
	rideID, _ := decodeMap(t, raw)["id"].(string)
	if rideID == "" {
		t.Fatalf("no ride id in %s", raw)
	}
	f.TrackRide(rideID)

	// Deterministically plant this driver's outstanding offer, then accept it.
	if err := f.Store.Redis.Set(f.Ctx, "offer:driver:"+driverID, rideID, 12*time.Second).Err(); err != nil {
		t.Fatalf("seed offer: %v", err)
	}
	st, raw = c.do("POST", "/v1/drivers/"+driverID+"/accept", reqOpts{token: driverTok, body: `{"ride_id":"` + rideID + `"}`})
	wantStatus(t, st, http.StatusOK, raw)
	if decodeMap(t, raw)["status"] != "DRIVER_ASSIGNED" {
		t.Errorf("status after accept = %v, want DRIVER_ASSIGNED", decodeMap(t, raw)["status"])
	}

	// Rider regenerates the OTP to get a known plaintext value for start.
	st, raw = c.do("POST", "/v1/rides/"+rideID+"/otp", reqOpts{token: riderTok, body: `{}`})
	wantStatus(t, st, http.StatusOK, raw)
	otp, _ := decodeMap(t, raw)["otp"].(string)
	if otp == "" {
		t.Fatalf("no otp in %s", raw)
	}

	// Driver progresses to ARRIVED.
	st, raw = c.do("POST", "/v1/rides/"+rideID+"/arriving", reqOpts{token: driverTok})
	wantStatus(t, st, http.StatusOK, raw)
	st, raw = c.do("POST", "/v1/rides/"+rideID+"/arrived", reqOpts{token: driverTok})
	wantStatus(t, st, http.StatusOK, raw)

	// Driver starts the trip with the OTP.
	st, raw = c.do("POST", "/v1/trips/"+rideID+"/start", reqOpts{token: driverTok, body: `{"otp":"` + otp + `"}`})
	wantStatus(t, st, http.StatusOK, raw)
	if decodeMap(t, raw)["ride_status"] != "IN_PROGRESS" {
		t.Errorf("ride_status after start = %v, want IN_PROGRESS", decodeMap(t, raw)["ride_status"])
	}

	// Driver ends the trip (idempotent).
	st, raw = c.do("POST", "/v1/trips/"+rideID+"/end", reqOpts{token: driverTok, idemKey: "end-1"})
	wantStatus(t, st, http.StatusOK, raw)
	if decodeMap(t, raw)["ride_status"] != "COMPLETED" {
		t.Errorf("ride_status after end = %v, want COMPLETED", decodeMap(t, raw)["ride_status"])
	}

	// Rider triggers payment (idempotent) → PROCESSING with a psp_ref.
	st, raw = c.do("POST", "/v1/payments", reqOpts{token: riderTok, idemKey: "pay-1", body: `{"ride_id":"` + rideID + `"}`})
	wantStatus(t, st, http.StatusOK, raw)
	payResp := decodeMap(t, raw)
	pspRef, _ := payResp["psp_ref"].(string)
	if pspRef == "" || payResp["status"] != "PROCESSING" {
		t.Fatalf("unexpected payment response: %s", raw)
	}

	// PSP webhook settles the payment (signed with the shared secret).
	whBody := `{"psp_ref":"` + pspRef + `","status":"success"}`
	sig := payments.Sign(testsupport.PSPSecret, []byte(whBody))
	st, raw = c.do("POST", "/v1/webhooks/psp", reqOpts{sig: sig, body: whBody})
	wantStatus(t, st, http.StatusOK, raw)
	if n := f.Count(`SELECT count(*) FROM payments WHERE psp_ref = $1 AND status = 'SUCCEEDED'`, pspRef); n != 1 {
		t.Errorf("payment not SUCCEEDED after webhook")
	}
	if n := f.Count(`SELECT count(*) FROM receipts WHERE ride_id = $1`, rideID); n != 1 {
		t.Errorf("receipt not created after webhook")
	}

	// History shows the completed ride.
	st, raw = c.do("GET", "/v1/riders/"+riderID+"/rides", reqOpts{token: riderTok})
	wantStatus(t, st, http.StatusOK, raw)
	if !bytes.Contains(raw, []byte(rideID)) {
		t.Errorf("history missing ride %s: %s", rideID, raw)
	}

	// State endpoints: rider has no active ride (completed), driver is available.
	st, raw = c.do("GET", "/v1/riders/"+riderID+"/state", reqOpts{token: riderTok})
	wantStatus(t, st, http.StatusOK, raw)
	if decodeMap(t, raw)["active_ride"] != nil {
		t.Errorf("rider active_ride = %v, want nil after completion", decodeMap(t, raw)["active_ride"])
	}
	st, raw = c.do("GET", "/v1/drivers/"+driverID+"/state", reqOpts{token: driverTok})
	wantStatus(t, st, http.StatusOK, raw)
	if decodeMap(t, raw)["status"] != "available" {
		t.Errorf("driver status = %v, want available", decodeMap(t, raw)["status"])
	}

	// GET the ride directly.
	st, raw = c.do("GET", "/v1/rides/"+rideID, reqOpts{token: riderTok})
	wantStatus(t, st, http.StatusOK, raw)
	if decodeMap(t, raw)["status"] != "COMPLETED" {
		t.Errorf("ride status = %v, want COMPLETED", decodeMap(t, raw)["status"])
	}
}

// TestAuthErrorsOverHTTP covers the auth/role/path guards through the router.
func TestAuthErrorsOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	riderID, riderTok := f.InsertRider()
	_, driverTok := f.InsertDriver("mini", "available")

	t.Run("unknown token → 401", func(t *testing.T) {
		st, raw := c.do("GET", "/v1/riders/"+riderID+"/state", reqOpts{token: "nope-not-a-real-token"})
		wantStatus(t, st, http.StatusUnauthorized, raw)
		wantCode(t, raw, "UNAUTHORIZED")
	})

	t.Run("missing token → 401", func(t *testing.T) {
		st, raw := c.do("GET", "/v1/riders/"+riderID+"/state", reqOpts{})
		wantStatus(t, st, http.StatusUnauthorized, raw)
		wantCode(t, raw, "UNAUTHORIZED")
	})

	t.Run("rider hitting a driver-only route → 403", func(t *testing.T) {
		st, raw := c.do("POST", "/v1/drivers/"+riderID+"/availability", reqOpts{token: riderTok, body: `{"available":true}`})
		wantStatus(t, st, http.StatusForbidden, raw)
		wantCode(t, raw, "FORBIDDEN")
	})

	t.Run("rider reading another rider's history → 403", func(t *testing.T) {
		otherID, _ := f.InsertRider()
		st, raw := c.do("GET", "/v1/riders/"+otherID+"/rides", reqOpts{token: riderTok})
		wantStatus(t, st, http.StatusForbidden, raw)
		wantCode(t, raw, "FORBIDDEN")
	})

	t.Run("driver acting on another driver's resource → 403", func(t *testing.T) {
		otherID, _ := f.InsertDriver("mini", "available")
		st, raw := c.do("POST", "/v1/drivers/"+otherID+"/location", reqOpts{token: driverTok, body: `{"lat":1,"lng":2}`})
		wantStatus(t, st, http.StatusForbidden, raw)
		wantCode(t, raw, "FORBIDDEN")
	})
}

// TestValidationErrorsOverHTTP covers a few 400 VALIDATION_FAILED envelopes
// through the full stack (complementing the white-box matrix in handlers_test).
func TestValidationErrorsOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	_, riderTok := f.InsertRider()

	t.Run("quote missing pickup → 400", func(t *testing.T) {
		st, raw := c.do("POST", "/v1/quotes", reqOpts{token: riderTok, body: `{"drop":{"lat":12.9,"lng":77.6}}`})
		wantStatus(t, st, http.StatusBadRequest, raw)
		wantCode(t, raw, "VALIDATION_FAILED")
	})

	t.Run("get ride with bad uuid → 400", func(t *testing.T) {
		st, raw := c.do("GET", "/v1/rides/not-a-uuid", reqOpts{token: riderTok})
		wantStatus(t, st, http.StatusBadRequest, raw)
		wantCode(t, raw, "VALIDATION_FAILED")
	})

	t.Run("book with bad tier → 400", func(t *testing.T) {
		st, raw := c.do("POST", "/v1/rides", reqOpts{token: riderTok, idemKey: "k1", body: `{"quote_id":"11111111-1111-1111-1111-111111111111","tier":"bike","payment_method":"upi"}`})
		wantStatus(t, st, http.StatusBadRequest, raw)
		wantCode(t, raw, "VALIDATION_FAILED")
	})
}

// TestBookErrorsOverHTTP covers ride-domain error envelopes: an expired quote
// (422) and a quote owned by another rider (403).
func TestBookErrorsOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	riderID, riderTok := f.InsertRider()

	t.Run("expired quote → 422 QUOTE_EXPIRED", func(t *testing.T) {
		quoteID := f.InsertQuote(riderID)
		// Force the quote into the past.
		if _, err := f.Store.PG.Exec(f.Ctx, `UPDATE quotes SET expires_at = now() - interval '1 minute' WHERE id = $1`, quoteID); err != nil {
			t.Fatalf("expire quote: %v", err)
		}
		st, raw := c.do("POST", "/v1/rides", reqOpts{token: riderTok, idemKey: "exp-1", body: `{"quote_id":"` + quoteID + `","tier":"mini","payment_method":"upi"}`})
		wantStatus(t, st, http.StatusUnprocessableEntity, raw)
		wantCode(t, raw, "QUOTE_EXPIRED")
	})

	t.Run("quote owned by another rider → 403", func(t *testing.T) {
		otherID, _ := f.InsertRider()
		quoteID := f.InsertQuote(otherID)
		st, raw := c.do("POST", "/v1/rides", reqOpts{token: riderTok, idemKey: "own-1", body: `{"quote_id":"` + quoteID + `","tier":"mini","payment_method":"upi"}`})
		wantStatus(t, st, http.StatusForbidden, raw)
		wantCode(t, raw, "FORBIDDEN")
	})

	t.Run("nonexistent quote → 404 QUOTE_NOT_FOUND", func(t *testing.T) {
		st, raw := c.do("POST", "/v1/rides", reqOpts{token: riderTok, idemKey: "nf-1", body: `{"quote_id":"00000000-0000-0000-0000-000000000000","tier":"mini","payment_method":"upi"}`})
		wantStatus(t, st, http.StatusNotFound, raw)
		wantCode(t, raw, "QUOTE_NOT_FOUND")
	})
}

// TestIdempotencyOverHTTP covers the middleware's replay and reuse semantics on
// a real booking: same key + same body replays the stored 201; same key +
// different body → 422 IDEMPOTENCY_KEY_REUSED.
func TestIdempotencyOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	riderID, riderTok := f.InsertRider()
	quoteID := f.InsertQuote(riderID)

	body := `{"quote_id":"` + quoteID + `","tier":"mini","payment_method":"upi"}`
	st, raw := c.do("POST", "/v1/rides", reqOpts{token: riderTok, idemKey: "idem-1", body: body})
	wantStatus(t, st, http.StatusCreated, raw)
	first := decodeMap(t, raw)["id"].(string)
	f.TrackRide(first)

	// Same key + same body → replay the identical stored response.
	st, raw = c.do("POST", "/v1/rides", reqOpts{token: riderTok, idemKey: "idem-1", body: body})
	wantStatus(t, st, http.StatusCreated, raw)
	if got := decodeMap(t, raw)["id"].(string); got != first {
		t.Errorf("replay returned a different ride id: got %s, want %s", got, first)
	}
	if n := f.Count(`SELECT count(*) FROM rides WHERE rider_id = $1`, riderID); n != 1 {
		t.Errorf("replay created a second ride (count = %d)", n)
	}

	// Same key + different body → 422 reuse.
	st, raw = c.do("POST", "/v1/rides", reqOpts{token: riderTok, idemKey: "idem-1", body: `{"quote_id":"` + quoteID + `","tier":"sedan","payment_method":"upi"}`})
	wantStatus(t, st, http.StatusUnprocessableEntity, raw)
	wantCode(t, raw, "IDEMPOTENCY_KEY_REUSED")

	// Missing key → 400 required.
	st, raw = c.do("POST", "/v1/rides", reqOpts{token: riderTok, body: body})
	wantStatus(t, st, http.StatusBadRequest, raw)
	wantCode(t, raw, "IDEMPOTENCY_KEY_REQUIRED")
}

// TestConflictsOverHTTP covers 409 INVALID_STATE surfaced through the stack: a
// trip start against a ride that is not ARRIVED.
func TestConflictsOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	riderID, _ := f.InsertRider()
	driverID, driverTok := f.InsertDriver("mini", "on_trip")
	quoteID := f.InsertQuote(riderID)
	// Ride assigned to the driver but only DRIVER_ARRIVING (not ARRIVED): start
	// must 409.
	rideID := f.InsertRide(riderID, quoteID, "mini", "DRIVER_ARRIVING", &driverID, nil)

	st, raw := c.do("POST", "/v1/trips/"+rideID+"/start", reqOpts{token: driverTok, body: `{"otp":"1234"}`})
	wantStatus(t, st, http.StatusConflict, raw)
	wantCode(t, raw, "INVALID_STATE")
}

// TestWebhookErrorsOverHTTP covers the unauthenticated webhook's error paths
// through the router: bad signature (401) and unknown psp_ref (404).
func TestWebhookErrorsOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}

	t.Run("bad signature → 401", func(t *testing.T) {
		body := `{"psp_ref":"x","status":"success"}`
		st, raw := c.do("POST", "/v1/webhooks/psp", reqOpts{sig: "deadbeef", body: body})
		wantStatus(t, st, http.StatusUnauthorized, raw)
		wantCode(t, raw, "INVALID_SIGNATURE")
	})

	t.Run("valid signature, unknown psp_ref → 404", func(t *testing.T) {
		body := `{"psp_ref":"00000000-0000-0000-0000-000000000000","status":"success"}`
		sig := payments.Sign(testsupport.PSPSecret, []byte(body))
		st, raw := c.do("POST", "/v1/webhooks/psp", reqOpts{sig: sig, body: body})
		wantStatus(t, st, http.StatusNotFound, raw)
		wantCode(t, raw, "NOT_FOUND")
	})
}

// TestHealthzOverHTTP hits the public health endpoint (real Postgres+Redis
// pings behind it).
func TestHealthzOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	st, raw := c.do("GET", "/healthz", reqOpts{})
	wantStatus(t, st, http.StatusOK, raw)
}

// TestCancelOverHTTP covers the cancel handler and writeRideErr's INVALID_STATE
// branch: a fresh booking cancels once (200), a repeat cancel 409s, and a bad
// id 400s.
func TestCancelOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	riderID, riderTok := f.InsertRider()
	quoteID := f.InsertQuote(riderID)

	st, raw := c.do("POST", "/v1/rides", reqOpts{token: riderTok, idemKey: "c-1", body: `{"quote_id":"` + quoteID + `","tier":"mini","payment_method":"upi"}`})
	wantStatus(t, st, http.StatusCreated, raw)
	rideID := decodeMap(t, raw)["id"].(string)
	f.TrackRide(rideID)

	st, raw = c.do("POST", "/v1/rides/"+rideID+"/cancel", reqOpts{token: riderTok, body: `{"reason":"changed my mind"}`})
	wantStatus(t, st, http.StatusOK, raw)

	// Repeat cancel on a terminal ride → 409 INVALID_STATE.
	st, raw = c.do("POST", "/v1/rides/"+rideID+"/cancel", reqOpts{token: riderTok, body: `{"reason":"again"}`})
	wantStatus(t, st, http.StatusConflict, raw)
	wantCode(t, raw, "INVALID_STATE")

	// Bad id → 400.
	st, raw = c.do("POST", "/v1/rides/not-a-uuid/cancel", reqOpts{token: riderTok})
	wantStatus(t, st, http.StatusBadRequest, raw)
	wantCode(t, raw, "VALIDATION_FAILED")

	// Malformed JSON body (non-empty) → 400 from decodeJSON.
	st, raw = c.do("POST", "/v1/rides/"+uuid.NewString()+"/cancel", reqOpts{token: riderTok, body: `{bad json`})
	wantStatus(t, st, http.StatusBadRequest, raw)
	wantCode(t, raw, "VALIDATION_FAILED")
}

// TestPaymentErrorsOverHTTP covers writePaymentErr's branches through the stack.
func TestPaymentErrorsOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	riderID, riderTok := f.InsertRider()

	t.Run("unknown ride → 404", func(t *testing.T) {
		st, raw := c.do("POST", "/v1/payments", reqOpts{token: riderTok, idemKey: "pe-1", body: `{"ride_id":"00000000-0000-0000-0000-000000000000"}`})
		wantStatus(t, st, http.StatusNotFound, raw)
		wantCode(t, raw, "NOT_FOUND")
	})

	t.Run("ride not completed → 409 INVALID_STATE", func(t *testing.T) {
		driverID, _ := f.InsertDriver("mini", "on_trip")
		quoteID := f.InsertQuote(riderID)
		rideID := f.InsertRide(riderID, quoteID, "mini", "IN_PROGRESS", &driverID, nil)
		st, raw := c.do("POST", "/v1/payments", reqOpts{token: riderTok, idemKey: "pe-2", body: `{"ride_id":"` + rideID + `"}`})
		wantStatus(t, st, http.StatusConflict, raw)
		wantCode(t, raw, "INVALID_STATE")
	})

	t.Run("ride owned by another rider → 403", func(t *testing.T) {
		other, _ := f.InsertRider()
		quoteID := f.InsertQuote(other)
		rideID := f.InsertRide(other, quoteID, "mini", "COMPLETED", nil, nil)
		st, raw := c.do("POST", "/v1/payments", reqOpts{token: riderTok, idemKey: "pe-3", body: `{"ride_id":"` + rideID + `"}`})
		wantStatus(t, st, http.StatusForbidden, raw)
		wantCode(t, raw, "FORBIDDEN")
	})
}

// insertStartedTrip seeds a rider + on_trip driver + quote + IN_PROGRESS ride
// and a STARTED trip row, returning (rideID, driverID, driverToken).
func insertStartedTrip(t *testing.T, f *testsupport.Fixture) (string, string, string) {
	t.Helper()
	riderID, _ := f.InsertRider()
	driverID, driverTok := f.InsertDriver("mini", "on_trip")
	quoteID := f.InsertQuote(riderID)
	rideID := f.InsertRide(riderID, quoteID, "mini", "IN_PROGRESS", &driverID, nil)
	if _, err := f.Store.PG.Exec(f.Ctx, `
		INSERT INTO trips (id, ride_id, status, started_at, paused_seconds)
		VALUES ($1,$2,'STARTED',now(),0)`, uuid.NewString(), rideID); err != nil {
		t.Fatalf("insert trip: %v", err)
	}
	return rideID, driverID, driverTok
}

// TestPauseResumeOverHTTP covers the pause/resume handlers through the router.
func TestPauseResumeOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	rideID, _, driverTok := insertStartedTrip(t, f)

	st, raw := c.do("POST", "/v1/trips/"+rideID+"/pause", reqOpts{token: driverTok})
	wantStatus(t, st, http.StatusOK, raw)
	if decodeMap(t, raw)["status"] != "PAUSED" {
		t.Errorf("status after pause = %v, want PAUSED", decodeMap(t, raw)["status"])
	}
	st, raw = c.do("POST", "/v1/trips/"+rideID+"/resume", reqOpts{token: driverTok})
	wantStatus(t, st, http.StatusOK, raw)
	if decodeMap(t, raw)["status"] != "STARTED" {
		t.Errorf("status after resume = %v, want STARTED", decodeMap(t, raw)["status"])
	}
}

// TestTripErrorsOverHTTP covers writeTripErr's branches: wrong OTP (422),
// non-assigned driver (403), and an unknown ride (404).
func TestTripErrorsOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	riderID, _ := f.InsertRider()
	driverID, driverTok := f.InsertDriver("mini", "on_trip")
	quoteID := f.InsertQuote(riderID)

	// ARRIVED ride with a known OTP hash so start can exercise the bcrypt compare.
	otpHash := mustHash(t, "4321")
	rideID := f.InsertRide(riderID, quoteID, "mini", "ARRIVED", &driverID, &otpHash)

	t.Run("wrong otp → 422 INVALID_OTP", func(t *testing.T) {
		st, raw := c.do("POST", "/v1/trips/"+rideID+"/start", reqOpts{token: driverTok, body: `{"otp":"0000"}`})
		wantStatus(t, st, http.StatusUnprocessableEntity, raw)
		wantCode(t, raw, "INVALID_OTP")
	})

	t.Run("non-assigned driver → 403", func(t *testing.T) {
		otherID, otherTok := f.InsertDriver("mini", "on_trip")
		_ = otherID
		st, raw := c.do("POST", "/v1/trips/"+rideID+"/pause", reqOpts{token: otherTok})
		wantStatus(t, st, http.StatusForbidden, raw)
		wantCode(t, raw, "FORBIDDEN")
	})

	t.Run("unknown ride → 404", func(t *testing.T) {
		st, raw := c.do("POST", "/v1/trips/00000000-0000-0000-0000-000000000000/pause", reqOpts{token: driverTok})
		wantStatus(t, st, http.StatusNotFound, raw)
		wantCode(t, raw, "NOT_FOUND")
	})
}

// TestOTPRegenConflictOverHTTP covers riderOTP's INVALID_STATE branch (ride not
// in an OTP-regenerable state) and its bad-uuid validation.
func TestOTPRegenConflictOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	riderID, riderTok := f.InsertRider()
	quoteID := f.InsertQuote(riderID)
	// REQUESTED ride: OTP regen is only valid between assignment and start.
	rideID := f.InsertRide(riderID, quoteID, "mini", "REQUESTED", nil, nil)

	st, raw := c.do("POST", "/v1/rides/"+rideID+"/otp", reqOpts{token: riderTok, body: `{}`})
	wantStatus(t, st, http.StatusConflict, raw)
	wantCode(t, raw, "INVALID_STATE")

	st, raw = c.do("POST", "/v1/rides/not-a-uuid/otp", reqOpts{token: riderTok, body: `{}`})
	wantStatus(t, st, http.StatusBadRequest, raw)
	wantCode(t, raw, "VALIDATION_FAILED")
}

// TestAcceptDeclineOverHTTP covers the accept/decline handlers: accept with no
// held offer → 409 OFFER_EXPIRED; a seeded offer declines cleanly (200).
func TestAcceptDeclineOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	riderID, _ := f.InsertRider()
	driverID, driverTok := f.InsertDriver("mini", "available")
	quoteID := f.InsertQuote(riderID)
	rideID := f.InsertRide(riderID, quoteID, "mini", "MATCHING", nil, nil)

	t.Run("accept with no held offer → 409 OFFER_EXPIRED", func(t *testing.T) {
		st, raw := c.do("POST", "/v1/drivers/"+driverID+"/accept", reqOpts{token: driverTok, body: `{"ride_id":"` + rideID + `"}`})
		wantStatus(t, st, http.StatusConflict, raw)
		wantCode(t, raw, "OFFER_EXPIRED")
	})

	t.Run("decline a held offer → 200", func(t *testing.T) {
		if err := f.Store.Redis.Set(f.Ctx, "offer:driver:"+driverID, rideID, 12*time.Second).Err(); err != nil {
			t.Fatalf("seed offer: %v", err)
		}
		st, raw := c.do("POST", "/v1/drivers/"+driverID+"/decline", reqOpts{token: driverTok, body: `{"ride_id":"` + rideID + `"}`})
		wantStatus(t, st, http.StatusOK, raw)
	})

	t.Run("decline an unknown ride → 404", func(t *testing.T) {
		unknown := uuid.NewString()
		if err := f.Store.Redis.Set(f.Ctx, "offer:driver:"+driverID, unknown, 12*time.Second).Err(); err != nil {
			t.Fatalf("seed offer: %v", err)
		}
		f.TrackRedisKey("offer:ride:" + unknown)
		st, raw := c.do("POST", "/v1/drivers/"+driverID+"/decline", reqOpts{token: driverTok, body: `{"ride_id":"` + unknown + `"}`})
		wantStatus(t, st, http.StatusNotFound, raw)
		wantCode(t, raw, "NOT_FOUND")
	})
}

// TestAcceptRideGoneOverHTTP covers acceptOffer's ErrRideGone → 409 branch: the
// driver holds a live offer, but the ride is no longer MATCHING, so the
// assignment transaction affects zero rows.
func TestAcceptRideGoneOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	riderID, _ := f.InsertRider()
	// Ride already DRIVER_ASSIGNED (to someone) → not MATCHING.
	assigned, _ := f.InsertDriver("mini", "on_trip")
	quoteID := f.InsertQuote(riderID)
	rideID := f.InsertRide(riderID, quoteID, "mini", "DRIVER_ASSIGNED", &assigned, nil)

	// A different available driver holds a stale offer for the same ride.
	driverID, driverTok := f.InsertDriver("mini", "available")
	if err := f.Store.Redis.Set(f.Ctx, "offer:driver:"+driverID, rideID, 12*time.Second).Err(); err != nil {
		t.Fatalf("seed offer: %v", err)
	}
	st, raw := c.do("POST", "/v1/drivers/"+driverID+"/accept", reqOpts{token: driverTok, body: `{"ride_id":"` + rideID + `"}`})
	wantStatus(t, st, http.StatusConflict, raw)
	wantCode(t, raw, "INVALID_STATE")
}

// TestRideAccessErrorsOverHTTP covers getRide + rideProgress + writeRideErr
// branches: not-found (404), forbidden (403), already-active (409), and
// progression by a non-assigned driver.
func TestRideAccessErrorsOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	riderID, riderTok := f.InsertRider()

	t.Run("get nonexistent ride → 404", func(t *testing.T) {
		st, raw := c.do("GET", "/v1/rides/00000000-0000-0000-0000-000000000000", reqOpts{token: riderTok})
		wantStatus(t, st, http.StatusNotFound, raw)
		wantCode(t, raw, "NOT_FOUND")
	})

	t.Run("get another rider's ride → 403", func(t *testing.T) {
		other, _ := f.InsertRider()
		quoteID := f.InsertQuote(other)
		rideID := f.InsertRide(other, quoteID, "mini", "MATCHING", nil, nil)
		st, raw := c.do("GET", "/v1/rides/"+rideID, reqOpts{token: riderTok})
		wantStatus(t, st, http.StatusForbidden, raw)
		wantCode(t, raw, "FORBIDDEN")
	})

	t.Run("book a second active ride → 409 RIDE_ALREADY_ACTIVE", func(t *testing.T) {
		q1 := f.InsertQuote(riderID)
		st, raw := c.do("POST", "/v1/rides", reqOpts{token: riderTok, idemKey: "aa-1", body: `{"quote_id":"` + q1 + `","tier":"mini","payment_method":"upi"}`})
		wantStatus(t, st, http.StatusCreated, raw)
		f.TrackRide(decodeMap(t, raw)["id"].(string))
		q2 := f.InsertQuote(riderID)
		st, raw = c.do("POST", "/v1/rides", reqOpts{token: riderTok, idemKey: "aa-2", body: `{"quote_id":"` + q2 + `","tier":"mini","payment_method":"upi"}`})
		wantStatus(t, st, http.StatusConflict, raw)
		wantCode(t, raw, "RIDE_ALREADY_ACTIVE")
	})

	t.Run("arriving by a non-assigned driver → 403", func(t *testing.T) {
		other, _ := f.InsertRider()
		quoteID := f.InsertQuote(other)
		assigned, _ := f.InsertDriver("mini", "on_trip")
		rideID := f.InsertRide(other, quoteID, "mini", "DRIVER_ASSIGNED", &assigned, nil)
		_, notAssignedTok := f.InsertDriver("mini", "available")
		st, raw := c.do("POST", "/v1/rides/"+rideID+"/arriving", reqOpts{token: notAssignedTok})
		wantStatus(t, st, http.StatusForbidden, raw)
		wantCode(t, raw, "FORBIDDEN")
	})

	t.Run("arrived on unknown ride → 404", func(t *testing.T) {
		_, driverTok := f.InsertDriver("mini", "available")
		st, raw := c.do("POST", "/v1/rides/00000000-0000-0000-0000-000000000000/arrived", reqOpts{token: driverTok})
		wantStatus(t, st, http.StatusNotFound, raw)
		wantCode(t, raw, "NOT_FOUND")
	})
}

// TestRiderStateForbiddenOverHTTP covers riderState's self-only 403 branch.
func TestRiderStateForbiddenOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	_, riderTok := f.InsertRider()
	other, _ := f.InsertRider()
	st, raw := c.do("GET", "/v1/riders/"+other+"/state", reqOpts{token: riderTok})
	wantStatus(t, st, http.StatusForbidden, raw)
	wantCode(t, raw, "FORBIDDEN")

	// bad uuid → 400
	st, raw = c.do("GET", "/v1/riders/not-a-uuid/state", reqOpts{token: riderTok})
	wantStatus(t, st, http.StatusBadRequest, raw)
	wantCode(t, raw, "VALIDATION_FAILED")
}

// TestPaymentRetriesExhaustedOverHTTP covers writePaymentErr's
// PAYMENT_RETRIES_EXHAUSTED (409) branch.
func TestPaymentRetriesExhaustedOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	riderID, riderTok := f.InsertRider()
	quoteID := f.InsertQuote(riderID)
	rideID := f.InsertRide(riderID, quoteID, "mini", "COMPLETED", nil, nil)
	if _, err := f.Store.PG.Exec(f.Ctx,
		`INSERT INTO payments (id, ride_id, amount, method, status, retry_count, psp_ref)
		 VALUES (gen_random_uuid(), $1, 22000, 'upi', 'FAILED', 3, gen_random_uuid())`, rideID); err != nil {
		t.Fatalf("insert payment: %v", err)
	}
	st, raw := c.do("POST", "/v1/payments", reqOpts{token: riderTok, idemKey: "rex-1", body: `{"ride_id":"` + rideID + `"}`})
	wantStatus(t, st, http.StatusConflict, raw)
	wantCode(t, raw, "PAYMENT_RETRIES_EXHAUSTED")
}

// TestDriverAvailabilityConflictOverHTTP covers setAvailability's INVALID_STATE
// branch (an on-trip driver with an active ride cannot toggle availability) and
// updateLocation's rate-limit (429) branch.
func TestDriverAvailabilityConflictOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	riderID, _ := f.InsertRider()
	driverID, driverTok := f.InsertDriver("mini", "on_trip")
	quoteID := f.InsertQuote(riderID)
	// Active ride assigned to the on-trip driver blocks the availability toggle.
	f.InsertRide(riderID, quoteID, "mini", "IN_PROGRESS", &driverID, nil)

	st, raw := c.do("POST", "/v1/drivers/"+driverID+"/availability", reqOpts{token: driverTok, body: `{"available":false}`})
	wantStatus(t, st, http.StatusConflict, raw)
	wantCode(t, raw, "INVALID_STATE")
}

func TestLocationRateLimitOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	driverID, driverTok := f.InsertDriver("mini", "available")

	// SPEC: max 3 pings/sec. Fire a burst; at least one must be rate-limited.
	var got429 bool
	for i := 0; i < 6; i++ {
		st, _ := c.do("POST", "/v1/drivers/"+driverID+"/location", reqOpts{token: driverTok, body: `{"lat":12.97,"lng":77.59}`})
		if st == http.StatusTooManyRequests {
			got429 = true
		}
	}
	if !got429 {
		t.Errorf("expected at least one 429 in a 6-ping burst")
	}
}

// TestSSEOverHTTP covers the SSE stream handlers end to end: a successful ride
// stream (headers + serveSSE), the rider/driver authz + validation branches,
// and the driver stream.
func TestSSEOverHTTP(t *testing.T) {
	f := testsupport.New(t)
	c := &client{t: t, f: f, url: f.Server.URL}
	riderID, riderTok := f.InsertRider()
	driverID, driverTok := f.InsertDriver("mini", "available")
	quoteID := f.InsertQuote(riderID)
	rideID := f.InsertRide(riderID, quoteID, "mini", "MATCHING", nil, nil)

	// openStream connects, asserts the status, and (on 200) confirms the SSE
	// content type before closing the connection to unblock serveSSE.
	openStream := func(path, token string, wantCode int) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "GET", c.url+path, nil)
		if err != nil {
			t.Fatalf("new sse request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("sse do: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != wantCode {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("sse %s status = %d, want %d (body: %s)", path, resp.StatusCode, wantCode, b)
		}
		if wantCode == http.StatusOK {
			if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
				t.Errorf("sse Content-Type = %q, want text/event-stream", ct)
			}
		}
	}

	t.Run("rider streams own ride → 200", func(t *testing.T) {
		openStream("/v1/events?ride_id="+rideID, riderTok, http.StatusOK)
	})
	t.Run("bad ride_id → 400", func(t *testing.T) {
		openStream("/v1/events?ride_id=not-a-uuid", riderTok, http.StatusBadRequest)
	})
	t.Run("unknown ride → 404", func(t *testing.T) {
		openStream("/v1/events?ride_id=00000000-0000-0000-0000-000000000000", riderTok, http.StatusNotFound)
	})
	t.Run("other rider forbidden → 403", func(t *testing.T) {
		_, otherTok := f.InsertRider()
		openStream("/v1/events?ride_id="+rideID, otherTok, http.StatusForbidden)
	})
	t.Run("driver streams own channel → 200", func(t *testing.T) {
		openStream("/v1/events/driver/"+driverID, driverTok, http.StatusOK)
	})
	t.Run("driver streams another driver's channel → 403", func(t *testing.T) {
		otherID, _ := f.InsertDriver("mini", "available")
		openStream("/v1/events/driver/"+otherID, driverTok, http.StatusForbidden)
	})
}
