//go:build integration

package pricing

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const defaultTestRedisAddr = "localhost:6379"

func testRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("GORIDE_REDIS_ADDR")
	if addr == "" {
		addr = defaultTestRedisAddr
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis unavailable at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// testCity returns a unique per-test city name so geo/demand keys never
// collide with the shared "BLR" data other suites (or a running server)
// might be touching concurrently.
func testCity(t *testing.T) string {
	return "TESTCITY-" + uuid.NewString()[:8]
}

func TestComputeSurgeNoSupplyNoDemand(t *testing.T) {
	rdb := testRedisClient(t)
	ctx := context.Background()
	city := testCity(t)
	now := time.Now().UTC()

	// No drivers ever added to this city's geo set, no demand recorded.
	got, err := ComputeSurge(ctx, rdb, city, 12.9716, 77.5946, now)
	if err != nil {
		t.Fatalf("ComputeSurge: %v", err)
	}
	if got != 200 {
		t.Fatalf("ComputeSurge with zero supply = %d, want 200", got)
	}
}

func TestComputeSurgeWithSupplyNoDemand(t *testing.T) {
	rdb := testRedisClient(t)
	ctx := context.Background()
	city := testCity(t)
	lat, lng := 12.9716, 77.5946

	geoSetKey := geoKey(city)
	if err := rdb.GeoAdd(ctx, geoSetKey, &redis.GeoLocation{Name: "driver-1", Longitude: lng, Latitude: lat}).Err(); err != nil {
		t.Fatalf("seed geo: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Del(context.Background(), geoSetKey).Err() })

	got, err := ComputeSurge(ctx, rdb, city, lat, lng, time.Now().UTC())
	if err != nil {
		t.Fatalf("ComputeSurge: %v", err)
	}
	// Supply=1, demand=0 -> ratio 0 -> Bucket 100.
	if got != 100 {
		t.Fatalf("ComputeSurge with supply, no demand = %d, want 100", got)
	}
}

func TestIncrementDemandThenComputeSurge(t *testing.T) {
	rdb := testRedisClient(t)
	ctx := context.Background()
	city := testCity(t)
	lat, lng := 12.9716, 77.5946
	now := time.Now().UTC()

	geoSetKey := geoKey(city)
	if err := rdb.GeoAdd(ctx, geoSetKey, &redis.GeoLocation{Name: "driver-1", Longitude: lng, Latitude: lat}).Err(); err != nil {
		t.Fatalf("seed geo: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Del(context.Background(), geoSetKey).Err() })

	// Record 5 demand events in the current minute bucket; supply is 1 driver
	// -> ratio 5.0 -> Bucket "else" branch -> 200.
	for i := 0; i < 5; i++ {
		if err := IncrementDemand(ctx, rdb, city, lat, lng, now); err != nil {
			t.Fatalf("IncrementDemand: %v", err)
		}
	}

	cell := Geohash(lat, lng, CellPrecision)
	key := demandKey(city, cell, now.Unix()/60)
	t.Cleanup(func() { _ = rdb.Del(context.Background(), key).Err() })

	count, err := rdb.Get(ctx, key).Int()
	if err != nil {
		t.Fatalf("read demand key %q: %v", key, err)
	}
	if count != 5 {
		t.Fatalf("demand counter = %d, want 5", count)
	}

	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl <= 0 || ttl > demandTTL {
		t.Fatalf("demand key TTL = %v, want (0, %v]", ttl, demandTTL)
	}

	got, err := ComputeSurge(ctx, rdb, city, lat, lng, now)
	if err != nil {
		t.Fatalf("ComputeSurge: %v", err)
	}
	if got != 200 {
		t.Fatalf("ComputeSurge with demand=5 supply=1 = %d, want 200", got)
	}
}

func TestComputeSurgeSumsWindowMinuteBuckets(t *testing.T) {
	rdb := testRedisClient(t)
	ctx := context.Background()
	city := testCity(t)
	lat, lng := 12.9716, 77.5946
	now := time.Now().UTC()

	geoSetKey := geoKey(city)
	// 5 drivers -> demand=4 (below) gives ratio <1.
	for i := 0; i < 5; i++ {
		name := "driver-" + string(rune('a'+i))
		if err := rdb.GeoAdd(ctx, geoSetKey, &redis.GeoLocation{Name: name, Longitude: lng, Latitude: lat}).Err(); err != nil {
			t.Fatalf("seed geo: %v", err)
		}
	}
	t.Cleanup(func() { _ = rdb.Del(context.Background(), geoSetKey).Err() })

	// Spread 2 demand events across the current minute and one minute ago;
	// ComputeSurge sums the whole demandWindow, so both should count.
	cell := Geohash(lat, lng, CellPrecision)
	minute := now.Unix() / 60
	keyNow := demandKey(city, cell, minute)
	keyPrev := demandKey(city, cell, minute-1)
	t.Cleanup(func() { _ = rdb.Del(context.Background(), keyNow, keyPrev).Err() })

	if err := rdb.Incr(ctx, keyNow).Err(); err != nil {
		t.Fatalf("incr now: %v", err)
	}
	if err := rdb.Incr(ctx, keyPrev).Err(); err != nil {
		t.Fatalf("incr prev: %v", err)
	}

	// demand=2, supply=5 -> ratio 0.4 <1 -> Bucket 100.
	got, err := ComputeSurge(ctx, rdb, city, lat, lng, now)
	if err != nil {
		t.Fatalf("ComputeSurge: %v", err)
	}
	if got != 100 {
		t.Fatalf("ComputeSurge summed-window = %d, want 100", got)
	}
}
