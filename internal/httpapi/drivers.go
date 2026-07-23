package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/lokeshbm/goride/internal/drivers"
	"github.com/lokeshbm/goride/internal/matching"
	"github.com/lokeshbm/goride/internal/rides"
)

// ---- request DTOs ----

type locationRequest struct {
	Lat *float64 `json:"lat"`
	Lng *float64 `json:"lng"`
}

type availabilityRequest struct {
	Available *bool `json:"available"`
}

type offerActionRequest struct {
	RideID string `json:"ride_id"`
}

// ---- helpers ----

// driverSelf resolves the path {id}, requires the caller to be that driver, and
// returns the id. Writes the appropriate error and returns ok=false otherwise.
func driverSelf(w http.ResponseWriter, r *http.Request) (string, bool) {
	actor, _ := ActorFrom(r.Context())
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeValidation(w, "id", "must be a valid UUID")
		return "", false
	}
	if actor.ID != id {
		WriteErr(w, http.StatusForbidden, CodeForbidden, "driver may only act on its own resource")
		return "", false
	}
	return id, true
}

// ---- driver handlers ----

// updateLocation handles POST /v1/drivers/{id}/location (driver, no idempotency
// key per SPEC — location pings are high-frequency and inherently last-writer).
func (deps Deps) updateLocation(w http.ResponseWriter, r *http.Request) {
	id, ok := driverSelf(w, r)
	if !ok {
		return
	}
	var req locationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Lat == nil || req.Lng == nil {
		writeValidation(w, "lat/lng", "are required")
		return
	}
	if *req.Lat < -90 || *req.Lat > 90 {
		writeValidation(w, "lat", "must be within [-90, 90]")
		return
	}
	if *req.Lng < -180 || *req.Lng > 180 {
		writeValidation(w, "lng", "must be within [-180, 180]")
		return
	}

	err := deps.Drivers.UpdateLocation(r.Context(), id, *req.Lat, *req.Lng)
	if errors.Is(err, drivers.ErrRateLimited) {
		WriteErr(w, http.StatusTooManyRequests, CodeRateLimited, "too many location updates")
		return
	}
	if err != nil {
		deps.Logger.Error(logMsgUpdateLocationFailed, "error", err)
		WriteErr(w, http.StatusInternalServerError, CodeInternal, "could not record location")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// setAvailability handles POST /v1/drivers/{id}/availability (driver).
func (deps Deps) setAvailability(w http.ResponseWriter, r *http.Request) {
	id, ok := driverSelf(w, r)
	if !ok {
		return
	}
	var req availabilityRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Available == nil {
		writeValidation(w, "available", "is required")
		return
	}

	err := deps.Drivers.SetAvailability(r.Context(), id, *req.Available)
	if errors.Is(err, drivers.ErrInvalidState) {
		WriteErr(w, http.StatusConflict, CodeInvalidState, "driver is on a trip and cannot change availability")
		return
	}
	if errors.Is(err, drivers.ErrNotFound) {
		WriteErr(w, http.StatusNotFound, CodeNotFound, "driver not found")
		return
	}
	if err != nil {
		deps.Logger.Error(logMsgSetAvailabilityFailed, "error", err)
		WriteErr(w, http.StatusInternalServerError, CodeInternal, "could not update availability")
		return
	}
	status := drivers.StatusOffline
	if *req.Available {
		status = drivers.StatusAvailable
	}
	WriteJSON(w, http.StatusOK, map[string]any{"driver_id": id, "status": status})
}

// acceptOffer handles POST /v1/drivers/{id}/accept (driver, idempotent).
// Replay-safe: if the driver is already the assigned driver of the ride, return
// 200 with the current state instead of 409.
func (deps Deps) acceptOffer(w http.ResponseWriter, r *http.Request) {
	id, ok := driverSelf(w, r)
	if !ok {
		return
	}
	var req offerActionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if _, err := uuid.Parse(req.RideID); err != nil {
		writeValidation(w, "ride_id", "must be a valid UUID")
		return
	}

	ride, err := deps.Match.Accept(r.Context(), id, req.RideID)
	switch {
	case errors.Is(err, matching.ErrOfferExpired):
		WriteErr(w, http.StatusConflict, CodeOfferExpired, "offer expired or not held by this driver")
	case errors.Is(err, matching.ErrRideGone):
		WriteErr(w, http.StatusConflict, CodeInvalidState, "ride is no longer available for assignment")
	case errors.Is(err, matching.ErrNotFound):
		WriteErr(w, http.StatusNotFound, CodeNotFound, "ride not found")
	case err != nil:
		deps.Logger.Error(logMsgAcceptOfferFailed, "error", err)
		WriteErr(w, http.StatusInternalServerError, CodeInternal, "could not accept offer")
	default:
		WriteJSON(w, http.StatusOK, ride)
	}
}

// declineOffer handles POST /v1/drivers/{id}/decline (driver).
func (deps Deps) declineOffer(w http.ResponseWriter, r *http.Request) {
	id, ok := driverSelf(w, r)
	if !ok {
		return
	}
	var req offerActionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if _, err := uuid.Parse(req.RideID); err != nil {
		writeValidation(w, "ride_id", "must be a valid UUID")
		return
	}

	err := deps.Match.Decline(r.Context(), id, req.RideID)
	if errors.Is(err, matching.ErrNotFound) {
		WriteErr(w, http.StatusNotFound, CodeNotFound, "ride not found")
		return
	}
	if err != nil {
		deps.Logger.Error(logMsgDeclineOfferFailed, "error", err)
		WriteErr(w, http.StatusInternalServerError, CodeInternal, "could not decline offer")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- driver-side ride progression ----

// rideArriving handles POST /v1/rides/{id}/arriving (assigned driver):
// DRIVER_ASSIGNED → DRIVER_ARRIVING.
func (deps Deps) rideArriving(w http.ResponseWriter, r *http.Request) {
	deps.rideProgress(w, r, deps.Rides.Arriving)
}

// rideArrived handles POST /v1/rides/{id}/arrived (assigned driver):
// DRIVER_ARRIVING → ARRIVED.
func (deps Deps) rideArrived(w http.ResponseWriter, r *http.Request) {
	deps.rideProgress(w, r, deps.Rides.Arrived)
}

func (deps Deps) rideProgress(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, id, driverID string) (*rides.View, error)) {
	actor, _ := ActorFrom(r.Context())
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeValidation(w, "id", "must be a valid UUID")
		return
	}
	ride, err := fn(r.Context(), id, actor.ID)
	if err != nil {
		writeRideErr(w, deps, "rideProgress", err)
		return
	}
	WriteJSON(w, http.StatusOK, ride)
}
