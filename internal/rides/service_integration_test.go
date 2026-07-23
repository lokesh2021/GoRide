//go:build integration

// Integration tests for the ride Service against live Postgres + Redis, wired
// through the shared testsupport fixture. They drive real state transitions and
// assert the funnel's side effects: the guarded status write, ride:cache:{id}
// invalidation, and the published SSE envelope. Pure state-machine coverage
// lives in state_test.go (untagged).
package rides_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/lokeshbm/goride/internal/rides"
	"github.com/lokeshbm/goride/internal/testsupport"
)

// rideChannel mirrors the SPEC Redis contract (events:ride:{id}); duplicated
// here rather than imported so the test pins the wire contract independently.
func rideChannel(id string) string { return "events:ride:" + id }

func cacheKey(id string) string { return "ride:cache:" + id }

// subscribe opens a confirmed subscription to a ride channel and returns the
// message channel plus a cleanup.
func subscribe(t *testing.T, rdb *redis.Client, ctx context.Context, id string) (<-chan *redis.Message, func()) {
	t.Helper()
	sub := rdb.Subscribe(ctx, rideChannel(id))
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatalf("subscribe %s: %v", id, err)
	}
	return sub.Channel(), func() { _ = sub.Close() }
}

// waitEnvelope reads one envelope off ch or fails after a short timeout.
func waitEnvelope(t *testing.T, ch <-chan *redis.Message) map[string]any {
	t.Helper()
	select {
	case msg := <-ch:
		var env map[string]any
		if err := json.Unmarshal([]byte(msg.Payload), &env); err != nil {
			t.Fatalf("unmarshal envelope %q: %v", msg.Payload, err)
		}
		return env
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for published event")
		return nil
	}
}

func TestCreate(t *testing.T) {
	f := testsupport.New(t)
	riderID, _ := f.InsertRider()

	t.Run("success transitions to MATCHING and fires seam", func(t *testing.T) {
		matched := make(chan string, 1)
		f.Rides.MatchRequested = func(_ context.Context, id string) { matched <- id }

		qid := f.InsertQuote(riderID)
		v, err := f.Rides.Create(f.Ctx, riderID, qid, "mini", "upi")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if v.Status != string(rides.StatusMatching) {
			t.Errorf("status = %s, want MATCHING", v.Status)
		}
		if v.Tier != "mini" || v.FareTotal == nil || *v.FareTotal != 15000 {
			t.Errorf("tier/fare = %s/%v, want mini/15000", v.Tier, v.FareTotal)
		}
		if got := f.RideStatus(v.ID); got != "MATCHING" {
			t.Errorf("db status = %s, want MATCHING", got)
		}
		select {
		case id := <-matched:
			if id != v.ID {
				t.Errorf("MatchRequested got %s, want %s", id, v.ID)
			}
		case <-time.After(time.Second):
			t.Error("MatchRequested seam not fired")
		}
	})

	t.Run("quote not found", func(t *testing.T) {
		f.Rides.MatchRequested = func(context.Context, string) {}
		_, err := f.Rides.Create(f.Ctx, riderID, uuid.NewString(), "mini", "upi")
		if !errors.Is(err, rides.ErrQuoteNotFound) {
			t.Errorf("err = %v, want ErrQuoteNotFound", err)
		}
	})

	t.Run("quote not owned", func(t *testing.T) {
		otherRider, _ := f.InsertRider()
		qid := f.InsertQuote(otherRider)
		_, err := f.Rides.Create(f.Ctx, riderID, qid, "mini", "upi")
		if !errors.Is(err, rides.ErrQuoteNotOwned) {
			t.Errorf("err = %v, want ErrQuoteNotOwned", err)
		}
	})

	t.Run("quote expired", func(t *testing.T) {
		qid := f.InsertQuote(riderID)
		if _, err := f.Store.PG.Exec(f.Ctx,
			`UPDATE quotes SET expires_at = now() - interval '1 minute' WHERE id = $1`, qid); err != nil {
			t.Fatalf("expire quote: %v", err)
		}
		_, err := f.Rides.Create(f.Ctx, riderID, qid, "mini", "upi")
		if !errors.Is(err, rides.ErrQuoteExpired) {
			t.Errorf("err = %v, want ErrQuoteExpired", err)
		}
	})

	t.Run("tier unavailable", func(t *testing.T) {
		qid := f.InsertQuote(riderID)
		_, err := f.Rides.Create(f.Ctx, riderID, qid, "bike", "upi")
		if !errors.Is(err, rides.ErrTierUnavailable) {
			t.Errorf("err = %v, want ErrTierUnavailable", err)
		}
	})

	t.Run("already active", func(t *testing.T) {
		f.Rides.MatchRequested = func(context.Context, string) {}
		ar, _ := f.InsertRider()
		q1 := f.InsertQuote(ar)
		if _, err := f.Rides.Create(f.Ctx, ar, q1, "mini", "upi"); err != nil {
			t.Fatalf("first Create: %v", err)
		}
		q2 := f.InsertQuote(ar)
		_, err := f.Rides.Create(f.Ctx, ar, q2, "mini", "upi")
		if !errors.Is(err, rides.ErrAlreadyActive) {
			t.Errorf("err = %v, want ErrAlreadyActive", err)
		}
	})
}

