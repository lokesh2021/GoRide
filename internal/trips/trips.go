// Package trips owns the trip lifecycle: OTP-verified start, pause/resume with
// accumulated paused time, and end with actual-fare finalization.
//
// Like the matching engine, trips runs its own guarded transactions against the
// rides/trips tables (rather than routing through rides' private status funnel)
// and then reuses the exported rides.InvalidateCache + rides.PublishRide seams
// post-commit, keeping cache invalidation and event publishing consistent with
// the rest of the system.
//
// Schema note: the trips table has no paused_at column, so an in-flight pause's
// start time is mirrored in Redis at trip:paused_at:{ride_id} (a rebuildable
// cache, in the same spirit as the driver:* mirrors). On resume/end the elapsed
// pause is folded into trips.paused_seconds and the key is deleted.
package trips

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"

	"github.com/lokeshbm/goride/internal/drivers"
	"github.com/lokeshbm/goride/internal/pricing"
	"github.com/lokeshbm/goride/internal/quotes"
	"github.com/lokeshbm/goride/internal/rides"
	"github.com/lokeshbm/goride/internal/store"
)

// Domain errors, mapped to HTTP codes by the handler layer.
var (
	ErrNotFound     = errors.New("trips: not found")
	ErrForbidden    = errors.New("trips: forbidden")
	ErrInvalidState = errors.New("trips: invalid state")
	ErrInvalidOTP   = errors.New("trips: invalid otp")
)

// Trip is the read model returned by the service.
type Trip struct {
	RideID        string             `json:"ride_id"`
	Status        string             `json:"status"`
	RideStatus    string             `json:"ride_status"`
	StartedAt     time.Time          `json:"started_at"`
	EndedAt       *time.Time         `json:"ended_at,omitempty"`
	PausedSeconds int                `json:"paused_seconds"`
	DistanceM     *int               `json:"distance_m,omitempty"`
	Fare          *pricing.Breakdown `json:"fare,omitempty"`
}

// Service is the trip domain service.
type Service struct {
	st      *store.Store
	rides   *rides.Service
	drivers *drivers.Service
	quotes  *quotes.Service
	log     *slog.Logger
}

// NewService constructs a trip Service.
func NewService(st *store.Store, r *rides.Service, d *drivers.Service, q *quotes.Service, log *slog.Logger) *Service {
	return &Service{st: st, rides: r, drivers: d, quotes: q, log: log}
}

// ---- start ----

// Start verifies the rider OTP and starts the trip. The caller must be the
// ride's assigned driver and the ride must be ARRIVED. A wrong OTP returns
// ErrInvalidOTP with no state change. On success it runs one transaction: ride
// ARRIVED → IN_PROGRESS (guarded) + insert the STARTED trip row, then invalidates
// the ride cache and publishes ride.status_changed post-commit.
func (s *Service) Start(ctx context.Context, driverID, rideID, otp string) (*Trip, error) {
	var (
		assigned *string
		status   string
		otpHash  *string
	)
	err := s.st.PG.QueryRow(ctx,
		`SELECT driver_id, status, otp_hash FROM rides WHERE id = $1`, rideID,
	).Scan(&assigned, &status, &otpHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("trips: start load: %w", err)
	}
	if assigned == nil || *assigned != driverID {
		return nil, ErrForbidden
	}
	if status != string(rides.StatusArrived) {
		return nil, ErrInvalidState
	}
	if otpHash == nil {
		return nil, ErrInvalidState
	}
	// bcrypt compare against the hash set at assignment. Wrong OTP: no state change.
	if bcrypt.CompareHashAndPassword([]byte(*otpHash), []byte(otp)) != nil {
		return nil, ErrInvalidOTP
	}

	startedAt := time.Now().UTC()
	tx, err := s.st.PG.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("trips: start begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE rides SET status = 'IN_PROGRESS', updated_at = now()
		 WHERE id = $1 AND status = 'ARRIVED'`, rideID)
	if err != nil {
		return nil, fmt.Errorf("trips: start ride update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrInvalidState
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO trips (id, ride_id, status, started_at, paused_seconds)
		 VALUES ($1, $2, 'STARTED', $3, 0)`,
		uuid.NewString(), rideID, startedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return nil, ErrInvalidState // a trip already exists for this ride
		}
		return nil, fmt.Errorf("trips: insert trip: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("trips: start commit: %w", err)
	}

	// Reset the metered-distance counter so only movement from trip start (not
	// the pickup approach, which also carries the driver:ride marker) is counted.
	s.drivers.ReadAndClearTripDistance(ctx, rideID)

	s.afterRideStatus(ctx, rideID, rides.StatusInProgress, nil)

	return &Trip{
		RideID:     rideID,
		Status:     StatusStarted,
		RideStatus: string(rides.StatusInProgress),
		StartedAt:  startedAt,
	}, nil
}

// ---- pause / resume ----

