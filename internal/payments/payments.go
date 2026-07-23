// Package payments owns payment intents, the mock-PSP webhook confirmation flow,
// immutable receipts, and rider ride-history reads.
//
// Payment lifecycle (SPEC): PENDING → PROCESSING → SUCCEEDED | FAILED, with
// FAILED retryable up to maxRetries. Trigger moves a payable payment to
// PROCESSING and assigns a fresh psp_ref, then hands off to the in-process mock
// PSP (psp.go), which POSTs a signed webhook back to us. The webhook handler is
// idempotent on psp_ref: the guarded PROCESSING → terminal update means replays
// (and concurrent deliveries) are safe no-ops.
package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lokeshbm/goride/internal/rides"
	"github.com/lokeshbm/goride/internal/store"
)

// Domain errors, mapped to HTTP codes by the handler layer.
var (
	ErrNotFound         = errors.New("payments: not found")
	ErrForbidden        = errors.New("payments: forbidden")
	ErrInvalidState     = errors.New("payments: invalid state")
	ErrRetriesExhausted = errors.New("payments: retries exhausted")
)

// Payment is the read model returned by Trigger.
type Payment struct {
	ID         string `json:"id"`
	RideID     string `json:"ride_id"`
	Amount     int    `json:"amount"`
	Method     string `json:"method"`
	Status     string `json:"status"`
	PSPRef     string `json:"psp_ref,omitempty"`
	RetryCount int    `json:"retry_count"`
}

// Service is the payments domain service.
type Service struct {
	st     *store.Store
	rides  *rides.Service
	psp    *PSP
	secret string
	log    *slog.Logger
}

// NewService constructs a payments Service. The rides service supplies the
// exported publish/invalidate seams for payment.updated events; psp is the
// in-process mock provider; secret is the shared HMAC key for webhook verify.
func NewService(st *store.Store, r *rides.Service, psp *PSP, secret string, log *slog.Logger) *Service {
	return &Service{st: st, rides: r, psp: psp, secret: secret, log: log}
}

// ---- trigger ----