func TestGetReadThroughCache(t *testing.T) {
	f := testsupport.New(t)
	riderID, _ := f.InsertRider()
	qid := f.InsertQuote(riderID)
	rideID := f.InsertRide(riderID, qid, "mini", "MATCHING", nil, nil)

	// Cold: no cache key yet.
	if n, _ := f.Store.Redis.Exists(f.Ctx, cacheKey(rideID)).Result(); n != 0 {
		t.Fatalf("cache key present before first Get")
	}

	// Miss populates the cache.
	v, err := f.Rides.Get(f.Ctx, rideID, riderID, rides.RoleRider)
	if err != nil {
		t.Fatalf("Get (miss): %v", err)
	}
	if v.ID != rideID {
		t.Errorf("id = %s, want %s", v.ID, rideID)
	}
	if n, _ := f.Store.Redis.Exists(f.Ctx, cacheKey(rideID)).Result(); n != 1 {
		t.Errorf("cache not populated after miss")
	}

	// Hit is served from Redis: mutate the DB row out of band and confirm the
	// cached (stale) view is returned unchanged.
	if _, err := f.Store.PG.Exec(f.Ctx, `UPDATE rides SET tier = 'xl' WHERE id = $1`, rideID); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	v2, err := f.Rides.Get(f.Ctx, rideID, riderID, rides.RoleRider)
	if err != nil {
		t.Fatalf("Get (hit): %v", err)
	}
	if v2.Tier != "mini" {
		t.Errorf("cache hit returned tier %s, want stale mini", v2.Tier)
	}

	t.Run("forbidden actor", func(t *testing.T) {
		_, err := f.Rides.Get(f.Ctx, rideID, uuid.NewString(), rides.RoleRider)
		if !errors.Is(err, rides.ErrForbidden) {
			t.Errorf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("unknown role forbidden", func(t *testing.T) {
		_, err := f.Rides.Get(f.Ctx, rideID, riderID, "admin")
		if !errors.Is(err, rides.ErrForbidden) {
			t.Errorf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, err := f.Rides.Get(f.Ctx, uuid.NewString(), riderID, rides.RoleRider)
		if !errors.Is(err, rides.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

// TestUpdateStatusFunnelSideEffects asserts the funnel (via Expire) invalidates
// the cache and publishes a ride.status_changed envelope with the SPEC shape.
func TestUpdateStatusFunnelSideEffects(t *testing.T) {
	f := testsupport.New(t)
	riderID, _ := f.InsertRider()
	qid := f.InsertQuote(riderID)
	rideID := f.InsertRide(riderID, qid, "mini", "MATCHING", nil, nil)

	// Warm the cache so we can prove the funnel deletes it.
	if _, err := f.Rides.Get(f.Ctx, rideID, riderID, rides.RoleRider); err != nil {
		t.Fatalf("warm cache: %v", err)
	}
	if n, _ := f.Store.Redis.Exists(f.Ctx, cacheKey(rideID)).Result(); n != 1 {
		t.Fatalf("cache not warm")
	}

	ch, done := subscribe(t, f.Store.Redis, f.Ctx, rideID)
	defer done()

	if err := f.Rides.Expire(f.Ctx, rideID); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if got := f.RideStatus(rideID); got != "EXPIRED" {
		t.Errorf("status = %s, want EXPIRED", got)
	}
	if n, _ := f.Store.Redis.Exists(f.Ctx, cacheKey(rideID)).Result(); n != 0 {
		t.Errorf("cache not invalidated by funnel")
	}

	env := waitEnvelope(t, ch)
	if env["type"] != "ride.status_changed" {
		t.Errorf("type = %v, want ride.status_changed", env["type"])
	}
	if env["ride_id"] != rideID {
		t.Errorf("ride_id = %v, want %s", env["ride_id"], rideID)
	}
	if _, ok := env["ts"]; !ok {
		t.Error("ts missing")
	}
	data, _ := env["data"].(map[string]any)
	if data["status"] != "EXPIRED" {
		t.Errorf("data.status = %v, want EXPIRED", data["status"])
	}
}

func TestExpireInvalidState(t *testing.T) {
	f := testsupport.New(t)
	riderID, _ := f.InsertRider()
	qid := f.InsertQuote(riderID)
	// COMPLETED is terminal; Expire's MATCHING guard matches zero rows.
	rideID := f.InsertRide(riderID, qid, "mini", "COMPLETED", nil, nil)
	if err := f.Rides.Expire(f.Ctx, rideID); !errors.Is(err, rides.ErrInvalidState) {
		t.Errorf("err = %v, want ErrInvalidState", err)
	}
}

func TestCancel(t *testing.T) {
	f := testsupport.New(t)

	t.Run("rider cancel from MATCHING", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		qid := f.InsertQuote(riderID)
		rideID := f.InsertRide(riderID, qid, "mini", "MATCHING", nil, nil)
		// Warm cache to assert invalidation.
		_, _ = f.Rides.Get(f.Ctx, rideID, riderID, rides.RoleRider)

		v, err := f.Rides.Cancel(f.Ctx, rideID, riderID, rides.RoleRider, "changed my mind")
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if v.Status != "CANCELLED_BY_RIDER" {
			t.Errorf("status = %s, want CANCELLED_BY_RIDER", v.Status)
		}
		if v.CancelReason == nil || *v.CancelReason != "changed my mind" {
			t.Errorf("cancel_reason = %v, want set", v.CancelReason)
		}
		if n, _ := f.Store.Redis.Exists(f.Ctx, cacheKey(rideID)).Result(); n != 0 {
			t.Errorf("cache not invalidated")
		}
	})

	t.Run("rider cancel releases assigned driver", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		qid := f.InsertQuote(riderID)
		driverID, _ := f.InsertDriver("mini", "on_trip")
		rideID := f.InsertRide(riderID, qid, "mini", "DRIVER_ASSIGNED", &driverID, nil)

		released := make(chan string, 1)
		f.Rides.OnDriverReleased = func(_ context.Context, id string) { released <- id }

		v, err := f.Rides.Cancel(f.Ctx, rideID, riderID, rides.RoleRider, "")
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if v.Status != "CANCELLED_BY_RIDER" {
			t.Errorf("status = %s, want CANCELLED_BY_RIDER", v.Status)
		}
		if v.CancelReason != nil {
			t.Errorf("cancel_reason = %v, want nil for empty reason", v.CancelReason)
		}
		if got := f.DriverStatus(driverID); got != "available" {
			t.Errorf("driver status = %s, want available", got)
		}
		select {
		case id := <-released:
			if id != driverID {
				t.Errorf("OnDriverReleased got %s, want %s", id, driverID)
			}
		case <-time.After(time.Second):
			t.Error("OnDriverReleased not fired")
		}
	})

	t.Run("driver cancel", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		qid := f.InsertQuote(riderID)
		driverID, _ := f.InsertDriver("mini", "on_trip")
		rideID := f.InsertRide(riderID, qid, "mini", "DRIVER_ASSIGNED", &driverID, nil)

		v, err := f.Rides.Cancel(f.Ctx, rideID, driverID, rides.RoleDriver, "no show")
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if v.Status != "CANCELLED_BY_DRIVER" {
			t.Errorf("status = %s, want CANCELLED_BY_DRIVER", v.Status)
		}
	})

	t.Run("forbidden rider", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		qid := f.InsertQuote(riderID)
		rideID := f.InsertRide(riderID, qid, "mini", "MATCHING", nil, nil)
		_, err := f.Rides.Cancel(f.Ctx, rideID, uuid.NewString(), rides.RoleRider, "")
		if !errors.Is(err, rides.ErrForbidden) {
			t.Errorf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("forbidden driver not assigned", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		qid := f.InsertQuote(riderID)
		rideID := f.InsertRide(riderID, qid, "mini", "MATCHING", nil, nil)
		_, err := f.Rides.Cancel(f.Ctx, rideID, uuid.NewString(), rides.RoleDriver, "")
		if !errors.Is(err, rides.ErrForbidden) {
			t.Errorf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("unknown role forbidden", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		qid := f.InsertQuote(riderID)
		rideID := f.InsertRide(riderID, qid, "mini", "MATCHING", nil, nil)
		_, err := f.Rides.Cancel(f.Ctx, rideID, riderID, "admin", "")
		if !errors.Is(err, rides.ErrForbidden) {
			t.Errorf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, err := f.Rides.Cancel(f.Ctx, uuid.NewString(), uuid.NewString(), rides.RoleRider, "")
		if !errors.Is(err, rides.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("invalid state on terminal ride", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		qid := f.InsertQuote(riderID)
		rideID := f.InsertRide(riderID, qid, "mini", "COMPLETED", nil, nil)
		_, err := f.Rides.Cancel(f.Ctx, rideID, riderID, rides.RoleRider, "")
		if !errors.Is(err, rides.ErrInvalidState) {
			t.Errorf("err = %v, want ErrInvalidState", err)
		}
	})
}

func TestArrivingArrived(t *testing.T) {
	f := testsupport.New(t)
	riderID, _ := f.InsertRider()
	qid := f.InsertQuote(riderID)
	driverID, _ := f.InsertDriver("mini", "on_trip")
	rideID := f.InsertRide(riderID, qid, "mini", "DRIVER_ASSIGNED", &driverID, nil)

	v, err := f.Rides.Arriving(f.Ctx, rideID, driverID)
	if err != nil {
		t.Fatalf("Arriving: %v", err)
	}
	if v.Status != "DRIVER_ARRIVING" {
		t.Errorf("status = %s, want DRIVER_ARRIVING", v.Status)
	}
	// The assigned-driver card populates the view (exercises the load join).
	if v.Driver == nil || v.Driver.Name == "" {
		t.Errorf("driver card not populated: %+v", v.Driver)
	}

	v, err = f.Rides.Arrived(f.Ctx, rideID, driverID)
	if err != nil {
		t.Fatalf("Arrived: %v", err)
	}
	if v.Status != "ARRIVED" {
		t.Errorf("status = %s, want ARRIVED", v.Status)
	}

	t.Run("forbidden non-assigned driver", func(t *testing.T) {
		_, err := f.Rides.Arriving(f.Ctx, rideID, uuid.NewString())
		if !errors.Is(err, rides.ErrForbidden) {
			t.Errorf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, err := f.Rides.Arriving(f.Ctx, uuid.NewString(), driverID)
		if !errors.Is(err, rides.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("invalid transition", func(t *testing.T) {
		// Ride now ARRIVED; Arriving expects DRIVER_ASSIGNED.
		_, err := f.Rides.Arriving(f.Ctx, rideID, driverID)
		if !errors.Is(err, rides.ErrInvalidState) {
			t.Errorf("err = %v, want ErrInvalidState", err)
		}
	})
}

func TestActiveFor(t *testing.T) {
	f := testsupport.New(t)

	t.Run("rider with active ride", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		qid := f.InsertQuote(riderID)
		rideID := f.InsertRide(riderID, qid, "mini", "MATCHING", nil, nil)
		v, err := f.Rides.ActiveFor(f.Ctx, riderID, rides.RoleRider)
		if err != nil {
			t.Fatalf("ActiveFor: %v", err)
		}
		if v == nil || v.ID != rideID {
			t.Errorf("got %v, want ride %s", v, rideID)
		}
	})

	t.Run("rider with none", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		v, err := f.Rides.ActiveFor(f.Ctx, riderID, rides.RoleRider)
		if err != nil {
			t.Fatalf("ActiveFor: %v", err)
		}
		if v != nil {
			t.Errorf("got %v, want nil", v)
		}
	})

	t.Run("driver with active ride", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		qid := f.InsertQuote(riderID)
		driverID, _ := f.InsertDriver("mini", "on_trip")
		rideID := f.InsertRide(riderID, qid, "mini", "DRIVER_ASSIGNED", &driverID, nil)
		v, err := f.Rides.ActiveFor(f.Ctx, driverID, rides.RoleDriver)
		if err != nil {
			t.Fatalf("ActiveFor: %v", err)
		}
		if v == nil || v.ID != rideID {
			t.Errorf("got %v, want ride %s", v, rideID)
		}
	})
}

func TestRegenerateOTP(t *testing.T) {
	f := testsupport.New(t)

	t.Run("success on assigned ride", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		qid := f.InsertQuote(riderID)
		driverID, _ := f.InsertDriver("mini", "on_trip")
		rideID := f.InsertRide(riderID, qid, "mini", "DRIVER_ASSIGNED", &driverID, nil)

		var before *string
		_ = f.Store.PG.QueryRow(f.Ctx, `SELECT otp_hash FROM rides WHERE id = $1`, rideID).Scan(&before)

		otp, err := f.Rides.RegenerateOTP(f.Ctx, rideID, riderID)
		if err != nil {
			t.Fatalf("RegenerateOTP: %v", err)
		}
		if len(otp) != 4 {
			t.Errorf("otp = %q, want 4 digits", otp)
		}
		for _, c := range otp {
			if c < '0' || c > '9' {
				t.Errorf("otp %q not all digits", otp)
				break
			}
		}
		var after *string
		if err := f.Store.PG.QueryRow(f.Ctx, `SELECT otp_hash FROM rides WHERE id = $1`, rideID).Scan(&after); err != nil {
			t.Fatalf("read otp_hash: %v", err)
		}
		if after == nil || *after == "" {
			t.Error("otp_hash not written")
		}
	})

	t.Run("invalid state before assignment", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		qid := f.InsertQuote(riderID)
		rideID := f.InsertRide(riderID, qid, "mini", "MATCHING", nil, nil)
		_, err := f.Rides.RegenerateOTP(f.Ctx, rideID, riderID)
		if !errors.Is(err, rides.ErrInvalidState) {
			t.Errorf("err = %v, want ErrInvalidState", err)
		}
	})

	t.Run("invalid state wrong rider", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		qid := f.InsertQuote(riderID)
		driverID, _ := f.InsertDriver("mini", "on_trip")
		rideID := f.InsertRide(riderID, qid, "mini", "DRIVER_ASSIGNED", &driverID, nil)
		_, err := f.Rides.RegenerateOTP(f.Ctx, rideID, uuid.NewString())
		if !errors.Is(err, rides.ErrInvalidState) {
			t.Errorf("err = %v, want ErrInvalidState", err)
		}
	})
}

func TestLoadViewAndInvalidateCache(t *testing.T) {
	f := testsupport.New(t)
	riderID, _ := f.InsertRider()
	qid := f.InsertQuote(riderID)
	rideID := f.InsertRide(riderID, qid, "mini", "MATCHING", nil, nil)

	v, err := f.Rides.LoadView(f.Ctx, rideID)
	if err != nil {
		t.Fatalf("LoadView: %v", err)
	}
	if v.ID != rideID || v.RiderID != riderID {
		t.Errorf("LoadView returned %+v", v)
	}

	t.Run("not found", func(t *testing.T) {
		_, err := f.Rides.LoadView(f.Ctx, uuid.NewString())
		if !errors.Is(err, rides.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("invalidate cache removes key", func(t *testing.T) {
		if _, err := f.Rides.Get(f.Ctx, rideID, riderID, rides.RoleRider); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if n, _ := f.Store.Redis.Exists(f.Ctx, cacheKey(rideID)).Result(); n != 1 {
			t.Fatalf("cache not warm")
		}
		if err := f.Rides.InvalidateCache(f.Ctx, rideID); err != nil {
			t.Fatalf("InvalidateCache: %v", err)
		}
		if n, _ := f.Store.Redis.Exists(f.Ctx, cacheKey(rideID)).Result(); n != 0 {
			t.Errorf("cache key not deleted")
		}
	})
}

// TestPublishRideDriver exercises the exported publish seams end to end against
// the wired real Publisher.
func TestPublishRideDriver(t *testing.T) {
	f := testsupport.New(t)
	riderID, _ := f.InsertRider()
	qid := f.InsertQuote(riderID)
	rideID := f.InsertRide(riderID, qid, "mini", "MATCHING", nil, nil)

	ch, done := subscribe(t, f.Store.Redis, f.Ctx, rideID)
	defer done()

	if err := f.Rides.PublishRide(f.Ctx, rideID, "ride.offer", map[string]any{"k": "v"}); err != nil {
		t.Fatalf("PublishRide: %v", err)
	}
	env := waitEnvelope(t, ch)
	if env["type"] != "ride.offer" {
		t.Errorf("type = %v, want ride.offer", env["type"])
	}

	// PublishDriver goes to a driver channel; just assert it does not error.
	driverID, _ := f.InsertDriver("mini", "available")
	if err := f.Rides.PublishDriver(f.Ctx, driverID, "ride.offer", map[string]any{"ride_id": rideID}); err != nil {
		t.Fatalf("PublishDriver: %v", err)
	}
}
