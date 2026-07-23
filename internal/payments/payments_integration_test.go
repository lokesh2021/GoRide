//go:build integration

package payments_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lokeshbm/goride/internal/payments"
	"github.com/lokeshbm/goride/internal/rides"
	"github.com/lokeshbm/goride/internal/testsupport"
)

// completedRide seeds a rider + assigned driver + quote and a COMPLETED ride
// with fare_total set, returning (rideID, riderID, driverID).
func completedRide(t *testing.T, f *testsupport.Fixture) (string, string, string) {
	t.Helper()
	riderID, _ := f.InsertRider()
	driverID, _ := f.InsertDriver("mini", "available")
	quoteID := f.InsertQuote(riderID)
	rideID := f.InsertRide(riderID, quoteID, "mini", string(rides.StatusCompleted), &driverID, nil)
	if _, err := f.Store.PG.Exec(f.Ctx,
		`UPDATE rides SET fare_total = 22000 WHERE id = $1`, rideID); err != nil {
		t.Fatalf("set fare_total: %v", err)
	}
	return rideID, riderID, driverID
}

// insertPayment inserts a payment row for a ride in the given state.
func insertPayment(t *testing.T, f *testsupport.Fixture, rideID, status string, retry int, pspRef *string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := f.Store.PG.Exec(f.Ctx,
		`INSERT INTO payments (id, ride_id, amount, method, status, retry_count, psp_ref)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id, rideID, 22000, "upi", status, retry, pspRef); err != nil {
		t.Fatalf("insert payment: %v", err)
	}
	return id
}

// insertEndedTrip inserts an ENDED trip row with a fare breakdown so the
// success-webhook receipt build (which reads trips) has a row to copy.
func insertEndedTrip(t *testing.T, f *testsupport.Fixture, rideID string) {
	t.Helper()
	fare, _ := json.Marshal(map[string]any{"base": 3000, "total": 22000})
	started := time.Now().UTC().Add(-10 * time.Minute)
	ended := time.Now().UTC()
	if _, err := f.Store.PG.Exec(f.Ctx, `
		INSERT INTO trips (id, ride_id, status, started_at, ended_at, paused_seconds, distance_m, fare)
		VALUES ($1,$2,'ENDED',$3,$4,0,9000,$5)`,
		uuid.NewString(), rideID, started, ended, fare); err != nil {
		t.Fatalf("insert ended trip: %v", err)
	}
}

// ---- Trigger ----

func TestTriggerHappyPath(t *testing.T) {
	f := testsupport.New(t)
	rideID, riderID, _ := completedRide(t, f)
	insertPayment(t, f, rideID, payments.StatusPending, 0, nil)

	p, err := f.Payments.Trigger(f.Ctx, riderID, rideID)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if p.Status != payments.StatusProcessing {
		t.Errorf("status = %q, want PROCESSING", p.Status)
	}
	if p.PSPRef == "" {
		t.Errorf("expected a psp_ref to be assigned")
	}
	if n := f.Count(`SELECT count(*) FROM payments WHERE ride_id = $1 AND status = 'PROCESSING' AND psp_ref IS NOT NULL`, rideID); n != 1 {
		t.Errorf("payment not moved to PROCESSING with psp_ref")
	}
}

func TestTriggerGuards(t *testing.T) {
	f := testsupport.New(t)

	t.Run("unknown ride → not found", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		_, err := f.Payments.Trigger(f.Ctx, riderID, "00000000-0000-0000-0000-000000000000")
		if !errors.Is(err, payments.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("ride owned by another rider → forbidden", func(t *testing.T) {
		rideID, _, _ := completedRide(t, f)
		other, _ := f.InsertRider()
		_, err := f.Payments.Trigger(f.Ctx, other, rideID)
		if !errors.Is(err, payments.ErrForbidden) {
			t.Fatalf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("ride not COMPLETED → invalid state", func(t *testing.T) {
		riderID, _ := f.InsertRider()
		driverID, _ := f.InsertDriver("mini", "on_trip")
		quoteID := f.InsertQuote(riderID)
		rideID := f.InsertRide(riderID, quoteID, "mini", string(rides.StatusInProgress), &driverID, nil)
		_, err := f.Payments.Trigger(f.Ctx, riderID, rideID)
		if !errors.Is(err, payments.ErrInvalidState) {
			t.Fatalf("err = %v, want ErrInvalidState", err)
		}
	})

	t.Run("already PROCESSING → invalid state", func(t *testing.T) {
		rideID, riderID, _ := completedRide(t, f)
		ref := uuid.NewString()
		insertPayment(t, f, rideID, payments.StatusProcessing, 0, &ref)
		_, err := f.Payments.Trigger(f.Ctx, riderID, rideID)
		if !errors.Is(err, payments.ErrInvalidState) {
			t.Fatalf("err = %v, want ErrInvalidState", err)
		}
	})

	t.Run("retries exhausted → retries exhausted", func(t *testing.T) {
		rideID, riderID, _ := completedRide(t, f)
		ref := uuid.NewString()
		insertPayment(t, f, rideID, payments.StatusFailed, 3, &ref)
		_, err := f.Payments.Trigger(f.Ctx, riderID, rideID)
		if !errors.Is(err, payments.ErrRetriesExhausted) {
			t.Fatalf("err = %v, want ErrRetriesExhausted", err)
		}
	})

	t.Run("FAILED under the cap is retriable", func(t *testing.T) {
		rideID, riderID, _ := completedRide(t, f)
		ref := uuid.NewString()
		insertPayment(t, f, rideID, payments.StatusFailed, 1, &ref)
		p, err := f.Payments.Trigger(f.Ctx, riderID, rideID)
		if err != nil {
			t.Fatalf("Trigger retry: %v", err)
		}
		if p.Status != payments.StatusProcessing {
			t.Errorf("status = %q, want PROCESSING", p.Status)
		}
	})
}

func TestTriggerCompletedRideWithoutPayment(t *testing.T) {
	f := testsupport.New(t)
	// COMPLETED ride but no payment row at all → the payment lookup misses and
	// Trigger surfaces ErrNotFound.
	rideID, riderID, _ := completedRide(t, f)
	_, err := f.Payments.Trigger(f.Ctx, riderID, rideID)
	if !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// ---- HandleWebhook ----

func TestWebhookSuccessCreatesReceipt(t *testing.T) {
	f := testsupport.New(t)
	rideID, _, _ := completedRide(t, f)
	insertEndedTrip(t, f, rideID)
	ref := uuid.NewString()
	insertPayment(t, f, rideID, payments.StatusProcessing, 0, &ref)

	if err := f.Payments.HandleWebhook(f.Ctx, ref, "success"); err != nil {
		t.Fatalf("HandleWebhook success: %v", err)
	}
	if n := f.Count(`SELECT count(*) FROM payments WHERE psp_ref = $1 AND status = 'SUCCEEDED'`, ref); n != 1 {
		t.Errorf("payment not SUCCEEDED")
	}
	if n := f.Count(`SELECT count(*) FROM receipts WHERE ride_id = $1`, rideID); n != 1 {
		t.Errorf("receipt rows = %d, want 1", n)
	}

	// Replay: a second delivery for the now-terminal payment is a no-op and does
	// not create a second receipt.
	if err := f.Payments.HandleWebhook(f.Ctx, ref, "success"); err != nil {
		t.Fatalf("HandleWebhook replay: %v", err)
	}
	if n := f.Count(`SELECT count(*) FROM receipts WHERE ride_id = $1`, rideID); n != 1 {
		t.Errorf("replay created a duplicate receipt (count = %d)", n)
	}
}

func TestWebhookFailureIncrementsRetry(t *testing.T) {
	f := testsupport.New(t)
	rideID, _, _ := completedRide(t, f)
	ref := uuid.NewString()
	insertPayment(t, f, rideID, payments.StatusProcessing, 0, &ref)

	if err := f.Payments.HandleWebhook(f.Ctx, ref, "failure"); err != nil {
		t.Fatalf("HandleWebhook failure: %v", err)
	}
	if n := f.Count(`SELECT count(*) FROM payments WHERE psp_ref = $1 AND status = 'FAILED' AND retry_count = 1`, ref); n != 1 {
		t.Errorf("payment not FAILED with retry_count=1")
	}
	if n := f.Count(`SELECT count(*) FROM receipts WHERE ride_id = $1`, rideID); n != 0 {
		t.Errorf("failure should not create a receipt (count = %d)", n)
	}
}

func TestWebhookIdempotencyAndNotFound(t *testing.T) {
	f := testsupport.New(t)

	t.Run("unknown psp_ref → not found", func(t *testing.T) {
		err := f.Payments.HandleWebhook(f.Ctx, uuid.NewString(), "success")
		if !errors.Is(err, payments.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("webhook on already-terminal payment is a no-op", func(t *testing.T) {
		rideID, _, _ := completedRide(t, f)
		ref := uuid.NewString()
		insertPayment(t, f, rideID, payments.StatusSucceeded, 0, &ref)
		if err := f.Payments.HandleWebhook(f.Ctx, ref, "failure"); err != nil {
			t.Fatalf("HandleWebhook on terminal: %v", err)
		}
		// Unchanged: still SUCCEEDED, retry_count still 0.
		if n := f.Count(`SELECT count(*) FROM payments WHERE psp_ref = $1 AND status = 'SUCCEEDED' AND retry_count = 0`, ref); n != 1 {
			t.Errorf("terminal payment was mutated by a replayed webhook")
		}
	})
}

// ---- History ----

func TestHistory(t *testing.T) {
	f := testsupport.New(t)
	riderID, _ := f.InsertRider()
	driverID, _ := f.InsertDriver("mini", "available")
	quoteID := f.InsertQuote(riderID)

	// A completed+paid ride (with driver + receipt) and a plain requested ride.
	paidRide := f.InsertRide(riderID, quoteID, "mini", string(rides.StatusCompleted), &driverID, nil)
	if _, err := f.Store.PG.Exec(f.Ctx, `UPDATE rides SET fare_total = 22000 WHERE id = $1`, paidRide); err != nil {
		t.Fatalf("set fare: %v", err)
	}
	insertEndedTrip(t, f, paidRide)
	ref := uuid.NewString()
	insertPayment(t, f, paidRide, payments.StatusProcessing, 0, &ref)
	if err := f.Payments.HandleWebhook(f.Ctx, ref, "success"); err != nil {
		t.Fatalf("settle payment: %v", err)
	}
	f.InsertRide(riderID, quoteID, "mini", string(rides.StatusRequested), nil, nil)

	items, err := f.Payments.History(f.Ctx, riderID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("history len = %d, want 2", len(items))
	}
	// Locate the paid ride and assert its driver + receipt are populated.
	var found bool
	for _, it := range items {
		if it.RideID == paidRide {
			found = true
			if it.Driver == nil || it.Driver.Name == "" {
				t.Errorf("paid ride missing driver view: %+v", it.Driver)
			}
			if it.Receipt == nil || it.Receipt.Total != 22000 {
				t.Errorf("paid ride missing/incorrect receipt: %+v", it.Receipt)
			}
			if it.FareTotal == nil || *it.FareTotal != 22000 {
				t.Errorf("fare_total = %v, want 22000", it.FareTotal)
			}
		}
	}
	if !found {
		t.Errorf("paid ride not present in history")
	}

	// Empty history for a rider with no rides.
	other, _ := f.InsertRider()
	empty, err := f.Payments.History(f.Ctx, other)
	if err != nil {
		t.Fatalf("History empty: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("expected empty history, got %d", len(empty))
	}
}
