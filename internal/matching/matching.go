// Package matching is the driver–rider matching engine: candidate search,
// atomic offer claims, accept/decline, and a sweeper that advances or expires
// MATCHING rides.
//
// Concurrency contract (the point of this milestone):
//   - No driver double-offer: an offer is a Redis `SET offer:driver:{id} NX`;
//     only one matcher can hold a driver at a time.
//   - No ride double-assignment: Accept flips the ride MATCHING→DRIVER_ASSIGNED
//     with an optimistic guarded UPDATE inside one transaction that also flips
//     the driver available→on_trip; concurrent accepts see zero rows and lose.
//   - Safe with N instances: the sweeper uses FOR UPDATE SKIP LOCKED and all
//     writes are guarded, so no leader election or queue is needed.
package matching

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/newrelic/go-agent/v3/newrelic"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"

	"github.com/lokeshbm/goride/internal/drivers"
	"github.com/lokeshbm/goride/internal/rides"
	"github.com/lokeshbm/goride/internal/store"
)

// Domain errors, mapped to HTTP codes by the handler layer.
var (
	// ErrOfferExpired: the driver does not hold a live offer for this ride.
	ErrOfferExpired = errors.New("matching: offer expired or not held")
	// ErrRideGone: the ride is no longer MATCHING (assigned/cancelled/expired).
	ErrRideGone = errors.New("matching: ride not available for assignment")
	// ErrNotFound: no such ride.
	ErrNotFound = errors.New("matching: ride not found")
)

// driverStatus mirrors the JSON at driver:status:{id} written by the drivers
// package. Duplicated (not imported) to keep the search loop self-contained.
type driverStatus struct {
	Status string `json:"status"`
	Tier   string `json:"tier"`
	City   string `json:"city"`
}

// offerState is the JSON stored at offer:ride:{id}: the current outstanding
// offer, read by the sweeper and decline path.
type offerState struct {
	DriverID  string `json:"driver_id"`
	ExpiresAt int64  `json:"expires_at"` // Unix seconds
}

// rideCtx is the minimal ride view the offer loop needs.
type rideCtx struct {
	ID        string
	Tier      string
	PickupLat float64
	PickupLng float64
	City      string
	Status    string
	CreatedAt time.Time
}

// Engine is the matching engine.
type Engine struct {
	st      *store.Store
	rides   *rides.Service
	drivers *drivers.Service
	log     *slog.Logger
	// obs is the New Relic application used for the custom offer-latency
	// metric and accept/decline/expire counters (see recordOffer*/offerNext/
	// Accept/Decline/sweep below). Nil by default (and whenever monitoring is
	// disabled) — every RecordCustomMetric call is a documented no-op on a
	// nil *newrelic.Application, so this needs no separate guard.
	obs *newrelic.Application
}

// NewEngine constructs a matching Engine.
func NewEngine(st *store.Store, r *rides.Service, d *drivers.Service, log *slog.Logger) *Engine {
	return &Engine{st: st, rides: r, drivers: d, log: log}
}

// SetObservability wires the New Relic application used for matching's custom
// metrics. Optional: leaving it unset (nil) is a clean no-op.
func (e *Engine) SetObservability(app *newrelic.Application) { e.obs = app }

// ---- offer loop ----

// loadRideCtx loads the fields the offer loop needs, joining quotes for the
// city (rides has no city column; the quote is the source).
func (e *Engine) loadRideCtx(ctx context.Context, rideID string) (*rideCtx, error) {
	var r rideCtx
	r.ID = rideID
	err := e.st.PG.QueryRow(ctx, `
		SELECT r.tier, r.pickup_lat, r.pickup_lng, r.status, r.created_at, q.city
		FROM rides r JOIN quotes q ON q.id = r.quote_id
		WHERE r.id = $1`, rideID,
	).Scan(&r.Tier, &r.PickupLat, &r.PickupLng, &r.Status, &r.CreatedAt, &r.City)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("matching: load ride ctx: %w", err)
	}
	return &r, nil
}

