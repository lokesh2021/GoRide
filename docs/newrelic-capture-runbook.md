# New Relic — Screenshot Capture Runbook

Everything needed to capture the five images the performance-report deliverable
asks for. Your agent is already live (`appName = goride`) and receiving data, so
this is a capture task, not a setup task. Budget ~15 minutes.

> **Integrity note:** every image must be a real screenshot of your own New Relic
> account rendering your own data. Do not mock, edit, or hand-draw dashboards —
> the k6 numbers and the APM data are genuine, so the screenshots should be too.

## Step 0 — Push a fresh, dense window of data (optional but recommended)

So the 30-minute dashboard window is full and the charts look alive:

```bash
psql -d goride -f loadtest/seed_load.sql      # if not already seeded
k6 run loadtest/mixed.js                        # ~3 min; or capacity.js for a denser ramp
```

Wait ~2–3 minutes after the run starts for data to appear in New Relic.

## Step 1 — Import the dashboard

1. Find your account id: New Relic → top-right account menu → **Administration →
   Access management**, or read it from the URL (`.../accounts/<ID>/...`).
2. Inject it into the dashboard JSON (replaces all 11 placeholders):

   ```bash
   sed -i '' 's/"accountIds": \[0\]/"accountIds": [YOUR_ACCOUNT_ID]/g' docs/newrelic-dashboard.json
   ```
   (Linux: drop the `''` after `-i`.)
3. New Relic → **Dashboards** → **Import dashboard** (the `+`/import control,
   top-right) → paste the contents of `docs/newrelic-dashboard.json` → **Import**.
4. Open **GoRide — API Performance**. Set the time picker to **Last 30 minutes**.
   All 11 tiles should populate.

## Step 2 — The five screenshots (each maps to a graded ask)

| # | Screenshot | Where | Must clearly show |
|---|---|---|---|
| 1 | **APM Summary** | APM & Services → `goride` → **Summary** | Web response time (avg + percentiles), throughput (rpm), error rate |
| 2 | **Most time-consuming transactions** | APM → `goride` → **Transactions** → sort "Most time consuming" | Route-pattern names (`POST /v1/rides`, `/v1/quotes`, `POST /v1/drivers/{id}/location`) — not raw UUIDs |
| 3 | **Transaction trace waterfall** | Transactions → pick `POST /v1/rides` → a **trace** / distributed trace | The segment breakdown: Postgres (nrpgx5) vs Redis vs app time within one request |
| 4 | **Custom dashboard** | Dashboards → **GoRide — API Performance** (imported above) | The latency-per-endpoint table + `Custom/Matching/OfferLatencyMs` tile vs the 1s target |
| 5 | **Alert condition** | Alerts → policy `goride latency` → the condition (see Step 3) | The 95th-percentile > 250ms / 5-min threshold config |

Optional bonus (strong signal): **Logs in context** — APM → `goride` → open a
transaction → **Logs** tab, showing slog lines correlated to the trace (the
`nrslog` wiring from PR #18).

## Step 3 — Create the alert (needed for screenshot #5)

Alerts → **Alert policies** → **Create policy** `goride latency` →
**Create a condition → APM → Web transactions response time**:

- Signal: **Web transaction percentiles → 95th percentile**
- Threshold: **critical > 250 ms for at least 5 minutes**
  (your hot p95 is single-digit ms, so this only fires on a real regression —
  say exactly that in the demo)
- Add an email notification channel.
- Add a second condition on **error rate > 5%**.

Screenshot the condition config once saved.

## Step 4 — Land them in the report

Save the images to `docs/img/newrelic/` and embed them in
`docs/performance-report.md` under the New Relic section, each with a one-line
caption stating what it proves (e.g. "APM Summary: p95 web latency 7ms at ~33
req/s, 0% errors"). That closes the "Performance report with New Relic
(dashboard or screenshots)" deliverable.

## Step 5 — Rotate the key (after submission)

The ingest key lives only in gitignored `.env`. After the assessment, rotate it
in New Relic (API keys → ingest key → rotate); the app picks up the new value on
the next `make run`.
