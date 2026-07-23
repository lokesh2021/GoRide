// Package httpapi wires the chi router, shared middleware, and JSON
// response helpers used by all HTTP handlers.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/newrelic/go-agent/v3/newrelic"

	"github.com/lokeshbm/goride/internal/drivers"
	"github.com/lokeshbm/goride/internal/events"
	"github.com/lokeshbm/goride/internal/matching"
	"github.com/lokeshbm/goride/internal/observability"
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
	// SlowRequestMs is the duration threshold (ms) above which requestLogger
	// emits a request at Warn level instead of Info (GORIDE_SLOW_REQUEST_MS).
	SlowRequestMs int
	// Obs is the New Relic application (nil when monitoring is disabled per
	// GORIDE_NEWRELIC_LICENSE). NewRouter wires it in as per-request
	// transaction middleware; nil makes that middleware a pass-through no-op.
	Obs *newrelic.Application
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
	r.Use(requestLogger(deps.Logger, int64(deps.SlowRequestMs)))
	r.Use(middleware.Recoverer)
	// New Relic transaction per request, named by the resolved chi route
	// pattern (SSE streams excluded — see observability.Middleware doc).
	r.Use(observability.Middleware(deps.Obs))

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
		rt.Get("/riders/{id}/state", requireRole(rides.RoleRider, deps.riderState))
		rt.Post("/rides/{id}/otp", requireRole(rides.RoleRider, deps.riderOTP))
		rt.Get("/drivers/{id}/state", requireRole(rides.RoleDriver, deps.driverState))
	})
}

// requestLogger logs each request via slog, tagged with the chi request ID.
//
// The log level reflects the outcome so operators can alert on it: status>=500
// → Error, status>=400 or a slow response (duration_ms > slowMs) → Warn, else
// Info. The chi route PATTERN (e.g. "/v1/rides/{id}") is read AFTER
// next.ServeHTTP — it is only known once chi has finished routing — and logged
// as "route" so lines aggregate by endpoint instead of by raw UUID path. SSE
// stream routes are never slow-warned: they are long-lived by design, so a
// large duration_ms is expected and is not a latency signal.
func requestLogger(logger *slog.Logger, slowMs int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, req.ProtoMajor)

			next.ServeHTTP(ww, req)

			duration := time.Since(start).Milliseconds()
			status := ww.Status()
			attrs := []any{
				"request_id", middleware.GetReqID(req.Context()),
				"method", req.Method,
				"path", req.URL.Path,
				"route", chi.RouteContext(req.Context()).RoutePattern(),
				"status", status,
				"bytes", ww.BytesWritten(),
				"duration_ms", duration,
			}

			isSSE := strings.HasPrefix(req.URL.Path, sseRoutePathPrefix)
			switch {
			case status >= http.StatusInternalServerError:
				logger.Error(logMsgHTTPRequest, attrs...)
			case status >= http.StatusBadRequest || (!isSSE && duration > slowMs):
				logger.Warn(logMsgHTTPRequest, attrs...)
			default:
				logger.Info(logMsgHTTPRequest, attrs...)
			}
		})
	}
}
