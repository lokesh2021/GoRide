//go:build integration

// Package integration holds the build-tagged integration and concurrency tests
// (SPEC "Testing conventions"). They hit a real Postgres + Redis from
// GORIDE_PG_DSN / GORIDE_REDIS_ADDR and exercise the assignment §5 invariants
// against an in-process copy of the real server (router + services + store).
//
// Each test creates its own entities with random UUIDs and tokens (never the
// seed rows), and registers a cleanup that deletes those rows and their Redis
// keys, so the suite is safe to run repeatedly and leaves the datastore clean.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lokeshbm/goride/internal/config"
	"github.com/lokeshbm/goride/internal/drivers"
	"github.com/lokeshbm/goride/internal/events"
	"github.com/lokeshbm/goride/internal/httpapi"
	"github.com/lokeshbm/goride/internal/matching"
	"github.com/lokeshbm/goride/internal/payments"
	"github.com/lokeshbm/goride/internal/quotes"
	"github.com/lokeshbm/goride/internal/rides"
	"github.com/lokeshbm/goride/internal/store"
	"github.com/lokeshbm/goride/internal/trips"
)

const (
	defaultPGDSN     = "postgres://lokesh@localhost:5432/goride?sslmode=disable"
	defaultRedisAddr = "localhost:6379"
	testPSPSecret    = "integration-test-secret"
	testCity         = "BLR"
)

