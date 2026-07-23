package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/lokeshbm/goride/internal/payments"
	"github.com/lokeshbm/goride/internal/rides"
)

// maxWebhookBody caps the PSP webhook body we buffer for HMAC verification.
const maxWebhookBody = 1 << 16 // 64 KiB

// paymentRequest is the body for POST /v1/payments.
type paymentRequest struct {
	RideID string `json:"ride_id"`
}

// webhookRequest is the PSP confirmation body.
type webhookRequest struct {
	PSPRef string `json:"psp_ref"`
	Status string `json:"status"`
}

// triggerPayment handles POST /v1/payments (rider, idempotent).
func (deps Deps) triggerPayment(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())
	var req paymentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if _, err := uuid.Parse(req.RideID); err != nil {
		writeValidation(w, "ride_id", "must be a valid UUID")
		return
	}

	p, err := deps.Payments.Trigger(r.Context(), actor.ID, req.RideID)
	if err != nil {
		writePaymentErr(w, deps, "triggerPayment", err)
		return
	}
	WriteJSON(w, http.StatusOK, p)
}

// pspWebhook handles POST /v1/webhooks/psp — unauthenticated (external caller),
// authenticated instead by an HMAC-SHA256 signature over the raw body in
// X-PSP-Signature. Idempotent on psp_ref: replays return 200 no-op.
func (deps Deps) pspWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		WriteErr(w, http.StatusBadRequest, "VALIDATION_FAILED", "unable to read request body")
		return
	}
	sig := r.Header.Get("X-PSP-Signature")
	if !deps.Payments.VerifySignature(body, sig) {
		WriteErr(w, http.StatusUnauthorized, "INVALID_SIGNATURE", "webhook signature verification failed")
		return
	}

	var req webhookRequest
	if err := json.Unmarshal(body, &req); err != nil {
		WriteErr(w, http.StatusBadRequest, "VALIDATION_FAILED", "invalid webhook body")
		return
	}
	if req.PSPRef == "" {
		writeValidation(w, "psp_ref", "is required")
		return
	}

	if err := deps.Payments.HandleWebhook(r.Context(), req.PSPRef, req.Status); err != nil {
		if errors.Is(err, payments.ErrNotFound) {
			WriteErr(w, http.StatusNotFound, "NOT_FOUND", "no payment for psp_ref")
			return
		}
		deps.Logger.Error("pspWebhook failed", "error", err)
		WriteErr(w, http.StatusInternalServerError, "INTERNAL", "could not process webhook")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// riderHistory handles GET /v1/riders/{id}/rides (rider, self only).
func (deps Deps) riderHistory(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeValidation(w, "id", "must be a valid UUID")
		return
	}
	if actor.Role != rides.RoleRider || actor.ID != id {
		WriteErr(w, http.StatusForbidden, "FORBIDDEN", "rider may only read their own history")
		return
	}

	items, err := deps.Payments.History(r.Context(), id)
	if err != nil {
		deps.Logger.Error("riderHistory failed", "error", err)
		WriteErr(w, http.StatusInternalServerError, "INTERNAL", "could not load history")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"rides": items})
}

// writePaymentErr maps payment domain errors to HTTP status/codes.
func writePaymentErr(w http.ResponseWriter, deps Deps, op string, err error) {
	switch {
	case errors.Is(err, payments.ErrNotFound):
		WriteErr(w, http.StatusNotFound, "NOT_FOUND", "ride or payment not found")
	case errors.Is(err, payments.ErrForbidden):
		WriteErr(w, http.StatusForbidden, "FORBIDDEN", "not permitted to pay for this ride")
	case errors.Is(err, payments.ErrRetriesExhausted):
		WriteErr(w, http.StatusConflict, "PAYMENT_RETRIES_EXHAUSTED", "payment has exhausted its retries")
	case errors.Is(err, payments.ErrInvalidState):
		WriteErr(w, http.StatusConflict, "INVALID_STATE", "payment is not in a payable state")
	default:
		deps.Logger.Error(op+" failed", "error", err)
		WriteErr(w, http.StatusInternalServerError, "INTERNAL", "internal error")
	}
}
