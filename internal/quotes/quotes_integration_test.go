//go:build integration

package quotes_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lokeshbm/goride/internal/pricing"
	"github.com/lokeshbm/goride/internal/quotes"
	"github.com/lokeshbm/goride/internal/testsupport"
)

const (
	pickupLat, pickupLng = 12.9716, 77.5946
	dropLat, dropLng     = 12.9352, 77.6245
)

func TestCreateInsertsQuoteWithComputedPricing(t *testing.T) {
	f := testsupport.New(t)
	riderID, _ := f.InsertRider()

	before := time.Now().UTC()
	q, err := f.Quotes.Create(f.Ctx, riderID,
		quotes.Coord{Lat: pickupLat, Lng: pickupLng},
		quotes.Coord{Lat: dropLat, Lng: dropLng},
		testsupport.City)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if q.ID == "" {
		t.Error("Create: empty quote ID")
	}
	if q.RiderID != riderID {
		t.Errorf("RiderID = %q, want %q", q.RiderID, riderID)
	}
	if q.City != testsupport.City {
		t.Errorf("City = %q, want %q", q.City, testsupport.City)
	}
	if q.Pickup != (quotes.Coord{Lat: pickupLat, Lng: pickupLng}) {
		t.Errorf("Pickup = %+v, want %+v", q.Pickup, quotes.Coord{Lat: pickupLat, Lng: pickupLng})
	}
	if q.Drop != (quotes.Coord{Lat: dropLat, Lng: dropLng}) {
		t.Errorf("Drop = %+v, want %+v", q.Drop, quotes.Coord{Lat: dropLat, Lng: dropLng})
	}

	wantDistanceM, wantDurationS := pricing.Estimate(pickupLat, pickupLng, dropLat, dropLng)
	if q.DistanceM != wantDistanceM {
		t.Errorf("DistanceM = %d, want %d", q.DistanceM, wantDistanceM)
	}
	if q.DurationS != wantDurationS {
		t.Errorf("DurationS = %d, want %d", q.DurationS, wantDurationS)
	}

	switch q.SurgeX100 {
	case 100, 120, 150, 200:
		// valid bucket
	default:
		t.Errorf("SurgeX100 = %d, not a valid surge bucket", q.SurgeX100)
	}

	wantPrices := pricing.Prices(wantDistanceM, wantDurationS, q.SurgeX100)
	if len(q.Prices) != len(pricing.Tiers) {
		t.Fatalf("Prices has %d tiers, want %d", len(q.Prices), len(pricing.Tiers))
	}
	for tier, want := range wantPrices {
		got, ok := q.Prices[tier]
		if !ok {
			t.Errorf("Prices missing tier %q", tier)
			continue
		}
		if got != want {
			t.Errorf("Prices[%q] = %d, want %d", tier, got, want)
		}
	}

	// expires_at is now+3m; created_at is now, both UTC.
	if q.CreatedAt.Before(before) || q.CreatedAt.After(time.Now().UTC().Add(2*time.Second)) {
		t.Errorf("CreatedAt = %v, want within a couple seconds of %v", q.CreatedAt, before)
	}
	wantExpiry := q.CreatedAt.Add(3 * time.Minute)
	if diff := q.ExpiresAt.Sub(wantExpiry); diff < -time.Second || diff > time.Second {
		t.Errorf("ExpiresAt = %v, want ~%v (created_at + 3m)", q.ExpiresAt, wantExpiry)
	}

	// Row is actually persisted in Postgres.
	if n := f.Count(`SELECT COUNT(*) FROM quotes WHERE id = $1 AND rider_id = $2`, q.ID, riderID); n != 1 {
		t.Errorf("quotes row count for %s = %d, want 1", q.ID, n)
	}
}

func TestCreateIncrementsDemandCounter(t *testing.T) {
	f := testsupport.New(t)
	riderID, _ := f.InsertRider()

	now := time.Now().UTC()
	_, err := f.Quotes.Create(f.Ctx, riderID,
		quotes.Coord{Lat: pickupLat, Lng: pickupLng},
		quotes.Coord{Lat: dropLat, Lng: dropLng},
		testsupport.City)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cell := pricing.Geohash(pickupLat, pickupLng, pricing.CellPrecision)
	key := fmt.Sprintf("surge:req:%s:%s:%d", testsupport.City, cell, now.Unix()/60)
	f.TrackRedisKey(key)

	count, err := f.Store.Redis.Get(f.Ctx, key).Int()
	if err != nil {
		t.Fatalf("read demand key %q: %v", key, err)
	}
	if count < 1 {
		t.Errorf("demand counter for %q = %d, want >= 1", key, count)
	}
}