// env is the shared per-test fixture: a live store, the wired domain services,
// and an in-process httptest server running the real router.
type env struct {
	t        *testing.T
	ctx      context.Context
	st       *store.Store
	cfg      config.Config
	quotes   *quotes.Service
	rides    *rides.Service
	drivers  *drivers.Service
	match    *matching.Engine
	trips    *trips.Service
	payments *payments.Service
	server   *httptest.Server
	log      *slog.Logger

	mu         sync.Mutex
	riderIDs   []string
	driverIDs  []string
	rideIDs    []string
	redisKeys  []string
	geoMembers []string
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// newEnv connects to Postgres + Redis and wires the full server exactly as
// cmd/server does (minus the sweeper, which is deliberately not started so the
// concurrency tests fully control offer/assignment timing). If infra is
// unreachable the test fails loudly rather than skipping.
func newEnv(t *testing.T) *env {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := config.Config{
		Addr:          ":0",
		PGDSN:         getenv("GORIDE_PG_DSN", defaultPGDSN),
		RedisAddr:     getenv("GORIDE_REDIS_ADDR", defaultRedisAddr),
		Env:           "test",
		PSPSecret:     testPSPSecret,
		PSPWebhookURL: "http://127.0.0.1:1/unused", // async PSP callback is never exercised here
	}

	ctx := context.Background()
	st, err := store.New(ctx, cfg)
	if err != nil {
		t.Fatalf("infra unavailable (Postgres=%s Redis=%s): %v", cfg.PGDSN, cfg.RedisAddr, err)
	}

	quoteSvc := quotes.NewService(st, log)
	rideSvc := rides.NewService(st, quoteSvc, log)
	driverSvc := drivers.NewService(st, log)
	matchEngine := matching.NewEngine(st, rideSvc, driverSvc, log)
	tripSvc := trips.NewService(st, rideSvc, driverSvc, quoteSvc, log)
	psp := payments.NewPSP(cfg.PSPWebhookURL, cfg.PSPSecret, log)
	paymentSvc := payments.NewService(st, rideSvc, psp, cfg.PSPSecret, log)

	eventPub := events.NewPublisher(st.Redis, log)
	rideSvc.SetEventPublisher(eventPub)
	driverSvc.SetPublisher(eventPub)
	eventHub := events.NewHub(ctx, st.Redis, log)

	rideSvc.MatchRequested = matchEngine.RequestMatch
	rideSvc.OnDriverReleased = func(ctx context.Context, driverID string) {
		_ = driverSvc.Release(ctx, driverID)
	}

	router := httpapi.NewRouter(httpapi.Deps{
		Health:   st,
		Store:    st,
		Quotes:   quoteSvc,
		Rides:    rideSvc,
		Drivers:  driverSvc,
		Match:    matchEngine,
		Trips:    tripSvc,
		Payments: paymentSvc,
		Events:   eventHub,
		Logger:   log,
	})
	srv := httptest.NewServer(router)

	e := &env{
		t: t, ctx: ctx, st: st, cfg: cfg,
		quotes: quoteSvc, rides: rideSvc, drivers: driverSvc,
		match: matchEngine, trips: tripSvc, payments: paymentSvc,
		server: srv, log: log,
	}
	t.Cleanup(e.cleanup)
	return e
}

// cleanup removes every row and Redis key this test created, in FK-safe order,
// leaving only the 8 seed rows behind.
func (e *env) cleanup() {
	ctx := context.Background()

	// Discover HTTP-created ride ids (random uuids we never tracked) so their
	// Redis keys are cleaned too.
	rideIDs := append([]string(nil), e.rideIDs...)
	if len(e.riderIDs) > 0 {
		rows, err := e.st.PG.Query(ctx, `SELECT id::text FROM rides WHERE rider_id = ANY($1)`, e.riderIDs)
		if err == nil {
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					rideIDs = append(rideIDs, id)
				}
			}
			rows.Close()
		}
	}

	// Redis: tracked keys + per-ride + per-driver mirror/offer keys.
	keys := append([]string(nil), e.redisKeys...)
	for _, rid := range rideIDs {
		keys = append(keys,
			"ride:cache:"+rid, "offer:ride:"+rid, "offered:ride:"+rid,
			"trip:dist:"+rid, "trip:paused_at:"+rid,
		)
	}
	for _, did := range e.driverIDs {
		keys = append(keys,
			"offer:driver:"+did, "driver:status:"+did,
			"driver:last:"+did, "driver:ride:"+did,
		)
	}
	if len(keys) > 0 {
		_ = e.st.Redis.Del(ctx, keys...).Err()
	}
	if len(e.driverIDs) > 0 {
		_ = e.st.Redis.ZRem(ctx, "geo:drivers:"+testCity, toAny(e.driverIDs)...).Err()
	}

	// Postgres, FK order: receipts/payments/trips → rides → idem → quotes → drivers/riders.
	if len(e.riderIDs) > 0 {
		for _, q := range []string{
			`DELETE FROM receipts WHERE ride_id IN (SELECT id FROM rides WHERE rider_id = ANY($1))`,
			`DELETE FROM payments WHERE ride_id IN (SELECT id FROM rides WHERE rider_id = ANY($1))`,
			`DELETE FROM trips    WHERE ride_id IN (SELECT id FROM rides WHERE rider_id = ANY($1))`,
			`DELETE FROM rides    WHERE rider_id = ANY($1)`,
			`DELETE FROM quotes   WHERE rider_id = ANY($1)`,
		} {
			if _, err := e.st.PG.Exec(ctx, q, e.riderIDs); err != nil {
				e.t.Logf("cleanup: %v", err)
			}
		}
	}
	actors := append(append([]string(nil), e.riderIDs...), e.driverIDs...)
	if len(actors) > 0 {
		_, _ = e.st.PG.Exec(ctx, `DELETE FROM idempotency_keys WHERE actor_id = ANY($1)`, actors)
	}
	if len(e.driverIDs) > 0 {
		_, _ = e.st.PG.Exec(ctx, `DELETE FROM drivers WHERE id = ANY($1)`, e.driverIDs)
	}
	if len(e.riderIDs) > 0 {
		_, _ = e.st.PG.Exec(ctx, `DELETE FROM riders WHERE id = ANY($1)`, e.riderIDs)
	}

	e.server.Close()
	e.st.Close()
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func (e *env) trackRide(id string) {
	e.mu.Lock()
	e.rideIDs = append(e.rideIDs, id)
	e.mu.Unlock()
}

func (e *env) trackRedisKey(k string) {
	e.mu.Lock()
	e.redisKeys = append(e.redisKeys, k)
	e.mu.Unlock()
}

// ---- entity factories (random uuids + tokens, never the seed rows) ----

