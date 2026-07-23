//go:build integration

package trips_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/lokeshbm/goride/internal/rides"
	"github.com/lokeshbm/goride/internal/testsupport"
	"github.com/lokeshbm/goride/internal/trips"
)

// knownOTP is the plaintext OTP whose bcrypt hash we plant on the ride so Start
// tests can exercise the real bcrypt compare with a known-good/known-bad value.
const knownOTP = "4321"

func hashOTP(t *testing.T, code string) *string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt hash: %v", err)
	}
	s := string(h)
	return &s
}

// arrivedRide seeds a rider, an on_trip driver, a quote, and a ride in ARRIVED
// with that driver assigned and the given otpHash. Returns (rideID, driverID).
func arrivedRide(t *testing.T, f *testsupport.Fixture, otpHash *string) (string, string) {
	t.Helper()
	riderID, _ := f.InsertRider()
	driverID, _ := f.InsertDriver("mini", "on_trip")
	quoteID := f.InsertQuote(riderID)
	rideID := f.InsertRide(riderID, quoteID, "mini", string(rides.StatusArrived), &driverID, otpHash)
	return rideID, driverID
}

// ---- Start ----

func TestStartHappyPath(t *testing.T) {
	f := testsupport.New(t)
	rideID, driverID := arrivedRide(t, f, hashOTP(t, knownOTP))

	trip, err := f.Trips.Start(f.Ctx, driverID, rideID, knownOTP)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if trip.Status != trips.StatusStarted {
		t.Errorf("trip.Status = %q, want STARTED", trip.Status)
	}
	if trip.RideStatus != string(rides.StatusInProgress) {
		t.Errorf("trip.RideStatus = %q, want IN_PROGRESS", trip.RideStatus)
	}
	if got := f.RideStatus(rideID); got != string(rides.StatusInProgress) {
		t.Errorf("ride status = %q, want IN_PROGRESS", got)
	}
	if n := f.Count(`SELECT count(*) FROM trips WHERE ride_id = $1 AND status = 'STARTED'`, rideID); n != 1 {
		t.Errorf("STARTED trip rows = %d, want 1", n)
	}
}

func TestStartWrongOTPNoStateChange(t *testing.T) {
	f := testsupport.New(t)
	rideID, driverID := arrivedRide(t, f, hashOTP(t, knownOTP))

	_, err := f.Trips.Start(f.Ctx, driverID, rideID, "0000")
	if !errors.Is(err, trips.ErrInvalidOTP) {
		t.Fatalf("Start wrong otp err = %v, want ErrInvalidOTP", err)
	}
	// No state change: ride stays ARRIVED and no trip row exists.
	if got := f.RideStatus(rideID); got != string(rides.StatusArrived) {
		t.Errorf("ride status = %q, want ARRIVED (unchanged)", got)
	}
	if n := f.Count(`SELECT count(*) FROM trips WHERE ride_id = $1`, rideID); n != 0 {
		t.Errorf("trip rows = %d, want 0 (no state change)", n)
	}
}

