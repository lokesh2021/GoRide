// Package rides owns the ride domain: the lifecycle state machine and the
// Service that creates, reads (read-through cached), and cancels rides.
//
// Matching (M3), trips, payments, and real-time events (M5) are out of scope
// here; the Service exposes clearly-named seams for them.
package rides

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"

	"github.com/lokeshbm/goride/internal/quotes"
	"github.com/lokeshbm/goride/internal/store"
)

// Domain errors, mapped to HTTP codes by the handler layer.
var (
	ErrNotFound         = errors.New("rides: not found")
	ErrForbidden        = errors.New("rides: forbidden")
	ErrInvalidState     = errors.New("rides: invalid state")
	ErrAlreadyActive    = errors.New("rides: rider already has an active ride")
	ErrQuoteNotFound    = errors.New("rides: quote not found")
	ErrQuoteExpired     = errors.New("rides: quote expired")
	ErrQuoteNotOwned    = errors.New("rides: quote belongs to another rider")
	ErrTierUnavailable  = errors.New("rides: tier not available in quote")
	ErrPaymentMethodBad = errors.New("rides: invalid payment method")
)

// EventPublisher receives domain events. M5 wires this to the SSE/Redis pub-sub
// hub; the default is a no-op. PublishRideEvent targets a ride channel
// (events:ride:{id}); PublishDriverEvent targets a driver channel
// (events:driver:{id}) and carries offers/assignments (added in M3).
type EventPublisher interface {
	PublishRideEvent(ctx context.Context, rideID, eventType string, data any) error
	PublishDriverEvent(ctx context.Context, driverID, eventType string, data any) error
}

// NoopPublisher discards events. Default until M5.
type NoopPublisher struct{}

// PublishRideEvent implements EventPublisher.
func (NoopPublisher) PublishRideEvent(context.Context, string, string, any) error { return nil }

// PublishDriverEvent implements EventPublisher.
func (NoopPublisher) PublishDriverEvent(context.Context, string, string, any) error { return nil }

// DriverCard is the assigned-driver summary returned with a ride.
type DriverCard struct {
	Name         string  `json:"name"`
	VehicleModel string  `json:"vehicle_model"`
	Plate        string  `json:"plate"`
	Rating       float64 `json:"rating"`
}

