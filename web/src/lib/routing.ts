// Road-following routes from the PUBLIC OSRM demo server.
//
// Best-effort only: ANY failure (network error, 4s timeout, non-OK status,
// empty geometry, offline) returns null and every caller falls back to the
// local buildRoute / straight-line interpolation. The demo must keep working
// fully offline — OSRM is an enhancement, never a dependency.
//
// Fair-use note: router.project-osrm.org is a free community *demo* tier — do
// not hammer it. The k6/loadtest bots are server-side identities that never
// load this frontend and MUST NOT call OSRM; only the in-app simulator bots
// touch it, and they go through fetchRoadThrottled (a small global concurrency
// gate) and reuse the shared in-memory cache, so 5 bots stay well within
// fair-use.

import type { LatLng } from "./geo";

export interface RoadRoute {
  /** Decoded polyline, [lat,lng] points, pickup → drop. */
  path: LatLng[];
  /** Road distance in metres. */
  distanceM: number;
  /** Estimated driving duration in seconds (OSRM's own estimate). */
  durationS: number;
}

const OSRM_BASE = "https://router.project-osrm.org/route/v1/driving";
const TIMEOUT_MS = 4000;

// Success cache keyed by endpoints rounded to 4 decimals (~11 m). Nulls are not
// cached so a transient outage can recover on a later call; the throttle gate
// (below) is what protects the endpoint from bursts.
const cache = new Map<string, RoadRoute>();
// De-dupe concurrent identical requests (both panels + bots can ask at once).
const inflight = new Map<string, Promise<RoadRoute | null>>();

function key(a: LatLng, b: LatLng): string {
  const r = (n: number) => n.toFixed(4);
  return `${r(a[0])},${r(a[1])};${r(b[0])},${r(b[1])}`;
}

// fetchRoad returns a road-following route between two points, or null on any
// failure. Cached successes and in-flight requests are reused.
export async function fetchRoad(a: LatLng, b: LatLng): Promise<RoadRoute | null> {
  const k = key(a, b);
  const cached = cache.get(k);
  if (cached) return cached;
  const pending = inflight.get(k);
  if (pending) return pending;

  const p = doFetch(a, b, k);
  inflight.set(k, p);
  try {
    return await p;
  } finally {
    inflight.delete(k);
  }
}

async function doFetch(a: LatLng, b: LatLng, k: string): Promise<RoadRoute | null> {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), TIMEOUT_MS);
  try {
    // OSRM wants lng,lat order.
    const url = `${OSRM_BASE}/${a[1]},${a[0]};${b[1]},${b[0]}?overview=full&geometries=geojson`;
    const res = await fetch(url, { signal: ctrl.signal });
    if (!res.ok) return null;
    const json = (await res.json()) as {
      routes?: { distance: number; duration: number; geometry: { coordinates: [number, number][] } }[];
    };
    const route = json.routes?.[0];
    const coords = route?.geometry?.coordinates;
    if (!route || !coords || coords.length < 2) return null;
    const path: LatLng[] = coords.map(([lng, lat]) => [lat, lng]);
    const out: RoadRoute = {
      path,
      distanceM: Math.round(route.distance),
      durationS: Math.round(route.duration),
    };
    cache.set(k, out);
    return out;
  } catch {
    // Timeout / abort / network / parse — fall back.
    return null;
  } finally {
    clearTimeout(timer);
  }
}

// ---- concurrency gate for background (simulator) callers ----

const MAX_CONCURRENT = 2;
let active = 0;
const waiters: (() => void)[] = [];

function acquire(): Promise<void> {
  if (active < MAX_CONCURRENT) {
    active++;
    return Promise.resolve();
  }
  return new Promise((resolve) => waiters.push(resolve));
}

function release() {
  const next = waiters.shift();
  if (next) next();
  else active--;
}

// fetchRoadThrottled is the bot-safe entry point: cached routes return
// immediately (no gate); uncached ones queue behind at most MAX_CONCURRENT
// concurrent OSRM connections so a fleet of bots can't burst the demo server.
export async function fetchRoadThrottled(a: LatLng, b: LatLng): Promise<RoadRoute | null> {
  const cached = cache.get(key(a, b));
  if (cached) return cached;
  await acquire();
  try {
    return await fetchRoad(a, b);
  } finally {
    release();
  }
}
