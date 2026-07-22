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

	"github.com/lokeshbm/goride/internal/quotes"
	"github.com/lokeshbm/goride/internal/rides"
	"github.com/lokeshbm/goride/internal/store"
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
	Health HealthChecker
	Store  *store.Store
	Quotes *quotes.Service
	Rides  *rides.Service
	Logger *slog.Logger
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