// Pause transitions the trip STARTED → PAUSED (optimistic guard) for the ride's
// assigned driver and records the pause-start instant in Redis.
func (s *Service) Pause(ctx context.Context, driverID, rideID string) (*Trip, error) {
	if err := s.assertAssignedDriver(ctx, rideID, driverID); err != nil {
		return nil, err
	}
	tag, err := s.st.PG.Exec(ctx,
		`UPDATE trips SET status = 'PAUSED' WHERE ride_id = $1 AND status = 'STARTED'`, rideID)
	if err != nil {
		return nil, fmt.Errorf("trips: pause update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrInvalidState
	}
	// Mirror the pause-start instant (no paused_at column); folded in on resume.
	if err := s.st.Redis.Set(ctx, pausedAtKey(rideID), time.Now().UTC().Unix(), pausedAtTTL).Err(); err != nil {
		s.log.Warn(logMsgSetPausedAtFailed, "error", err, "ride_id", rideID)
	}
	return s.load(ctx, rideID)
}

// Resume transitions the trip PAUSED → STARTED (optimistic guard), accumulating
// the elapsed pause into paused_seconds.
func (s *Service) Resume(ctx context.Context, driverID, rideID string) (*Trip, error) {
	if err := s.assertAssignedDriver(ctx, rideID, driverID); err != nil {
		return nil, err
	}
	elapsed := s.consumePause(ctx, rideID)
	tag, err := s.st.PG.Exec(ctx,
		`UPDATE trips SET status = 'STARTED', paused_seconds = paused_seconds + $2
		 WHERE ride_id = $1 AND status = 'PAUSED'`, rideID, elapsed)
	if err != nil {
		return nil, fmt.Errorf("trips: resume update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrInvalidState
	}
	return s.load(ctx, rideID)
}

// ---- end ----

// End finalizes the trip: it computes the actual fare (actual distance from the
// metered ping path with a quote fallback, actual duration net of paused time,
// at the quoted surge and the booked tier's rates — never live surge), then in
// one transaction moves the ride IN_PROGRESS → COMPLETED (writing fare_total),
// the trip → ENDED (writing paused_seconds/distance/fare breakdown), frees the
// driver, and creates a PENDING payment. Post-commit it releases the driver back
// into the geo pool, invalidates the ride cache, and publishes
// ride.status_changed carrying the fare breakdown.
func (s *Service) End(ctx context.Context, driverID, rideID string) (*Trip, error) {
	var (
		assigned      *string
		rideStatus    string
		quoteID       string
		tier          string
		paymentMethod *string
	)
	err := s.st.PG.QueryRow(ctx,
		`SELECT driver_id, status, quote_id, tier, payment_method FROM rides WHERE id = $1`, rideID,
	).Scan(&assigned, &rideStatus, &quoteID, &tier, &paymentMethod)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("trips: end load ride: %w", err)
	}
	if assigned == nil || *assigned != driverID {
		return nil, ErrForbidden
	}
	if rideStatus != string(rides.StatusInProgress) {
		return nil, ErrInvalidState
	}

	// Load the trip row for duration + accumulated pause.
	var (
		tripStatus    string
		startedAt     time.Time
		pausedSeconds int
	)
	err = s.st.PG.QueryRow(ctx,
		`SELECT status, started_at, paused_seconds FROM trips WHERE ride_id = $1`, rideID,
	).Scan(&tripStatus, &startedAt, &pausedSeconds)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidState // ride IN_PROGRESS but no trip row (shouldn't happen)
	}
	if err != nil {
		return nil, fmt.Errorf("trips: end load trip: %w", err)
	}
	// If the trip is still paused at end, fold the in-flight pause in too.
	if tripStatus == StatusPaused {
		pausedSeconds += s.consumePause(ctx, rideID)
	}

	endedAt := time.Now().UTC()
	rawDuration := int(endedAt.Sub(startedAt).Seconds())
	actualDuration := rawDuration - pausedSeconds
	if actualDuration < 0 {
		actualDuration = 0
	}

	// Actual distance: metered ping path, else the quote's estimate.
	q, err := s.quotes.Get(ctx, quoteID)
	if err != nil {
		return nil, fmt.Errorf("trips: end load quote: %w", err)
	}
	distanceM, ok := s.drivers.ReadAndClearTripDistance(ctx, rideID)
	if !ok {
		distanceM = q.DistanceM
	}

	// Fare at the QUOTED surge and the booked tier's rates (never live surge).
	rates, ok := pricing.Tiers[tier]
	if !ok {
		return nil, fmt.Errorf("trips: unknown tier %q", tier)
	}
	breakdown := pricing.FinalFare(rates, distanceM, actualDuration, q.SurgeX100)
	fareJSON, err := json.Marshal(breakdown)
	if err != nil {
		return nil, fmt.Errorf("trips: marshal fare: %w", err)
	}

	tx, err := s.st.PG.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("trips: end begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE rides SET status = 'COMPLETED', fare_total = $2, updated_at = now()
		 WHERE id = $1 AND status = 'IN_PROGRESS'`, rideID, breakdown.Total)
	if err != nil {
		return nil, fmt.Errorf("trips: end ride update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrInvalidState
	}

	tag, err = tx.Exec(ctx,
		`UPDATE trips SET status = 'ENDED', ended_at = $2, paused_seconds = $3,
		        distance_m = $4, fare = $5
		 WHERE ride_id = $1 AND status IN ('STARTED', 'PAUSED')`,
		rideID, endedAt, pausedSeconds, distanceM, fareJSON)
	if err != nil {
		return nil, fmt.Errorf("trips: end trip update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrInvalidState
	}

	// Free the driver in the same transaction (mirrors Cancel); the geo re-add
	// happens post-commit via drivers.Release.
	if _, err := tx.Exec(ctx,
		`UPDATE drivers SET status = 'available' WHERE id = $1 AND status = 'on_trip'`, driverID,
	); err != nil {
		return nil, fmt.Errorf("trips: end free driver: %w", err)
	}

	// Payment intent: PENDING, amount = final total, method from the ride.
	method := "cash"
	if paymentMethod != nil {
		method = *paymentMethod
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO payments (id, ride_id, amount, method, status)
		 VALUES ($1, $2, $3, $4, 'PENDING')`,
		uuid.NewString(), rideID, breakdown.Total, method,
	); err != nil {
		return nil, fmt.Errorf("trips: end insert payment: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("trips: end commit: %w", err)
	}

	// Post-commit: re-add driver to geo pool, invalidate cache, publish w/ fare.
	if err := s.drivers.Release(ctx, driverID); err != nil {
		s.log.Warn(logMsgDriverReleaseFailed, "error", err, "driver_id", driverID)
	}
	s.afterRideStatus(ctx, rideID, rides.StatusCompleted, map[string]any{"fare": breakdown})

	return &Trip{
		RideID:        rideID,
		Status:        StatusEnded,
		RideStatus:    string(rides.StatusCompleted),
		StartedAt:     startedAt,
		EndedAt:       &endedAt,
		PausedSeconds: pausedSeconds,
		DistanceM:     &distanceM,
		Fare:          &breakdown,
	}, nil
}

// ---- helpers ----

// afterRideStatus invalidates the ride cache and publishes ride.status_changed,
// mirroring the matching engine's post-commit side effects. extra is merged into
// the event data (e.g. the fare breakdown on end).
func (s *Service) afterRideStatus(ctx context.Context, rideID string, to rides.Status, extra map[string]any) {
	if err := s.rides.InvalidateCache(ctx, rideID); err != nil {
		s.log.Warn(logMsgCacheInvalidateFailed, "error", err, "ride_id", rideID)
	}
	data := map[string]any{"status": string(to)}
	for k, v := range extra {
		data[k] = v
	}
	if err := s.rides.PublishRide(ctx, rideID, eventRideStatusChanged, data); err != nil {
		s.log.Warn(logMsgPublishStatusFailed, "error", err, "ride_id", rideID)
	}
}

// assertAssignedDriver verifies the actor is the ride's currently assigned
// driver (404 if no such ride, 403 if not the assigned driver).
func (s *Service) assertAssignedDriver(ctx context.Context, rideID, driverID string) error {
	var assigned *string
	err := s.st.PG.QueryRow(ctx, `SELECT driver_id FROM rides WHERE id = $1`, rideID).Scan(&assigned)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("trips: assert driver: %w", err)
	}
	if assigned == nil || *assigned != driverID {
		return ErrForbidden
	}
	return nil
}

