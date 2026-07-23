package pricing

import "testing"

func TestValidTier(t *testing.T) {
	tests := []struct {
		tier string
		want bool
	}{
		{"mini", true},
		{"sedan", true},
		{"xl", true},
		{"luxury", false},
		{"", false},
		{"MINI", false}, // case-sensitive
	}
	for _, tt := range tests {
		t.Run(tt.tier, func(t *testing.T) {
			if got := ValidTier(tt.tier); got != tt.want {
				t.Errorf("ValidTier(%q) = %v, want %v", tt.tier, got, tt.want)
			}
		})
	}
}

func TestPrices(t *testing.T) {
	const distanceM, durationS, surgeX100 = 10000, 1800, 100

	got := Prices(distanceM, durationS, surgeX100)

	if len(got) != len(Tiers) {
		t.Fatalf("Prices() returned %d tiers, want %d", len(got), len(Tiers))
	}
	for tier, rates := range Tiers {
		want := Fare(rates, distanceM, durationS, surgeX100)
		if got[tier] != want {
			t.Errorf("Prices()[%q] = %d, want %d (matching Fare)", tier, got[tier], want)
		}
	}
	for _, tier := range TierOrder {
		if _, ok := got[tier]; !ok {
			t.Errorf("Prices() missing tier %q from TierOrder", tier)
		}
	}
}

func TestPricesSurgeScalesAllTiers(t *testing.T) {
	base := Prices(5000, 900, 100)
	surged := Prices(5000, 900, 200)
	for tier := range Tiers {
		if surged[tier] <= base[tier] {
			t.Errorf("tier %q: surged price %d should exceed base price %d", tier, surged[tier], base[tier])
		}
	}
}

func TestDemandKey(t *testing.T) {
	got := demandKey("BLR", "tdr1v", 123456)
	want := "surge:req:BLR:tdr1v:123456"
	if got != want {
		t.Fatalf("demandKey() = %q, want %q", got, want)
	}
}

func TestGeoKey(t *testing.T) {
	got := geoKey("BLR")
	want := "geo:drivers:BLR"
	if got != want {
		t.Fatalf("geoKey() = %q, want %q", got, want)
	}
}
