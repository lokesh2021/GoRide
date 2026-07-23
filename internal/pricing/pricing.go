// Package pricing holds GoRide's fare model: per-tier rate constants, the
// distance/duration estimate, per-tier fare computation, and surge pricing.
//
// Money is integer paise (INR). Distances are meters. Durations are seconds.
// All formulas follow docs/SPEC.md "Pricing".
package pricing

import "math"

// TierRates are the per-tier fare components, all in integer paise (INR).
type TierRates struct {
	BasePaise   int // flat base fare
	PerKmPaise  int // per kilometre
	PerMinPaise int // per minute
}

// Tiers holds the rate card. Plausible Bengaluru-like values.
var Tiers = map[string]TierRates{
	"mini":  {BasePaise: 3000, PerKmPaise: 1100, PerMinPaise: 150}, // ₹30 base, ₹11/km, ₹1.5/min
	"sedan": {BasePaise: 5000, PerKmPaise: 1500, PerMinPaise: 200}, // ₹50 base, ₹15/km, ₹2/min
	"xl":    {BasePaise: 8000, PerKmPaise: 2000, PerMinPaise: 300}, // ₹80 base, ₹20/km, ₹3/min
}

// TierOrder is the canonical display order.
var TierOrder = []string{"mini", "sedan", "xl"}

// ValidTier reports whether tier is one of the known tiers.
func ValidTier(tier string) bool {
	_, ok := Tiers[tier]
	return ok
}

const (
	// roadFactor inflates straight-line distance to an approximate road path.
	roadFactor = 1.3
	// citySpeedKmh is the assumed average city driving speed.
	citySpeedKmh = 22.0
	// earthRadiusM is the mean Earth radius in metres, for haversine.
	earthRadiusM = 6371000.0
)

// Haversine returns the great-circle distance in metres between two
// latitude/longitude points (degrees).
func Haversine(lat1, lng1, lat2, lng2 float64) float64 {
	rlat1 := lat1 * math.Pi / 180
	rlat2 := lat2 * math.Pi / 180
	dlat := (lat2 - lat1) * math.Pi / 180
	dlng := (lng2 - lng1) * math.Pi / 180

	a := math.Sin(dlat/2)*math.Sin(dlat/2) +
		math.Cos(rlat1)*math.Cos(rlat2)*math.Sin(dlng/2)*math.Sin(dlng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusM * c
}

// Estimate returns the estimated road distance (metres) and trip duration
// (seconds) for a pickup→drop leg: haversine × road factor, at city speed.
func Estimate(pickupLat, pickupLng, dropLat, dropLng float64) (distanceM, durationS int) {
	straight := Haversine(pickupLat, pickupLng, dropLat, dropLng)
	road := straight * roadFactor
	distanceM = int(math.Round(road))

	speedMS := citySpeedKmh * 1000 / 3600 // km/h → m/s
	durationS = int(math.Round(road / speedMS))
	return distanceM, durationS
}

// Fare computes the paise fare for one tier at a given surge multiplier
// (expressed as an integer ×100, e.g. 120 = 1.2×). The result is rounded to
// the nearest whole rupee (100 paise), per SPEC.
func Fare(rates TierRates, distanceM, durationS, surgeX100 int) int {
	km := float64(distanceM) / 1000.0
	mins := float64(durationS) / 60.0

	base := float64(rates.BasePaise) + km*float64(rates.PerKmPaise) + mins*float64(rates.PerMinPaise)
	surged := base * float64(surgeX100) / 100.0

	rupees := math.Round(surged / 100.0)
	return int(rupees) * 100
}

// Breakdown is the itemised final-fare breakdown written to the trip row and
// copied (immutably) into the receipt. The base/distance/time components are
// pre-surge paise; SurgeX100 is the quoted multiplier (×100) applied to their
// sum; Total is the final fare rounded to the nearest rupee.
type Breakdown struct {
	Base              int `json:"base"`
	DistanceComponent int `json:"distance_component"`
	TimeComponent     int `json:"time_component"`
	SurgeX100         int `json:"surge_x100"`
	Total             int `json:"total"`
}

// FinalFare computes the itemised actual fare on trip end: the pre-surge base,
// distance, and time components (each rounded to whole paise for display), the
// quoted surge multiplier, and the surge-applied total rounded to the nearest
// rupee. The surge and rates are always the ones locked at quote time — never a
// live re-read (SPEC "Final fare"). Total is derived from the rounded components
// so the receipt's line items always sum (× surge) back to the total.
func FinalFare(rates TierRates, distanceM, durationS, surgeX100 int) Breakdown {
	km := float64(distanceM) / 1000.0
	mins := float64(durationS) / 60.0

	base := rates.BasePaise
	dist := int(math.Round(km * float64(rates.PerKmPaise)))
	tm := int(math.Round(mins * float64(rates.PerMinPaise)))

	surged := float64(base+dist+tm) * float64(surgeX100) / 100.0
	total := int(math.Round(surged/100.0)) * 100

	return Breakdown{
		Base:              base,
		DistanceComponent: dist,
		TimeComponent:     tm,
		SurgeX100:         surgeX100,
		Total:             total,
	}
}

// Prices computes the per-tier fare map (tier → paise) for a leg and surge.
func Prices(distanceM, durationS, surgeX100 int) map[string]int {
	out := make(map[string]int, len(Tiers))
	for tier, rates := range Tiers {
		out[tier] = Fare(rates, distanceM, durationS, surgeX100)
	}
	return out
}

// Bucket maps a demand/supply ratio to a surge multiplier (×100), per SPEC:
// ratio <1 → 100, <2 → 120, <3 → 150, else 200. Zero (or negative) supply → 200.
func Bucket(demand, supply int) int {
	if supply <= 0 {
		return 200
	}
	ratio := float64(demand) / float64(supply)
	switch {
	case ratio < 1:
		return 100
	case ratio < 2:
		return 120
	case ratio < 3:
		return 150
	default:
		return 200
	}
}
