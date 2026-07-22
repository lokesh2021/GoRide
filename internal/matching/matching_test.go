package matching

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/lokeshbm/goride/internal/drivers"
)

// TestCandidateEligible covers the pure candidate filter: a driver is eligible
// only when available, tier-matched, and fresh.
func TestCandidateEligible(t *testing.T) {
	tests := []struct {
		name     string
		mirror   driverStatus
		fresh    bool
		wantTier string
		want     bool
	}{
		{
			name:     "available, tier match, fresh",
			mirror:   driverStatus{Status: drivers.StatusAvailable, Tier: "mini", City: "BLR"},
			fresh:    true,
			wantTier: "mini",
			want:     true,
		},
		{
			name:     "tier mismatch",
			mirror:   driverStatus{Status: drivers.StatusAvailable, Tier: "sedan", City: "BLR"},
			fresh:    true,
			wantTier: "mini",
			want:     false,
		},
		{
			name:     "stale last position",
			mirror:   driverStatus{Status: drivers.StatusAvailable, Tier: "mini"},
			fresh:    false,
			wantTier: "mini",
			want:     false,
		},
		{
			name:     "on trip",
			mirror:   driverStatus{Status: drivers.StatusOnTrip, Tier: "mini"},
			fresh:    true,
			wantTier: "mini",
			want:     false,
		},
		{
			name:     "offline",
			mirror:   driverStatus{Status: drivers.StatusOffline, Tier: "mini"},
			fresh:    true,
			wantTier: "mini",
			want:     false,
		},
		{
			name:     "empty mirror (no status key)",
			mirror:   driverStatus{},
			fresh:    true,
			wantTier: "mini",
			want:     false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := candidateEligible(tc.mirror, tc.fresh, tc.wantTier); got != tc.want {
				t.Fatalf("candidateEligible = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOfferStateRoundTrip verifies the offer:ride JSON contract survives a
// marshal/unmarshal cycle with the expected field names.
func TestOfferStateRoundTrip(t *testing.T) {
	in := offerState{DriverID: "00000000-0000-0000-0000-000000000011", ExpiresAt: 1_700_000_012}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Field names are part of the Redis key contract; assert them explicitly.
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal generic: %v", err)
	}
	if _, ok := generic["driver_id"]; !ok {
		t.Errorf("missing driver_id field in %s", raw)
	}
	if _, ok := generic["expires_at"]; !ok {
		t.Errorf("missing expires_at field in %s", raw)
	}

	var out offerState
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

// TestGenOTPFormat verifies OTPs are always exactly 4 decimal digits.
func TestGenOTPFormat(t *testing.T) {
	re := regexp.MustCompile(`^[0-9]{4}$`)
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		otp, err := genOTP()
		if err != nil {
			t.Fatalf("genOTP error: %v", err)
		}
		if !re.MatchString(otp) {
			t.Fatalf("otp %q is not 4 digits", otp)
		}
		seen[otp] = true
	}
	// crypto/rand should produce good spread; require a healthy number of
	// distinct values over 2000 draws (guards against a constant/broken source).
	if len(seen) < 500 {
		t.Fatalf("otp entropy too low: only %d distinct values in 2000 draws", len(seen))
	}
}

// TestOfferMatches covers accept-ownership semantics: the consumed offer value
// must equal the ride being accepted, and an empty value (no offer held) never
// matches.
func TestOfferMatches(t *testing.T) {
	const ride = "ride-abc"
	tests := []struct {
		name string
		held string
		want bool
	}{
		{"holds the same ride", ride, true},
		{"holds a different ride", "ride-xyz", false},
		{"holds no offer", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := offerMatches(tc.held, ride); got != tc.want {
				t.Fatalf("offerMatches(%q, %q) = %v, want %v", tc.held, ride, got, tc.want)
			}
		})
	}
}

// TestAssignedOrLater documents which statuses count as "already assigned" for
// accept replay detection.
func TestAssignedOrLater(t *testing.T) {
	assigned := []string{"DRIVER_ASSIGNED", "DRIVER_ARRIVING", "ARRIVED", "IN_PROGRESS", "COMPLETED"}
	for _, s := range assigned {
		if !assignedOrLater(s) {
			t.Errorf("assignedOrLater(%q) = false, want true", s)
		}
	}
	notAssigned := []string{"REQUESTED", "MATCHING", "CANCELLED_BY_RIDER", "EXPIRED", ""}
	for _, s := range notAssigned {
		if assignedOrLater(s) {
			t.Errorf("assignedOrLater(%q) = true, want false", s)
		}
	}
}
