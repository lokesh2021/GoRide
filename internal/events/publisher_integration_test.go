//go:build integration

// Integration tests for the events Publisher and Hub against live Redis, wired
// through the shared testsupport fixture. The Publisher tests subscribe to the
// real pub/sub channel and assert the marshaled SPEC envelope shape; the Hub
// tests drive a real SUBSCRIBE → SSE-frame path. Pure envelope/frame formatting
// is covered untagged in events_test.go / hub_test.go.
package events_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/lokeshbm/goride/internal/events"
	"github.com/lokeshbm/goride/internal/testsupport"
)

func waitMsg(t *testing.T, ch <-chan *redis.Message) map[string]any {
	t.Helper()
	select {
	case msg := <-ch:
		var env map[string]any
		if err := json.Unmarshal([]byte(msg.Payload), &env); err != nil {
			t.Fatalf("unmarshal %q: %v", msg.Payload, err)
		}
		return env
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for published message")
		return nil
	}
}

func TestPublishRideEvent(t *testing.T) {
	f := testsupport.New(t)
	rideID := uuid.NewString()
	f.TrackRedisKey(events.RideChannel(rideID)) // pub/sub, but harmless to track

	sub := f.Store.Redis.Subscribe(f.Ctx, events.RideChannel(rideID))
	defer sub.Close()
	if _, err := sub.Receive(f.Ctx); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	ch := sub.Channel()

	if err := f.Events.PublishRideEvent(f.Ctx, rideID, "ride.status_changed",
		map[string]any{"status": "DRIVER_ASSIGNED"}); err != nil {
		t.Fatalf("PublishRideEvent: %v", err)
	}

	env := waitMsg(t, ch)
	if env["type"] != "ride.status_changed" {
		t.Errorf("type = %v, want ride.status_changed", env["type"])
	}
	if env["ride_id"] != rideID {
		t.Errorf("ride_id = %v, want %s", env["ride_id"], rideID)
	}
	if _, ok := env["ts"]; !ok {
		t.Error("ts missing")
	}
	if ts, _ := env["ts"].(string); ts == "" {
		t.Error("ts empty")
	}
	data, ok := env["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %v, want object", env["data"])
	}
	if data["status"] != "DRIVER_ASSIGNED" {
		t.Errorf("data.status = %v, want DRIVER_ASSIGNED", data["status"])
	}
}

func TestPublishDriverEvent(t *testing.T) {
	f := testsupport.New(t)
	driverID := uuid.NewString()

	sub := f.Store.Redis.Subscribe(f.Ctx, events.DriverChannel(driverID))
	defer sub.Close()
	if _, err := sub.Receive(f.Ctx); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	ch := sub.Channel()

	if err := f.Events.PublishDriverEvent(f.Ctx, driverID, "ride.offer",
		map[string]any{"ride_id": "r1"}); err != nil {
		t.Fatalf("PublishDriverEvent: %v", err)
	}

	env := waitMsg(t, ch)
	if env["type"] != "ride.offer" {
		t.Errorf("type = %v, want ride.offer", env["type"])
	}
	// Driver-channel envelopes carry an empty top-level ride_id (see Envelope doc).
	if env["ride_id"] != "" {
		t.Errorf("ride_id = %v, want empty", env["ride_id"])
	}
	data, ok := env["data"].(map[string]any)
	if !ok || data["ride_id"] != "r1" {
		t.Errorf("data = %v, want ride_id r1 inside", env["data"])
	}
}

// lockedBuffer is a concurrency-safe io.Writer for reading Serve's output from
// the test goroutine while Serve writes from its own.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForSubscriber polls until Redis reports at least one subscriber on
// channel, so a publish is not raced ahead of Serve's SUBSCRIBE.
func waitForSubscriber(t *testing.T, rdb *redis.Client, ctx context.Context, channel string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		counts, err := rdb.PubSubNumSub(ctx, channel).Result()
		if err == nil && counts[channel] >= 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no subscriber on %s within deadline", channel)
}

