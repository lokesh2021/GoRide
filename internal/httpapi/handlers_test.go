package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/lokeshbm/goride/internal/payments"
	"github.com/lokeshbm/goride/internal/rides"
)

// These are white-box handler unit tests: no Postgres/Redis. Every case here
// fails validation (or auth/role/path) strictly before any Store or service
// call, so a Deps with nil services is safe. Cases that require a real token
// lookup (unknown token → 401, valid-token role checks) hit the DB and live in
// the integration suite instead.

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// validActorID is a well-formed UUID used where an actor id must parse.
const validActorID = "11111111-1111-1111-1111-111111111111"

// doReq invokes h directly with an optional actor in context and optional chi
// URL params, returning the recorded response.
func doReq(h http.HandlerFunc, method, target, body string, actor *Actor, params map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	ctx := req.Context()
	if params != nil {
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	}
	if actor != nil {
		ctx = context.WithValue(ctx, actorCtxKey, *actor)
	}
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// assertErrEnvelope asserts the response is the SPEC error envelope with the
// wanted status and code, a non-empty message, and JSON content type.
func assertErrEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) ErrorBody {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, wantStatus, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not a JSON error envelope: %v (body: %s)", err, rec.Body.String())
	}
	if body.Error.Code != wantCode {
		t.Errorf("error.code = %q, want %q (body: %s)", body.Error.Code, wantCode, rec.Body.String())
	}
	if body.Error.Message == "" {
		t.Errorf("error.message is empty (body: %s)", rec.Body.String())
	}
	return body
}

func riderActor() *Actor  { return &Actor{ID: validActorID, Role: rides.RoleRider} }
func driverActor() *Actor { return &Actor{ID: validActorID, Role: rides.RoleDriver} }

