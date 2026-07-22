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
)

// HealthChecker is implemented by the store layer; kept as an interface here
// so httpapi has no dependency on pgx/redis types directly.
type HealthChecker interface {
	PingPostgres(ctx context.Context) error
	PingRedis(ctx context.Context) error
}

// Deps holds the dependencies handlers need. Later milestones extend this
// with quote/ride/matching/payment services.
type Deps struct {
	Health HealthChecker
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

// Routes mounts all HTTP routes. Only /healthz is mounted in this milestone;
// later milestones add quotes/rides/drivers/trips/payments/events under it.
func Routes(r chi.Router, deps Deps) {
	r.Get("/healthz", healthzHandler(deps))
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
