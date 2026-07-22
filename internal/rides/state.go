package rides

import (
	"errors"
	"fmt"
)

// Status is a ride lifecycle state.
type Status string

// Ride statuses (SPEC "State machines").
const (
	StatusRequested       Status = "REQUESTED"
	StatusMatching        Status = "MATCHING"
	StatusDriverAssigned  Status = "DRIVER_ASSIGNED"
	StatusDriverArriving  Status = "DRIVER_ARRIVING"
	StatusArrived         Status = "ARRIVED"
	StatusInProgress      Status = "IN_PROGRESS"
	StatusCompleted       Status = "COMPLETED"
	StatusCancelledRider  Status = "CANCELLED_BY_RIDER"
	StatusCancelledDriver Status = "CANCELLED_BY_DRIVER"
	StatusExpired         Status = "EXPIRED"
)

// ErrInvalidTransition is returned by Transition for a disallowed edge.
var ErrInvalidTransition = errors.New("rides: invalid transition")

// transitions is the authoritative state-machine table. A destination present
// in the inner set is a legal move from the outer key.
//
// Cancellation (CANCELLED_BY_RIDER / CANCELLED_BY_DRIVER) is legal only from
// pre-IN_PROGRESS states. EXPIRED is legal only from MATCHING. Terminal states
// (COMPLETED, CANCELLED_*, EXPIRED) have no outgoing edges.
var transitions = map[Status]map[Status]bool{
	StatusRequested: {
		StatusMatching:        true,
		StatusCancelledRider:  true,
		StatusCancelledDriver: true,
	},
	StatusMatching: {
		StatusDriverAssigned:  true,
		StatusExpired:         true,
		StatusCancelledRider:  true,
		StatusCancelledDriver: true,
	},
	StatusDriverAssigned: {
		StatusDriverArriving:  true,
		StatusCancelledRider:  true,
		StatusCancelledDriver: true,
	},
	StatusDriverArriving: {
		StatusArrived:         true,
		StatusCancelledRider:  true,
		StatusCancelledDriver: true,
	},
	StatusArrived: {
		StatusInProgress:      true,
		StatusCancelledRider:  true,
		StatusCancelledDriver: true,
	},
	StatusInProgress: {
		StatusCompleted: true,
	},
}

// Transition reports whether moving from → to is legal, returning
// ErrInvalidTransition (wrapped) otherwise.
func Transition(from, to Status) error {
	if transitions[from][to] {
		return nil
	}
	return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
}

// sourcesFor returns every status from which `to` is a legal transition,
// derived from the single-source-of-truth table above.
func sourcesFor(to Status) []Status {
	var out []Status
	for from, dests := range transitions {
		if dests[to] {
			out = append(out, from)
		}
	}
	return out
}