// TestValidationMatrix drives every mutating POST endpoint through its
// required-field / range violations, asserting a 400 VALIDATION_FAILED whose
// message names the offending field, and that every body is a valid envelope.
func TestValidationMatrix(t *testing.T) {
	deps := Deps{Logger: testLogger()}

	tests := []struct {
		name      string
		handler   http.HandlerFunc
		method    string
		target    string
		body      string
		actor     *Actor
		params    map[string]string
		wantField string // substring expected in the error message
	}{
		// ---- POST /v1/quotes ----
		{"quotes: unknown field", deps.createQuote, "POST", "/v1/quotes", `{"nope":1}`, riderActor(), nil, "invalid JSON"},
		{"quotes: pickup.lat missing", deps.createQuote, "POST", "/v1/quotes", `{"drop":{"lat":12.9,"lng":77.6}}`, riderActor(), nil, "pickup.lat"},
		{"quotes: pickup.lng missing", deps.createQuote, "POST", "/v1/quotes", `{"pickup":{"lat":12.9},"drop":{"lat":12.9,"lng":77.6}}`, riderActor(), nil, "pickup.lng"},
		{"quotes: drop.lat missing", deps.createQuote, "POST", "/v1/quotes", `{"pickup":{"lat":12.9,"lng":77.6}}`, riderActor(), nil, "drop.lat"},
		{"quotes: pickup.lat out of range", deps.createQuote, "POST", "/v1/quotes", `{"pickup":{"lat":95,"lng":77.6},"drop":{"lat":12.9,"lng":77.6}}`, riderActor(), nil, "pickup.lat"},
		{"quotes: pickup.lng out of range", deps.createQuote, "POST", "/v1/quotes", `{"pickup":{"lat":12.9,"lng":200},"drop":{"lat":12.9,"lng":77.6}}`, riderActor(), nil, "pickup.lng"},
		{"quotes: drop.lng out of range", deps.createQuote, "POST", "/v1/quotes", `{"pickup":{"lat":12.9,"lng":77.6},"drop":{"lat":12.9,"lng":-190}}`, riderActor(), nil, "drop.lng"},
		{"quotes: pickup == drop", deps.createQuote, "POST", "/v1/quotes", `{"pickup":{"lat":12.9,"lng":77.6},"drop":{"lat":12.9,"lng":77.6}}`, riderActor(), nil, "drop"},

		// ---- POST /v1/rides ----
		{"rides: quote_id not a uuid", deps.createRide, "POST", "/v1/rides", `{"quote_id":"not-a-uuid","tier":"mini","payment_method":"upi"}`, riderActor(), nil, "quote_id"},
		{"rides: bad tier", deps.createRide, "POST", "/v1/rides", `{"quote_id":"` + validActorID + `","tier":"bike","payment_method":"upi"}`, riderActor(), nil, "tier"},
		{"rides: bad payment_method", deps.createRide, "POST", "/v1/rides", `{"quote_id":"` + validActorID + `","tier":"mini","payment_method":"bitcoin"}`, riderActor(), nil, "payment_method"},

		// ---- POST /v1/rides/{id}/cancel ----
		{"cancel: id not a uuid", deps.cancelRide, "POST", "/v1/rides/x/cancel", ``, riderActor(), map[string]string{"id": "not-a-uuid"}, "id"},

		// ---- POST /v1/drivers/{id}/location ----
		{"location: id not a uuid", deps.updateLocation, "POST", "/v1/drivers/x/location", `{"lat":1,"lng":2}`, driverActor(), map[string]string{"id": "nope"}, "id"},
		{"location: lat/lng missing", deps.updateLocation, "POST", "/v1/drivers/x/location", `{}`, driverActor(), map[string]string{"id": validActorID}, "lat/lng"},
		{"location: lat out of range", deps.updateLocation, "POST", "/v1/drivers/x/location", `{"lat":95,"lng":2}`, driverActor(), map[string]string{"id": validActorID}, "lat"},
		{"location: lng out of range", deps.updateLocation, "POST", "/v1/drivers/x/location", `{"lat":1,"lng":181}`, driverActor(), map[string]string{"id": validActorID}, "lng"},

		// ---- POST /v1/drivers/{id}/availability ----
		{"availability: missing available", deps.setAvailability, "POST", "/v1/drivers/x/availability", `{}`, driverActor(), map[string]string{"id": validActorID}, "available"},

		// ---- POST /v1/drivers/{id}/accept & /decline ----
		{"accept: ride_id not a uuid", deps.acceptOffer, "POST", "/v1/drivers/x/accept", `{"ride_id":"nope"}`, driverActor(), map[string]string{"id": validActorID}, "ride_id"},
		{"decline: ride_id not a uuid", deps.declineOffer, "POST", "/v1/drivers/x/decline", `{"ride_id":"nope"}`, driverActor(), map[string]string{"id": validActorID}, "ride_id"},

		// ---- POST /v1/trips/{id}/start ----
		{"start: id not a uuid", deps.startTrip, "POST", "/v1/trips/x/start", `{"otp":"1234"}`, driverActor(), map[string]string{"id": "nope"}, "id"},
		{"start: otp missing", deps.startTrip, "POST", "/v1/trips/x/start", `{}`, driverActor(), map[string]string{"id": validActorID}, "otp"},

		// ---- POST /v1/payments ----
		{"payments: ride_id not a uuid", deps.triggerPayment, "POST", "/v1/payments", `{"ride_id":"nope"}`, riderActor(), nil, "ride_id"},

		// ---- GET /v1/riders/{id}/rides ----
		{"history: id not a uuid", deps.riderHistory, "GET", "/v1/riders/x/rides", ``, riderActor(), map[string]string{"id": "nope"}, "id"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(tc.handler, tc.method, tc.target, tc.body, tc.actor, tc.params)
			body := assertErrEnvelope(t, rec, http.StatusBadRequest, CodeValidationFailed)
			if !strings.Contains(body.Error.Message, tc.wantField) {
				t.Errorf("message %q does not name field %q", body.Error.Message, tc.wantField)
			}
		})
	}
}

