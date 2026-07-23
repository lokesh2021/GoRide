package pricing

import "testing"

// TestFinalFare checks the actual-fare finalization math with known answers,
// including paused-time-adjusted durations and the surge multiplier taken from
// the quote (never live).
func TestFinalFare(t *testing.T) {
	mini := Tiers["mini"]   // base 3000, /km 1100, /min 150 paise
	sedan := Tiers["sedan"] // base 5000, /km 1500, /min 200 paise

	tests := []struct {
		name      string
		rates     TierRates
		distanceM int
		durationS int
		surgeX100 int
		want      Breakdown
	}{
		{
			// mini, 10km, 30min, no surge.
			// base 3000; dist 10*1100=11000; time 30*150=4500.
			// subtotal 18500 *1.00 = 18500 -> round to rupee = 18500.
			name: "mini no surge round number", rates: mini,
			distanceM: 10000, durationS: 1800, surgeX100: 100,
			want: Breakdown{Base: 3000, DistanceComponent: 11000, TimeComponent: 4500, SurgeX100: 100, Total: 18500},
		},
		{
			// mini, 10km, 30min, 1.5x surge.
			// subtotal 18500 * 1.5 = 27750 -> round(277.5)=278 rupees -> 27800.
			name: "mini surge 1.5x rounds up", rates: mini,
			distanceM: 10000, durationS: 1800, surgeX100: 150,
			want: Breakdown{Base: 3000, DistanceComponent: 11000, TimeComponent: 4500, SurgeX100: 150, Total: 27800},
		},
		{
			// sedan, 5.4km, 18min actual (after subtracting paused time), 1.2x.
			// dist round(5.4*1500)=8100; time round(18*200)=3600.
			// subtotal 5000+8100+3600=16700 *1.2 = 20040 -> round(200.4)=200 -> 20000.
			name: "sedan with paused-adjusted duration 1.2x", rates: sedan,
			distanceM: 5400, durationS: 1080, surgeX100: 120,
			want: Breakdown{Base: 5000, DistanceComponent: 8100, TimeComponent: 3600, SurgeX100: 120, Total: 20000},
		},
		{
			// zero distance & duration: only base, 2x surge.
			// subtotal 3000 *2 = 6000 -> 60 rupees -> 6000.
			name: "degenerate zero leg 2x", rates: mini,
			distanceM: 0, durationS: 0, surgeX100: 200,
			want: Breakdown{Base: 3000, DistanceComponent: 0, TimeComponent: 0, SurgeX100: 200, Total: 6000},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FinalFare(tc.rates, tc.distanceM, tc.durationS, tc.surgeX100)
			if got != tc.want {
				t.Fatalf("FinalFare(%+v, %d, %d, %d) = %+v, want %+v",
					tc.rates, tc.distanceM, tc.durationS, tc.surgeX100, got, tc.want)
			}
		})
	}
}

// TestFinalFarePausedTimeExcluded documents that paused time is excluded by the
// caller before FinalFare: two trips over the same distance but different net
// durations differ only in the time component.
func TestFinalFarePausedTimeExcluded(t *testing.T) {
	mini := Tiers["mini"]
	withPause := FinalFare(mini, 8000, 1200, 100)    // 20 min net
	withoutPause := FinalFare(mini, 8000, 1800, 100) // 30 min net
	if withPause.TimeComponent >= withoutPause.TimeComponent {
		t.Fatalf("expected paused-adjusted (shorter) duration to yield a smaller time component: %d vs %d",
			withPause.TimeComponent, withoutPause.TimeComponent)
	}
	// Distance component identical (same distance), only time differs.
	if withPause.DistanceComponent != withoutPause.DistanceComponent {
		t.Fatalf("distance component should be unaffected by duration: %d vs %d",
			withPause.DistanceComponent, withoutPause.DistanceComponent)
	}
}