// View is the read model for a ride, returned by Get/Cancel and cached in Redis.
// RiderID/DriverID are included for authorization and downstream use.
type View struct {
	ID            string      `json:"id"`
	RiderID       string      `json:"rider_id"`
	DriverID      *string     `json:"driver_id"`
	QuoteID       string      `json:"quote_id"`
	Tier          string      `json:"tier"`
	Status        string      `json:"status"`
	PickupLat     float64     `json:"pickup_lat"`
	PickupLng     float64     `json:"pickup_lng"`
	DropLat       float64     `json:"drop_lat"`
	DropLng       float64     `json:"drop_lng"`
	PaymentMethod *string     `json:"payment_method"`
	FareTotal     *int        `json:"fare_total"`
	CancelReason  *string     `json:"cancel_reason"`
	Driver        *DriverCard `json:"driver,omitempty"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

// Service is the ride domain service.
type Service struct {
	st     *store.Store
	quotes *quotes.Service
	log    *slog.Logger
	events EventPublisher

	// MatchRequested is invoked after a ride enters MATCHING. M3's matching
	// engine replaces the default no-op.
	MatchRequested func(ctx context.Context, rideID string)

	// OnDriverReleased is invoked after a cancelled ride's assigned driver is
	// set back to 'available'. M3 uses it to re-add the driver to the geo set.
	OnDriverReleased func(ctx context.Context, driverID string)
}

// NewService constructs a ride Service with no-op seams and publisher.
func NewService(st *store.Store, q *quotes.Service, log *slog.Logger) *Service {
	return &Service{
		st:               st,
		quotes:           q,
		log:              log,
		events:           NoopPublisher{},
		MatchRequested:   func(context.Context, string) {},
		OnDriverReleased: func(context.Context, string) {},
	}
}

// SetEventPublisher overrides the default no-op publisher (used by M5).
func (s *Service) SetEventPublisher(p EventPublisher) { s.events = p }

// Create validates the quote, creates a REQUESTED ride with the quote's
// coordinates and locked tier fare, then immediately transitions it to
// MATCHING and fires the MatchRequested seam. The partial unique index enforces
// one active ride per rider; a unique violation becomes ErrAlreadyActive.
func (s *Service) Create(ctx context.Context, riderID, quoteID, tier, paymentMethod string) (*View, error) {
	q, err := s.quotes.Get(ctx, quoteID)
	if errors.Is(err, quotes.ErrNotFound) {
		return nil, ErrQuoteNotFound
	}
	if err != nil {
		return nil, err
	}
	if q.RiderID != riderID {
		return nil, ErrQuoteNotOwned
	}
	if time.Now().UTC().After(q.ExpiresAt) {
		return nil, ErrQuoteExpired
	}
	fare, ok := q.Prices[tier]
	if !ok {
		return nil, ErrTierUnavailable
	}

	id := uuid.NewString()
	const insertSQL = `
		INSERT INTO rides
			(id, rider_id, quote_id, tier, status,
			 pickup_lat, pickup_lng, drop_lat, drop_lng,
			 payment_method, fare_total)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`
	_, err = s.st.PG.Exec(ctx, insertSQL,
		id, riderID, quoteID, tier, string(StatusRequested),
		q.Pickup.Lat, q.Pickup.Lng, q.Drop.Lat, q.Drop.Lng,
		paymentMethod, fare,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return nil, ErrAlreadyActive
		}
		return nil, fmt.Errorf("rides: insert: %w", err)
	}

	// REQUESTED → MATCHING immediately (matching engine is M3).
	if err := s.updateStatus(ctx, id, []Status{StatusRequested}, StatusMatching, nil); err != nil {
		return nil, err
	}
	s.MatchRequested(ctx, id)

	return s.load(ctx, id)
}

// Get returns a ride view for the actor, served read-through from Redis
// (ride:cache:{id}, TTL 60s). Only the ride's rider or assigned driver may read.
func (s *Service) Get(ctx context.Context, id, actorID, actorRole string) (*View, error) {
	v, err := s.cachedLoad(ctx, id)
	if err != nil {
		return nil, err
	}
	if !authorized(v, actorID, actorRole) {
		return nil, ErrForbidden
	}
	return v, nil
}

// Cancel cancels a ride on behalf of the actor (rider or assigned driver),
// enforcing the state machine via a guarded UPDATE. On success it invalidates
// the cache, releases any assigned driver in the same transaction, and fires
// the OnDriverReleased seam.
func (s *Service) Cancel(ctx context.Context, id, actorID, actorRole, reason string) (*View, error) {
	// Authorize against current ownership before attempting the guarded update.
	var (
		riderID  string
		driverID *string
		status   string
	)
	err := s.st.PG.QueryRow(ctx,
		`SELECT rider_id, driver_id, status FROM rides WHERE id = $1`, id,
	).Scan(&riderID, &driverID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("rides: cancel load: %w", err)
	}

	var target Status
	switch actorRole {
	case RoleRider:
		if riderID != actorID {
			return nil, ErrForbidden
		}
		target = StatusCancelledRider
	case RoleDriver:
		if driverID == nil || *driverID != actorID {
			return nil, ErrForbidden
		}
		target = StatusCancelledDriver
	default:
		return nil, ErrForbidden
	}

	// The driver to release is re-read INSIDE the transaction, not taken from
	// the authorization snapshot above: an Accept can commit between that
	// SELECT and our guarded status UPDATE (CANCELLED_* is legal from
	// DRIVER_ASSIGNED), and the snapshot's nil driver_id would then skip
	// freeing the just-assigned driver, orphaning them on_trip. By the time
	// mutate runs, updateStatus's UPDATE holds the row lock, so this read is
	// race-free.
	var lockedDriverID *string
	mutate := func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`UPDATE rides SET cancel_reason = $1 WHERE id = $2 RETURNING driver_id`,
			nullIfEmpty(reason), id,
		).Scan(&lockedDriverID); err != nil {
			return err
		}
		if lockedDriverID != nil {
			if _, err := tx.Exec(ctx,
				`UPDATE drivers SET status = 'available' WHERE id = $1 AND status = 'on_trip'`, *lockedDriverID,
			); err != nil {
				return err
			}
		}
		return nil
	}

	if err := s.updateStatus(ctx, id, sourcesFor(target), target, mutate); err != nil {
		return nil, err
	}

	// Post-commit seam: geo re-add of the released driver is M3's concern.
	if lockedDriverID != nil {
		s.OnDriverReleased(ctx, *lockedDriverID)
	}

	return s.load(ctx, id)
}

// Expire transitions a MATCHING ride to EXPIRED via the funnel. Used by the
// matching sweeper when candidates are exhausted / the 60s TTL passes. The
// guarded UPDATE makes it safe to call post-commit and from any instance;
// ErrInvalidState (already advanced/terminal) is returned if the ride left
// MATCHING in the meantime.
func (s *Service) Expire(ctx context.Context, id string) error {
	return s.updateStatus(ctx, id, []Status{StatusMatching}, StatusExpired, nil)
}

// Arriving transitions DRIVER_ASSIGNED → DRIVER_ARRIVING for the assigned
// driver. Rejects a non-assigned actor (ErrForbidden).
func (s *Service) Arriving(ctx context.Context, id, driverID string) (*View, error) {
	return s.driverProgress(ctx, id, driverID, []Status{StatusDriverAssigned}, StatusDriverArriving)
}

// Arrived transitions DRIVER_ARRIVING → ARRIVED for the assigned driver.
func (s *Service) Arrived(ctx context.Context, id, driverID string) (*View, error) {
	return s.driverProgress(ctx, id, driverID, []Status{StatusDriverArriving}, StatusArrived)
}

// driverProgress verifies the actor is the ride's assigned driver, then runs a
// guarded transition through the funnel.
func (s *Service) driverProgress(ctx context.Context, id, driverID string, from []Status, to Status) (*View, error) {
	var assigned *string
	err := s.st.PG.QueryRow(ctx, `SELECT driver_id FROM rides WHERE id = $1`, id).Scan(&assigned)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("rides: progress load: %w", err)
	}
	if assigned == nil || *assigned != driverID {
		return nil, ErrForbidden
	}
	if err := s.updateStatus(ctx, id, from, to, nil); err != nil {
		return nil, err
	}
	return s.load(ctx, id)
}

// InvalidateCache deletes the read-through cache entry for a ride. Exported for
// the matching engine, which assigns drivers in its own transaction (not via
// the funnel) and must invalidate the cache post-commit.
func (s *Service) InvalidateCache(ctx context.Context, id string) error {
	return s.st.Redis.Del(ctx, cacheKey(id)).Err()
}

// PublishRide publishes an event onto a ride channel via the configured
// publisher. Exported so the matching engine can emit assignment/OTP events.
func (s *Service) PublishRide(ctx context.Context, id, eventType string, data any) error {
	return s.events.PublishRideEvent(ctx, id, eventType, data)
}

// PublishDriver publishes an event onto a driver channel (offers/assignments).
func (s *Service) PublishDriver(ctx context.Context, driverID, eventType string, data any) error {
	return s.events.PublishDriverEvent(ctx, driverID, eventType, data)
}

// LoadView loads a ride view straight from Postgres with no authorization
// check. Exported for internal callers (matching) that have already authorized
// the actor by other means.
// RegenerateOTP mints a fresh trip-start OTP for the ride's rider. The server
// stores only a bcrypt hash, so a rider who loses the delivered OTP (refresh,
// new device) would otherwise be stranded pre-trip; regeneration invalidates
// the old code and returns the new one to the authenticated rider only.
// Legal while a driver is assigned but the trip has not started.
func (s *Service) RegenerateOTP(ctx context.Context, rideID, riderID string) (string, error) {
	otp, err := generateOTP()
	if err != nil {
		return "", fmt.Errorf("rides: generate otp: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(otp), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("rides: hash otp: %w", err)
	}
	tag, err := s.st.PG.Exec(ctx, `
		UPDATE rides SET otp_hash = $1, updated_at = now()
		WHERE id = $2 AND rider_id = $3
		  AND status IN ('DRIVER_ASSIGNED','DRIVER_ARRIVING','ARRIVED')`,
		string(hash), rideID, riderID)
	if err != nil {
		return "", fmt.Errorf("rides: regenerate otp: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrInvalidState
	}
	return otp, nil
}

// generateOTP returns a uniformly-random 4-digit OTP using crypto/rand.
func generateOTP() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%04d", n.Int64()), nil
}

// ActiveFor returns the actor's current non-terminal ride view, or nil when
// none exists. The partial unique indexes guarantee at most one row and make
// the lookup an index-only probe. Role is RoleRider or RoleDriver.
func (s *Service) ActiveFor(ctx context.Context, actorID, role string) (*View, error) {
	col := "rider_id"
	if role == RoleDriver {
		col = "driver_id"
	}
	var id string
	err := s.st.PG.QueryRow(ctx, `
		SELECT id FROM rides
		WHERE `+col+` = $1
		  AND status IN ('REQUESTED','MATCHING','DRIVER_ASSIGNED',
		                 'DRIVER_ARRIVING','ARRIVED','IN_PROGRESS')`, actorID,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rides: active lookup: %w", err)
	}
	return s.LoadView(ctx, id)
}

func (s *Service) LoadView(ctx context.Context, id string) (*View, error) {
	return s.load(ctx, id)
}

// updateStatus is the single funnel for every ride status write. It (a) runs
// the optimistic guarded UPDATE (status ∈ fromStates → to) inside a
// transaction, running the optional mutate for same-tx side effects, then on
// commit (b) invalidates ride:cache:{id} and (c) publishes a status-changed
// event. Zero rows affected ⇒ ErrInvalidState.
func (s *Service) updateStatus(ctx context.Context, id string, fromStates []Status, to Status, mutate func(context.Context, pgx.Tx) error) error {
	tx, err := s.st.PG.Begin(ctx)
	if err != nil {
		return fmt.Errorf("rides: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE rides SET status = $1, updated_at = now() WHERE id = $2 AND status = ANY($3)`,
		string(to), id, statusStrings(fromStates),
	)
	if err != nil {
		return fmt.Errorf("rides: update status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrInvalidState
	}

	if mutate != nil {
		if err := mutate(ctx, tx); err != nil {
			return fmt.Errorf("rides: status mutate: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rides: commit: %w", err)
	}

	// Write-through cache invalidation + event publish, post-commit.
	if err := s.st.Redis.Del(ctx, cacheKey(id)).Err(); err != nil {
		s.log.Warn(logMsgCacheInvalidateFailed, "error", err, "ride_id", id)
	}
	if err := s.events.PublishRideEvent(ctx, id, eventRideStatusChanged, map[string]any{"status": string(to)}); err != nil {
		s.log.Warn(logMsgPublishEventFailed, "error", err, "ride_id", id)
	}
	return nil
}

// cachedLoad returns the ride view from Redis if present, else loads from
// Postgres and populates the cache.
func (s *Service) cachedLoad(ctx context.Context, id string) (*View, error) {
	if raw, err := s.st.Redis.Get(ctx, cacheKey(id)).Bytes(); err == nil {
		var v View
		if json.Unmarshal(raw, &v) == nil {
			return &v, nil
		}
	}
	v, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if raw, err := json.Marshal(v); err == nil {
		if err := s.st.Redis.Set(ctx, cacheKey(id), raw, cacheTTL).Err(); err != nil {
			s.log.Warn(logMsgCacheSetFailed, "error", err, "ride_id", id)
		}
	}
	return v, nil
}

// load reads a ride and (when assigned) its driver card straight from Postgres.
func (s *Service) load(ctx context.Context, id string) (*View, error) {
	const selectSQL = `
		SELECT r.id, r.rider_id, r.driver_id, r.quote_id, r.tier, r.status,
		       r.pickup_lat, r.pickup_lng, r.drop_lat, r.drop_lng,
		       r.payment_method, r.fare_total, r.cancel_reason,
		       r.created_at, r.updated_at,
		       d.name, d.vehicle_model, d.plate, d.rating
		FROM rides r
		LEFT JOIN drivers d ON d.id = r.driver_id
		WHERE r.id = $1`

	var (
		v      View
		dName  *string
		dModel *string
		dPlate *string
		dRate  *float64
	)
	err := s.st.PG.QueryRow(ctx, selectSQL, id).Scan(
		&v.ID, &v.RiderID, &v.DriverID, &v.QuoteID, &v.Tier, &v.Status,
		&v.PickupLat, &v.PickupLng, &v.DropLat, &v.DropLng,
		&v.PaymentMethod, &v.FareTotal, &v.CancelReason,
		&v.CreatedAt, &v.UpdatedAt,
		&dName, &dModel, &dPlate, &dRate,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("rides: load: %w", err)
	}
	if v.DriverID != nil && dName != nil {
		v.Driver = &DriverCard{
			Name:         *dName,
			VehicleModel: derefStr(dModel),
			Plate:        derefStr(dPlate),
			Rating:       derefFloat(dRate),
		}
	}
	return &v, nil
}

func authorized(v *View, actorID, actorRole string) bool {
	switch actorRole {
	case RoleRider:
		return v.RiderID == actorID
	case RoleDriver:
		return v.DriverID != nil && *v.DriverID == actorID
	default:
		return false
	}
}

func statusStrings(ss []Status) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = string(s)
	}
	return out
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
