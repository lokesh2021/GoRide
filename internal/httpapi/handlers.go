package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/lokeshbm/goride/internal/pricing"
	"github.com/lokeshbm/goride/internal/quotes"
	"github.com/lokeshbm/goride/internal/rides"
)

// ---- request/response DTOs ----

type coordDTO struct {
	Lat *float64 `json:"lat"`
	Lng *float64 `json:"lng"`
}

type quoteRequest struct {
	Pickup coordDTO `json:"pickup"`
	Drop   coordDTO `json:"drop"`
	City   string   `json:"city"`
}

type quoteResponse struct {
	QuoteID   string         `json:"quote_id"`
	City      string         `json:"city"`
	DistanceM int            `json:"distance_m"`
	DurationS int            `json:"duration_s"`
	Surge     float64        `json:"surge"`
	SurgeX100 int            `json:"surge_x100"`
	Prices    map[string]int `json:"prices"`
	ExpiresAt string         `json:"expires_at"`
}

type rideRequest struct {
	QuoteID       string `json:"quote_id"`
	Tier          string `json:"tier"`
	PaymentMethod string `json:"payment_method"`
}

type cancelRequest struct {
	Reason string `json:"reason"`
}

// ---- handlers ----

// createQuote handles POST /v1/quotes (rider).
func (deps Deps) createQuote(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())

	var req quoteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.City == "" {
		req.City = "BLR"
	}
	pickup, drop, ok := validateLeg(w, req.Pickup, req.Drop)
	if !ok {
		return
	}

	q, err := deps.Quotes.Create(r.Context(), actor.ID, pickup, drop, req.City)
	if err != nil {
		deps.Logger.Error("createQuote failed", "error", err)
		WriteErr(w, http.StatusInternalServerError, "INTERNAL", "could not create quote")
		return
	}

	WriteJSON(w, http.StatusOK, quoteResponse{
		QuoteID:   q.ID,
		City:      q.City,
		DistanceM: q.DistanceM,
		DurationS: q.DurationS,
		Surge:     float64(q.SurgeX100) / 100.0,
		SurgeX100: q.SurgeX100,
		Prices:    q.Prices,
		ExpiresAt: q.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

// createRide handles POST /v1/rides (rider, idempotent).
func (deps Deps) createRide(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())

	var req rideRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if _, err := uuid.Parse(req.QuoteID); err != nil {
		writeValidation(w, "quote_id", "must be a valid UUID")
		return
	}
	if !pricing.ValidTier(req.Tier) {
		writeValidation(w, "tier", "must be one of mini, sedan, xl")
		return
	}
	if !validPaymentMethod(req.PaymentMethod) {
		writeValidation(w, "payment_method", "must be one of upi, card, cash")
		return
	}

	ride, err := deps.Rides.Create(r.Context(), actor.ID, req.QuoteID, req.Tier, req.PaymentMethod)
	if err != nil {
		writeRideErr(w, deps, "createRide", err)
		return
	}
	WriteJSON(w, http.StatusCreated, ride)
}

// getRide handles GET /v1/rides/{id} (rider|driver).
func (deps Deps) getRide(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeValidation(w, "id", "must be a valid UUID")
		return
	}

	ride, err := deps.Rides.Get(r.Context(), id, actor.ID, actor.Role)
	if err != nil {
		writeRideErr(w, deps, "getRide", err)
		return
	}
	WriteJSON(w, http.StatusOK, ride)
}

// cancelRide handles POST /v1/rides/{id}/cancel (rider|driver).
func (deps Deps) cancelRide(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeValidation(w, "id", "must be a valid UUID")
		return
	}

	var req cancelRequest
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}

	ride, err := deps.Rides.Cancel(r.Context(), id, actor.ID, actor.Role, req.Reason)
	if err != nil {
		writeRideErr(w, deps, "cancelRide", err)
		return
	}
	WriteJSON(w, http.StatusOK, ride)
}

// ---- helpers ----

// decodeJSON strictly decodes the JSON body, writing a 400 on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		WriteErr(w, http.StatusBadRequest, "VALIDATION_FAILED", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// validateLeg validates pickup/drop coordinate ranges and distinctness.
func validateLeg(w http.ResponseWriter, pickup, drop coordDTO) (quotes.Coord, quotes.Coord, bool) {
	p, ok := validateCoord(w, "pickup", pickup)
	if !ok {
		return quotes.Coord{}, quotes.Coord{}, false
	}
	d, ok := validateCoord(w, "drop", drop)
	if !ok {
		return quotes.Coord{}, quotes.Coord{}, false
	}
	if p.Lat == d.Lat && p.Lng == d.Lng {
		writeValidation(w, "drop", "pickup and drop must differ")
		return quotes.Coord{}, quotes.Coord{}, false
	}
	return p, d, true
}

func validateCoord(w http.ResponseWriter, field string, c coordDTO) (quotes.Coord, bool) {
	if c.Lat == nil {
		writeValidation(w, field+".lat", "is required")
		return quotes.Coord{}, false
	}
	if c.Lng == nil {
		writeValidation(w, field+".lng", "is required")
		return quotes.Coord{}, false
	}
	if *c.Lat < -90 || *c.Lat > 90 {
		writeValidation(w, field+".lat", "must be within [-90, 90]")
		return quotes.Coord{}, false
	}
	if *c.Lng < -180 || *c.Lng > 180 {
		writeValidation(w, field+".lng", "must be within [-180, 180]")
		return quotes.Coord{}, false
	}
	return quotes.Coord{Lat: *c.Lat, Lng: *c.Lng}, true
}

func validPaymentMethod(m string) bool {
	switch m {
	case "upi", "card", "cash":
		return true
	}
	return false
}

func writeValidation(w http.ResponseWriter, field, msg string) {
	WriteErr(w, http.StatusBadRequest, "VALIDATION_FAILED", field+" "+msg)
}

// writeRideErr maps ride domain errors to HTTP status/codes.
func writeRideErr(w http.ResponseWriter, deps Deps, op string, err error) {
	switch {
	case errors.Is(err, rides.ErrQuoteNotFound):
		WriteErr(w, http.StatusNotFound, "QUOTE_NOT_FOUND", "quote not found")
	case errors.Is(err, rides.ErrQuoteExpired):
		WriteErr(w, http.StatusUnprocessableEntity, "QUOTE_EXPIRED", "quote has expired")
	case errors.Is(err, rides.ErrQuoteNotOwned):
		WriteErr(w, http.StatusForbidden, "FORBIDDEN", "quote belongs to another rider")
	case errors.Is(err, rides.ErrTierUnavailable):
		writeValidation(w, "tier", "not available in the quote")
	case errors.Is(err, rides.ErrAlreadyActive):
		WriteErr(w, http.StatusConflict, "RIDE_ALREADY_ACTIVE", "rider already has an active ride")
	case errors.Is(err, rides.ErrNotFound):
		WriteErr(w, http.StatusNotFound, "NOT_FOUND", "ride not found")
	case errors.Is(err, rides.ErrForbidden):
		WriteErr(w, http.StatusForbidden, "FORBIDDEN", "not permitted to access this ride")
	case errors.Is(err, rides.ErrInvalidState):
		WriteErr(w, http.StatusConflict, "INVALID_STATE", "ride is not in a valid state for this action")
	default:
		deps.Logger.Error(op+" failed", "error", err)
		WriteErr(w, http.StatusInternalServerError, "INTERNAL", "internal error")
	}
}
