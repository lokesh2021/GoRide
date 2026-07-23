// SSE client built on fetch + ReadableStream rather than the native
// EventSource. Reason: EventSource cannot send an Authorization header, and the
// backend authenticates every /v1 route (including the event streams) with
// `Authorization: Bearer <token>`. fetch lets us attach the header and still
// consume text/event-stream incrementally. Works through the Vite /v1 proxy.
//
// Frame format produced by the backend (internal/events/hub.go FormatFrame):
//   event: <type>\ndata: <json>\n\n
// plus `: ping\n\n` heartbeats every 15s. We parse on blank-line boundaries and
// hand back the parsed JSON envelope (the `type` also lives inside data).

import type { SSEEnvelope } from "../api/types";

export interface SSEHandle {
  close: () => void;
}

export type StreamStatus = "open" | "reconnecting";

interface StreamOpts {
  onEvent: (env: SSEEnvelope) => void;
  onError?: (err: unknown) => void;
  onOpen?: () => void;
  /** Connection status transitions, for a "Reconnecting…" indicator. */
  onStatus?: (s: StreamStatus) => void;
}

// openStream connects to an SSE path with the given bearer token and invokes
// onEvent for every parsed envelope. It auto-reconnects with a short backoff
// until close() is called.
export function openStream(path: string, token: string, opts: StreamOpts): SSEHandle {
  let closed = false;
  let controller: AbortController | null = null;
  let retryTimer: ReturnType<typeof setTimeout> | null = null;

  const connect = async () => {
    if (closed) return;
    controller = new AbortController();
    try {
      const res = await fetch(path, {
        headers: { Authorization: `Bearer ${token}`, Accept: "text/event-stream" },
        signal: controller.signal,
      });
      if (!res.ok || !res.body) {
        throw new Error(`SSE ${path} failed: ${res.status}`);
      }
      opts.onOpen?.();
      opts.onStatus?.("open");

      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";

      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });

        // Split complete frames (terminated by a blank line).
        let sep: number;
        while ((sep = buffer.indexOf("\n\n")) !== -1) {
          const frame = buffer.slice(0, sep);
          buffer = buffer.slice(sep + 2);
          handleFrame(frame, opts.onEvent);
        }
      }
      // Stream ended cleanly (server closed) — reconnect unless we're done.
      throw new Error("SSE stream ended");
    } catch (err) {
      if (closed) return;
      opts.onError?.(err);
      opts.onStatus?.("reconnecting");
      // Reconnect after a short delay.
      retryTimer = setTimeout(connect, 1500);
    }
  };

  connect();

  return {
    close: () => {
      closed = true;
      if (retryTimer) clearTimeout(retryTimer);
      controller?.abort();
    },
  };
}

function handleFrame(frame: string, onEvent: (env: SSEEnvelope) => void) {
  // Collect data: lines; ignore event:/id:/comment lines (the type is inside
  // the JSON payload too, which is what we rely on).
  const dataLines: string[] = [];
  for (const line of frame.split("\n")) {
    if (line.startsWith(":")) continue; // heartbeat / comment
    if (line.startsWith("data:")) dataLines.push(line.slice(5).replace(/^ /, ""));
  }
  if (dataLines.length === 0) return;
  const payload = dataLines.join("\n");
  try {
    const env = JSON.parse(payload) as SSEEnvelope;
    onEvent(env);
  } catch {
    // Ignore unparseable payloads (should not happen — we are the only source).
  }
}
