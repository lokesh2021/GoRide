package observability

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/newrelic/go-agent/v3/newrelic"
)

// Middleware returns chi-compatible middleware that starts a New Relic
// Transaction for each request and attaches it to the request context (via
// newrelic.NewContext) so datastore segments (nrpgx5, nrredis-v9) and any
// custom instrumentation downstream can find it with newrelic.FromContext.
//
// Naming: a Transaction must be named before it can be usefully aggregated,
// but the resolved chi route PATTERN (e.g. "POST /v1/rides", not
// "POST /v1/rides/3fa4...") is only fully known once chi has finished routing
// the request — chi.RouteContext accumulates RoutePatterns as it walks the
// route tree, which happens inside next.ServeHTTP. So this middleware starts
// the transaction with a placeholder name, calls next.ServeHTTP, and only
// then reads chi.RouteContext(r.Context()).RoutePattern() to rename it —
// the standard post-serve naming approach for tree routers, since there is no
// pattern to read before routing happens.
//
// SSE stream routes (see sseRoutePrefix) are excluded unconditionally: they
// are long-lived, potentially hours-long connections, and a "transaction"
// spanning one would report a nonsense duration and pollute every latency
// percentile the agent computes for the rest of the app. A nil app
// (monitoring disabled) makes the whole middleware a pass-through no-op.
func Middleware(app *newrelic.Application) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if app == nil || strings.HasPrefix(r.URL.Path, sseRoutePrefix) {
				next.ServeHTTP(w, r)
				return
			}

			txn := app.StartTransaction(r.Method + " " + r.URL.Path)
			defer txn.End()

			txn.SetWebRequestHTTP(r)
			ww := txn.SetWebResponse(w)
			rc := r.WithContext(newrelic.NewContext(r.Context(), txn))

			next.ServeHTTP(ww, rc)

			if pattern := chi.RouteContext(rc.Context()).RoutePattern(); pattern != "" {
				txn.SetName(r.Method + " " + pattern)
			}
		})
	}
}
