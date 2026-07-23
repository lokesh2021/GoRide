package httpapi

import (
	"testing"

	"github.com/lokeshbm/goride/internal/rides"
)

// TestCanStreamRide is the pure authz decision for GET /v1/events?ride_id=...:
// the ride's rider or its currently assigned driver may stream; any other
// rider, or a driver not currently assigned, may not.
func TestCanStreamRide(t *testing.T) {
	driverID := "driver-1"
	otherDriverID := "driver-2"
	view := &rides.View{
		RiderID:  "rider-1",
		DriverID: &driverID,
	}

	tests := []struct {
		name  string
		actor Actor
		want  bool
	}{
		{"the ride's rider", Actor{ID: "rider-1", Role: rides.RoleRider}, true},
		{"the assigned driver", Actor{ID: driverID, Role: rides.RoleDriver}, true},
		{"a different rider", Actor{ID: "rider-2", Role: rides.RoleRider}, false},
		{"a different, unassigned driver", Actor{ID: otherDriverID, Role: rides.RoleDriver}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := canStreamRide(view, tc.actor); got != tc.want {
				t.Errorf("canStreamRide(actor=%+v) = %v, want %v", tc.actor, got, tc.want)
			}
		})
	}
}

// TestCanStreamRideUnassigned covers a ride with no assigned driver yet: only
// the rider may stream, no driver can.
func TestCanStreamRideUnassigned(t *testing.T) {
	view := &rides.View{RiderID: "rider-1", DriverID: nil}

	if !canStreamRide(view, Actor{ID: "rider-1", Role: rides.RoleRider}) {
		t.Error("rider should be able to stream their own unmatched ride")
	}
	if canStreamRide(view, Actor{ID: "driver-1", Role: rides.RoleDriver}) {
		t.Error("no driver should be able to stream a ride with no assigned driver")
	}
}