// consumePause reads and deletes trip:paused_at:{ride_id}, returning the elapsed
// pause in seconds (0 if unset or in the future — defensive).
func (s *Service) consumePause(ctx context.Context, rideID string) int {
	raw, err := s.st.Redis.GetDel(ctx, pausedAtKey(rideID)).Result()
	if errors.Is(err, redis.Nil) || raw == "" {
		return 0
	}
	if err != nil {
		s.log.Warn(logMsgReadPausedAtFailed, "error", err, "ride_id", rideID)
		return 0
	}
	pausedAt, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	elapsed := time.Now().UTC().Unix() - pausedAt
	if elapsed < 0 {
		return 0
	}
	return int(elapsed)
}

// load reads the current trip row into a Trip view (used by pause/resume).
func (s *Service) load(ctx context.Context, rideID string) (*Trip, error) {
	var (
		t         Trip
		endedAt   *time.Time
		distanceM *int
		fareJSON  []byte
	)
	t.RideID = rideID
	err := s.st.PG.QueryRow(ctx,
		`SELECT t.status, t.started_at, t.ended_at, t.paused_seconds, t.distance_m, t.fare, r.status
		 FROM trips t JOIN rides r ON r.id = t.ride_id
		 WHERE t.ride_id = $1`, rideID,
	).Scan(&t.Status, &t.StartedAt, &endedAt, &t.PausedSeconds, &distanceM, &fareJSON, &t.RideStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("trips: load: %w", err)
	}
	t.EndedAt = endedAt
	t.DistanceM = distanceM
	if len(fareJSON) > 0 {
		var b pricing.Breakdown
		if json.Unmarshal(fareJSON, &b) == nil {
			t.Fare = &b
		}
	}
	return &t, nil
}
