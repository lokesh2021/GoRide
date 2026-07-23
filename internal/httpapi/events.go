package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/lokeshbm/goride/internal/events"
	"github.com/lokeshbm/goride/internal/rides"
)

// streamRideEvents handles GET /v1/events?ride_id=... — the ride's rider or
// its currently assigned driver may subscribe. The ride is loaded once,
// straight from Postgres (rides.LoadView bypasses the read-through cache,
// keeping the authz check independent of cache staleness), purely for that
// authorization decision.
func (deps Deps) streamRideEvents(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())
	rideID := r.URL.Query().Get("ride_id")
	if _, err := uuid.Parse(rideID); err != nil {
		writeValidation(w, "ride_id", "must be a valid UUID")
		return
	}

	v, err := deps.Rides.LoadView(r.Context(), rideID)
	if errors.Is(err, rides.ErrNotFound) {
		WriteErr(w, http.StatusNotFound, CodeNotFound, "ride not found")
		return
	}
	if err != nil {
		deps.Logger.Error(logMsgStreamRideEventsFailed, "error", err)
		WriteErr(w, http.StatusInternalServerError, CodeInternal, "could not load ride")
		return
	}
	if !canStreamRide(v, actor) {
		WriteErr(w, http.StatusForbidden, CodeForbidden, "not permitted to stream this ride's events")
		return
	}

	deps.serveSSE(w, r, events.RideChannel(rideID))
}

// streamDriverEvents handles GET /v1/events/driver/{id} — driver self only,
// same path-actor guard as the other /drivers/{id}/... routes.
func (deps Deps) streamDriverEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := driverSelf(w, r)
	if !ok {
		return
	}
	deps.serveSSE(w, r, events.DriverChannel(id))
}

// canStreamRide is the pure authorization decision for ride-events access: the
// ride's rider, or its currently assigned driver, may stream; anyone else
// (including a driver not currently assigned) may not.
func canStreamRide(v *rides.View, actor Actor) bool {
	switch actor.Role {
	case rides.RoleRider:
		return v.RiderID == actor.ID
	case rides.RoleDriver:
		return v.DriverID != nil && *v.DriverID == actor.ID
	default:
		return false
	}
}

// serveSSE sets the SSE response headers and streams channel to the client
// until it disconnects or the server shuts down (see events.Hub.Serve). No
// idempotency and no write timeout apply here: the /v1/events routes are
// mounted without the request-timeout middleware that wraps the rest of the
// API (router.go), and cmd/server's http.Server sets no WriteTimeout, so
// neither can cut the stream short.
func (deps Deps) serveSSE(w http.ResponseWriter, r *http.Request, channel string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteErr(w, http.StatusInternalServerError, CodeInternal, "streaming unsupported")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable nginx-style proxy buffering
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	if err := deps.Events.Serve(r.Context(), w, flusher.Flush, channel); err != nil {
		deps.Logger.Warn(logMsgEventsStreamEnded, "error", err, "channel", channel)
	}
}
