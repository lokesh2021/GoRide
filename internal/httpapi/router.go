// Package httpapi wires the chi router, shared middleware, and JSON
// response helpers used by all HTTP handlers.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/lokeshbm/goride/internal/drivers"
	"github.com/lokeshbm/goride/internal/events"
	"github.com/lokeshbm/goride/internal/matching"
	"github.com/lokeshbm/goride/internal/payments"
	"github.com/lokeshbm/goride/internal/quotes"
	"github.com/lokeshbm/goride/internal/rides"
	"github.com/lokeshbm/goride/internal/store"
	"github.com/lokeshbm/goride/internal/trips"
)

// HealthChecker is implemented by the store layer; kept as an interface here
// so httpapi has no dependency on pgx/redis types directly.
type HealthChecker interface {
	PingPostgres(ctx context.Context) error
	PingRedis(ctx context.Context) error
}

// Deps holds the dependencies handlers need. Later milestones extend this
// with matching/trip/payment services.
type Deps struct {
	Health   HealthChecker
	Store    *store.Store
	Quotes   *quotes.Service
	Rides    *rides.Service
	Drivers  *drivers.Service
	Match    *matching.Engine
	Trips    *trips.Service
	Payments *payments.Service
	Events   *events.Hub
	Logger   *slog.Logger
}

// NewRouter builds the chi router with base middleware and mounted routes.
//
// The 60s request timeout is not applied here at the root: it is added
// per-route in Routes (via r.With) to every route except /v1/events and
// /v1/events/driver/{id}, whose SSE streams are long-lived by design and must
// not be cut off by it.
func NewRouter(deps Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(requestLogger(deps.Logger))
	r.Use(middleware.Recoverer)

	Routes(r, deps)

	return r
}

// Routes mounts all HTTP routes: the public health check plus the
// authenticated v1 API (quotes/rides/drivers/trips/payments/events).
func Routes(r chi.Router, deps Deps) {
	timeout := middleware.Timeout(60 * time.Second)

	r.With(timeout).Get("/healthz", healthzHandler(deps))

	// PSP webhook is unauthenticated (external caller) — authenticated instead by
	// an HMAC signature over the body. Registered outside the /v1 auth group.
	r.With(timeout).Post("/v1/webhooks/psp", deps.pspWebhook)

	r.Route("/v1", func(r chi.Router) {
		r.Use(deps.authMiddleware)

		// SSE event streams — deliberately registered on r (no timeout
		// middleware) rather than on the rt (timeout-wrapped) group below.
		r.Get("/events", requireAnyRole([]string{rides.RoleRider, rides.RoleDriver}, deps.streamRideEvents))
		r.Get("/events/driver/{id}", requireRole(rides.RoleDriver, deps.streamDriverEvents))

		// Every other /v1 route keeps the pre-existing 60s request timeout.
		rt := r.With(timeout)

		// Quotes — rider only.
		rt.Post("/quotes", requireRole(rides.RoleRider, deps.createQuote))

		// Rides.
		rt.Post("/rides", requireRole(rides.RoleRider, deps.idempotency(deps.createRide)))
		rt.Get("/rides/{id}", requireAnyRole([]string{rides.RoleRider, rides.RoleDriver}, deps.getRide))
		// Cancel is state-machine guarded (repeat cancels return 409), so it is
		// intentionally not idempotency-wrapped: a retry must re-evaluate state
		// rather than replay a stored 200.
		rt.Post("/rides/{id}/cancel", requireAnyRole([]string{rides.RoleRider, rides.RoleDriver}, deps.cancelRide))

		// Driver-side ride progression — assigned driver only, guarded funnel.
		rt.Post("/rides/{id}/arriving", requireRole(rides.RoleDriver, deps.rideArriving))
		rt.Post("/rides/{id}/arrived", requireRole(rides.RoleDriver, deps.rideArrived))

		// Drivers — driver only, actor must match {id} (enforced in handler → 403).
		// location + availability are idempotency-exempt per SPEC. accept/decline
		// are state-machine guarded and replay-safe by design (see cancel above),
		// so they are likewise not idempotency-wrapped.
		rt.Post("/drivers/{id}/location", requireRole(rides.RoleDriver, deps.updateLocation))
		rt.Post("/drivers/{id}/availability", requireRole(rides.RoleDriver, deps.setAvailability))
		rt.Post("/drivers/{id}/accept", requireRole(rides.RoleDriver, deps.acceptOffer))
		rt.Post("/drivers/{id}/decline", requireRole(rides.RoleDriver, deps.declineOffer))

		// Trips — assigned driver only. {id} is the RIDE id (trips are 1:1 with
		// rides). start/pause/resume are state-machine guarded (replay → 409),
		// like accept/arrived, so they are not idempotency-wrapped; end finalizes
		// the fare and is idempotency-wrapped so a retry replays the stored result.
		rt.Post("/trips/{id}/start", requireRole(rides.RoleDriver, deps.startTrip))
		rt.Post("/trips/{id}/pause", requireRole(rides.RoleDriver, deps.pauseTrip))
		rt.Post("/trips/{id}/resume", requireRole(rides.RoleDriver, deps.resumeTrip))
		rt.Post("/trips/{id}/end", requireRole(rides.RoleDriver, deps.idempotency(deps.endTrip)))

		// Payments — rider only. Trigger is idempotency-wrapped.
		rt.Post("/payments", requireRole(rides.RoleRider, deps.idempotency(deps.triggerPayment)))

		// Ride history — rider, self only.
		rt.Get("/riders/{id}/rides", requireRole(rides.RoleRider, deps.riderHistory))
	})
}

// requestLogger logs each request via slog, tagged with the chi request ID.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, req.ProtoMajor)

			next.ServeHTTP(ww, req)

			logger.Info(logMsgHTTPRequest,
				"request_id", middleware.GetReqID(req.Context()),
				"method", req.Method,
				"path", req.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}
