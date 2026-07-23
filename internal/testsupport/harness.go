//go:build integration

// Package testsupport provides a shared, live-infra test fixture for the
// package-local integration tests across internal/* (SPEC "Testing
// conventions"). It wires the real store + domain services + in-process HTTP
// server against a real Postgres + Redis (GORIDE_PG_DSN / GORIDE_REDIS_ADDR),
// and gives each test isolated entities (random UUIDs + tokens) with automatic
// FK-safe cleanup, so suites are safe to run repeatedly and leave the datastore
// clean.
//
// It is build-tagged `integration` so it never compiles into unit builds
// (`go test ./...`); the package-local tests that import it are likewise
// tagged. Mirrors the standalone harness in integration/ (kept separate so the
// established §5 concurrency suite is untouched).
package testsupport

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
	// PSPSecret is the shared HMAC secret the fixture wires into payments, so
	// webhook-signature tests can sign bodies with the same value.
	PSPSecret = "testsupport-secret"
	// City is the shard all fixture entities live in.
	City = "BLR"
)

// Fixture is the per-test bundle: live store, wired services, in-process server.
// Fields are exported so package tests call services directly or hit the HTTP
// server, whichever fits.
type Fixture struct {
	TB       testing.TB
	Ctx      context.Context
	Store    *store.Store
	Cfg      config.Config
	Quotes   *quotes.Service
	Rides    *rides.Service
	Drivers  *drivers.Service
	Match    *matching.Engine
	Trips    *trips.Service
	Payments *payments.Service
	PSP      *payments.PSP
	Events   *events.Publisher
	Server   *httptest.Server
	Log      *slog.Logger

	mu        sync.Mutex
	riderIDs  []string
	driverIDs []string
	rideIDs   []string
	redisKeys []string
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// New connects to Postgres + Redis and wires the full server exactly as
// cmd/server does (minus the sweeper, so tests fully control offer/assignment
// timing). Fails loudly if infra is unreachable.
func New(tb testing.TB) *Fixture {
	tb.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := config.Config{
		Addr:          ":0",
		PGDSN:         getenv("GORIDE_PG_DSN", defaultPGDSN),
		RedisAddr:     getenv("GORIDE_REDIS_ADDR", defaultRedisAddr),
		Env:           "test",
		PSPSecret:     PSPSecret,
		PSPWebhookURL: "http://127.0.0.1:1/unused",
	}

	ctx := context.Background()
	st, err := store.New(ctx, cfg)
	if err != nil {
		tb.Fatalf("infra unavailable (Postgres=%s Redis=%s): %v", cfg.PGDSN, cfg.RedisAddr, err)
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
		Health: st, Store: st, Quotes: quoteSvc, Rides: rideSvc,
		Drivers: driverSvc, Match: matchEngine, Trips: tripSvc,
		Payments: paymentSvc, Events: eventHub, Logger: log,
	})
	srv := httptest.NewServer(router)

	f := &Fixture{
		TB: tb, Ctx: ctx, Store: st, Cfg: cfg,
		Quotes: quoteSvc, Rides: rideSvc, Drivers: driverSvc, Match: matchEngine,
		Trips: tripSvc, Payments: paymentSvc, PSP: psp, Events: eventPub,
		Server: srv, Log: log,
	}
	tb.Cleanup(f.cleanup)
	return f
}

