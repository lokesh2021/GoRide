// Package events is the M5 real-time fan-out: a Publisher that marshals the
// SPEC event envelope and PUBLISHes it to Redis, and a Hub that subscribes to
// those channels on behalf of SSE clients (see hub.go).
//
// Redis key contract (SPEC "Redis key contract"): pub/sub channels
// `events:ride:{ride_id}` (ride lifecycle + throttled driver location + OTP +
// payment updates) and `events:driver:{driver_id}` (offers). Any instance can
// publish to, or subscribe from, either channel — this is what lets any
// instance serve any client's stream regardless of which instance processed
// the underlying state change.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// RideChannel returns the pub/sub channel a ride's events are published to.
func RideChannel(rideID string) string { return rideChannelPrefix + rideID }

// DriverChannel returns the pub/sub channel a driver's events are published to.
func DriverChannel(driverID string) string { return driverChannelPrefix + driverID }

// Envelope is the SPEC SSE event envelope: {"type","ride_id","data","ts"}.
// It is marshaled verbatim as both the Redis pub/sub payload and the SSE
// `data:` frame the client receives — the wire format is identical end to end.
//
// RideID is populated for ride-channel events (PublishRideEvent); it is left
// empty for driver-channel events (PublishDriverEvent), which are not scoped
// to a single ride at the envelope level — where a driver event does concern
// a specific ride (e.g. ride.offer), that ride_id already travels inside Data.
type Envelope struct {
	Type   string `json:"type"`
	RideID string `json:"ride_id"`
	Data   any    `json:"data"`
	Ts     string `json:"ts"`
}

// Publisher implements rides.EventPublisher (PublishRideEvent/PublishDriverEvent)
// and drivers.RidePublisher (PublishRideEvent) — both are small structural
// interfaces satisfied by these two methods, so this one type wires into both
// domain packages with no import cycle.
type Publisher struct {
	rdb *redis.Client
	log *slog.Logger
}

// NewPublisher constructs a Publisher over the shared Redis client.
func NewPublisher(rdb *redis.Client, log *slog.Logger) *Publisher {
	return &Publisher{rdb: rdb, log: log}
}

// PublishRideEvent marshals the envelope and PUBLISHes it to
// events:ride:{rideID}. Fire-and-forget: a marshal or Redis failure is
// log-warned and swallowed (returns nil) rather than propagated — a lost
// real-time notification must never fail the caller's write path.
func (p *Publisher) PublishRideEvent(ctx context.Context, rideID, eventType string, data any) error {
	p.publish(ctx, RideChannel(rideID), Envelope{
		Type:   eventType,
		RideID: rideID,
		Data:   data,
		Ts:     nowRFC3339(),
	})
	return nil
}

// PublishDriverEvent marshals the envelope and PUBLISHes it to
// events:driver:{driverID}. Same fire-and-forget contract as PublishRideEvent.
func (p *Publisher) PublishDriverEvent(ctx context.Context, driverID, eventType string, data any) error {
	p.publish(ctx, DriverChannel(driverID), Envelope{
		Type: eventType,
		Data: data,
		Ts:   nowRFC3339(),
	})
	return nil
}

// publish is the single PUBLISH call site: marshal then fire one Redis
// command. Never returns an error to keep every caller's write path fast and
// unconditional.
func (p *Publisher) publish(ctx context.Context, channel string, env Envelope) {
	raw, err := json.Marshal(env)
	if err != nil {
		p.log.Warn(logMsgMarshalEnvelopeFailed, "error", err, "channel", channel, "type", env.Type)
		return
	}
	if err := p.rdb.Publish(ctx, channel, raw).Err(); err != nil {
		p.log.Warn(logMsgPublishFailed, "error", err, "channel", channel, "type", env.Type)
	}
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