// insertRider creates a rider and returns (id, token).
func (e *env) insertRider() (string, string) {
	id := uuid.NewString()
	token := "it-rider-" + uuid.NewString()
	_, err := e.st.PG.Exec(e.ctx,
		`INSERT INTO riders (id, name, phone, api_token) VALUES ($1,$2,$3,$4)`,
		id, "IT Rider", "+9199"+randDigits(), token)
	if err != nil {
		e.t.Fatalf("insert rider: %v", err)
	}
	e.mu.Lock()
	e.riderIDs = append(e.riderIDs, id)
	e.mu.Unlock()
	return id, token
}

// insertDriver creates a driver with the given tier + status and returns (id, token).
func (e *env) insertDriver(tier, status string) (string, string) {
	id := uuid.NewString()
	token := "it-driver-" + uuid.NewString()
	_, err := e.st.PG.Exec(e.ctx,
		`INSERT INTO drivers (id, name, phone, city, tier, vehicle_model, plate, rating, status, api_token)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		id, "IT Driver", "+9198"+randDigits(), testCity, tier,
		"Test Model", "KA-99-"+randDigits(), 4.5, status, token)
	if err != nil {
		e.t.Fatalf("insert driver: %v", err)
	}
	e.mu.Lock()
	e.driverIDs = append(e.driverIDs, id)
	e.mu.Unlock()
	return id, token
}

// insertQuote creates a non-expired quote for the rider with prices for all
// tiers, returning its id.
func (e *env) insertQuote(riderID string) string {
	id := uuid.NewString()
	prices := map[string]int{"mini": 15000, "sedan": 22000, "xl": 30000}
	pricesJSON, _ := json.Marshal(prices)
	_, err := e.st.PG.Exec(e.ctx, `
		INSERT INTO quotes
			(id, rider_id, city, pickup_lat, pickup_lng, drop_lat, drop_lng,
			 distance_m, duration_s, surge_x100, prices, expires_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		id, riderID, testCity, 12.9716, 77.5946, 12.9352, 77.6245,
		9000, 1470, 100, pricesJSON, time.Now().UTC().Add(3*time.Minute), time.Now().UTC())
	if err != nil {
		e.t.Fatalf("insert quote: %v", err)
	}
	return id
}

// insertRide inserts a ride row directly in the given status with an optional
// assigned driver and otp hash, returning its id. Bypasses the service funnel so
// tests can construct arbitrary starting states.
func (e *env) insertRide(riderID, quoteID, tier, status string, driverID, otpHash *string) string {
	id := uuid.NewString()
	_, err := e.st.PG.Exec(e.ctx, `
		INSERT INTO rides
			(id, rider_id, driver_id, quote_id, tier, status,
			 pickup_lat, pickup_lng, drop_lat, drop_lng, payment_method, otp_hash, fare_total)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		id, riderID, driverID, quoteID, tier, status,
		12.9716, 77.5946, 12.9352, 77.6245, "upi", otpHash, nil)
	if err != nil {
		e.t.Fatalf("insert ride: %v", err)
	}
	e.trackRide(id)
	return id
}

func (e *env) driverStatus(id string) string {
	var s string
	if err := e.st.PG.QueryRow(e.ctx, `SELECT status FROM drivers WHERE id = $1`, id).Scan(&s); err != nil {
		e.t.Fatalf("read driver status: %v", err)
	}
	return s
}

func (e *env) rideStatus(id string) string {
	var s string
	if err := e.st.PG.QueryRow(e.ctx, `SELECT status FROM rides WHERE id = $1`, id).Scan(&s); err != nil {
		e.t.Fatalf("read ride status: %v", err)
	}
	return s
}

func (e *env) rideDriver(id string) *string {
	var d *string
	if err := e.st.PG.QueryRow(e.ctx, `SELECT driver_id::text FROM rides WHERE id = $1`, id).Scan(&d); err != nil {
		e.t.Fatalf("read ride driver: %v", err)
	}
	return d
}

func (e *env) count(query string, args ...any) int {
	var n int
	if err := e.st.PG.QueryRow(e.ctx, query, args...).Scan(&n); err != nil {
		e.t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func randDigits() string { return fmt.Sprintf("%09d", uuid.New().ID()%1_000_000_000) }