func TestStartGuards(t *testing.T) {
	f := testsupport.New(t)

	t.Run("not the assigned driver → forbidden", func(t *testing.T) {
		rideID, _ := arrivedRide(t, f, hashOTP(t, knownOTP))
		otherDriver, _ := f.InsertDriver("mini", "on_trip")
		_, err := f.Trips.Start(f.Ctx, otherDriver, rideID, knownOTP)
		if !errors.Is(err, trips.ErrForbidden) {
			t.Fatalf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("ride not ARRIVED → invalid state", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		driverID, _ := f.InsertDriver("mini", "on_trip")
		quoteID := f.InsertQuote(riderID)
		rideID := f.InsertRide(riderID, quoteID, "mini", string(rides.StatusDriverArriving), &driverID, hashOTP(t, knownOTP))
		_, err := f.Trips.Start(f.Ctx, driverID, rideID, knownOTP)
		if !errors.Is(err, trips.ErrInvalidState) {
			t.Fatalf("err = %v, want ErrInvalidState", err)
		}
	})

	t.Run("no otp hash → invalid state", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		driverID, _ := f.InsertDriver("mini", "on_trip")
		quoteID := f.InsertQuote(riderID)
		rideID := f.InsertRide(riderID, quoteID, "mini", string(rides.StatusArrived), &driverID, nil)
		_, err := f.Trips.Start(f.Ctx, driverID, rideID, knownOTP)
		if !errors.Is(err, trips.ErrInvalidState) {
			t.Fatalf("err = %v, want ErrInvalidState", err)
		}
	})

	t.Run("unknown ride → not found", func(t *testing.T) {
		driverID, _ := f.InsertDriver("mini", "on_trip")
		_, err := f.Trips.Start(f.Ctx, driverID, "00000000-0000-0000-0000-000000000000", knownOTP)
		if !errors.Is(err, trips.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

// TestStartDuplicateTripInvalidState covers the unique-violation arm of Start's
// trip insert: an existing trip row for the ride makes the INSERT collide
// (trips.ride_id is unique), so Start rolls back and returns ErrInvalidState.
func TestStartDuplicateTripInvalidState(t *testing.T) {
	f := testsupport.New(t)
	rideID, driverID := arrivedRide(t, f, hashOTP(t, knownOTP))
	// Pre-seed a trip row so the Start insert violates the ride_id unique index.
	if _, err := f.Store.PG.Exec(f.Ctx, `
		INSERT INTO trips (id, ride_id, status, started_at, paused_seconds)
		VALUES (gen_random_uuid(), $1, 'STARTED', now(), 0)`, rideID); err != nil {
		t.Fatalf("pre-seed trip: %v", err)
	}
	_, err := f.Trips.Start(f.Ctx, driverID, rideID, knownOTP)
	if !errors.Is(err, trips.ErrInvalidState) {
		t.Fatalf("Start err = %v, want ErrInvalidState", err)
	}
	// The ride update is rolled back with the failed insert: stays ARRIVED.
	if got := f.RideStatus(rideID); got != string(rides.StatusArrived) {
		t.Errorf("ride status = %q, want ARRIVED (rolled back)", got)
	}
}

// ---- Pause / Resume ----

// TestPauseLoadsFareBreakdown covers load's fare-unmarshal path: a trip row that
// already carries a fare breakdown is read back into Trip.Fare by the Pause →
// load round-trip.
func TestPauseLoadsFareBreakdown(t *testing.T) {
	f := testsupport.New(t)
	rideID, driverID := startTrip(t, f)
	if _, err := f.Store.PG.Exec(f.Ctx,
		`UPDATE trips SET fare = $2 WHERE ride_id = $1`, rideID, []byte(`{"total":12345}`)); err != nil {
		t.Fatalf("set fare: %v", err)
	}
	trip, err := f.Trips.Pause(f.Ctx, driverID, rideID)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if trip.Fare == nil || trip.Fare.Total != 12345 {
		t.Errorf("loaded fare = %+v, want total 12345", trip.Fare)
	}
}

// TestConsumePauseWrongType covers consumePause's non-Nil GetDel error branch: a
// paused_at key of the wrong Redis type makes GETDEL fail (WRONGTYPE), which is
// logged and treated as zero elapsed rather than propagated.
func TestConsumePauseWrongType(t *testing.T) {
	f := testsupport.New(t)
	rideID, driverID := startTrip(t, f)
	if _, err := f.Trips.Pause(f.Ctx, driverID, rideID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// Replace the string mirror with a list so GETDEL returns a WRONGTYPE error.
	key := "trip:paused_at:" + rideID
	if err := f.Store.Redis.Del(f.Ctx, key).Err(); err != nil {
		t.Fatalf("del: %v", err)
	}
	if err := f.Store.Redis.LPush(f.Ctx, key, "x").Err(); err != nil {
		t.Fatalf("lpush: %v", err)
	}
	f.TrackRedisKey(key)
	if _, err := f.Trips.Resume(f.Ctx, driverID, rideID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if n := f.Count(`SELECT paused_seconds FROM trips WHERE ride_id = $1`, rideID); n != 0 {
		t.Errorf("paused_seconds = %d, want 0 (WRONGTYPE treated as zero)", n)
	}
}

// ---- Pause / Resume (continued) ----

// startTrip seeds an ARRIVED ride and starts it, returning (rideID, driverID)
// with a live STARTED trip.
func startTrip(t *testing.T, f *testsupport.Fixture) (string, string) {
	t.Helper()
	rideID, driverID := arrivedRide(t, f, hashOTP(t, knownOTP))
	if _, err := f.Trips.Start(f.Ctx, driverID, rideID, knownOTP); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return rideID, driverID
}

func TestPauseResumeAccumulatesPausedSeconds(t *testing.T) {
	f := testsupport.New(t)
	rideID, driverID := startTrip(t, f)

	if _, err := f.Trips.Pause(f.Ctx, driverID, rideID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if n := f.Count(`SELECT count(*) FROM trips WHERE ride_id = $1 AND status = 'PAUSED'`, rideID); n != 1 {
		t.Fatalf("trip not PAUSED after Pause")
	}

	// Rewind the paused_at mirror 5s into the past so Resume folds in a
	// deterministic, non-zero elapsed pause without a real sleep.
	if err := f.Store.Redis.Set(f.Ctx, "trip:paused_at:"+rideID, time.Now().UTC().Unix()-5, 2*time.Hour).Err(); err != nil {
		t.Fatalf("rewind paused_at: %v", err)
	}

	if _, err := f.Trips.Resume(f.Ctx, driverID, rideID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	var paused int
	paused = f.Count(`SELECT paused_seconds FROM trips WHERE ride_id = $1`, rideID)
	if paused < 5 || paused > 7 {
		t.Errorf("paused_seconds = %d, want ~5", paused)
	}
	if got := f.Count(`SELECT count(*) FROM trips WHERE ride_id = $1 AND status = 'STARTED'`, rideID); got != 1 {
		t.Errorf("trip not back to STARTED after Resume")
	}
}

func TestPauseAlreadyPausedIsInvalidState(t *testing.T) {
	f := testsupport.New(t)
	rideID, driverID := startTrip(t, f)
	if _, err := f.Trips.Pause(f.Ctx, driverID, rideID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// Second pause: the trip is no longer STARTED, so the guarded update affects
	// zero rows → ErrInvalidState.
	_, err := f.Trips.Pause(f.Ctx, driverID, rideID)
	if !errors.Is(err, trips.ErrInvalidState) {
		t.Fatalf("second Pause err = %v, want ErrInvalidState", err)
	}
}

func TestConsumePauseEdgeCases(t *testing.T) {
	f := testsupport.New(t)

	t.Run("garbage paused_at parses to zero", func(t *testing.T) {
		rideID, driverID := startTrip(t, f)
		if _, err := f.Trips.Pause(f.Ctx, driverID, rideID); err != nil {
			t.Fatalf("Pause: %v", err)
		}
		if err := f.Store.Redis.Set(f.Ctx, "trip:paused_at:"+rideID, "not-a-number", 2*time.Hour).Err(); err != nil {
			t.Fatalf("set garbage: %v", err)
		}
		if _, err := f.Trips.Resume(f.Ctx, driverID, rideID); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if n := f.Count(`SELECT paused_seconds FROM trips WHERE ride_id = $1`, rideID); n != 0 {
			t.Errorf("paused_seconds = %d, want 0 for unparseable mirror", n)
		}
	})

	t.Run("future paused_at yields zero elapsed", func(t *testing.T) {
		rideID, driverID := startTrip(t, f)
		if _, err := f.Trips.Pause(f.Ctx, driverID, rideID); err != nil {
			t.Fatalf("Pause: %v", err)
		}
		if err := f.Store.Redis.Set(f.Ctx, "trip:paused_at:"+rideID, time.Now().UTC().Unix()+3600, 2*time.Hour).Err(); err != nil {
			t.Fatalf("set future: %v", err)
		}
		if _, err := f.Trips.Resume(f.Ctx, driverID, rideID); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if n := f.Count(`SELECT paused_seconds FROM trips WHERE ride_id = $1`, rideID); n != 0 {
			t.Errorf("paused_seconds = %d, want 0 for a future paused_at", n)
		}
	})
}

// TestEndInvalidTripRows covers End's trip-side guards: a missing trip row and a
// trip that is no longer in a STARTED/PAUSED state both yield ErrInvalidState.
func TestEndInvalidTripRows(t *testing.T) {
	f := testsupport.New(t)

	t.Run("ride IN_PROGRESS but no trip row", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		driverID, _ := f.InsertDriver("mini", "on_trip")
		quoteID := f.InsertQuote(riderID)
		rideID := f.InsertRide(riderID, quoteID, "mini", string(rides.StatusInProgress), &driverID, nil)
		_, err := f.Trips.End(f.Ctx, driverID, rideID)
		if !errors.Is(err, trips.ErrInvalidState) {
			t.Fatalf("err = %v, want ErrInvalidState", err)
		}
	})

	t.Run("ride IN_PROGRESS but trip already ENDED", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		driverID, _ := f.InsertDriver("mini", "on_trip")
		quoteID := f.InsertQuote(riderID)
		rideID := f.InsertRide(riderID, quoteID, "mini", string(rides.StatusInProgress), &driverID, nil)
		if _, err := f.Store.PG.Exec(f.Ctx, `
			INSERT INTO trips (id, ride_id, status, started_at, ended_at, paused_seconds)
			VALUES (gen_random_uuid(), $1, 'ENDED', now(), now(), 0)`, rideID); err != nil {
			t.Fatalf("insert ended trip: %v", err)
		}
		_, err := f.Trips.End(f.Ctx, driverID, rideID)
		if !errors.Is(err, trips.ErrInvalidState) {
			t.Fatalf("err = %v, want ErrInvalidState", err)
		}
		// The ride must remain IN_PROGRESS (transaction rolled back).
		if got := f.RideStatus(rideID); got != string(rides.StatusInProgress) {
			t.Errorf("ride status = %q, want IN_PROGRESS (rolled back)", got)
		}
	})
}

// TestServiceDBErrorContract exercises the wrapped-DB-error return paths: a
// malformed ride id makes the initial lookup fail (a non-sentinel error), which
// each entry point must surface rather than swallow.
func TestServiceDBErrorContract(t *testing.T) {
	f := testsupport.New(t)
	driverID, _ := f.InsertDriver("mini", "on_trip")
	const badID = "not-a-uuid"

	assertWrapped := func(name string, err error) {
		if err == nil {
			t.Errorf("%s: expected a wrapped DB error, got nil", name)
			return
		}
		if errors.Is(err, trips.ErrNotFound) || errors.Is(err, trips.ErrForbidden) ||
			errors.Is(err, trips.ErrInvalidState) || errors.Is(err, trips.ErrInvalidOTP) {
			t.Errorf("%s: got a domain sentinel %v, want a wrapped DB error", name, err)
		}
	}

	_, err := f.Trips.Start(f.Ctx, driverID, badID, "1234")
	assertWrapped("Start", err)
	_, err = f.Trips.End(f.Ctx, driverID, badID)
	assertWrapped("End", err)
	_, err = f.Trips.Pause(f.Ctx, driverID, badID)
	assertWrapped("Pause", err)
	_, err = f.Trips.Resume(f.Ctx, driverID, badID)
	assertWrapped("Resume", err)
}

// TestResumeForbidden covers Resume's assigned-driver guard (distinct from
// Pause's, which lives on a different line).
func TestResumeForbidden(t *testing.T) {
	f := testsupport.New(t)
	rideID, driverID := startTrip(t, f)
	if _, err := f.Trips.Pause(f.Ctx, driverID, rideID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	other, _ := f.InsertDriver("mini", "on_trip")
	_, err := f.Trips.Resume(f.Ctx, other, rideID)
	if !errors.Is(err, trips.ErrForbidden) {
		t.Fatalf("Resume by non-assigned driver err = %v, want ErrForbidden", err)
	}
}

func TestPauseResumeGuards(t *testing.T) {
	f := testsupport.New(t)
	rideID, driverID := startTrip(t, f)

	t.Run("resume when not paused → invalid state", func(t *testing.T) {
		_, err := f.Trips.Resume(f.Ctx, driverID, rideID)
		if !errors.Is(err, trips.ErrInvalidState) {
			t.Fatalf("err = %v, want ErrInvalidState", err)
		}
	})

	t.Run("pause by non-assigned driver → forbidden", func(t *testing.T) {
		other, _ := f.InsertDriver("mini", "on_trip")
		_, err := f.Trips.Pause(f.Ctx, other, rideID)
		if !errors.Is(err, trips.ErrForbidden) {
			t.Fatalf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("pause unknown ride → not found", func(t *testing.T) {
		_, err := f.Trips.Pause(f.Ctx, driverID, "00000000-0000-0000-0000-000000000000")
		if !errors.Is(err, trips.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

// ---- End ----

func TestEndFinalizesTrip(t *testing.T) {
	f := testsupport.New(t)
	rideID, driverID := startTrip(t, f)

	trip, err := f.Trips.End(f.Ctx, driverID, rideID)
	if err != nil {
		t.Fatalf("End: %v", err)
	}
	if trip.Status != trips.StatusEnded {
		t.Errorf("trip.Status = %q, want ENDED", trip.Status)
	}
	if trip.Fare == nil || trip.Fare.Total <= 0 {
		t.Errorf("expected a positive fare total, got %+v", trip.Fare)
	}
	if got := f.RideStatus(rideID); got != string(rides.StatusCompleted) {
		t.Errorf("ride status = %q, want COMPLETED", got)
	}
	// fare_total persisted on the ride.
	if n := f.Count(`SELECT count(*) FROM rides WHERE id = $1 AND fare_total > 0`, rideID); n != 1 {
		t.Errorf("ride fare_total not persisted")
	}
	// Driver freed back to available.
	if got := f.DriverStatus(driverID); got != "available" {
		t.Errorf("driver status = %q, want available", got)
	}
	// Exactly one PENDING payment created.
	if n := f.Count(`SELECT count(*) FROM payments WHERE ride_id = $1 AND status = 'PENDING'`, rideID); n != 1 {
		t.Errorf("PENDING payment rows = %d, want 1", n)
	}
	// Trip row ENDED with the fare breakdown written.
	if n := f.Count(`SELECT count(*) FROM trips WHERE ride_id = $1 AND status = 'ENDED' AND fare IS NOT NULL`, rideID); n != 1 {
		t.Errorf("ENDED trip row not finalized")
	}
}

func TestEndUsesMeteredDistance(t *testing.T) {
	f := testsupport.New(t)
	rideID, driverID := startTrip(t, f)

	// Plant a metered distance larger than the quote's 9000m estimate; End must
	// prefer it over the fallback.
	if err := f.Store.Redis.Set(f.Ctx, "trip:dist:"+rideID, 15000, time.Hour).Err(); err != nil {
		t.Fatalf("set trip:dist: %v", err)
	}
	trip, err := f.Trips.End(f.Ctx, driverID, rideID)
	if err != nil {
		t.Fatalf("End: %v", err)
	}
	if trip.DistanceM == nil || *trip.DistanceM != 15000 {
		t.Errorf("distance = %v, want 15000 (metered)", trip.DistanceM)
	}
}

func TestEndWhilePausedFoldsInFlightPause(t *testing.T) {
	f := testsupport.New(t)
	rideID, driverID := startTrip(t, f)

	if _, err := f.Trips.Pause(f.Ctx, driverID, rideID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// Rewind the in-flight pause 3s so End folds a deterministic amount.
	if err := f.Store.Redis.Set(f.Ctx, "trip:paused_at:"+rideID, time.Now().UTC().Unix()-3, 2*time.Hour).Err(); err != nil {
		t.Fatalf("rewind paused_at: %v", err)
	}

	trip, err := f.Trips.End(f.Ctx, driverID, rideID)
	if err != nil {
		t.Fatalf("End while paused: %v", err)
	}
	if trip.PausedSeconds < 3 || trip.PausedSeconds > 5 {
		t.Errorf("paused_seconds = %d, want ~3 (in-flight pause folded in)", trip.PausedSeconds)
	}
	if got := f.RideStatus(rideID); got != string(rides.StatusCompleted) {
		t.Errorf("ride status = %q, want COMPLETED", got)
	}
}

func TestResumeWithoutMirrorAccumulatesZero(t *testing.T) {
	f := testsupport.New(t)
	rideID, driverID := startTrip(t, f)

	if _, err := f.Trips.Pause(f.Ctx, driverID, rideID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// Drop the paused_at mirror: consumePause must treat a missing key as 0.
	if err := f.Store.Redis.Del(f.Ctx, "trip:paused_at:"+rideID).Err(); err != nil {
		t.Fatalf("del paused_at: %v", err)
	}
	if _, err := f.Trips.Resume(f.Ctx, driverID, rideID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if n := f.Count(`SELECT paused_seconds FROM trips WHERE ride_id = $1`, rideID); n != 0 {
		t.Errorf("paused_seconds = %d, want 0 (no mirror)", n)
	}
}

// TestEndConcurrentOnlyOneWins fires two End calls at the same started trip.
// The guarded ride update (WHERE status = 'IN_PROGRESS') serializes on the row
// lock, so exactly one commits COMPLETED and the loser sees zero rows affected →
// ErrInvalidState. This covers End's optimistic-guard failure arm.
func TestEndConcurrentOnlyOneWins(t *testing.T) {
	f := testsupport.New(t)
	rideID, driverID := startTrip(t, f)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.Trips.End(f.Ctx, driverID, rideID)
		}(i)
	}
	wg.Wait()

	var wins, invalid int
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, trips.ErrInvalidState):
			invalid++
		default:
			t.Errorf("unexpected End error: %v", err)
		}
	}
	if wins != 1 || invalid != 1 {
		t.Fatalf("End race: wins=%d invalid=%d, want 1/1", wins, invalid)
	}
	if got := f.RideStatus(rideID); got != string(rides.StatusCompleted) {
		t.Errorf("ride status = %q, want COMPLETED", got)
	}
	if n := f.Count(`SELECT count(*) FROM payments WHERE ride_id = $1`, rideID); n != 1 {
		t.Errorf("payment rows = %d, want exactly 1 (loser rolled back)", n)
	}
}

func TestEndGuards(t *testing.T) {
	f := testsupport.New(t)

	t.Run("ride not IN_PROGRESS → invalid state", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		driverID, _ := f.InsertDriver("mini", "on_trip")
		quoteID := f.InsertQuote(riderID)
		rideID := f.InsertRide(riderID, quoteID, "mini", string(rides.StatusArrived), &driverID, hashOTP(t, knownOTP))
		_, err := f.Trips.End(f.Ctx, driverID, rideID)
		if !errors.Is(err, trips.ErrInvalidState) {
			t.Fatalf("err = %v, want ErrInvalidState", err)
		}
	})

	t.Run("end by non-assigned driver → forbidden", func(t *testing.T) {
		rideID, _ := startTrip(t, f)
		other, _ := f.InsertDriver("mini", "on_trip")
		_, err := f.Trips.End(f.Ctx, other, rideID)
		if !errors.Is(err, trips.ErrForbidden) {
			t.Fatalf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("end unknown ride → not found", func(t *testing.T) {
		driverID, _ := f.InsertDriver("mini", "on_trip")
		_, err := f.Trips.End(f.Ctx, driverID, "00000000-0000-0000-0000-000000000000")
		if !errors.Is(err, trips.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}
