package pricing

import (
	"fmt"
	"time"
)

// ---- domain constants ----

const (
	// roadFactor inflates straight-line distance to an approximate road path.
	roadFactor = 1.3
	// citySpeedKmh is the assumed average city driving speed.
	citySpeedKmh = 22.0
	// earthRadiusM is the mean Earth radius in metres, for haversine.
	earthRadiusM = 6371000.0
)

// CellPrecision is the geohash precision used for surge demand cells.
const CellPrecision = 5

// surgeRadiusKm is the supply-search radius around pickup (SPEC: 3km).
const surgeRadiusKm = 3.0

// demandWindow is how many minute buckets of demand to sum (SPEC: last 5min).
const demandWindow = 5

// demandTTL matches the SPEC TTL for the demand counters.
const demandTTL = 5 * time.Minute

// base32 is the geohash alphabet (no a, i, l, o).
const base32 = "0123456789bcdefghjkmnpqrstuvwxyz"

// ---- Redis key prefixes/builders ----

// demandKey builds the SPEC key surge:req:{city}:{cell}:{minute}, where minute
// is the Unix epoch minute (Unix seconds / 60).
func demandKey(city, cell string, minute int64) string {
	return fmt.Sprintf("surge:req:%s:%s:%d", city, cell, minute)
}

// geoKey builds the per-city available-drivers GEO key.
func geoKey(city string) string {
	return "geo:drivers:" + city
}
