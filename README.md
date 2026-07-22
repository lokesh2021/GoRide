# GoRide — Ride-Hailing System

A ride-hailing platform (think Uber/Ola) built for the SDE-2 assessment: upfront fare quotes, driver–rider matching, dynamic surge pricing, full trip lifecycle with OTP verification, payments, receipts, real-time tracking, and production-grade monitoring.

> **Status:** Planning. This README is the execution plan; implementation follows the milestones in [§10](#10-execution-milestones-pr-plan).

**Scale targets (from the assignment, kept in mind throughout):** ~100k drivers, ~10k ride requests/min, ~200k location updates/sec, matching within 1s p95.

---

## 1. Overview & Design Philosophy

GoRide is a **single modular monolith** (Go) with a React frontend that mirrors a real rider app's booking flow. The monolith is a deliberate choice, not a shortcut:

- The assignment explicitly allows synchronous APIs over queues/topics.
- A monolith with clean package boundaries demonstrates the same separation-of-concerns as microservices, with none of the ungraded operational overhead.
- **Every component is stateless and horizontally scalable** — all shared state lives in Postgres/Redis, so `N` instances behind a load balancer work with zero code changes (details in §3).
- Concerns that genuinely need infrastructure we won't deploy (multi-region, geo-sharding, real PSP) are addressed **in the HLD as design discussions** with a clear evolution path.

**Principle: build a product that feels real, on an architecture that scales horizontally, and show judgment about what *not* to build.**

## 2. Product Scope — What "Real Rider App" Means Here

The end-to-end journey, modeled on Uber/Ola:

**Rider journey**
1. Set pickup & destination on the map → get **upfront fare quotes** for all vehicle tiers (Mini / Sedan / XL) with ETA and surge shown transparently.
2. Book with a chosen quote → live **matching** ("finding your driver…").
3. Driver assigned → **driver card** (name, vehicle model, plate, rating) + live car position and pickup ETA on the map.
4. Driver arrives → rider shares **4-digit OTP** → trip starts only on OTP verification.
5. Live trip tracking → destination reached → **fare finalized** (base + distance + time × surge, from the locked quote).
6. Payment (mock UPI/card/cash) → **receipt** with fare breakdown → ride history.
7. Cancellation allowed pre-trip (with reason); driver can decline offers.

**Driver journey**
1. Go online → stream location pings (1–2/sec).
2. Receive **ride offer** with pickup distance and fare — accept/decline within a countdown (~10s).
3. Navigate to pickup → mark **arrived** → verify rider OTP → start trip.
4. End trip at destination → fare computed → next offer.

Everything above is implemented. What's *simulated*: the PSP (mock module with async webhooks), driver movement (bot drivers driving realistic routes in Bengaluru), and navigation (interpolated paths, no external routing API).

## 3. Architecture (HLD Summary)

Full HLD/LLD will live in `docs/`. The essentials:

### Package layout

```
goride/
├── cmd/server/            # main, wiring
├── internal/
│   ├── quotes/            # fare estimation, quote lock + expiry
│   ├── rides/             # ride lifecycle + state machine
│   ├── drivers/           # driver profile, availability, location ingestion
│   ├── matching/          # candidate search, offer/accept/decline/timeout
│   ├── trips/             # OTP verification, trip lifecycle, fare finalization
│   ├── pricing/           # surge computation, tier pricing
│   ├── payments/          # mock PSP + webhook handling, receipts
│   ├── events/            # SSE hub backed by Redis pub/sub
│   ├── store/             # Postgres + Redis access
│   └── httpapi/           # handlers, middleware (auth, validation, idempotency, rate limit)
├── migrations/
├── web/                   # React frontend (rider app + driver app + simulator)
├── loadtest/              # k6 scripts
└── docs/                  # HLD, LLD, performance report
```

### How each hot path meets the scale target

**Location ingestion — 200k updates/sec.** Pings go to Redis `GEOADD` only (latest position, TTL for staleness). The path is: validate → auth → one Redis write. **No Postgres write per ping**, no locks, O(1). Redis GEO keys are **sharded by city** (`geo:{city}`), which is also the natural multi-region evolution story. Per-driver rate limiting (token bucket in Redis) caps abusive clients. A single Redis node handles ~100k+ ops/sec; the HLD documents the shard-by-city / Redis Cluster path beyond that.

**Matching — 1s p95 for 10k requests/min.** `GEOSEARCH` on the city shard returns the K nearest available drivers in one round trip (~1ms). Offers are claimed atomically in Redis (`SET NX` with TTL) so concurrent matchers never double-offer a driver. Accept is a single short Postgres transaction (`SELECT … FOR UPDATE` on ride + driver rows). Offer timeouts are recovered by a sweeper using `FOR UPDATE SKIP LOCKED` — **safe with any number of API instances running**, no leader election, no queue.

**Reads — ride status polling + tracking.** `GET /v1/rides/{id}` is served from a Redis read-through cache, invalidated write-through on every state transition. Live tracking never polls: it flows over SSE.

**Real-time fan-out — SSE across N instances.** The in-process SSE hub subscribes to **Redis pub/sub** channels (`ride:{id}`, `driver:{id}`). Any instance can serve any client's event stream regardless of which instance processed the state change — this is what keeps the API layer genuinely stateless. (At Uber scale you'd use a dedicated push service + regional brokers; documented in HLD, not built.)

