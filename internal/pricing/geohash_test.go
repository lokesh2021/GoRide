package pricing

import "testing"

func TestGeohashKnownVectors(t *testing.T) {
	tests := []struct {
		name      string
		lat, lng  float64
		precision int
		want      string
	}{
		// Classic reference vector from the geohash literature.
		{"jutland p12", 57.64911, 10.40744, 12, "u4pruydqqvj8"},
		{"jutland p5", 57.64911, 10.40744, 5, "u4pru"},
		// San Francisco.
		{"sf p5", 37.7749, -122.4194, 5, "9q8yy"},
		// Null island.
		{"origin p5", 0, 0, 5, "s0000"},
		// Bengaluru (used across the demo).
		{"blr p5", 12.9716, 77.5946, 5, "tdr1v"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Geohash(tt.lat, tt.lng, tt.precision); got != tt.want {
				t.Fatalf("Geohash(%v, %v, %d) = %q, want %q",
					tt.lat, tt.lng, tt.precision, got, tt.want)
			}
		})
	}
}

func TestGeohashPrefixProperty(t *testing.T) {
	// A shorter geohash must be a prefix of a longer one for the same point.
	full := Geohash(12.9716, 77.5946, 8)
	short := Geohash(12.9716, 77.5946, 5)
	if full[:5] != short {
		t.Fatalf("precision-5 %q is not a prefix of precision-8 %q", short, full)
	}
}