func TestHubServeDeliversFrameThenClosesOnCancel(t *testing.T) {
	f := testsupport.New(t)
	hub := events.NewHub(f.Ctx, f.Store.Redis, f.Log)

	rideID := uuid.NewString()
	channel := events.RideChannel(rideID)

	reqCtx, cancel := context.WithCancel(f.Ctx)
	var flushes int
	var flushMu sync.Mutex
	flush := func() {
		flushMu.Lock()
		flushes++
		flushMu.Unlock()
	}
	lb := &lockedBuffer{}

	done := make(chan error, 1)
	go func() { done <- hub.Serve(reqCtx, lb, flush, channel) }()

	waitForSubscriber(t, f.Store.Redis, f.Ctx, channel)

	if err := f.Events.PublishRideEvent(f.Ctx, rideID, "ride.status_changed",
		map[string]any{"status": "ARRIVED"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Poll for the SSE frame.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(lb.String(), "ARRIVED") {
		time.Sleep(20 * time.Millisecond)
	}
	out := lb.String()
	if !strings.Contains(out, "event: ride.status_changed") {
		t.Errorf("frame missing event line:\n%s", out)
	}
	if !strings.Contains(out, "data: ") || !strings.Contains(out, "ARRIVED") {
		t.Errorf("frame missing data:\n%s", out)
	}
	if !strings.HasSuffix(out, "\n\n") {
		t.Errorf("frame not terminated by blank line:\n%q", out)
	}
	flushMu.Lock()
	if flushes < 1 {
		t.Error("flush never called after a frame")
	}
	flushMu.Unlock()

	// Client disconnect: cancelling reqCtx returns Serve cleanly.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v, want nil on client cancel", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

// TestHubServeHeartbeat covers the 15s heartbeat branch: with no messages
// published, Serve must still emit a `: ping` comment frame and flush. Slow by
// nature (one real heartbeat interval); gated to the integration suite.
func TestHubServeHeartbeat(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 15s heartbeat test in -short mode")
	}
	f := testsupport.New(t)
	hub := events.NewHub(f.Ctx, f.Store.Redis, f.Log)
	channel := events.RideChannel(uuid.NewString())

	reqCtx, cancel := context.WithCancel(f.Ctx)
	defer cancel()
	lb := &lockedBuffer{}
	flushed := make(chan struct{}, 8)
	done := make(chan error, 1)
	go func() {
		done <- hub.Serve(reqCtx, lb, func() {
			select {
			case flushed <- struct{}{}:
			default:
			}
		}, channel)
	}()

	waitForSubscriber(t, f.Store.Redis, f.Ctx, channel)

	// Heartbeat fires at 15s; allow generous slack.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(lb.String(), ": ping") {
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(lb.String(), ": ping\n\n") {
		t.Errorf("no heartbeat frame emitted:\n%q", lb.String())
	}
	select {
	case <-flushed:
	default:
		t.Error("flush not called for heartbeat")
	}
}

// TestPublishMarshalFailure covers the publish marshal-error branch: an
// unmarshalable Data (a channel) is log-warned and swallowed, never propagated
// or PUBLISHed. The fire-and-forget contract means the caller still sees nil.
func TestPublishMarshalFailure(t *testing.T) {
	f := testsupport.New(t)
	if err := f.Events.PublishRideEvent(f.Ctx, uuid.NewString(), "ride.status_changed", make(chan int)); err != nil {
		t.Errorf("PublishRideEvent returned %v, want nil (swallowed)", err)
	}
}

// errWriter fails every write, exercising Serve's write-frame error path.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }

func TestHubServeWriteError(t *testing.T) {
	f := testsupport.New(t)
	hub := events.NewHub(f.Ctx, f.Store.Redis, f.Log)
	rideID := uuid.NewString()
	channel := events.RideChannel(rideID)

	reqCtx, cancel := context.WithCancel(f.Ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- hub.Serve(reqCtx, errWriter{}, func() {}, channel) }()

	waitForSubscriber(t, f.Store.Redis, f.Ctx, channel)
	if err := f.Events.PublishRideEvent(f.Ctx, rideID, "ride.status_changed",
		map[string]any{"status": "ARRIVED"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Error("Serve returned nil, want a write-frame error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after write error")
	}
}

// TestHubServeClosesOnServerShutdown proves the root (server-lifetime) context
// also cuts an in-flight stream even when the request context stays open.
func TestHubServeClosesOnServerShutdown(t *testing.T) {
	f := testsupport.New(t)
	rootCtx, shutdown := context.WithCancel(f.Ctx)
	hub := events.NewHub(rootCtx, f.Store.Redis, f.Log)

	channel := events.RideChannel(uuid.NewString())
	done := make(chan error, 1)
	go func() { done <- hub.Serve(context.Background(), &lockedBuffer{}, func() {}, channel) }()

	waitForSubscriber(t, f.Store.Redis, f.Ctx, channel)
	shutdown()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v, want nil on shutdown", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return on server shutdown")
	}
}
