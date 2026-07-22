package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Actor is the authenticated caller resolved from the bearer token.
type Actor struct {
	ID   string
	Role string // "rider" | "driver"
}

type ctxKey int

const actorCtxKey ctxKey = iota

// ActorFrom returns the authenticated actor stored in ctx by authMiddleware.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorCtxKey).(Actor)
	return a, ok
}

// authMiddleware resolves `Authorization: Bearer <token>` against riders and
// drivers api_token columns and stores the Actor in the request context.
// Missing/invalid tokens → 401 UNAUTHORIZED.
func (deps Deps) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			WriteErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing or malformed Authorization header")
			return
		}

		actor, err := resolveToken(r.Context(), deps, token)
		if errors.Is(err, errNoActor) {
			WriteErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid token")
			return
		}
		if err != nil {
			deps.Logger.Error("auth: resolve token failed", "error", err)
			WriteErr(w, http.StatusInternalServerError, "INTERNAL", "authentication failed")
			return
		}

		ctx := context.WithValue(r.Context(), actorCtxKey, actor)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireRole guards a handler to a single actor role, else 403 FORBIDDEN.
func requireRole(role string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := ActorFrom(r.Context())
		if !ok {
			WriteErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
			return
		}
		if actor.Role != role {
			WriteErr(w, http.StatusForbidden, "FORBIDDEN", "requires "+role+" role")
			return
		}
		next(w, r)
	}
}

// requireAnyRole guards a handler to any of the given roles, else 403.
func requireAnyRole(roles []string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := ActorFrom(r.Context())
		if !ok {
			WriteErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
			return
		}
		for _, role := range roles {
			if actor.Role == role {
				next(w, r)
				return
			}
		}
		WriteErr(w, http.StatusForbidden, "FORBIDDEN", "role not permitted")
	}
}

var errNoActor = errors.New("auth: no actor for token")

// resolveToken looks the token up in riders then drivers via a single query.
func resolveToken(ctx context.Context, deps Deps, token string) (Actor, error) {
	const q = `
		SELECT id::text, 'rider' AS role FROM riders WHERE api_token = $1
		UNION ALL
		SELECT id::text, 'driver' AS role FROM drivers WHERE api_token = $1
		LIMIT 1`
	var a Actor
	err := deps.Store.PG.QueryRow(ctx, q, token).Scan(&a.ID, &a.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Actor{}, errNoActor
	}
	if err != nil {
		return Actor{}, err
	}
	return a, nil
}

// bearerToken extracts the token from an Authorization: Bearer header.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
