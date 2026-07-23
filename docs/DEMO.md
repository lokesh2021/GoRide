# GoRide — Demo Script

A ~5-minute walkthrough that exercises every graded surface. Assumes the quickstart from the README is done (Postgres + Redis up, migrations applied, API on :8080, `cd web && npm run dev` on :5173).

## 1. The happy path (rider + driver, one screen)

Open http://localhost:5173.

1. **Turn on Demo mode** (header toggle, 5 bots) — five bot drivers come online and start driving around Bengaluru. Point out: every marker movement is a real `POST /v1/drivers/{id}/location` through the public API; the bots are genuine matchable supply.
2. **Driver panel** (right phone): pick a driver persona (e.g. Suresh Kumar · Mini), toggle **Online**. The car appears on both maps.
3. **Rider panel** (left phone): tap the map to set pickup near the driver, tap again for destination. Tier cards appear with upfront prices and, if supply is thin, a surge badge (e.g. 1.5×). Point out the price is a **locked quote** (3 min).
4. **Book** (Mini). Watch: "Finding your driver…" → within a second the driver panel gets the **offer modal with a 12-second countdown** (pickup distance + fare shown).
5. **Accept** on the driver panel. Rider panel flips to the **driver card** (name, Alto, KA-01 plate, rating) and the car starts approaching — live SSE, markers interpolated.
6. Driver: **Arriving** → **Arrived**. Rider panel now shows the **4-digit OTP**.
7. Driver: type the OTP → **Start trip**. Wrong OTP first, if you want the 422 demo.
8. Watch the car drive the route; then driver: **End trip**. Rider gets the **fare breakdown** (base + distance + time × quoted surge — point out it's the *metered* distance, not the estimate).
9. Rider: **Pay** → payment goes PENDING → PROCESSING → webhook lands → **receipt**. Open **History** to show the stored receipt.

## 2. Edge cases (curl or UI)

```bash
TOKEN="Authorization: Bearer rider1-token"
```

- **Idempotency replay**: book twice with the same `Idempotency-Key` → identical response, one ride. Different body, same key → `422 IDEMPOTENCY_KEY_REUSED`.
- **Double booking**: second ride while one is active → `409 RIDE_ALREADY_ACTIVE` (enforced by a partial unique index, not app code — show `\d rides` if asked).
- **Decline cascade**: driver declines the offer → next candidate is offered immediately; with no candidates left the ride expires at 60s (sweeper).
- **Rate limiting**: burst >3 location pings/sec → `429 RATE_LIMITED`.
- **Authorization**: rider2's token on rider1's ride → `403`; missing token → `401`.
- **Webhook tamper**: replay a captured PSP webhook with a bad signature → `401 INVALID_SIGNATURE`; replay with the good signature → no-op (idempotent on `psp_ref`).

## 3. The scalability story (terminal)

1. **Stateless fan-out**: start a second instance — `GORIDE_ADDR=:8081 make run`. Open the rider stream against :8081 (`curl -N 'localhost:8081/v1/events?ride_id=…'`) while driving the lifecycle on :8080 — events arrive. One line: *"any instance serves any stream; state lives in Postgres/Redis."*
2. **Hot path**: show a location ping in the logs — no SQL. `redis-cli MONITOR` during pings shows the 2 pipelined round-trips.
3. **Concurrency invariants**: `make test-integration` — parallel accepts, double booking, duplicate webhooks, cancel/accept race, all under `-race`.
4. **Load**: `psql -d goride -f loadtest/seed_load.sql && k6 run loadtest/mixed.js` — thresholds encode the latency targets; New Relic dashboard shows the same run from the inside (see performance report).

## 4. Where things live (for code questions)

| Question | File |
|---|---|
| State machines | `internal/rides/state.go` (transition table) |
| Matching + offers | `internal/matching/matching.go` |
| Fare math | `internal/pricing/pricing.go` (+ `FinalFare`) |
| Idempotency | `internal/httpapi/idempotency.go` |
| Consistency invariants | `migrations/0004_rides.up.sql` (partial unique indexes) + `integration/concurrency_test.go` |
| SSE / pub-sub | `internal/events/hub.go` |
| Design rationale | `docs/HLD.md`; contracts in `docs/SPEC.md` |
