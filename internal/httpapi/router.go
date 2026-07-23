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
	Logger   *slog.Logger
}

// NewRouter builds the chi router with base middleware and mounted routes.
func NewRouter(deps Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(requestLogger(deps.Logger))
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	Routes(r, deps)

	return r
}

// Routes mounts all HTTP routes: the public health check plus the
// authenticated v1 quote/ride API. Drivers/trips/payments/events land in later
// milestones.
func Routes(r chi.Router, deps Deps) {
	r.Get("/healthz", healthzHandler(deps))

	// PSP webhook is unauthenticated (external caller) — authenticated instead by
	// an HMAC signature over the body. Registered outside the /v1 auth group.
	r.Post("/v1/webhooks/psp", deps.pspWebhook)

	r.Route("/v1", func(r chi.Router) {
		r.Use(deps.authMiddleware)

		// Quotes — rider only.
		r.Post("/quotes", requireRole(rides.RoleRider, deps.createQuote))

		// Rides.
		r.Post("/rides", requireRole(rides.RoleRider, deps.idempotency(deps.createRide)))
		r.Get("/rides/{id}", requireAnyRole([]string{rides.RoleRider, rides.RoleDriver}, deps.getRide))
		// Cancel is state-machine guarded (repeat cancels return 409), so it is
		// intentionally not idempotency-wrapped: a retry must re-evaluate state
		// rather than replay a stored 200.
		r.Post("/rides/{id}/cancel", requireAnyRole([]string{rides.RoleRider, rides.RoleDriver}, deps.cancelRide))

		// Driver-side ride progression — assigned driver only, guarded funnel.
		r.Post("/rides/{id}/arriving", requireRole(rides.RoleDriver, deps.rideArriving))
		r.Post("/rides/{id}/arrived", requireRole(rides.RoleDriver, deps.rideArrived))

		// Drivers — driver only, actor must match {id} (enforced in handler → 403).
		// location + availability are idempotency-exempt per SPEC. accept/decline
		// are state-machine guarded and replay-safe by design (see cancel above),
		// so they are likewise not idempotency-wrapped.
		r.Post("/drivers/{id}/location", requireRole(rides.RoleDriver, deps.updateLocation))
		r.Post("/drivers/{id}/availability", requireRole(rides.RoleDriver, deps.setAvailability))
		r.Post("/drivers/{id}/accept", requireRole(rides.RoleDriver, deps.acceptOffer))
		r.Post("/drivers/{id}/decline", requireRole(rides.RoleDriver, deps.declineOffer))

		// Trips — assigned driver only. {id} is the RIDE id (trips are 1:1 with
		// rides). start/pause/resume are state-machine guarded (replay → 409),
		// like accept/arrived, so they are not idempotency-wrapped; end finalizes
		// the fare and is idempotency-wrapped so a retry replays the stored result.
		r.Post("/trips/{id}/start", requireRole(rides.RoleDriver, deps.startTrip))
		r.Post("/trips/{id}/pause", requireRole(rides.RoleDriver, deps.pauseTrip))
		r.Post("/trips/{id}/resume", requireRole(rides.RoleDriver, deps.resumeTrip))
		r.Post("/trips/{id}/end", requireRole(rides.RoleDriver, deps.idempotency(deps.endTrip)))

		// Payments — rider only. Trigger is idempotency-wrapped.
		r.Post("/payments", requireRole(rides.RoleRider, deps.idempotency(deps.triggerPayment)))

		// Ride history — rider, self only.
		r.Get("/riders/{id}/rides", requireRole(rides.RoleRider, deps.riderHistory))
	})
}

// requestLogger logs each request via slog, tagged with the chi request ID.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, req.ProtoMajor)

			next.ServeHTTP(ww, req)

			logger.Info("http_request",
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
