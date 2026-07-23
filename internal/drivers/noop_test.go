package drivers

import (
	"context"
	"testing"
)

// TestNoopPublisher confirms the default (unwired) ride publisher is a safe
// no-op: it swallows events and never errors, so the location hot path is
// correct before the SSE hub is attached.
func TestNoopPublisher(t *testing.T) {
	if err := (noopPublisher{}).PublishRideEvent(context.Background(), "ride-1", "ride.driver_location", nil); err != nil {
		t.Fatalf("noopPublisher.PublishRideEvent = %v, want nil", err)
	}
}