### State machines (enforced in one place, richer to match the real flow)

```
Ride:    REQUESTED → MATCHING → DRIVER_ASSIGNED → DRIVER_ARRIVING → ARRIVED
         → IN_PROGRESS → COMPLETED
         ↳ CANCELLED_BY_RIDER / CANCELLED_BY_DRIVER (pre-trip) / EXPIRED (no driver found)
Trip:    STARTED (OTP verified) → [PAUSED ⇄ RESUMED] → ENDED
Payment: PENDING → PROCESSING → SUCCEEDED | FAILED (retryable)
```

Invalid transitions are rejected at the domain layer; every transition emits an SSE event and invalidates the ride cache.

### Pricing

- **Upfront quotes:** fare = base + per-km + per-min, per tier, × surge at quote time. Quote is **locked for 3 minutes** (stored with expiry) — booking honors the quoted price even if surge moves, exactly like real apps.
- **Surge:** demand/supply ratio per geohash cell (requests vs. available drivers, sliding window in Redis counters), bucketed multipliers (1.0× / 1.2× / 1.5× / 2.0×), shown to the rider before booking. Simple by design; the graded part is consistency and invalidation, not the pricing model.

### Payments & receipts

Mock PSP simulating a real provider: payment intent → async webhook confirmation → idempotent processing (webhook replays safe) → retry with backoff on transient failure. On success, a **receipt** (fare breakdown: base, distance, time, surge, total) is generated and available in ride history.

### Consistency & atomicity (assignment §5)

- Driver assignment in a single transaction with row locks — no double-assignment under concurrent accepts.
- **Partial unique indexes**: at most one active ride per driver and per rider, enforced by the DB.
- Atomic offer claims in Redis (`SET NX` + TTL) prevent two matchers offering the same driver.
- `Idempotency-Key` required on all mutating POSTs, backed by a unique-keyed table; retries return the original response.
- Write-through cache invalidation on every state transition keeps reads current.

### Deferred to HLD discussion (intentionally not built)
Multi-region deployment (region-local writes, async cross-region sync), multi-tenancy (`tenant_id` scoping), Redis Cluster / geo-sharding beyond city keys, real PSP, message queues, dedicated push infrastructure, Kubernetes.

## 4. API Surface

All endpoints validated, authenticated (bearer token middleware, separate rider/driver roles), rate-limited, and idempotent where they mutate. The assignment's 6 core APIs plus the endpoints a real booking flow needs:

| Endpoint | Purpose | Notes |
|---|---|---|
| `POST /v1/quotes` | Upfront fare estimates (all tiers, surge included) | Quote locked 3 min |
| `POST /v1/rides` | Book a ride with a quote | `Idempotency-Key` required |
| `GET /v1/rides/{id}` | Ride status + assigned driver details | Redis-cached |
| `POST /v1/rides/{id}/cancel` | Cancel pre-trip (rider or driver, with reason) | State-machine guarded |
| `POST /v1/drivers/{id}/location` | Location ping (1–2/sec) | Redis-only write, rate-limited |
| `POST /v1/drivers/{id}/availability` | Go online/offline | |
| `POST /v1/drivers/{id}/accept` | Accept ride offer | Transactional; replay-safe |
| `POST /v1/drivers/{id}/decline` | Decline offer → next candidate | |
| `POST /v1/trips/{id}/start` | Start trip with rider OTP | OTP verified server-side |
| `POST /v1/trips/{id}/end` | End trip → fare finalization | `Idempotency-Key` required |
| `POST /v1/payments` | Trigger payment for completed trip | `Idempotency-Key` required |
| `POST /v1/webhooks/psp` | Mock PSP confirmation callback | Signature-checked, idempotent |
| `GET /v1/riders/{id}/rides` | Ride history + receipts | |
| `GET /v1/events?ride_id=…` | SSE stream (rider & driver live updates) | Backed by Redis pub/sub |

