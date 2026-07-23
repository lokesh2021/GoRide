# AGENTS.md — GoRide

Navigation guide for AI coding agents (and humans) working in this repo. Read
this first, then `docs/SPEC.md` (the binding LLD contract) before touching code.

GoRide is a ride-hailing backend (Go) + live React frontend, built for an
SDE-2 assessment. Module: `github.com/lokeshbm/goride`.

## Ground rules

- **`docs/SPEC.md` is the contract.** Conventions, schema, Redis keys, state
  machines, error format all live there. If code and SPEC disagree, fix the
  code or propose a SPEC change — don't silently drift.
- **Money is integer paise (INR), never floats.** Distances = metres (int),
  durations = seconds (int). Timestamps = `timestamptz`, UTC.
- **Constants & log messages are segregated.** Every `internal/` package has a
  `constants.go` holding its domain constants, Redis key builders, and
  `logMsg*` message constants. No raw string literals in `log`/`slog` calls in
  logic files. API error codes live only in `internal/httpapi/codes.go`.
- **Postgres is the source of truth; Redis is a rebuildable projection.**
  Mirrors (`driver:status:*`, `driver:ride:*`), the geo index, offers, and
  caches all self-heal if Redis is flushed.
- **State transitions funnel through one place per entity** (e.g.
  `rides.Service.updateStatus`): guarded `UPDATE … WHERE status = $from` →
  cache invalidation → event publish. Handlers never write status strings.

## Commands

```bash
make run              # start API on :8080 (loads .env for secrets)
make migrate          # apply migrations + demo seed
make migrate-down     # roll back one migration
make test             # unit tests, -race
make test-integration # concurrency/invariant tests against live PG+Redis, -race
make vet              # go vet
make tidy             # go mod tidy
cd web && npm run dev # frontend on :5173 (Vite; auto-bumps port if taken)
cd web && npm run build   # type-check + production build
```

Load tests (need seeded LDT identities):
```bash
psql -d goride -f loadtest/seed_load.sql     # provision load identities (LDT city)
k6 run loadtest/mixed.js                      # steady ~33 req/s watchable workload
k6 run loadtest/capacity.js                   # ramping capacity probe
psql -d goride -f loadtest/clean_load.sql    # remove them afterward
```

## Local environment

- **Postgres 16** + **Redis 7** run natively via Homebrew (no Docker on this
  machine, though `docker-compose.yml` ships for evaluators).
- Dev DSN: `postgres://lokesh@localhost:5432/goride?sslmode=disable` (trust
  auth, OS user `lokesh`). Redis: `localhost:6379`.
  Binaries: `/opt/homebrew/opt/postgresql@16/bin/psql`,
  `/opt/homebrew/opt/redis/bin/redis-cli`.
- Secrets (New Relic license, PSP secret) live in `.env` (gitignored). `make
  run` sources it. Never commit or log secret values.
- Config is env-only, `GORIDE_` prefix: `ADDR`, `PG_DSN`, `REDIS_ADDR`, `ENV`,
  `LOG_LEVEL`, `SLOW_REQUEST_MS`, `NEWRELIC_LICENSE`, `NEWRELIC_APP_NAME`,
  `PSP_SECRET`, `PSP_WEBHOOK_URL`. See `internal/config/config.go`.

## Package map (`internal/`)

| Package | Responsibility |
|---|---|
| `config` | Env parsing, defaults |
| `store` | pgx pool + go-redis client; NR datastore tracers (see `nrtracer.go`) |
| `httpapi` | chi router, middleware (auth, idempotency, request logging), handlers, `codes.go` |
| `quotes` | Upfront fare quotes, 3-min lock |
| `pricing` | Tier rate card, haversine, surge (geohash cells), `FinalFare`, geohash |
| `rides` | Ride lifecycle + state machine (`state.go`), read-through cache, `/state` + OTP regen |
| `drivers` | Availability, location hot path (Redis-only), status mirrors |
| `matching` | GEOSEARCH candidates, `SET NX` offer claim, guarded-UPDATE accept, `SKIP LOCKED` sweeper |
| `trips` | OTP start, pause/resume, end + metered fare finalization |
| `payments` | Mock PSP, signed idempotent webhooks, receipts, ride history |
| `events` | SSE hub over Redis pub/sub (`hub.go`), publisher (`events.go`) |
| `observability` | Nil-safe New Relic APM bootstrap + route-pattern txn middleware |

