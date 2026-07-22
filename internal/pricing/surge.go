package pricing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// CellPrecision is the geohash precision used for surge demand cells.
const CellPrecision = 5

// surgeRadiusKm is the supply-search radius around pickup (SPEC: 3km).
const surgeRadiusKm = 3.0

// demandWindow is how many minute buckets of demand to sum (SPEC: last 5min).
const demandWindow = 5

// demandTTL matches the SPEC TTL for the demand counters.
const demandTTL = 5 * time.Minute

// demandKey builds the SPEC key surge:req:{city}:{cell}:{minute}, where minute
// is the Unix epoch minute (Unix seconds / 60).
func demandKey(city, cell string, minute int64) string {
	return fmt.Sprintf("surge:req:%s:%s:%d", city, cell, minute)
}

// geoKey builds the per-city available-drivers GEO key.
func geoKey(city string) string {
	return "geo:drivers:" + city
}

// ComputeSurge reads current surge (×100) for a pickup point: demand is the sum
// of the last demandWindow minute buckets for the pickup's geohash cell; supply
// is the count of available drivers within surgeRadiusKm via GEOSEARCH. The
// ratio is bucketed per SPEC.
func ComputeSurge(ctx context.Context, rdb redis.Cmdable, city string, pickupLat, pickupLng float64, now time.Time) (int, error) {
	cell := Geohash(pickupLat, pickupLng, CellPrecision)
	minute := now.Unix() / 60

	demand := 0
	for i := 0; i < demandWindow; i++ {
		key := demandKey(city, cell, minute-int64(i))
		v, err := rdb.Get(ctx, key).Int()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("pricing: read demand: %w", err)
		}
		demand += v
	}

	res, err := rdb.GeoSearch(ctx, geoKey(city), &redis.GeoSearchQuery{
		Longitude:  pickupLng,
		Latitude:   pickupLat,
		Radius:     surgeRadiusKm,
		RadiusUnit: "km",
		Sort:       "ASC",
	}).Result()
	if err != nil {
		return 0, fmt.Errorf("pricing: geosearch supply: %w", err)
	}

	return Bucket(demand, len(res)), nil
}

// IncrementDemand bumps the demand counter for the pickup cell's current minute
// bucket and (re)sets its TTL. Called at quote time.
func IncrementDemand(ctx context.Context, rdb redis.Cmdable, city string, pickupLat, pickupLng float64, now time.Time) error {
	cell := Geohash(pickupLat, pickupLng, CellPrecision)
	key := demandKey(city, cell, now.Unix()/60)

	pipe := rdb.Pipeline()
	pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, demandTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("pricing: increment demand: %w", err)
	}
	return nil
}
