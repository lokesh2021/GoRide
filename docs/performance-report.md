# GoRide — Performance Report

## Methodology

Load generated with k6 (`loadtest/mixed.js`) against a single API instance on an M-series MacBook Air (Postgres 16 + Redis 7 local). The scenario mirrors the assignment's traffic shape, scaled to demo hardware:

- **30 driver VUs** — 1 location ping/sec each (the dominant write path), going online first
- **10 rider VUs** — quote → book → 3× status poll → cancel loop
- 3-minute window, ~33 req/s sustained, ~6,300 requests
- Load identities run in a dedicated city shard (`LDT`) so runs are reproducible and never disturb demo data — provision with `loadtest/seed_load.sql`, remove with `clean_load.sql`

Thresholds encode the latency targets and fail the run if breached.

## Baseline results (all thresholds green)

| Path | p50 | p90 | p95 | max | Target | Verdict |
|---|---|---|---|---|---|---|
| `POST /v1/drivers/{id}/location` | 3.8ms | 5.9ms | **6.8ms** | 15.3ms | <50ms | ✅ 7× headroom |
| `POST /v1/quotes` | 3.2ms | 6.2ms | **7.4ms** | 15.8ms | <150ms | ✅ 20× headroom |
| `POST /v1/rides` (book) | 5.0ms | 6.5ms | **7.1ms** | 10.2ms | <200ms | ✅ 28× headroom |
| `GET /v1/rides/{id}` (cached) | 0.7ms | 1.8ms | **2.4ms** | 5.3ms | <50ms | ✅ 20× headroom |
| Booking error rate | | | | | <5% | ✅ 0.00% |

Reading the shape:
- **Status reads (2.4ms p95) vs quotes (7.4ms p95)** shows the Redis read-through cache working — cached reads are ~3× faster than the cheapest DB-touching write despite both being "small" operations.
- **Location pings at 6.8ms p95** include auth (one indexed Postgres SELECT) plus the 2 pipelined Redis round-trips. The domain part is sub-millisecond; auth dominates. At production scale the token lookup becomes a Redis-cached session check — documented in HLD §2.1.
- **Booking at 7.1ms p95** covers idempotency-key bookkeeping, quote validation, the guarded INSERT with partial-unique-index check, and the synchronous matching kick. Well under the 200ms budget, leaving room for the 1s end-to-end match target even with offer round-trips.

## Capacity probe — how hard one local instance takes it (`loadtest/capacity.js`)

The baseline above is a steady, watchable workload. This probe answers the
harder question — *how much can a single local instance actually sustain?* — by
ramping a mixed arrival rate (60% location pings, 25% authenticated reads, 15%
quotes — the heaviest DB path) from 50 → **800 req/s** in held steps over ~2.7
minutes, against a 200-driver / 20-rider seeded pool in the `LDT` shard.

**Result: the server never saturated.** 58,226 requests at a mean 364 req/s
(peak arrival target 800 req/s), latency flat across the entire ramp:

| Path | p50 | p90 | p95 | Notes |
|---|---|---|---|---|
| Location ping | 0.53ms | 0.87ms | **0.98ms** | Redis-only hot path |
| Authenticated read | 0.47ms | 0.77ms | **0.88ms** | |
| Quote (heaviest) | 1.22ms | 1.93ms | **2.19ms** | estimate + surge + insert |
| **All requests** | 0.59ms | 1.2ms | **1.68ms** | |

- **Zero server errors (0 × 5xx).** k6 checks: 100% passed.
- **~5% of responses were `429`** — the per-driver ping limiter (3/s) shedding
  brief bursts at peak when random distribution briefly pushed a driver over
  cap. That is correct back-pressure, not failure — and it's *why* the server
  stayed flat: excess load is rejected cheaply before it touches Postgres.
- **23 dropped iterations** (0.14/s) — negligible k6-side scheduling at peak.

The honest reading: on one laptop (one Go process, one Postgres, one Redis) the
relational and cache paths are nowhere near their knee at 800 req/s — p95 held
under 2.2ms on every path. The ceiling we *can* observe locally isn't the app;
it's the single-node hardware and the (deliberate) per-driver rate limit.

### Measured locally vs. projected at assignment scale

| Target (assignment §2) | Measured locally | Path to the target |
|---|---|---|
| ~10k ride requests/min (~167/s) | 800 req/s mixed sustained at p95 <2.2ms, single instance | Already exceeded on one box; N stateless instances behind an LB scale linearly |
| ~200k location updates/sec | ping p95 <1ms; path is 2 Redis round-trips, **zero Postgres** | City-sharded Redis (`geo:{city}`) → cluster; ~1.2M Redis ops/s across a small cluster (HLD §2.1) |
| ~100k drivers | — (pool of 200 here) | Drivers are Redis GEO members + one profile row; no per-driver hot Postgres path |
| Matching ≤1s p95 | request→offer well under 1s at demo load | GEOSEARCH on the city shard is ~1ms; accept is one short tx |

What is **measured** (defensible with the numbers above): single-instance
latency and that it doesn't degrade to 800 req/s. What is **projected** (argued
from architecture, not load-proven at that scale): the 200k/s and 100k-driver
figures — those need horizontal fan-out and a Redis cluster no laptop can
stand in for. The design makes the projection credible; the report does not
claim it as measured.

## Optimizations already baked in (design-time, assignment §4)

| Technique | Where |
|---|---|
| DB indexing | Partial unique indexes double as the hot "active ride" lookup; `(status, created_at)` sweeper index; `(rider_id, created_at DESC)` history index; unique token + psp_ref indexes |
| Caching | Ride view read-through cache (write-through invalidation in the transition funnel); Redis mirrors keep Postgres out of the matching search loop entirely |
| Query shape | No N+1 anywhere; single-tx guarded UPDATEs for state changes; driver card joined in one query |
| Concurrency | Row locks held microseconds (guarded UPDATE pattern); `SKIP LOCKED` sweeper; per-driver rate limiting at the cheapest point |
| Connection pooling | pgx pool sized 4×CPU with health-checked idle recycling |

## New Relic

Instrumentation is wired and **live** (PR #10 + logging PR #18): route-pattern
APM transactions, pgx + Redis datastore segments, custom matching/payment
metrics, and — with the logging work — application **logs forwarded in context**
(linked to the owning trace via `nrslog`). The license key in `.env` is active;
the load runs above pushed **>7,400 real requests** into the account, so APM has
data to show.

What to capture for the deliverable (steps in `docs/newrelic-setup.md`):
- Per-endpoint p95 dashboard + throughput/error panels (APM → Transactions)
- Slow-query traces (datastore segment breakdown per transaction)
- `Custom/Matching/OfferLatencyMs` chart vs the 1s match target
- Logs-in-context view (a transaction with its correlated log lines)
- An alert policy on hot-endpoint p95 breach

These are UI screenshots only — the data is already flowing. Rotate the license
key after submission (it was handled locally and is gitignored, never committed).
