package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// maxIdempotentBody caps the request body we buffer for hashing/replay.
const maxIdempotentBody = 1 << 20 // 1 MiB

// idempotency wraps a mutating handler with Idempotency-Key semantics per SPEC:
//   - Requires the Idempotency-Key header (400 IDEMPOTENCY_KEY_REQUIRED if absent).
//   - Key is scoped by (key, actor_id, endpoint); request hash = SHA256 of
//     method + path + body.
//   - Same key + same hash → replay the stored response.
//   - Same key + different hash → 422 IDEMPOTENCY_KEY_REUSED.
//
// Concurrency: two in-flight requests with the same key can both miss the
// pre-check and both run the handler (side effects may occur twice — for
// ride creation the partial unique index makes the second a benign 409). We
// close the *response* consistency window with INSERT ... ON CONFLICT DO
// NOTHING followed by a re-read: the loser returns the winner's stored response
// so both callers observe an identical result.
func (deps Deps) idempotency(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			WriteErr(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key header is required")
			return
		}
		actor, ok := ActorFrom(r.Context())
		if !ok {
			WriteErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxIdempotentBody))
		if err != nil {
			WriteErr(w, http.StatusBadRequest, "VALIDATION_FAILED", "unable to read request body")
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body)) // let the handler re-read it

		endpoint := chi.RouteContext(r.Context()).RoutePattern()
		hash := requestHash(r.Method, r.URL.Path, body)
		ctx := r.Context()

		// Pre-check: existing row for this (key, actor, endpoint)?
		storedHash, status, respBody, found, err := deps.loadIdemKey(ctx, key, actor.ID, endpoint)
		if err != nil {
			deps.Logger.Error("idempotency: load failed", "error", err)
			WriteErr(w, http.StatusInternalServerError, "INTERNAL", "idempotency check failed")
			return
		}
		if found {
			if storedHash != hash {
				WriteErr(w, http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED", "Idempotency-Key reused with a different request body")
				return
			}
			writeStored(w, status, respBody)
			return
		}

		// Run the handler, capturing its response instead of streaming it.
		cw := &captureWriter{ResponseWriter: w}
		next(cw, r)
		if cw.status == 0 {
			cw.status = http.StatusOK
		}

		// Persist deterministic outcomes (skip transient 5xx so retries re-run).
		if cw.status < 500 {
			inserted, err := deps.insertIdemKey(ctx, key, actor.ID, endpoint, hash, cw.status, jsonOrNull(cw.body.Bytes()))
			if err != nil {
				deps.Logger.Error("idempotency: store failed", "error", err)
			} else if !inserted {
				// Concurrent winner already stored a response; replay theirs.
				if sh, st, rb, ok, rerr := deps.loadIdemKey(ctx, key, actor.ID, endpoint); rerr == nil && ok && sh == hash {
					writeStored(w, st, rb)
					return
				}
			}
		}

		cw.flush(w)
	}
}

func requestHash(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func (deps Deps) loadIdemKey(ctx context.Context, key, actorID, endpoint string) (hash string, status int, body []byte, found bool, err error) {
	const q = `
		SELECT request_hash, response_status, response_body
		FROM idempotency_keys
		WHERE key = $1 AND actor_id = $2 AND endpoint = $3`
	err = deps.Store.PG.QueryRow(ctx, q, key, actorID, endpoint).Scan(&hash, &status, &body)
	if err == pgx.ErrNoRows {
		return "", 0, nil, false, nil
	}
	if err != nil {
		return "", 0, nil, false, err
	}
	return hash, status, body, true, nil
}

// insertIdemKey inserts the stored response; ON CONFLICT DO NOTHING means a
// return of false signals a concurrent writer already claimed the key.
func (deps Deps) insertIdemKey(ctx context.Context, key, actorID, endpoint, hash string, status int, body []byte) (bool, error) {
	const q = `
		INSERT INTO idempotency_keys
			(key, actor_id, endpoint, request_hash, response_status, response_body)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (key, actor_id, endpoint) DO NOTHING`
	tag, err := deps.Store.PG.Exec(ctx, q, key, actorID, endpoint, hash, status, body)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func writeStored(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// jsonOrNull guarantees a valid jsonb value for the response_body column.
func jsonOrNull(b []byte) []byte {
	if len(bytes.TrimSpace(b)) == 0 {
		return []byte("null")
	}
	return b
}

// captureWriter buffers the handler's status and body so the idempotency layer
// can decide whether to flush it or replay a concurrent winner's response.
// Header mutations pass through to the underlying writer (not yet flushed).
type captureWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (c *captureWriter) WriteHeader(status int) { c.status = status }

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(b)
}

func (c *captureWriter) flush(w http.ResponseWriter) {
	w.WriteHeader(c.status)
	_, _ = w.Write(c.body.Bytes())
}
