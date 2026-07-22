package pricing

import "testing"

func TestFareKnownAnswers(t *testing.T) {
	// 10km, 30min leg keeps the arithmetic exact and easy to verify by hand.
	const distanceM, durationS = 10000, 1800

	tests := []struct {
		name      string
		tier      string
		surgeX100 int
		want      int // paise
	}{
		// mini: 3000 + 10*1100 + 30*150 = 18500
		{"mini x1.0", "mini", 100, 18500},
		// 18500 * 1.5 = 27750 → round to rupee (277.5 → 278) → 27800
		{"mini x1.5", "mini", 150, 27800},
		// 18500 * 2.0 = 37000
		{"mini x2.0", "mini", 200, 37000},
		// sedan: 5000 + 10*1500 + 30*200 = 26000
		{"sedan x1.0", "sedan", 100, 26000},
		// xl: 8000 + 10*2000 + 30*300 = 37000
		{"xl x1.0", "xl", 100, 37000},
		// xl * 1.2 = 44400
		{"xl x1.2", "xl", 120, 44400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Fare(Tiers[tt.tier], distanceM, durationS, tt.surgeX100)
			if got != tt.want {
				t.Fatalf("Fare(%s, %d, %d, %d) = %d, want %d",
					tt.tier, distanceM, durationS, tt.surgeX100, got, tt.want)
			}
			if got%100 != 0 {
				t.Fatalf("fare %d not rounded to whole rupee", got)
			}
		})
	}
}

func TestBucket(t *testing.T) {
	tests := []struct {
		demand, supply, want int
	}{
		{0, 5, 100},  // ratio 0.0 <1
		{4, 5, 100},  // ratio 0.8 <1
		{5, 5, 120},  // ratio 1.0 → <2
		{9, 5, 120},  // ratio 1.8 <2
		{12, 5, 150}, // ratio 2.4 <3
		{14, 5, 150}, // ratio 2.8 <3
		{15, 5, 200}, // ratio 3.0 → else
		{20, 5, 200}, // ratio 4.0
		{10, 0, 200}, // zero supply → 2.0
		{0, 0, 200},  // zero supply, zero demand → 2.0
	}
	for _, tt := range tests {
		if got := Bucket(tt.demand, tt.supply); got != tt.want {
			t.Errorf("Bucket(%d, %d) = %d, want %d", tt.demand, tt.supply, got, tt.want)
		}
	}
}

func TestEstimate(t *testing.T) {
	// Same point → zero distance and duration.
	d, s := Estimate(12.9716, 77.5946, 12.9716, 77.5946)
	if d != 0 || s != 0 {
		t.Fatalf("Estimate(same point) = (%d, %d), want (0, 0)", d, s)
	}

	// Bengaluru pickup→drop: sanity bounds (road factor applied, city speed).
	d, s = Estimate(12.9716, 77.5946, 12.9352, 77.6245)
	if d < 5000 || d > 8000 {
		t.Errorf("distance %d m out of expected range for the leg", d)
	}
	if s < 800 || s > 1400 {
		t.Errorf("duration %d s out of expected range for the leg", s)
	}
}

func TestHaversineZero(t *testing.T) {
	if got := Haversine(1, 2, 1, 2); got != 0 {
		t.Fatalf("Haversine(identical points) = %v, want 0", got)
	}
}
