package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/lokeshbm/goride/internal/rides"
)

// Current-state endpoints: the server-authoritative answer to "what am I in
// the middle of?". Clients call these on load to rehydrate an in-flight ride
// after a refresh, new device, or crash — client-side storage is never the
// source of truth for ride state.

// riderState handles GET /v1/riders/{id}/state (rider, self only).
func (deps Deps) riderState(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeValidation(w, "id", "must be a valid UUID")
		return
	}
	if actor.Role != rides.RoleRider || actor.ID != id {
		WriteErr(w, http.StatusForbidden, CodeForbidden, "rider may only read their own state")
		return
	}
	v, err := deps.Rides.ActiveFor(r.Context(), id, rides.RoleRider)
	if err != nil {
		deps.Logger.Error(logMsgStateLookupFailed, "error", err, "rider_id", id)
		WriteErr(w, http.StatusInternalServerError, CodeInternal, "could not load state")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"active_ride": v})
}

// riderOTP handles POST /v1/rides/{id}/otp (rider, own ride, pre-trip): mints
// and returns a fresh trip-start OTP, replacing the previous one. Exists
// because the delivered OTP is client-held only (server keeps a hash) — a
// refresh or device switch must not strand the rider at pickup.
func (deps Deps) riderOTP(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeValidation(w, "id", "must be a valid UUID")
		return
	}
	otp, err := deps.Rides.RegenerateOTP(r.Context(), id, actor.ID)
	if err != nil {
		if errors.Is(err, rides.ErrInvalidState) {
			WriteErr(w, http.StatusConflict, CodeInvalidState, "OTP can only be regenerated between driver assignment and trip start")
			return
		}
		deps.Logger.Error(logMsgOTPRegenFailed, "error", err, "ride_id", id)
		WriteErr(w, http.StatusInternalServerError, CodeInternal, "could not regenerate OTP")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"otp": otp})
}

// driverState handles GET /v1/drivers/{id}/state (driver, self only).
func (deps Deps) driverState(w http.ResponseWriter, r *http.Request) {
	id, ok := driverSelf(w, r)
	if !ok {
		return
	}
	v, err := deps.Rides.ActiveFor(r.Context(), id, rides.RoleDriver)
	if err != nil {
		deps.Logger.Error(logMsgStateLookupFailed, "error", err, "driver_id", id)
		WriteErr(w, http.StatusInternalServerError, CodeInternal, "could not load state")
		return
	}
	var status string
	if err := deps.Store.PG.QueryRow(r.Context(),
		`SELECT status FROM drivers WHERE id = $1`, id).Scan(&status); err != nil {
		deps.Logger.Error(logMsgStateLookupFailed, "error", err, "driver_id", id)
		WriteErr(w, http.StatusInternalServerError, CodeInternal, "could not load state")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"status": status, "active_ride": v})
}