// TestPathActorMismatch covers path/actor mismatch → 403 for the {id}-scoped
// endpoints that enforce self-only access. These checks are pure (no DB): the
// actor id from the token is compared to the path id.
func TestPathActorMismatch(t *testing.T) {
	deps := Deps{Logger: testLogger()}
	other := "22222222-2222-2222-2222-222222222222" // valid uuid, != actor id

	tests := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		body    string
		actor   *Actor
	}{
		{"location: acting for another driver", deps.updateLocation, "POST", `{"lat":1,"lng":2}`, driverActor()},
		{"availability: acting for another driver", deps.setAvailability, "POST", `{"available":true}`, driverActor()},
		{"accept: acting for another driver", deps.acceptOffer, "POST", `{"ride_id":"` + validActorID + `"}`, driverActor()},
		{"decline: acting for another driver", deps.declineOffer, "POST", `{"ride_id":"` + validActorID + `"}`, driverActor()},
		{"history: reading another rider", deps.riderHistory, "GET", ``, riderActor()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(tc.handler, tc.method, "/", tc.body, tc.actor, map[string]string{"id": other})
			assertErrEnvelope(t, rec, http.StatusForbidden, CodeForbidden)
		})
	}
}

// TestRiderHistoryWrongRole covers a driver token calling the rider-only history
// endpoint for a matching path id → 403 (role check inside the handler).
func TestRiderHistoryWrongRole(t *testing.T) {
	deps := Deps{Logger: testLogger()}
	rec := doReq(deps.riderHistory, "GET", "/", "", driverActor(), map[string]string{"id": validActorID})
	assertErrEnvelope(t, rec, http.StatusForbidden, CodeForbidden)
}

// TestAuthMiddleware covers the token-shape checks that short-circuit before any
// DB lookup: a missing or malformed Authorization header → 401 UNAUTHORIZED, and
// the wrapped handler is never reached.
func TestAuthMiddleware(t *testing.T) {
	deps := Deps{Logger: testLogger()}
	tests := []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"wrong scheme", "Token abc123"},
		{"bearer with empty token", "Bearer "},
		{"bearer with only spaces", "Bearer    "},
		{"prefix only, no space", "Bearer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })
			req := httptest.NewRequest("GET", "/v1/rides/"+validActorID, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			deps.authMiddleware(next).ServeHTTP(rec, req)
			assertErrEnvelope(t, rec, http.StatusUnauthorized, CodeUnauthorized)
			if reached {
				t.Error("wrapped handler should not be reached on auth failure")
			}
		})
	}
}

// TestRequireRole covers the pure role-guard decisions: no actor → 401, wrong
// role → 403, correct role → passes through. requireAnyRole is covered too.
func TestRequireRole(t *testing.T) {
	pass := func() (http.HandlerFunc, *bool) {
		reached := new(bool)
		return func(w http.ResponseWriter, r *http.Request) { *reached = true }, reached
	}

	t.Run("no actor in context → 401", func(t *testing.T) {
		h, reached := pass()
		rec := doReq(requireRole(rides.RoleRider, h), "POST", "/", "", nil, nil)
		assertErrEnvelope(t, rec, http.StatusUnauthorized, CodeUnauthorized)
		if *reached {
			t.Error("handler reached despite missing actor")
		}
	})

	t.Run("wrong role → 403", func(t *testing.T) {
		h, reached := pass()
		rec := doReq(requireRole(rides.RoleRider, h), "POST", "/", "", driverActor(), nil)
		assertErrEnvelope(t, rec, http.StatusForbidden, CodeForbidden)
		if *reached {
			t.Error("handler reached despite wrong role")
		}
	})

	t.Run("correct role passes through", func(t *testing.T) {
		h, reached := pass()
		rec := doReq(requireRole(rides.RoleRider, h), "POST", "/", "", riderActor(), nil)
		if !*reached {
			t.Errorf("handler not reached for correct role (status %d)", rec.Code)
		}
	})

	t.Run("requireAnyRole rejects an unlisted role → 403", func(t *testing.T) {
		h, reached := pass()
		guard := requireAnyRole([]string{rides.RoleRider}, h)
		rec := doReq(guard, "POST", "/", "", driverActor(), nil)
		assertErrEnvelope(t, rec, http.StatusForbidden, CodeForbidden)
		if *reached {
			t.Error("handler reached despite unlisted role")
		}
	})

	t.Run("requireAnyRole accepts a listed role", func(t *testing.T) {
		h, reached := pass()
		guard := requireAnyRole([]string{rides.RoleRider, rides.RoleDriver}, h)
		rec := doReq(guard, "POST", "/", "", driverActor(), nil)
		if !*reached {
			t.Errorf("handler not reached for a listed role (status %d)", rec.Code)
		}
	})
}

