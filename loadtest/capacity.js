// GoRide capacity probe — find the single-instance ceiling.
//
// Unlike mixed.js (a steady, watchable ~33 req/s workload), this ramps a
// mixed read/write arrival rate upward in steps and reports where p95 latency
// starts to degrade. It answers the review question honestly: "how much can
// ONE local instance actually sustain, and at what latency?"
//
// Requires the seeded LDT identities:
//   psql -d goride -f loadtest/seed_load.sql
//   k6 run loadtest/capacity.js
//
// Not a distributed benchmark — this is one Go process + one Postgres + one
// Redis on a laptop. The numbers are a floor, not a ceiling for the design
// (which shards horizontally); see docs/performance-report.md for framing.

import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';

const BASE = __ENV.BASE || 'http://localhost:8080';
// Match loadtest/seed_load.sql. A large driver pool keeps each driver's ping
// rate under the 3/s limiter during the ramp, so 429 back-pressure doesn't
// mask true throughput (a small pool makes the limiter, not the server, the
// ceiling — correct behaviour, but not what a capacity probe wants to measure).
const SEEDED_DRIVERS = 200;
const SEEDED_RIDERS = 20;

const quoteLat = new Trend('cap_quote_latency', true);
const pingLat = new Trend('cap_ping_latency', true);
const statusLat = new Trend('cap_status_latency', true);

const BLR = { latMin: 12.85, latMax: 13.05, lngMin: 77.5, lngMax: 77.75 };
const rnd = (a, b) => a + Math.random() * (b - a);
const pt = () => ({ lat: rnd(BLR.latMin, BLR.latMax), lng: rnd(BLR.lngMin, BLR.lngMax) });
const auth = (t) => ({ headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${t}` } });

// Steady ramp of total requests/sec. Each step holds 30s so percentiles settle.
export const options = {
  scenarios: {
    ramp: {
      executor: 'ramping-arrival-rate',
      startRate: 50,
      timeUnit: '1s',
      preAllocatedVUs: 120,
      maxVUs: 400,
      stages: [
        { target: 50, duration: '20s' },
        { target: 150, duration: '30s' },
        { target: 300, duration: '30s' },
        { target: 500, duration: '30s' },
        { target: 800, duration: '30s' },
        { target: 800, duration: '20s' },
      ],
    },
  },
  // Informational: the summary reports the actual p95 per path; we read the
  // knee from the per-stage behaviour rather than pass/fail here.
  thresholds: {
    cap_ping_latency: ['p(95)<100'],
    cap_quote_latency: ['p(95)<200'],
    cap_status_latency: ['p(95)<100'],
  },
};

// A weighted mix roughly matching real traffic shape: location pings dominate,
// then status reads, then quotes (the heaviest DB path).
export default function () {
  const roll = Math.random();
  if (roll < 0.6) {
    // location ping (driver) — the hottest path
    const v = Math.floor(Math.random() * SEEDED_DRIVERS) + 1;
    const id = `30000000-0000-0000-0000-${String(v).padStart(12, '0')}`;
    const r = http.post(`${BASE}/v1/drivers/${id}/location`, JSON.stringify(pt()), auth(`driverload-${v}-token`));
    pingLat.add(r.timings.duration);
    check(r, { 'ping ok/limited': (x) => x.status === 200 || x.status === 429 });
  } else if (roll < 0.85) {
    // status read (rider) — cache-served; needs a ride id, so fall back to a
    // cheap authenticated read that exercises auth + routing under load.
    const v = Math.floor(Math.random() * SEEDED_RIDERS) + 1;
    const r = http.get(`${BASE}/v1/riders/20000000-0000-0000-0000-${String(v).padStart(12, '0')}/rides`, auth(`riderload-${v}-token`));
    statusLat.add(r.timings.duration);
    check(r, { 'history ok': (x) => x.status === 200 });
  } else {
    // quote (rider) — the heaviest path (estimate + surge + insert)
    const v = Math.floor(Math.random() * SEEDED_RIDERS) + 1;
    const r = http.post(`${BASE}/v1/quotes`, JSON.stringify({ pickup: pt(), drop: pt(), city: 'LDT' }), auth(`riderload-${v}-token`));
    quoteLat.add(r.timings.duration);
    check(r, { 'quote 200': (x) => x.status === 200 });
  }
}