// Trigger starts (or retries) payment for a completed ride owned by the rider.
// The payment must be PENDING, or FAILED with retry_count < maxRetries; it moves
// to PROCESSING with a fresh psp_ref (uuid) under an optimistic guard, then hands
// off to the mock PSP which will confirm asynchronously via webhook.
func (s *Service) Trigger(ctx context.Context, riderID, rideID string) (*Payment, error) {
	var (
		ownerID string
		status  string
	)
	err := s.st.PG.QueryRow(ctx,
		`SELECT rider_id, status FROM rides WHERE id = $1`, rideID,
	).Scan(&ownerID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("payments: trigger load ride: %w", err)
	}
	if ownerID != riderID {
		return nil, ErrForbidden
	}
	if status != string(rides.StatusCompleted) {
		return nil, ErrInvalidState // fare not finalized yet
	}

	var p Payment
	p.RideID = rideID
	err = s.st.PG.QueryRow(ctx,
		`SELECT id, amount, method, status, retry_count
		 FROM payments WHERE ride_id = $1 ORDER BY created_at DESC LIMIT 1`, rideID,
	).Scan(&p.ID, &p.Amount, &p.Method, &p.Status, &p.RetryCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("payments: trigger load payment: %w", err)
	}

	if !canTrigger(p.Status, p.RetryCount) {
		if p.Status == StatusFailed && p.RetryCount >= maxRetries {
			return nil, ErrRetriesExhausted
		}
		return nil, ErrInvalidState // already PROCESSING or SUCCEEDED
	}

	pspRef := uuid.NewString()
	// Guarded move to PROCESSING (re-asserting the payable predicate closes the
	// concurrent-trigger window).
	tag, err := s.st.PG.Exec(ctx,
		`UPDATE payments SET status = 'PROCESSING', psp_ref = $2, updated_at = now()
		 WHERE id = $1 AND (status = 'PENDING' OR (status = 'FAILED' AND retry_count < $3))`,
		p.ID, pspRef, maxRetries)
	if err != nil {
		return nil, fmt.Errorf("payments: trigger update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrInvalidState // lost a concurrent trigger race
	}

	p.Status = StatusProcessing
	p.PSPRef = pspRef

	// Hand off to the mock PSP; it confirms asynchronously via webhook.
	s.psp.Schedule(pspRef, p.Amount)

	return &p, nil
}

// ---- webhook ----

// VerifySignature reports whether sig authenticates body under the shared PSP
// secret (constant-time). Used by the unauthenticated webhook handler.
func (s *Service) VerifySignature(body []byte, sig string) bool {
	return Verify(s.secret, body, sig)
}

// HandleWebhook applies a PSP confirmation, keyed by psp_ref. It is idempotent:
// only a PROCESSING payment is advanced (guarded PROCESSING → SUCCEEDED/FAILED),
// so a replayed or duplicate delivery for an already-terminal payment is a
// no-op. On SUCCEEDED it creates the immutable receipt (in the same transaction)
// and publishes payment.updated; on FAILED it increments retry_count and
// publishes payment.updated.
func (s *Service) HandleWebhook(ctx context.Context, pspRef, pspStatus string) error {
	var (
		rideID     string
		current    string
		amount     int
		retryCount int
	)
	err := s.st.PG.QueryRow(ctx,
		`SELECT ride_id, status, amount, retry_count FROM payments WHERE psp_ref = $1`, pspRef,
	).Scan(&rideID, &current, &amount, &retryCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("payments: webhook load: %w", err)
	}

	// Idempotency: only a PROCESSING payment is actionable. Terminal → no-op.
	if !shouldApplyWebhook(current) {
		return nil
	}

	if pspStatus == pspSuccess {
		return s.applySuccess(ctx, pspRef, rideID)
	}
	return s.applyFailure(ctx, pspRef, rideID)
}

// applySuccess moves PROCESSING → SUCCEEDED and creates the receipt atomically,
// then publishes payment.updated post-commit.
func (s *Service) applySuccess(ctx context.Context, pspRef, rideID string) error {
	tx, err := s.st.PG.Begin(ctx)
	if err != nil {
		return fmt.Errorf("payments: success begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		paymentID string
		method    string
		amount    int
	)
	tag := tx.QueryRow(ctx,
		`UPDATE payments SET status = 'SUCCEEDED', updated_at = now()
		 WHERE psp_ref = $1 AND status = 'PROCESSING'
		 RETURNING id, method, amount`, pspRef)
	if err := tag.Scan(&paymentID, &method, &amount); errors.Is(err, pgx.ErrNoRows) {
		return nil // concurrent delivery already finalized it — no-op
	} else if err != nil {
		return fmt.Errorf("payments: success update: %w", err)
	}

	// Immutable receipt: copy the trip's fare breakdown, stamp method + ids.
	var fareJSON []byte
	if err := tx.QueryRow(ctx,
		`SELECT fare FROM trips WHERE ride_id = $1`, rideID,
	).Scan(&fareJSON); err != nil {
		return fmt.Errorf("payments: load trip fare: %w", err)
	}
	breakdown := map[string]any{}
	if len(fareJSON) > 0 {
		_ = json.Unmarshal(fareJSON, &breakdown)
	}
	breakdown["method"] = method
	breakdown["ride_id"] = rideID
	breakdown["payment_id"] = paymentID
	receiptJSON, err := json.Marshal(breakdown)
	if err != nil {
		return fmt.Errorf("payments: marshal receipt: %w", err)
	}

	// ON CONFLICT DO NOTHING: receipts.ride_id is unique, so a racing delivery
	// cannot create a second receipt.
	if _, err := tx.Exec(ctx,
		`INSERT INTO receipts (id, ride_id, breakdown, total)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (ride_id) DO NOTHING`,
		uuid.NewString(), rideID, receiptJSON, amount,
	); err != nil {
		return fmt.Errorf("payments: insert receipt: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("payments: success commit: %w", err)
	}

	s.publishPaymentUpdated(ctx, rideID, StatusSucceeded, 0)
	return nil
}

// applyFailure moves PROCESSING → FAILED, incrementing retry_count, then
// publishes payment.updated post-commit.
func (s *Service) applyFailure(ctx context.Context, pspRef, rideID string) error {
	var retryCount int
	err := s.st.PG.QueryRow(ctx,
		`UPDATE payments SET status = 'FAILED', retry_count = retry_count + 1, updated_at = now()
		 WHERE psp_ref = $1 AND status = 'PROCESSING'
		 RETURNING retry_count`, pspRef,
	).Scan(&retryCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // concurrent delivery already finalized it — no-op
	}
	if err != nil {
		return fmt.Errorf("payments: failure update: %w", err)
	}
	s.publishPaymentUpdated(ctx, rideID, StatusFailed, retryCount)
	return nil
}

// publishPaymentUpdated emits payment.updated on the ride channel (no-op
// publisher until M5).
func (s *Service) publishPaymentUpdated(ctx context.Context, rideID, status string, retryCount int) {
	if err := s.rides.PublishRide(ctx, rideID, eventPaymentUpdated, map[string]any{
		"status":      status,
		"retry_count": retryCount,
	}); err != nil {
		s.log.Warn(logMsgPublishPaymentUpdatedFailed, "error", err, "ride_id", rideID)
	}
}

// ---- history ----

// ReceiptView is the immutable fare breakdown attached to a completed ride.
type ReceiptView struct {
	Breakdown map[string]any `json:"breakdown"`
	Total     int            `json:"total"`
	CreatedAt time.Time      `json:"created_at"`
}

// DriverView is the assigned-driver summary shown in history.
type DriverView struct {
	Name  string `json:"name"`
	Plate string `json:"plate"`
}

// HistoryItem is one ride in a rider's history.
type HistoryItem struct {
	RideID    string       `json:"ride_id"`
	Status    string       `json:"status"`
	Tier      string       `json:"tier"`
	FareTotal *int         `json:"fare_total"`
	CreatedAt time.Time    `json:"created_at"`
	Driver    *DriverView  `json:"driver,omitempty"`
	Receipt   *ReceiptView `json:"receipt,omitempty"`
}

// History returns a rider's most-recent-first ride history (up to historyLimit),
// each with status, tier, fare_total, the assigned driver (when any), and the
// receipt (when the ride has been paid). Uses the rides_rider_history_idx index.
func (s *Service) History(ctx context.Context, riderID string) ([]HistoryItem, error) {
	rows, err := s.st.PG.Query(ctx, `
		SELECT r.id, r.status, r.tier, r.fare_total, r.created_at,
		       d.name, d.plate,
		       rc.breakdown, rc.total, rc.created_at
		FROM rides r
		LEFT JOIN drivers d ON d.id = r.driver_id
		LEFT JOIN receipts rc ON rc.ride_id = r.id
		WHERE r.rider_id = $1
		ORDER BY r.created_at DESC
		LIMIT $2`, riderID, historyLimit)
	if err != nil {
		return nil, fmt.Errorf("payments: history query: %w", err)
	}
	defer rows.Close()

	out := make([]HistoryItem, 0, historyLimit)
	for rows.Next() {
		var (
			it            HistoryItem
			dName, dPlate *string
			breakdownJSON []byte
			rcTotal       *int
			rcCreatedAt   *time.Time
		)
		if err := rows.Scan(
			&it.RideID, &it.Status, &it.Tier, &it.FareTotal, &it.CreatedAt,
			&dName, &dPlate,
			&breakdownJSON, &rcTotal, &rcCreatedAt,
		); err != nil {
			return nil, fmt.Errorf("payments: history scan: %w", err)
		}
		if dName != nil {
			it.Driver = &DriverView{Name: *dName, Plate: derefStr(dPlate)}
		}
		if breakdownJSON != nil && rcTotal != nil {
			r := &ReceiptView{Total: *rcTotal}
			_ = json.Unmarshal(breakdownJSON, &r.Breakdown)
			if rcCreatedAt != nil {
				r.CreatedAt = *rcCreatedAt
			}
			it.Receipt = r
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("payments: history rows: %w", err)
	}
	return out, nil
}

// ---- pure helpers (unit-tested) ----

// canTrigger reports whether a payment in the given state may be (re)triggered:
// PENDING, or FAILED with retry_count below the cap.
func canTrigger(status string, retryCount int) bool {
	return status == StatusPending || (status == StatusFailed && retryCount < maxRetries)
}

// shouldApplyWebhook reports whether a webhook is actionable for a payment in the
// given state. Only PROCESSING is actionable; terminal states are replays
// (no-op). This is the idempotency decision.
func shouldApplyWebhook(current string) bool {
	return current == StatusProcessing
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
