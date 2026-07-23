// GoRide mixed-workload load test.
//
// Simulates the assignment's traffic shape at local scale:
//   - drivers streaming location pings (the dominant write path)
//   - riders requesting quotes and booking rides
//   - riders polling ride status (cache-served reads)
//
// Usage:
//   k6 run loadtest/mixed.js                          # default: moderate local load
//   k6 run -e BASE=http://localhost:8080 -e DRIVERS=50 -e VUS_RIDERS=20 loadtest/mixed.js
//
// Seed data provides rider1/rider2 and driver1..driver6 tokens; additional
// load identities must be provisioned with loadtest/seed_load.sql first
// (creates load-test riders/drivers with tokens riderload-N / driverload-N).

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Rate } from 'k6/metrics';
import exec from 'k6/execution';

const BASE = __ENV.BASE || 'http://localhost:8080';
const N_DRIVERS = parseInt(__ENV.DRIVERS || '30');
const N_RIDERS = parseInt(__ENV.VUS_RIDERS || '10');

// How many identities loadtest/seed_load.sql actually provisions. VU IDs are
// mapped into these ranges with modulo so every VU resolves to a real seeded
// token regardless of how k6 numbers VUs across scenarios (idInTest is NOT
// guaranteed contiguous per scenario — assuming so caused spurious 401s).
const SEEDED_DRIVERS = 50;
const SEEDED_RIDERS = 20;

const quoteLatency = new Trend('goride_quote_latency', true);
const bookLatency = new Trend('goride_book_latency', true);
const statusLatency = new Trend('goride_status_latency', true);
const pingLatency = new Trend('goride_ping_latency', true);
const bookErrors = new Rate('goride_book_errors');

// Bengaluru bounding box for plausible coordinates.
const BLR = { latMin: 12.85, latMax: 13.05, lngMin: 77.5, lngMax: 77.75 };
const rnd = (min, max) => min + Math.random() * (max - min);
const point = () => ({ lat: rnd(BLR.latMin, BLR.latMax), lng: rnd(BLR.lngMin, BLR.lngMax) });

export const options = {
  scenarios: {
    driver_pings: {
      executor: 'constant-vus',
      exec: 'driverLoop',
      vus: N_DRIVERS,
      duration: '3m',
    },
    rider_flow: {
      executor: 'constant-vus',
      exec: 'riderLoop',
      vus: N_RIDERS,
      duration: '3m',
      startTime: '10s',
    },
  },
  thresholds: {
    goride_ping_latency: ['p(95)<50'],
    goride_quote_latency: ['p(95)<150'],
    goride_book_latency: ['p(95)<200'],
    goride_status_latency: ['p(95)<50'],
    goride_book_errors: ['rate<0.05'],
  },
};

function authHeaders(token) {
  return { headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` } };
}

// Each driver VU: go online once, then ping ~1/sec drifting around the city.
export function driverLoop() {
  // Map this VU into the seeded driver range (1..SEEDED_DRIVERS) so the token
  // always exists, whatever idInTest k6 assigned.
  const vu = ((exec.vu.idInTest - 1) % SEEDED_DRIVERS) + 1;
  const id = `30000000-0000-0000-0000-${String(vu).padStart(12, '0')}`;
  const token = `driverload-${vu}-token`;
  const me = point();

  if (__ITER === 0) {
    http.post(`${BASE}/v1/drivers/${id}/availability`, JSON.stringify({ available: true }), authHeaders(token));
  }
  me.lat += rnd(-0.0005, 0.0005);
  me.lng += rnd(-0.0005, 0.0005);
  const res = http.post(`${BASE}/v1/drivers/${id}/location`, JSON.stringify(me), authHeaders(token));
  pingLatency.add(res.timings.duration);
  check(res, { 'ping 2xx/429': (r) => r.status === 200 || r.status === 429 });
  sleep(1);
}

// Each rider VU: quote → book → poll status a few times → cancel → repeat.
export function riderLoop() {
  // Map into the seeded rider range (1..SEEDED_RIDERS) — see driverLoop.
  const vu = ((exec.vu.idInTest - 1) % SEEDED_RIDERS) + 1;
  const token = `riderload-${vu}-token`;
  const h = authHeaders(token);

  // 'LDT' is the load test's own city shard — same code paths, but its geo
  // pool, surge cells and offers are fully isolated from the BLR demo city.
  const q = http.post(`${BASE}/v1/quotes`, JSON.stringify({ pickup: point(), drop: point(), city: 'LDT' }), h);
  quoteLatency.add(q.timings.duration);
  if (!check(q, { 'quote 200': (r) => r.status === 200 })) { sleep(2); return; }
  const quoteID = q.json('quote_id') || q.json('id');

  const bookHeaders = { headers: { ...h.headers, 'Idempotency-Key': `k6-${__VU}-${__ITER}` } };
  const b = http.post(`${BASE}/v1/rides`, JSON.stringify({ quote_id: quoteID, tier: 'mini', payment_method: 'upi' }), bookHeaders);
  bookLatency.add(b.timings.duration);
  // 409 RIDE_ALREADY_ACTIVE is expected when the previous iteration's ride is
  // still being matched/cancelled — not an error for threshold purposes.
  bookErrors.add(!(b.status === 201 || b.status === 409));
  if (b.status !== 201) { sleep(2); return; }
  const rideID = b.json('id') || b.json('ride_id');

  for (let i = 0; i < 3; i++) {
    const s = http.get(`${BASE}/v1/rides/${rideID}`, h);
    statusLatency.add(s.timings.duration);
    check(s, { 'status 200': (r) => r.status === 200 });
    sleep(1);
  }

  // Cancel so this rider can book again next iteration (unless a load driver
  // accepted and progressed it — cancel is only legal pre-IN_PROGRESS).
  http.post(`${BASE}/v1/rides/${rideID}/cancel`, JSON.stringify({ reason: 'load test' }), bookHeaders);
  sleep(1);
}
