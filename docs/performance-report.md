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

## Optimizations already baked in (design-time, assignment §4)

| Technique | Where |
|---|---|
| DB indexing | Partial unique indexes double as the hot "active ride" lookup; `(status, created_at)` sweeper index; `(rider_id, created_at DESC)` history index; unique token + psp_ref indexes |
| Caching | Ride view read-through cache (write-through invalidation in the transition funnel); Redis mirrors keep Postgres out of the matching search loop entirely |
| Query shape | No N+1 anywhere; single-tx guarded UPDATEs for state changes; driver card joined in one query |
| Concurrency | Row locks held microseconds (guarded UPDATE pattern); `SKIP LOCKED` sweeper; per-driver rate limiting at the cheapest point |
| Connection pooling | pgx pool sized 4×CPU with health-checked idle recycling |

## New Relic

Instrumentation is fully wired (PR #10): route-pattern transactions, pgx + Redis datastore segments, custom matching/payment metrics. **Dashboard screenshots and alert-policy evidence pend a license key** — once `GORIDE_NEWRELIC_LICENSE` is set, rerunning the k6 scenario populates APM within minutes; this section will be updated with:
- Per-endpoint p95 dashboard + throughput/error panels
- Slow-query traces (datastore segment breakdown per transaction)
- `Custom/Matching/OfferLatencyMs` chart vs the 1s match target
- Alert policy on hot-endpoint p95 breach
