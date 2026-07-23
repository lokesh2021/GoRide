package events

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Hub streams Redis pub/sub channels to SSE clients, one subscription per HTTP
// connection.
//
// Scale tradeoff (documented, not built): go-redis's Subscribe leases a
// dedicated connection from the Redis client for the lifetime of the
// subscription, so this is one Redis connection per concurrently-streaming
// HTTP client. That is fine at demo scale (a handful of concurrent riders/
// drivers watching a stream) and keeps the code simple — no shared registry,
// no fan-out bookkeeping. The scale evolution is a single shared
// PSUBSCRIBE("events:*") subscription per instance feeding an in-process
// registry (channel -> set of subscriber chans) that fans one Redis message
// out to every local subscriber, amortizing one Redis connection across all
// of an instance's SSE clients.
type Hub struct {
	rdb *redis.Client
	log *slog.Logger
	// ctx is the server's root lifetime context (cancelled on SIGINT/SIGTERM
	// shutdown in cmd/server). Serve reacts to it in addition to the
	// caller-supplied per-request context, so an in-flight stream closes on
	// server shutdown even if the client never disconnects — without that,
	// http.Server.Shutdown would block waiting for a connection that never
	// goes idle.
	ctx context.Context
}

// NewHub constructs a Hub. ctx is the application's root context so that
// streams are cut on server shutdown (see the Hub doc).
func NewHub(ctx context.Context, rdb *redis.Client, log *slog.Logger) *Hub {
	return &Hub{ctx: ctx, rdb: rdb, log: log}
}

// Serve subscribes to channel and copies every message to w as an SSE frame
// until the client disconnects (reqCtx.Done()), the server shuts down
// (h.ctx.Done()), or the subscription itself errors. It writes a heartbeat
// comment frame every 15s. flush is called after every frame (including
// heartbeats) so proxies/clients see data immediately rather than buffered.
func (h *Hub) Serve(reqCtx context.Context, w io.Writer, flush func(), channel string) error {
	ctx, cancel := context.WithCancel(reqCtx)
	defer cancel()
	// Fold the server's shutdown signal into this stream's lifetime without
	// tying every other request's context to it (see Hub.ctx doc).
	go func() {
		select {
		case <-h.ctx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	sub := h.rdb.Subscribe(ctx, channel)
	defer func() {
		if err := sub.Close(); err != nil {
			h.log.Warn(logMsgSubscriptionCloseFailed, "error", err, "channel", channel)
		}
	}()

	// Receive confirms the SUBSCRIBE has actually been acknowledged by Redis,
	// surfacing a connectivity failure immediately instead of silently
	// hanging with a stream that will never deliver anything.
	if _, err := sub.Receive(ctx); err != nil {
		return fmt.Errorf("events: subscribe %s: %w", channel, err)
	}

	msgs := sub.Channel()
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-msgs:
			if !ok {
				return nil
			}
			if _, err := io.WriteString(w, FormatFrame(eventType(msg.Payload), msg.Payload)); err != nil {
				return fmt.Errorf("events: write frame: %w", err)
			}
			flush()
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return fmt.Errorf("events: write heartbeat: %w", err)
			}
			flush()
		}
	}
}

// FormatFrame renders one SSE frame: an `event:` line naming the envelope
// type, a `data:` line carrying the raw (already-JSON) payload verbatim, and
// the trailing blank line that terminates an SSE frame. Pure and exported for
// unit testing.
func FormatFrame(eventType, payload string) string {
	return "event: " + eventType + "\ndata: " + payload + "\n\n"
}

// eventType best-effort extracts the envelope's "type" field from a raw
// pub/sub payload, for use as the SSE `event:` line. An unparseable payload
// (should not happen — we are the only publisher) still streams: it comes
// through as the SSE default event (empty event: line, data intact).
func eventType(payload string) string {
	var probe struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(payload), &probe)
	return probe.Type
}
