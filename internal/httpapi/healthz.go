package httpapi

import "net/http"

// healthzHandler checks Postgres (SELECT 1) and Redis (PING), per SPEC.
func healthzHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if err := deps.Health.PingPostgres(ctx); err != nil {
			WriteErr(w, http.StatusServiceUnavailable, CodeDependencyUnavailable, "postgres unavailable: "+err.Error())
			return
		}
		if err := deps.Health.PingRedis(ctx); err != nil {
			WriteErr(w, http.StatusServiceUnavailable, CodeDependencyUnavailable, "redis unavailable: "+err.Error())
			return
		}

		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
