package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/lokeshbm/goride/internal/trips"
)

// startTripRequest is the body for POST /v1/trips/{id}/start.
type startTripRequest struct {
	OTP string `json:"otp"`
}

// startTrip handles POST /v1/trips/{id}/start (assigned driver). {id} is the
// RIDE id (trips are 1:1 with rides). Verifies the rider OTP and starts the trip.
func (deps Deps) startTrip(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())
	rideID, ok := tripRideID(w, r)
	if !ok {
		return
	}
	var req startTripRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.OTP == "" {
		writeValidation(w, "otp", "is required")
		return
	}

	trip, err := deps.Trips.Start(r.Context(), actor.ID, rideID, req.OTP)
	if err != nil {
		writeTripErr(w, deps, "startTrip", err)
		return
	}
	WriteJSON(w, http.StatusOK, trip)
}

// pauseTrip handles POST /v1/trips/{id}/pause (assigned driver).
func (deps Deps) pauseTrip(w http.ResponseWriter, r *http.Request) {
	deps.tripAction(w, r, deps.Trips.Pause)
}

// resumeTrip handles POST /v1/trips/{id}/resume (assigned driver).
func (deps Deps) resumeTrip(w http.ResponseWriter, r *http.Request) {
	deps.tripAction(w, r, deps.Trips.Resume)
}

// endTrip handles POST /v1/trips/{id}/end (assigned driver, idempotent).
func (deps Deps) endTrip(w http.ResponseWriter, r *http.Request) {
	deps.tripAction(w, r, deps.Trips.End)
}

// tripAction is the shared shell for driver trip actions keyed on the ride id.
func (deps Deps) tripAction(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, driverID, rideID string) (*trips.Trip, error)) {
	actor, _ := ActorFrom(r.Context())
	rideID, ok := tripRideID(w, r)
	if !ok {
		return
	}
	trip, err := fn(r.Context(), actor.ID, rideID)
	if err != nil {
		writeTripErr(w, deps, "tripAction", err)
		return
	}
	WriteJSON(w, http.StatusOK, trip)
}

// tripRideID extracts and validates the {id} path param (the ride id).
func tripRideID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeValidation(w, "id", "must be a valid UUID")
		return "", false
	}
	return id, true
}

// writeTripErr maps trip domain errors to HTTP status/codes.
func writeTripErr(w http.ResponseWriter, deps Deps, op string, err error) {
	switch {
	case errors.Is(err, trips.ErrNotFound):
		WriteErr(w, http.StatusNotFound, "NOT_FOUND", "ride not found")
	case errors.Is(err, trips.ErrForbidden):
		WriteErr(w, http.StatusForbidden, "FORBIDDEN", "not the assigned driver for this ride")
	case errors.Is(err, trips.ErrInvalidOTP):
		WriteErr(w, http.StatusUnprocessableEntity, "INVALID_OTP", "otp does not match")
	case errors.Is(err, trips.ErrInvalidState):
		WriteErr(w, http.StatusConflict, "INVALID_STATE", "trip is not in a valid state for this action")
	default:
		deps.Logger.Error(op+" failed", "error", err)
		WriteErr(w, http.StatusInternalServerError, "INTERNAL", "internal error")
	}
}
