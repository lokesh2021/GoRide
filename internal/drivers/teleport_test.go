package drivers

import "testing"

// TestMeteredPingDelta checks the teleport filter: small inter-ping moves count
// toward metered distance; jumps beyond maxPingDeltaM (200m) are rejected as
// spurious GPS fixes.
func TestMeteredPingDelta(t *testing.T) {
	// A ~15m step in central Bengaluru (about 0.000135 deg latitude).
	const baseLat, baseLng = 12.9716, 77.5946

	tests := []struct {
		name         string
		lat, lng     float64
		wantCount    bool
		wantAtLeastM float64
		wantAtMostM  float64
	}{
		{
			name: "tiny step counts", lat: baseLat + 0.0001, lng: baseLng,
			wantCount: true, wantAtLeastM: 5, wantAtMostM: 30,
		},
		{
			name: "no movement counts as zero", lat: baseLat, lng: baseLng,
			wantCount: true, wantAtLeastM: 0, wantAtMostM: 0.001,
		},
		{
			name: "teleport rejected", lat: baseLat + 0.02, lng: baseLng + 0.02, // ~3km
			wantCount: false,
		},
		{
			name: "just under threshold counts", lat: baseLat + 0.00170, lng: baseLng, // ~189m
			wantCount: true, wantAtLeastM: 150, wantAtMostM: 200,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, count := meteredPingDelta(baseLat, baseLng, tc.lat, tc.lng)
			if count != tc.wantCount {
				t.Fatalf("count = %v, want %v (delta=%.2fm, threshold=%.0fm)", count, tc.wantCount, d, maxPingDeltaM)
			}
			if tc.wantCount {
				if d < tc.wantAtLeastM || d > tc.wantAtMostM {
					t.Fatalf("delta %.2fm outside expected [%.2f, %.2f]", d, tc.wantAtLeastM, tc.wantAtMostM)
				}
			}
		})
	}
}
