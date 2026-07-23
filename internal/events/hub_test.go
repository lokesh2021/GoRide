package events

import (
	"strings"
	"testing"
)

// TestFormatFrame checks the SSE wire format: an `event:` line, a `data:`
// line carrying the payload verbatim, and the trailing blank line that
// terminates the frame.
func TestFormatFrame(t *testing.T) {
	payload := `{"type":"ride.offer","ride_id":"","data":{"ride_id":"r1"},"ts":"2026-07-22T10:00:00Z"}`
	got := FormatFrame("ride.offer", payload)
	want := "event: ride.offer\ndata: " + payload + "\n\n"
	if got != want {
		t.Fatalf("FormatFrame =\n%q\nwant\n%q", got, want)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Errorf("frame does not end with a trailing blank line: %q", got)
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("frame has %d lines before the trailing blank, want 2: %v", len(lines), lines)
	}
	if lines[0] != "event: ride.offer" {
		t.Errorf("first line = %q, want %q", lines[0], "event: ride.offer")
	}
	if lines[1] != "data: "+payload {
		t.Errorf("second line = %q, want %q", lines[1], "data: "+payload)
	}
}

// TestFormatFrameEmptyType covers the default-event case (unparseable/typeless
// payload): the event: line is still present but empty.
func TestFormatFrameEmptyType(t *testing.T) {
	got := FormatFrame("", "not-json")
	want := "event: \ndata: not-json\n\n"
	if got != want {
		t.Fatalf("FormatFrame = %q, want %q", got, want)
	}
}

// TestEventType covers the best-effort "type" field extraction used to pick
// the SSE event: line.
func TestEventType(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{"well formed", `{"type":"ride.status_changed","data":{}}`, "ride.status_changed"},
		{"missing type", `{"data":{}}`, ""},
		{"not json", `garbage`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventType(tc.payload); got != tc.want {
				t.Errorf("eventType(%q) = %q, want %q", tc.payload, got, tc.want)
			}
		})
	}
}
