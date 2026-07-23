package rides

import (
	"context"
	"testing"
)

// TestNoopPublisher pins the default publisher's fire-and-forget contract: both
// methods discard and never error. Pure, so it runs in the untagged unit build.
func TestNoopPublisher(t *testing.T) {
	var p NoopPublisher
	if err := p.PublishRideEvent(context.Background(), "r1", "ride.status_changed", nil); err != nil {
		t.Errorf("PublishRideEvent = %v, want nil", err)
	}
	if err := p.PublishDriverEvent(context.Background(), "d1", "ride.offer", nil); err != nil {
		t.Errorf("PublishDriverEvent = %v, want nil", err)
	}
}
