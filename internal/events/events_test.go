package events

import (
	"encoding/json"
	"testing"
)

// TestEnvelopeShape asserts the marshaled envelope carries exactly the SPEC
// fields (type/ride_id/data/ts) under their contracted JSON names.
func TestEnvelopeShape(t *testing.T) {
	env := Envelope{
		Type:   "ride.status_changed",
		RideID: "00000000-0000-0000-0000-000000000001",
		Data:   map[string]any{"status": "DRIVER_ASSIGNED"},
		Ts:     "2026-07-22T10:00:00Z",
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal generic: %v", err)
	}
	for _, field := range []string{"type", "ride_id", "data", "ts"} {
		if _, ok := generic[field]; !ok {
			t.Errorf("missing %q field in %s", field, raw)
		}
	}
	if generic["type"] != "ride.status_changed" {
		t.Errorf("type = %v, want ride.status_changed", generic["type"])
	}
	if generic["ride_id"] != env.RideID {
		t.Errorf("ride_id = %v, want %v", generic["ride_id"], env.RideID)
	}
	if generic["ts"] != env.Ts {
		t.Errorf("ts = %v, want %v", generic["ts"], env.Ts)
	}
	data, ok := generic["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %v, want an object", generic["data"])
	}
	if data["status"] != "DRIVER_ASSIGNED" {
		t.Errorf("data.status = %v, want DRIVER_ASSIGNED", data["status"])
	}
}

// TestEnvelopeDriverEventRideIDEmpty documents that driver-channel envelopes
// carry an empty top-level ride_id (see the Envelope doc) while still
// including the field per the SPEC contract.
func TestEnvelopeDriverEventRideIDEmpty(t *testing.T) {
	env := Envelope{Type: "ride.offer", Data: map[string]any{"ride_id": "r1"}, Ts: nowRFC3339()}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := generic["ride_id"]; !ok {
		t.Fatalf("ride_id field missing entirely: %s", raw)
	}
	if generic["ride_id"] != "" {
		t.Errorf("ride_id = %v, want empty string", generic["ride_id"])
	}
}

// TestRideDriverChannelKeys pins the Redis key contract (SPEC "Redis key
// contract"): events:ride:{id} / events:driver:{id}.
func TestRideDriverChannelKeys(t *testing.T) {
	if got := RideChannel("abc"); got != "events:ride:abc" {
		t.Errorf("RideChannel = %q, want events:ride:abc", got)
	}
	if got := DriverChannel("xyz"); got != "events:driver:xyz" {
		t.Errorf("DriverChannel = %q, want events:driver:xyz", got)
	}
}