// offerNext runs one step of the offer loop for a ride: search candidates,
// filter, and atomically claim the first eligible one. Returns true if an offer
// was made. Idempotent and safe to call from multiple instances — the SET NX is
// the arbiter.
func (e *Engine) offerNext(ctx context.Context, r *rideCtx) (bool, error) {
	members, err := e.st.Redis.GeoSearch(ctx, geoKey(r.City), &redis.GeoSearchQuery{
		Longitude:  r.PickupLng,
		Latitude:   r.PickupLat,
		Radius:     searchRadiusKm,
		RadiusUnit: "km",
		Sort:       "ASC",
		Count:      searchLimit,
	}).Result()
	if err != nil {
		return false, fmt.Errorf("matching: geosearch: %w", err)
	}

	triedList, err := e.st.Redis.SMembers(ctx, triedKey(r.ID)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, fmt.Errorf("matching: tried set: %w", err)
	}
	tried := make(map[string]bool, len(triedList))
	for _, id := range triedList {
		tried[id] = true
	}

	for _, driverID := range members {
		if tried[driverID] {
			continue
		}
		mirror, fresh, err := e.readCandidate(ctx, driverID)
		if err != nil {
			e.log.Warn(logMsgReadCandidateFailed, "error", err, "driver_id", driverID)
			continue
		}
		if !candidateEligible(mirror, fresh, r.Tier) {
			continue
		}

		// Atomic claim: one offer per driver across all instances.
		claimed, err := e.st.Redis.SetNX(ctx, offerDriver(driverID), r.ID, offerTTL).Result()
		if err != nil {
			return false, fmt.Errorf("matching: claim offer: %w", err)
		}
		if !claimed {
			continue // driver is holding another ride's offer
		}

		// tried is empty only on the very first successful claim for this ride
		// (every subsequent offer adds its driver to the tried-set below), so
		// this is exactly "the point the first offer is claimed for a ride" —
		// report request→offer latency using the ride's created_at.
		if len(tried) == 0 {
			e.obs.RecordCustomMetric(metricOfferLatencyMs, float64(time.Since(r.CreatedAt).Milliseconds()))
		}

		if err := e.recordOffer(ctx, r.ID, driverID); err != nil {
			return false, err
		}
		expiresAt := time.Now().Add(offerTTL)
		if err := e.rides.PublishDriver(ctx, driverID, eventRideOffer, map[string]any{
			"ride_id":    r.ID,
			"tier":       r.Tier,
			"pickup_lat": r.PickupLat,
			"pickup_lng": r.PickupLng,
			"expires_at": expiresAt.UTC().Format(time.RFC3339),
		}); err != nil {
			e.log.Warn(logMsgPublishOfferFailed, "error", err, "driver_id", driverID)
		}
		e.log.Info(logMsgOfferedRide, "ride_id", r.ID, "driver_id", driverID)
		return true, nil
	}
	return false, nil
}

// readCandidate reads a candidate driver's status mirror and freshness in one
// pipeline (no Postgres).
func (e *Engine) readCandidate(ctx context.Context, driverID string) (driverStatus, bool, error) {
	pipe := e.st.Redis.Pipeline()
	statusCmd := pipe.Get(ctx, statusKey(driverID))
	existsCmd := pipe.Exists(ctx, lastKey(driverID))
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return driverStatus{}, false, err
	}
	var mirror driverStatus
	if raw, err := statusCmd.Result(); err == nil {
		_ = json.Unmarshal([]byte(raw), &mirror)
	}
	return mirror, existsCmd.Val() == 1, nil
}

// recordOffer writes offer:ride, marks the driver tried, and refreshes the
// tried-set TTL — all in one round-trip.
func (e *Engine) recordOffer(ctx context.Context, rideID, driverID string) error {
	os := offerState{DriverID: driverID, ExpiresAt: time.Now().Add(offerTTL).Unix()}
	raw, _ := json.Marshal(os)
	pipe := e.st.Redis.Pipeline()
	pipe.Set(ctx, offerRide(rideID), raw, offerTTL)
	pipe.SAdd(ctx, triedKey(rideID), driverID)
	pipe.Expire(ctx, triedKey(rideID), triedTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("matching: record offer: %w", err)
	}
	return nil
}