func (f *Fixture) cleanup() {
	ctx := context.Background()

	rideIDs := append([]string(nil), f.rideIDs...)
	if len(f.riderIDs) > 0 {
		rows, err := f.Store.PG.Query(ctx, `SELECT id::text FROM rides WHERE rider_id = ANY($1)`, f.riderIDs)
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

	keys := append([]string(nil), f.redisKeys...)
	for _, rid := range rideIDs {
		keys = append(keys, "ride:cache:"+rid, "offer:ride:"+rid, "offered:ride:"+rid,
			"trip:dist:"+rid, "trip:paused_at:"+rid)
	}
	for _, did := range f.driverIDs {
		keys = append(keys, "offer:driver:"+did, "driver:status:"+did,
			"driver:last:"+did, "driver:ride:"+did)
	}
	if len(keys) > 0 {
		_ = f.Store.Redis.Del(ctx, keys...).Err()
	}
	if len(f.driverIDs) > 0 {
		_ = f.Store.Redis.ZRem(ctx, "geo:drivers:"+City, toAny(f.driverIDs)...).Err()
	}

	if len(f.riderIDs) > 0 {
		for _, q := range []string{
			`DELETE FROM receipts WHERE ride_id IN (SELECT id FROM rides WHERE rider_id = ANY($1))`,
			`DELETE FROM payments WHERE ride_id IN (SELECT id FROM rides WHERE rider_id = ANY($1))`,
			`DELETE FROM trips    WHERE ride_id IN (SELECT id FROM rides WHERE rider_id = ANY($1))`,
			`DELETE FROM rides    WHERE rider_id = ANY($1)`,
			`DELETE FROM quotes   WHERE rider_id = ANY($1)`,
		} {
			if _, err := f.Store.PG.Exec(ctx, q, f.riderIDs); err != nil {
				f.TB.Logf("cleanup: %v", err)
			}
		}
	}
	actors := append(append([]string(nil), f.riderIDs...), f.driverIDs...)
	if len(actors) > 0 {
		_, _ = f.Store.PG.Exec(ctx, `DELETE FROM idempotency_keys WHERE actor_id = ANY($1)`, actors)
	}
	if len(f.driverIDs) > 0 {
		_, _ = f.Store.PG.Exec(ctx, `DELETE FROM drivers WHERE id = ANY($1)`, f.driverIDs)
	}
	if len(f.riderIDs) > 0 {
		_, _ = f.Store.PG.Exec(ctx, `DELETE FROM riders WHERE id = ANY($1)`, f.riderIDs)
	}

	f.Server.Close()
	f.Store.Close()
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// TrackRide/TrackRedisKey register ids/keys created out-of-band (e.g. via HTTP)
// so cleanup removes them.
func (f *Fixture) TrackRide(id string) {
	f.mu.Lock()
	f.rideIDs = append(f.rideIDs, id)
	f.mu.Unlock()
}

func (f *Fixture) TrackRedisKey(k string) {
	f.mu.Lock()
	f.redisKeys = append(f.redisKeys, k)
	f.mu.Unlock()
}

// InsertRider creates a rider, returns (id, token).
func (f *Fixture) InsertRider() (string, string) {
	id := uuid.NewString()
	token := "ts-rider-" + uuid.NewString()
	if _, err := f.Store.PG.Exec(f.Ctx,
		`INSERT INTO riders (id, name, phone, api_token) VALUES ($1,$2,$3,$4)`,
		id, "TS Rider", "+9199"+randDigits(), token); err != nil {
		f.TB.Fatalf("insert rider: %v", err)
	}
	f.mu.Lock()
	f.riderIDs = append(f.riderIDs, id)
	f.mu.Unlock()
	return id, token
}

// InsertDriver creates a driver with the given tier + status, returns (id, token).
func (f *Fixture) InsertDriver(tier, status string) (string, string) {
	id := uuid.NewString()
	token := "ts-driver-" + uuid.NewString()
	if _, err := f.Store.PG.Exec(f.Ctx,
		`INSERT INTO drivers (id, name, phone, city, tier, vehicle_model, plate, rating, status, api_token)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		id, "TS Driver", "+9198"+randDigits(), City, tier,
		"Test Model", "KA-98-"+randDigits(), 4.6, status, token); err != nil {
		f.TB.Fatalf("insert driver: %v", err)
	}
	f.mu.Lock()
	f.driverIDs = append(f.driverIDs, id)
	f.mu.Unlock()
	return id, token
}

// InsertQuote creates a non-expired all-tier quote for the rider, returns id.
func (f *Fixture) InsertQuote(riderID string) string {
	id := uuid.NewString()
	prices, _ := json.Marshal(map[string]int{"mini": 15000, "sedan": 22000, "xl": 30000})
	if _, err := f.Store.PG.Exec(f.Ctx, `
		INSERT INTO quotes (id, rider_id, city, pickup_lat, pickup_lng, drop_lat, drop_lng,
			distance_m, duration_s, surge_x100, prices, expires_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		id, riderID, City, 12.9716, 77.5946, 12.9352, 77.6245,
		9000, 1470, 100, prices, time.Now().UTC().Add(3*time.Minute), time.Now().UTC()); err != nil {
		f.TB.Fatalf("insert quote: %v", err)
	}
	return id
}

// InsertRide inserts a ride row directly in the given status (bypassing the
// service funnel) so tests can construct arbitrary starting states.
func (f *Fixture) InsertRide(riderID, quoteID, tier, status string, driverID, otpHash *string) string {
	id := uuid.NewString()
	if _, err := f.Store.PG.Exec(f.Ctx, `
		INSERT INTO rides (id, rider_id, driver_id, quote_id, tier, status,
			pickup_lat, pickup_lng, drop_lat, drop_lng, payment_method, otp_hash, fare_total)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		id, riderID, driverID, quoteID, tier, status,
		12.9716, 77.5946, 12.9352, 77.6245, "upi", otpHash, nil); err != nil {
		f.TB.Fatalf("insert ride: %v", err)
	}
	f.TrackRide(id)
	return id
}

// DriverStatus / RideStatus / RideDriver / Count are read helpers for assertions.
func (f *Fixture) DriverStatus(id string) string {
	var s string
	if err := f.Store.PG.QueryRow(f.Ctx, `SELECT status FROM drivers WHERE id = $1`, id).Scan(&s); err != nil {
		f.TB.Fatalf("read driver status: %v", err)
	}
	return s
}

func (f *Fixture) RideStatus(id string) string {
	var s string
	if err := f.Store.PG.QueryRow(f.Ctx, `SELECT status FROM rides WHERE id = $1`, id).Scan(&s); err != nil {
		f.TB.Fatalf("read ride status: %v", err)
	}
	return s
}

func (f *Fixture) RideDriver(id string) *string {
	var d *string
	if err := f.Store.PG.QueryRow(f.Ctx, `SELECT driver_id::text FROM rides WHERE id = $1`, id).Scan(&d); err != nil {
		f.TB.Fatalf("read ride driver: %v", err)
	}
	return d
}

func (f *Fixture) Count(query string, args ...any) int {
	var n int
	if err := f.Store.PG.QueryRow(f.Ctx, query, args...).Scan(&n); err != nil {
		f.TB.Fatalf("count %q: %v", query, err)
	}
	return n
}

func randDigits() string { return fmt.Sprintf("%09d", uuid.New().ID()%1_000_000_000) }