func TestGetReturnsPersistedQuote(t *testing.T) {
	f := testsupport.New(t)
	riderID, _ := f.InsertRider()

	created, err := f.Quotes.Create(f.Ctx, riderID,
		quotes.Coord{Lat: pickupLat, Lng: pickupLng},
		quotes.Coord{Lat: dropLat, Lng: dropLng},
		testsupport.City)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := f.Quotes.Get(f.Ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ID != created.ID || got.RiderID != created.RiderID || got.City != created.City {
		t.Errorf("Get returned %+v, want fields matching %+v", got, created)
	}
	if got.Pickup != created.Pickup || got.Drop != created.Drop {
		t.Errorf("Get coords = pickup:%+v drop:%+v, want pickup:%+v drop:%+v",
			got.Pickup, got.Drop, created.Pickup, created.Drop)
	}
	if got.DistanceM != created.DistanceM || got.DurationS != created.DurationS {
		t.Errorf("Get distance/duration = %d/%d, want %d/%d",
			got.DistanceM, got.DurationS, created.DistanceM, created.DurationS)
	}
	if got.SurgeX100 != created.SurgeX100 {
		t.Errorf("Get SurgeX100 = %d, want %d", got.SurgeX100, created.SurgeX100)
	}
	if len(got.Prices) != len(created.Prices) {
		t.Fatalf("Get Prices has %d entries, want %d", len(got.Prices), len(created.Prices))
	}
	for tier, want := range created.Prices {
		if got.Prices[tier] != want {
			t.Errorf("Get Prices[%q] = %d, want %d", tier, got.Prices[tier], want)
		}
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("Get CreatedAt = %v, want %v", got.CreatedAt, created.CreatedAt)
	}
	if !got.ExpiresAt.Equal(created.ExpiresAt) {
		t.Errorf("Get ExpiresAt = %v, want %v", got.ExpiresAt, created.ExpiresAt)
	}
}

func TestGetNotFound(t *testing.T) {
	f := testsupport.New(t)

	_, err := f.Quotes.Get(f.Ctx, "00000000-0000-0000-0000-000000000999")
	if !errors.Is(err, quotes.ErrNotFound) {
		t.Fatalf("Get(unknown id) error = %v, want ErrNotFound", err)
	}
}

// TestGetMalformedIDReturnsWrappedError exercises Get's generic (non
// ErrNoRows) error branch: an id that isn't valid uuid input makes Postgres
// reject the query outright rather than simply matching zero rows.
func TestGetMalformedIDReturnsWrappedError(t *testing.T) {
	f := testsupport.New(t)

	_, err := f.Quotes.Get(f.Ctx, "not-a-uuid")
	if err == nil {
		t.Fatal("Get(malformed id): want error, got nil")
	}
	if errors.Is(err, quotes.ErrNotFound) {
		t.Fatalf("Get(malformed id) error = %v, want a generic select error, not ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "quotes: select") {
		t.Errorf("Get(malformed id) error = %q, want it to mention quotes: select", err.Error())
	}
}

// TestCreateFailsForUnknownRider exercises Create's insert-error branch: the
// quotes table's rider_id column has a foreign key to riders(id), so a rider
// that was never inserted makes the INSERT itself fail.
func TestCreateFailsForUnknownRider(t *testing.T) {
	f := testsupport.New(t)
	unknownRiderID := uuid.NewString()

	_, err := f.Quotes.Create(f.Ctx, unknownRiderID,
		quotes.Coord{Lat: pickupLat, Lng: pickupLng},
		quotes.Coord{Lat: dropLat, Lng: dropLng},
		testsupport.City)
	if err == nil {
		t.Fatal("Create with unknown rider_id: want error, got nil")
	}
	if !strings.Contains(err.Error(), "quotes: insert") {
		t.Errorf("Create error = %q, want it to mention quotes: insert", err.Error())
	}
}