// candidateEligible is the pure candidate filter: available, tier-matched, and
// fresh. The tried-set and offer-holder checks live in offerNext (the latter is
// the SET NX itself).
func candidateEligible(mirror driverStatus, fresh bool, wantTier string) bool {
	return mirror.Status == drivers.StatusAvailable && mirror.Tier == wantTier && fresh
}

// ---- hooks ----

// RequestMatch is the rides.MatchRequested hook: kick the first offer for a
// freshly-MATCHING ride immediately instead of waiting for the sweeper. It runs
// synchronously but on a context detached from the caller's request (which is
// about to complete), bounded by a short timeout.
func (e *Engine) RequestMatch(ctx context.Context, rideID string) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	r, err := e.loadRideCtx(dctx, rideID)
	if err != nil {
		e.log.Warn(logMsgRequestMatchLoadFailed, "error", err, "ride_id", rideID)
		return
	}
	if r.Status != string(rides.StatusMatching) {
		return
	}
	if _, err := e.offerNext(dctx, r); err != nil {
		e.log.Warn(logMsgRequestMatchOfferFailed, "error", err, "ride_id", rideID)
	}
}

// ---- accept / decline ----

// Accept assigns the ride to the driver iff the driver holds the live offer for
// it. Ownership is verified with an atomic GETDEL: the offer key is consumed
// exactly once, so two concurrent accepts cannot both proceed. On a mismatch
// (expired/never-held) it returns the current assigned view for a replay, else
// ErrOfferExpired.
//
// On mismatch or transaction failure the driver's offer key stays consumed;
// per SPEC we "restore nothing" — the driver simply gets the next offer.
func (e *Engine) Accept(ctx context.Context, driverID, rideID string) (*rides.View, error) {
	held, err := e.st.Redis.GetDel(ctx, offerDriver(driverID)).Result()
	if errors.Is(err, redis.Nil) {
		held = ""
	} else if err != nil {
		return nil, fmt.Errorf("matching: getdel offer: %w", err)
	}

	if !offerMatches(held, rideID) {
		// Replay: this driver may already be the assigned driver of the ride.
		if v, lerr := e.rides.LoadView(ctx, rideID); lerr == nil &&
			v.DriverID != nil && *v.DriverID == driverID && assignedOrLater(v.Status) {
			return v, nil
		}
		return nil, ErrOfferExpired
	}

	otp, err := genOTP()
	if err != nil {
		return nil, fmt.Errorf("matching: generate otp: %w", err)
	}
	otpHash, err := bcrypt.GenerateFromPassword([]byte(otp), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("matching: hash otp: %w", err)
	}

	if err := e.assignTx(ctx, rideID, driverID, string(otpHash)); err != nil {
		return nil, err
	}
	e.obs.RecordCustomMetric(metricOfferAccepted, 1)

	// Post-commit side effects. Read the driver card + city/tier for the mirror
	// and the assignment event.
	var name, model, plate, city, tier string
	var rating float64
	if err := e.st.PG.QueryRow(ctx,
		`SELECT name, vehicle_model, plate, rating, city, tier FROM drivers WHERE id = $1`, driverID,
	).Scan(&name, &model, &plate, &rating, &city, &tier); err != nil {
		e.log.Warn(logMsgLoadDriverCardFailed, "error", err, "driver_id", driverID)
	}

	if err := e.drivers.MarkOnTrip(ctx, driverID, rideID, city, tier); err != nil {
		e.log.Warn(logMsgMarkOnTripFailed, "error", err, "driver_id", driverID)
	}
	if err := e.rides.InvalidateCache(ctx, rideID); err != nil {
		e.log.Warn(logMsgCacheInvalidateFailed, "error", err, "ride_id", rideID)
	}
	// Delete the outstanding offer:ride marker (offer consumed).
	if err := e.st.Redis.Del(ctx, offerRide(rideID)).Err(); err != nil {
		e.log.Warn(logMsgDelOfferRideFailed, "error", err, "ride_id", rideID)
	}

	card := map[string]any{"name": name, "vehicle_model": model, "plate": plate, "rating": rating}
	if err := e.rides.PublishRide(ctx, rideID, eventRideStatusChanged, map[string]any{
		"status": string(rides.StatusDriverAssigned),
		"driver": card,
	}); err != nil {
		e.log.Warn(logMsgPublishStatusFailed, "error", err, "ride_id", rideID)
	}
	// OTP goes to the rider on the ride channel.
	if err := e.rides.PublishRide(ctx, rideID, eventRideOTP, map[string]any{"otp": otp}); err != nil {
		e.log.Warn(logMsgPublishOTPFailed, "error", err, "ride_id", rideID)
	}

	e.log.Info(logMsgRideAssigned, "ride_id", rideID, "driver_id", driverID)
	return e.rides.LoadView(ctx, rideID)
}

