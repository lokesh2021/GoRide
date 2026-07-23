package events

import "time"

// ---- domain constants ----

// heartbeatInterval is how often Serve writes a comment frame to keep idle
// connections (and any proxy in between) alive.
const heartbeatInterval = 15 * time.Second

// ---- Redis key prefixes/builders ----

const (
	rideChannelPrefix   = "events:ride:"
	driverChannelPrefix = "events:driver:"
)

// ---- log messages ----

const (
	logMsgMarshalEnvelopeFailed   = "events: marshal envelope failed"
	logMsgPublishFailed           = "events: publish failed"
	logMsgSubscriptionCloseFailed = "events: subscription close failed"
)