**Basic API security:** bearer auth with role separation, strict input validation everywhere, per-client rate limiting, webhook signature verification, secrets via env config only.

## 5. Data Model

| Table | Notes |
|---|---|
| `riders`, `drivers` | Profiles; vehicle details, rating, availability state on driver |
| `quotes` | Tier prices, surge multiplier, expiry — booking references a quote |
| `rides` | Full lifecycle state, FKs to rider/driver/quote, OTP hash, cancellation reason |
| `trips` | Start/end times, pauses, distance, fare breakdown |
| `payments` | Amount, method, PSP reference, state, retry count |
| `receipts` | Immutable fare breakdown per completed ride |
| `idempotency_keys` | Unique key + stored response |

Indexes designed for the latency work: rides by `(rider_id, status)`, **partial index on active rides** (small and hot), payments by ride, quotes by expiry. Driver live location is **not** a table — Redis GEO only.

## 6. Latency Optimization Plan (assignment §4)

Measure first, then optimize — with numbers:

1. **Baseline:** k6 simulating realistic mixed load (bot drivers pinging locations, riders quoting + booking, status reads) against the hot APIs. New Relic captures p50/p95/p99 and slow query traces.
2. **Optimize:** targeted indexes, query-shape fixes (no N+1, short single-tx assignment), read-through caching, `pgx` pool tuning, lock-contention reduction.
3. **Verify:** identical k6 scenario re-run; before/after published in `docs/performance-report.md`, including matching p95 vs. the 1s target.

## 7. Monitoring — New Relic (assignment §3)

- Go APM agent on all HTTP handlers, Postgres queries, and Redis calls (segment-level timing).
- Custom metrics where the product lives: **match latency**, offer-acceptance rate, active rides.
- Dashboard: per-endpoint p95, throughput, error rate, DB time; alert policy on hot-endpoint p95 breach.
- Deliverable: dashboard screenshots + bottleneck analysis in the performance report.

## 8. Frontend — Rider App + Driver App

React + Vite SPA styled like a real booking app, over a live **Leaflet map** (OpenStreetMap tiles — free, no API key). Demo bounded to Bengaluru so movement looks real.

**Rider app**
- Pickup/destination selection on the map → tier cards with upfront prices, ETAs, and surge badge.
- Booking → matching animation → driver card (name, vehicle, plate, rating) → car marker approaching live with smooth interpolation.
- OTP displayed at pickup → live trip progress along the route → fare receipt screen → ride history.

**Driver app (second pane)**
- Online/offline toggle, incoming offer modal with countdown + accept/decline, arrived → OTP entry → trip → earnings summary per trip.

**Simulator**
- A fleet of bot drivers (~50) driving realistic routes around the city, pinging locations through the same public API — this powers the demo *and* generates honest load for New Relic.

All live updates via SSE; markers animate between updates so movement looks continuous.

## 9. Testing

- Unit tests: ride/trip/payment state machines, fare + surge math, quote expiry, matching candidate selection, OTP verification.
- Handler tests: validation, idempotency (replay returns original response), auth/role guards.
- Concurrency tests: parallel accepts on one ride, duplicate webhook delivery, double-booking attempts — assert the invariants hold.

## 10. Execution Milestones (PR Plan)

Work lands as small, reviewable PRs — each independently green:

| PR | Scope |
|---|---|
| 1 | Scaffold: repo layout, Docker Compose, config, health endpoint, test/lint scripts |
| 2 | Migrations, data model, store layer (Postgres + Redis) |
| 3 | Quotes + pricing/surge + rides API with state machine |
| 4 | Driver APIs: location ingestion, availability, matching, offer accept/decline/timeout |
| 5 | Trip lifecycle (OTP start, pause, end), fare finalization, payments (mock PSP), receipts, idempotency |
| 6 | SSE events (Redis pub/sub) + React rider/driver apps with live map + bot simulator |
| 7 | New Relic integration, k6 load tests, latency optimizations |
| 8 | Docs: HLD/LLD, performance report, demo script |

## 11. Deliverables Checklist

Mapped 1:1 to the assignment:

- [ ] Backend code (Go) — APIs with validation + idempotency, clean state transitions, edge cases (timeouts, declines, retries, cancellations)
- [ ] Frontend code (React) — real booking flow with live map tracking
- [ ] New Relic performance report — dashboards, bottleneck analysis, before/after latency numbers
- [ ] Documentation — HLD, LLD, this README, demo script
- [ ] Unit + concurrency tests
- [ ] PR history — small, reviewable, well-described changes

## Running Locally (target DX)

```bash
docker compose up -d        # Postgres + Redis
make migrate                # apply schema
make run                    # start API on :8080
cd web && npm run dev       # frontend on :5173
```