Frontend: `web/src/` — `rider/`, `driver/` panels; `map/MapView.tsx`;
`sim/` bot simulator; `sse/stream.ts`; `api/{types,client}.ts` (types mirror
Go handler JSON 1:1); `lib/{geo,routing,alerts,money}.ts`; `ui/`.

## Where things live (index)

| Looking for… | File |
|---|---|
| Ride state machine (transition table) | `internal/rides/state.go` |
| Accept transaction / offer claim / sweeper | `internal/matching/matching.go` |
| Fare & surge math | `internal/pricing/pricing.go`, `surge.go` |
| Idempotency middleware | `internal/httpapi/idempotency.go` |
| Auth middleware | `internal/httpapi/auth.go` |
| API error codes | `internal/httpapi/codes.go` |
| Route table | `internal/httpapi/router.go` (`Routes`) |
| Consistency invariants (partial unique idx) | `migrations/0004_rides.up.sql` |
| Concurrency tests | `integration/concurrency_test.go` |
| SSE hub / pub-sub | `internal/events/hub.go` |
| Redis key contract | `docs/SPEC.md` → "Redis key contract" |
| Design rationale / scale story | `docs/HLD.md` |
| Demo walkthrough | `docs/DEMO.md` |
| Perf numbers | `docs/performance-report.md` |

## Common tasks (cookbook)

- **Add a config var:** field in `internal/config/config.go` + default in
  `internal/config/constants.go`; read via `getenv`/`getenvInt`.
- **Add an endpoint:** handler in `internal/httpapi/`, mount in `router.go`
  `Routes` (wrap with `requireRole`/`idempotency` as appropriate), add any new
  error code to `codes.go`, log via a `logMsg*` constant.
- **Add a migration:** `migrations/NNNN_name.{up,down}.sql` (next number),
  `make migrate`. Enums are `TEXT + CHECK`, not PG enum types.
- **Add a domain state transition:** extend the entity's transition table
  (e.g. `rides/state.go`); route the write through the entity's status funnel
  so cache-invalidation + event-publish stay wired.
- **Frontend API call:** add to `web/src/api/client.ts`, type in
  `api/types.ts` (must match the Go handler's JSON exactly).

## Gotchas (learned the hard way)

- **Vite proxy dead keep-alive:** after restarting the backend, the Vite dev
  proxy may pin a dead socket → `ECONNREFUSED`/404 on API calls. Fix: bounce
  `npm run dev`. Never edit code to "fix" this.
- **Ports:** API `:8080`, frontend `:5173` (bumps to `:5174`+ if taken by
  another project). SSE routes are exempt from the 60s timeout middleware.
- **Load-test isolation:** load identities live in the **`LDT` city shard**,
  never `BLR` (the demo city) — a mistake here means load bots steal demo
  offers. k6 VU→token mapping uses modulo into the seeded range.
- **Backend restarts during load tests** corrupt results — never restart the
  API mid-run.
- **OSRM navigation** uses the public demo server; every call falls back to
  straight-line interpolation on failure, so offline demos still work. Don't
  point load tests at OSRM.
- **`-race` is mandatory:** both `make test` targets enforce it. A real data
  race in `nrpgx5` v1.3.4 connect-tracing is worked around by the query-only
  tracer in `internal/store/nrtracer.go` — don't revert it.
- **Never log or commit secrets.** `.env` is gitignored; `docs/newrelic-setup.md`
  only shows the key *format*, not a real key.

## Change management

Work lands as small, single-concern PRs (foundation → domain → realtime →
hardening → polish). Commits are authored solely by the repo owner — no AI
co-author trailers. Keep that convention.
