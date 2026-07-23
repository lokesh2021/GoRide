# GoRide — Frontend (Rider + Driver + Simulator)

A React + Vite single-page app that demos the full GoRide backend: a rider
booking flow and a driver app, side by side as two phone-shaped panels over live
Leaflet maps of Bengaluru, plus a built-in bot-driver simulator.

Stack: Vite + React 18 + TypeScript, `leaflet` + `react-leaflet` with free
OpenStreetMap/CARTO dark tiles (no API key). State is plain React
(context + hooks). Styling is hand-written CSS (`src/styles.css`), dark theme.

## Prerequisites (backend)

The SPA talks only to the Go API. Bring these up first:

| Component | Where | Notes |
|---|---|---|
| Postgres | `localhost:5432`, db `goride` | migrations `0001..0009` applied (incl. seed data `0009`) |
| Redis | `localhost:6379` | GEO + pub/sub |
| Go API | `localhost:8080` | `GORIDE_PG_DSN="postgres://<user>@localhost:5432/goride?sslmode=disable" go run ./cmd/server` |

Health check: `curl http://localhost:8080/healthz` → `{"status":"ok"}`.

Demo credentials are the seed tokens/UUIDs from
`migrations/0009_seed_demo_data.up.sql` — hardcoded in `src/config/personas.ts`
(riders: Ananya Rao, Karthik Iyer; drivers: Suresh Kumar … Vinod Achar across
mini/sedan/xl tiers).

## Run

```bash
npm install
npm run dev      # http://localhost:5173  (Vite auto-picks the next free port if taken)
```

`npm run build` produces a clean, type-checked production bundle in `dist/`.

### Dev proxy

`vite.config.ts` proxies `/v1 → http://localhost:8080` (`changeOrigin`, no
timeouts so SSE stays open). Because the SPA calls the API same-origin through
this proxy, **no backend CORS configuration is needed in dev**. SSE streams
(`/v1/events`, `/v1/events/driver/{id}`) are consumed via `fetch` +
`ReadableStream` (not the native `EventSource`) so the `Authorization: Bearer`
header can be attached — `EventSource` cannot set headers, and every `/v1` route
is authenticated.

## Manual demo script

Open the app; the header shows an **API connected** pill, a **Bots** count, and a
**Demo mode** toggle. The left phone is the rider, the right phone is the driver.

### A. Full rider + driver trip (end to end)

1. **Driver (right panel):** the default driver is *Suresh Kumar · mini*. Click
   **“📍 Position at rider’s pickup”** (places the driver at the rider’s pickup so
   it’s the nearest match), then flip the **“You’re offline”** toggle to online.
   Leave **Auto-pilot** on.
2. **Rider (left panel):** pickup defaults to *MG Road*, destination to
   *Koramangala*. (Optional: click **Set** then tap the map, or use the quick-place
   chips.) Click **“Get fares.”**
3. Tier cards appear — **Mini / Sedan / XL** with ₹ prices, trip ETA, and a surge
   badge when surge > 1.0×. Pick a tier, pick a **payment method** (UPI/CARD/CASH),
   then click **“Book … .”**
4. Rider shows **“Finding your driver…”**. The driver panel pops a full-screen
   **offer modal** with a 12s countdown → click **“Accept.”**
5. Rider now shows the **driver card** (name, vehicle, plate, rating) and a status
   timeline. The driver car animates toward the pickup on both maps; Auto-pilot
   marks **arriving → arrived** automatically.
6. Rider displays a **4-digit OTP**. Read it, type it into the driver panel’s OTP
   field, and click **“Start trip.”**
7. Trip goes **in progress**; the car follows the route to the destination. On the
   driver panel click **“End trip”** → an earnings toast appears.
8. Rider shows the **fare breakdown** (base / distance / time / surge / total).
   Click **“Pay ₹… .”** Payment processes → **SUCCEEDED** → receipt state.
9. Click **“History”** on the rider panel to see the completed ride with its
   receipt.

### B. Simulator (bot supply + ambiance)

1. In the header, set **Bots** (1–5) and turn **Demo mode** on. That many *other*
   driver personas go online and drive looping routes around Bengaluru, pinging
   location through the public API at ~1/sec — visible as moving dots on both maps.
2. With drivers online, surge drops from 2.0× (no supply) toward 1.0×.
3. Book a ride **without** a manual driver online: a bot **auto-accepts** after
   2–4s, drives to the pickup, and marks arriving/arrived — then parks. Bots do
   **not** start/complete trips (that needs the rider’s OTP, shown only on the
   rider panel), so run the full lifecycle (section A) with the manual driver.

### C. Cancellation

While in *finding / assigned / arriving / arrived*, the rider panel shows a
**Cancel** button (the backend rejects cancels once the trip is in progress).

### Notes

- With **zero** drivers online the backend returns **2.0× surge** (supply = 0) and
  a booking will **EXPIRE** after ~60s with no match — that’s correct behavior.
  Put a driver online (A.1) or enable Demo mode (B) before booking.
- All errors surface the backend error-envelope message as a toast (with its
  stable `code`).

## Deferred backend need (not changed here)

- **Production CORS.** In dev, the Vite proxy makes API calls same-origin, so no
  CORS is required. If the built SPA is served from a *different* origin than the
  API in production, the API must send CORS headers
  (`Access-Control-Allow-Origin`, etc.) or the SPA must be served from the API’s
  origin. No backend files were modified for this frontend.

## Source layout

```
src/
├── main.tsx                # entry; imports leaflet + app CSS
├── App.tsx                 # shell: header, persona pickers, demo toggle, both panels
├── styles.css              # dark theme, phone frames, components
├── api/
│   ├── types.ts            # all request/response types — matches Go handler JSON
│   └── client.ts           # fetch wrapper, ApiError (error envelope), Idempotency-Key
├── sse/stream.ts           # fetch-based SSE reader (Bearer header, auto-reconnect)
├── config/personas.ts      # seed riders/drivers (tokens, UUIDs), BLR places/bounds
├── lib/
│   ├── geo.ts              # haversine, lerp, bearing, route builder, formatters
│   └── money.ts            # paise → ₹ formatting, surge label
├── map/MapView.tsx         # shared Leaflet map, pins, route, animated car, bots
├── ui/toast.tsx            # toast context (surfaces backend error messages)
├── rider/RiderPanel.tsx    # booking flow: quote → book → track → pay → history
├── driver/DriverPanel.tsx  # online/offline, offer modal, OTP, auto-pilot drive
└── sim/
    ├── simulator.ts        # bot drivers (public-API traffic, offer auto-accept)
    └── useSimulator.ts     # React binding for live bot positions
```
