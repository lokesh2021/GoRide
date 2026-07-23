// Package quotes creates and reads upfront fare quotes. A quote locks per-tier
// prices and the surge multiplier for 3 minutes; booking references it.
package quotes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lokeshbm/goride/internal/pricing"
	"github.com/lokeshbm/goride/internal/store"
)

// ErrNotFound is returned by Get when no quote matches the id.
var ErrNotFound = errors.New("quotes: not found")

// Coord is a latitude/longitude pair in degrees.
type Coord struct {
	Lat float64
	Lng float64
}

// Quote is a persisted fare quote.
type Quote struct {
	ID        string
	RiderID   string
	City      string
	Pickup    Coord
	Drop      Coord
	DistanceM int
	DurationS int
	SurgeX100 int
	Prices    map[string]int // tier → paise
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Service creates and reads quotes.
type Service struct {
	st  *store.Store
	log *slog.Logger
}

// NewService constructs a quotes Service.
func NewService(st *store.Store, log *slog.Logger) *Service {
	return &Service{st: st, log: log}
}

// Create computes an estimate and surge for pickup→drop, persists a quote row
// with per-tier prices, and increments the pickup cell's demand counter.
//
// Hot path: one Postgres INSERT plus ~3 Redis ops (demand read, GEOSEARCH,
// demand INCR+EXPIRE pipeline).
func (s *Service) Create(ctx context.Context, riderID string, pickup, drop Coord, city string) (*Quote, error) {
	now := time.Now().UTC()

	distanceM, durationS := pricing.Estimate(pickup.Lat, pickup.Lng, drop.Lat, drop.Lng)

	surgeX100, err := pricing.ComputeSurge(ctx, s.st.Redis, city, pickup.Lat, pickup.Lng, now)
	if err != nil {
		return nil, fmt.Errorf("quotes: compute surge: %w", err)
	}

	prices := pricing.Prices(distanceM, durationS, surgeX100)
	pricesJSON, err := json.Marshal(prices)
	if err != nil {
		return nil, fmt.Errorf("quotes: marshal prices: %w", err)
	}

	q := &Quote{
		ID:        uuid.NewString(),
		RiderID:   riderID,
		City:      city,
		Pickup:    pickup,
		Drop:      drop,
		DistanceM: distanceM,
		DurationS: durationS,
		SurgeX100: surgeX100,
		Prices:    prices,
		ExpiresAt: now.Add(quoteTTL),
		CreatedAt: now,
	}

	const insertSQL = `
		INSERT INTO quotes
			(id, rider_id, city, pickup_lat, pickup_lng, drop_lat, drop_lng,
			 distance_m, duration_s, surge_x100, prices, expires_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`
	if _, err := s.st.PG.Exec(ctx, insertSQL,
		q.ID, q.RiderID, q.City,
		q.Pickup.Lat, q.Pickup.Lng, q.Drop.Lat, q.Drop.Lng,
		q.DistanceM, q.DurationS, q.SurgeX100, pricesJSON, q.ExpiresAt, q.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("quotes: insert: %w", err)
	}

	// Demand signal for surge; best-effort but logged on failure.
	if err := pricing.IncrementDemand(ctx, s.st.Redis, city, pickup.Lat, pickup.Lng, now); err != nil {
		s.log.Warn(logMsgIncrementDemandFailed, "error", err, "quote_id", q.ID)
	}

	return q, nil
}

// Get loads a quote by id for booking-time validation.
func (s *Service) Get(ctx context.Context, id string) (*Quote, error) {
	const selectSQL = `
		SELECT id, rider_id, city, pickup_lat, pickup_lng, drop_lat, drop_lng,
		       distance_m, duration_s, surge_x100, prices, expires_at, created_at
		FROM quotes WHERE id = $1`

	var (
		q          Quote
		pricesJSON []byte
	)
	err := s.st.PG.QueryRow(ctx, selectSQL, id).Scan(
		&q.ID, &q.RiderID, &q.City,
		&q.Pickup.Lat, &q.Pickup.Lng, &q.Drop.Lat, &q.Drop.Lng,
		&q.DistanceM, &q.DurationS, &q.SurgeX100, &pricesJSON, &q.ExpiresAt, &q.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("quotes: select: %w", err)
	}
	if err := json.Unmarshal(pricesJSON, &q.Prices); err != nil {
		return nil, fmt.Errorf("quotes: unmarshal prices: %w", err)
	}
	return &q, nil
}
