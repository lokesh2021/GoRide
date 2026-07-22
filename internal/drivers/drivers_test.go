package drivers

import (
	"encoding/json"
	"testing"
)

// TestStatusMirrorRoundTrip verifies the driver:status:{id} JSON contract used
// by the matching search loop and the location hot path.
func TestStatusMirrorRoundTrip(t *testing.T) {
	in := statusMirror{Status: StatusAvailable, Tier: "sedan", City: "BLR"}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal generic: %v", err)
	}
	for _, f := range []string{"status", "tier", "city"} {
		if _, ok := generic[f]; !ok {
			t.Errorf("missing %q field in %s", f, raw)
		}
	}

	var out statusMirror
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

// TestLastPositionRoundTrip verifies the driver:last:{id} JSON contract.
func TestLastPositionRoundTrip(t *testing.T) {
	in := lastPosition{Lat: 12.9716, Lng: 77.5946, Ts: 1_700_000_000}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out lastPosition
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

// TestRateKeyPerSecond documents that rate-limit keys are per-driver,
// per-second (distinct seconds → distinct keys, so counters reset each second).
func TestRateKeyPerSecond(t *testing.T) {
	const id = "d1"
	if rateKey(id, 100) == rateKey(id, 101) {
		t.Fatal("rate keys for different seconds must differ")
	}
	if rateKey(id, 100) != rateKey(id, 100) {
		t.Fatal("rate key must be stable within a second")
	}
	if rateKey("d1", 100) == rateKey("d2", 100) {
		t.Fatal("rate keys for different drivers must differ")
	}
}
