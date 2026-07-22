package rides

import (
	"errors"
	"testing"
)

func TestTransitionAllowed(t *testing.T) {
	allowed := []struct{ from, to Status }{
		{StatusRequested, StatusMatching},
		{StatusMatching, StatusDriverAssigned},
		{StatusDriverAssigned, StatusDriverArriving},
		{StatusDriverArriving, StatusArrived},
		{StatusArrived, StatusInProgress},
		{StatusInProgress, StatusCompleted},
		// EXPIRED only from MATCHING.
		{StatusMatching, StatusExpired},
		// Cancellation legal from every pre-IN_PROGRESS state.
		{StatusRequested, StatusCancelledRider},
		{StatusMatching, StatusCancelledRider},
		{StatusDriverAssigned, StatusCancelledRider},
		{StatusDriverArriving, StatusCancelledRider},
		{StatusArrived, StatusCancelledRider},
		{StatusRequested, StatusCancelledDriver},
		{StatusArrived, StatusCancelledDriver},
	}
	for _, tc := range allowed {
		if err := Transition(tc.from, tc.to); err != nil {
			t.Errorf("Transition(%s, %s) = %v, want nil", tc.from, tc.to, err)
		}
	}
}

func TestTransitionRejected(t *testing.T) {
	rejected := []struct{ from, to Status }{
		// Cannot skip states.
		{StatusRequested, StatusDriverAssigned},
		{StatusMatching, StatusArrived},
		{StatusDriverAssigned, StatusInProgress},
		// Cannot cancel once in progress or later.
		{StatusInProgress, StatusCancelledRider},
		{StatusInProgress, StatusCancelledDriver},
		{StatusCompleted, StatusCancelledRider},
		// EXPIRED only from MATCHING.
		{StatusRequested, StatusExpired},
		{StatusDriverAssigned, StatusExpired},
		{StatusArrived, StatusExpired},
		// Terminal states have no outgoing edges.
		{StatusCompleted, StatusInProgress},
		{StatusCancelledRider, StatusMatching},
		{StatusCancelledDriver, StatusRequested},
		{StatusExpired, StatusMatching},
		// No self-loops.
		{StatusMatching, StatusMatching},
		// No backward moves.
		{StatusArrived, StatusDriverArriving},
	}
	for _, tc := range rejected {
		err := Transition(tc.from, tc.to)
		if err == nil {
			t.Errorf("Transition(%s, %s) = nil, want error", tc.from, tc.to)
			continue
		}
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("Transition(%s, %s) error = %v, want ErrInvalidTransition", tc.from, tc.to, err)
		}
	}
}

func TestSourcesForCancellation(t *testing.T) {
	got := sourcesFor(StatusCancelledRider)
	want := map[Status]bool{
		StatusRequested:      true,
		StatusMatching:       true,
		StatusDriverAssigned: true,
		StatusDriverArriving: true,
		StatusArrived:        true,
	}
	if len(got) != len(want) {
		t.Fatalf("sourcesFor(CANCELLED_BY_RIDER) = %v, want %d states", got, len(want))
	}
	for _, s := range got {
		if !want[s] {
			t.Errorf("unexpected cancel source %s", s)
		}
	}
}

func TestSourcesForExpired(t *testing.T) {
	got := sourcesFor(StatusExpired)
	if len(got) != 1 || got[0] != StatusMatching {
		t.Fatalf("sourcesFor(EXPIRED) = %v, want [MATCHING]", got)
	}
}