// assignTx is the single assignment transaction: guarded ride MATCHING →
// DRIVER_ASSIGNED (+driver_id, +otp_hash) and driver available → on_trip. Either
// guard failing rolls back and yields a 409-class error.
func (e *Engine) assignTx(ctx context.Context, rideID, driverID, otpHash string) error {
	tx, err := e.st.PG.Begin(ctx)
	if err != nil {
		return fmt.Errorf("matching: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE rides SET status = 'DRIVER_ASSIGNED', driver_id = $1, otp_hash = $2, updated_at = now()
		WHERE id = $3 AND status = 'MATCHING'`, driverID, otpHash, rideID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return ErrRideGone // driver already has an active ride (partial unique idx)
		}
		return fmt.Errorf("matching: update ride: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRideGone // ride left MATCHING (cancelled/expired/assigned)
	}

	tag2, err := tx.Exec(ctx,
		`UPDATE drivers SET status = 'on_trip' WHERE id = $1 AND status = 'available'`, driverID)
	if err != nil {
		return fmt.Errorf("matching: update driver: %w", err)
	}
	if tag2.RowsAffected() == 0 {
		return ErrRideGone // driver not available (offline/already on a trip)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("matching: commit: %w", err)
	}
	return nil
}

// Decline releases the driver's offer (only if held by this driver) and
// immediately advances the offer loop to the next candidate. The declining
// driver stays in the ride's tried-set, so they are not re-offered.
func (e *Engine) Decline(ctx context.Context, driverID, rideID string) error {
	held, err := e.st.Redis.GetDel(ctx, offerDriver(driverID)).Result()
	if errors.Is(err, redis.Nil) {
		held = ""
	} else if err != nil {
		return fmt.Errorf("matching: getdel offer: %w", err)
	}
	// Clear offer:ride only if it still points at this driver (avoid stomping a
	// newer offer to someone else).
	if raw, gerr := e.st.Redis.Get(ctx, offerRide(rideID)).Result(); gerr == nil {
		var os offerState
		if json.Unmarshal([]byte(raw), &os) == nil && os.DriverID == driverID {
			_ = e.st.Redis.Del(ctx, offerRide(rideID)).Err()
		}
	}
	// held == rideID confirms this driver actually held a live offer for this
	// ride (vs. a stale/expired call) — only count a genuine decline.
	if held == rideID {
		e.obs.RecordCustomMetric(metricOfferDeclined, 1)
	}

	// Advance immediately if the ride is still matching.
	r, err := e.loadRideCtx(ctx, rideID)
	if err != nil {
		return err
	}
	if r.Status == string(rides.StatusMatching) {
		if _, err := e.offerNext(ctx, r); err != nil {
			e.log.Warn(logMsgDeclineAdvanceFailed, "error", err, "ride_id", rideID)
		}
	}
	return nil
}

// ---- sweeper ----

// Start launches the sweeper goroutine (2s tick). It exits cleanly when ctx is
// cancelled; the ticker is stopped on exit (no leaked timer).
func (e *Engine) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		e.log.Info(logMsgSweeperStarted, "interval", sweepInterval.String())
		for {
			select {
			case <-ctx.Done():
				e.log.Info(logMsgSweeperStopped)
				return
			case <-t.C:
				e.sweep(ctx)
			}
		}
	}()
}

// sweep advances or expires MATCHING rides. It claims a batch under FOR UPDATE
// SKIP LOCKED, commits immediately (short tx), then acts: rides past the
// deadline are expired via the rides funnel (guarded, safe post-commit); rides
// with no live offer get one offer-loop step.
func (e *Engine) sweep(ctx context.Context) {
	rows, err := e.claimMatchingBatch(ctx)
	if err != nil {
		e.log.Warn(logMsgSweepClaimFailed, "error", err)
		return
	}
	now := time.Now()
	for i := range rows {
		r := rows[i]
		if now.Sub(r.CreatedAt) > matchDeadline {
			if err := e.rides.Expire(ctx, r.ID); err != nil && !errors.Is(err, rides.ErrInvalidState) {
				e.log.Warn(logMsgExpireFailed, "error", err, "ride_id", r.ID)
			} else if err == nil {
				// Candidates exhausted / 60s TTL passed with no accept — the
				// ride's whole matching window expired unresolved.
				e.obs.RecordCustomMetric(metricOfferExpired, 1)
			}
			continue
		}
		n, err := e.st.Redis.Exists(ctx, offerRide(r.ID)).Result()
		if err != nil {
			e.log.Warn(logMsgSweepExistsFailed, "error", err, "ride_id", r.ID)
			continue
		}
		if n == 0 {
			if _, err := e.offerNext(ctx, &r); err != nil {
				e.log.Warn(logMsgSweepOfferFailed, "error", err, "ride_id", r.ID)
			}
		}
	}
}

// claimMatchingBatch selects up to sweepBatch MATCHING rides under FOR UPDATE
// SKIP LOCKED (locks only rides), then commits to release the locks. Returned
// rows are a snapshot; all follow-up work uses guarded/atomic writes.
func (e *Engine) claimMatchingBatch(ctx context.Context) ([]rideCtx, error) {
	tx, err := e.st.PG.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("matching: sweep begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	pgRows, err := tx.Query(ctx, `
		SELECT r.id, r.tier, r.pickup_lat, r.pickup_lng, q.city, r.created_at
		FROM rides r JOIN quotes q ON q.id = r.quote_id
		WHERE r.status = 'MATCHING'
		ORDER BY r.created_at
		FOR UPDATE OF r SKIP LOCKED
		LIMIT $1`, sweepBatch)
	if err != nil {
		return nil, fmt.Errorf("matching: sweep query: %w", err)
	}
	var out []rideCtx
	for pgRows.Next() {
		var r rideCtx
		r.Status = string(rides.StatusMatching)
		if err := pgRows.Scan(&r.ID, &r.Tier, &r.PickupLat, &r.PickupLng, &r.City, &r.CreatedAt); err != nil {
			pgRows.Close()
			return nil, fmt.Errorf("matching: sweep scan: %w", err)
		}
		out = append(out, r)
	}
	pgRows.Close()
	if err := pgRows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("matching: sweep commit: %w", err)
	}
	return out, nil
}

// ---- pure helpers (unit-tested) ----

// offerMatches reports whether a consumed offer value matches the ride the
// driver is trying to accept. Empty held (no offer) never matches.
func offerMatches(held, rideID string) bool {
	return held != "" && held == rideID
}

// assignedOrLater reports whether a ride status is at or beyond DRIVER_ASSIGNED
// (used for accept replay detection).
func assignedOrLater(status string) bool {
	switch rides.Status(status) {
	case rides.StatusDriverAssigned, rides.StatusDriverArriving, rides.StatusArrived,
		rides.StatusInProgress, rides.StatusCompleted:
		return true
	}
	return false
}

// genOTP returns a uniformly-random 4-digit OTP ("0000".."9999") using
// crypto/rand.
func genOTP() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%04d", n.Int64()), nil
}