// TestIdempotencyKeyRequired covers the middleware's pre-DB guard: a mutating
// endpoint invoked without an Idempotency-Key header → 400, handler untouched.
func TestIdempotencyKeyRequired(t *testing.T) {
	deps := Deps{Logger: testLogger()}
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })
	rec := doReq(deps.idempotency(next), "POST", "/v1/rides", `{}`, riderActor(), nil)
	assertErrEnvelope(t, rec, http.StatusBadRequest, CodeIdempotencyKeyRequired)
	if reached {
		t.Error("handler reached despite missing Idempotency-Key")
	}
}

// TestPSPWebhookValidation covers the unauthenticated webhook's signature and
// body validation, which are pure (HMAC verify + JSON parse, no DB). VerifySignature
// only reads the shared secret, so a Payments service with nil store is safe here.
func TestPSPWebhookValidation(t *testing.T) {
	const secret = "test-secret"
	deps := Deps{
		Logger:   testLogger(),
		Payments: payments.NewService(nil, nil, nil, secret, testLogger()),
	}

	sign := func(body string) string { return payments.Sign(secret, []byte(body)) }

	t.Run("missing signature → 401", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/webhooks/psp", strings.NewReader(`{"psp_ref":"x","status":"success"}`))
		rec := httptest.NewRecorder()
		deps.pspWebhook(rec, req)
		assertErrEnvelope(t, rec, http.StatusUnauthorized, CodeInvalidSignature)
	})

	t.Run("wrong signature → 401", func(t *testing.T) {
		body := `{"psp_ref":"x","status":"success"}`
		req := httptest.NewRequest("POST", "/v1/webhooks/psp", strings.NewReader(body))
		req.Header.Set("X-PSP-Signature", "deadbeef")
		rec := httptest.NewRecorder()
		deps.pspWebhook(rec, req)
		assertErrEnvelope(t, rec, http.StatusUnauthorized, CodeInvalidSignature)
	})

	t.Run("valid signature, malformed JSON → 400", func(t *testing.T) {
		body := `{not json`
		req := httptest.NewRequest("POST", "/v1/webhooks/psp", strings.NewReader(body))
		req.Header.Set("X-PSP-Signature", sign(body))
		rec := httptest.NewRecorder()
		deps.pspWebhook(rec, req)
		assertErrEnvelope(t, rec, http.StatusBadRequest, CodeValidationFailed)
	})

	t.Run("valid signature, missing psp_ref → 400", func(t *testing.T) {
		body := `{"status":"success"}`
		req := httptest.NewRequest("POST", "/v1/webhooks/psp", strings.NewReader(body))
		req.Header.Set("X-PSP-Signature", sign(body))
		rec := httptest.NewRecorder()
		deps.pspWebhook(rec, req)
		b := assertErrEnvelope(t, rec, http.StatusBadRequest, CodeValidationFailed)
		if !strings.Contains(b.Error.Message, "psp_ref") {
			t.Errorf("message %q does not name psp_ref", b.Error.Message)
		}
	})
}
