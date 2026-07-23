# New Relic — Setup & Dashboard Guide

The GoRide backend ships with the New Relic Go APM agent already wired
(`internal/observability/`). It's a **clean no-op without a license key** and
activates the moment one is present.

## 1. Provide the license key (local only — never commit it)

The key lives in the **gitignored** `.env` file (never in `.env.example`, which
is tracked):

```
GORIDE_NEWRELIC_LICENSE=<your-ingest-license-key>   # ends in ...NRAL
GORIDE_NEWRELIC_APP_NAME=goride                      # optional, defaults to goride
```

Start the server so it loads `.env`:

```bash
make run
```

Confirm the agent connected — the log prints:

```
observability: New Relic application started app_name=goride
```

(If the key is absent it prints `monitoring disabled` and the app runs
identically — that's the nil-safe path.)

## 2. Generate load so the dashboards have data

```bash
psql -d goride -f loadtest/seed_load.sql   # isolated LDT city identities
k6 run loadtest/mixed.js                    # ~3 min mixed workload
```

Data appears in New Relic within ~2–3 minutes of the first requests.

## 3. Find the app in New Relic

1. Sign in at <https://one.newrelic.com>.
2. Left nav → **APM & Services** → the **goride** entity (appears once data flows).
3. The built-in **Summary** view already shows: response time (avg + percentiles),
   throughput (rpm), error rate, and the transaction/database/external breakdown —
   no setup needed. This alone satisfies "track API latencies under load".

## 4. The views that map to the assignment

| Assignment ask | Where in New Relic |
|---|---|
| API latencies under load | APM → goride → **Summary** (web transaction time, percentiles) |
| Slowest endpoints | APM → **Transactions** (sort by "Most time consuming") — `POST /v1/rides`, `/v1/quotes`, `/v1/drivers/{id}/location` name by route pattern, not raw UUIDs |
| Slow DB queries / bottlenecks | APM → **Databases** (Postgres segments via nrpgx5) and the transaction trace waterfall (Postgres vs Redis vs app time per request) |
| Custom product metrics | Query builder (see §5): `Custom/Matching/OfferLatencyMs`, offer accepted/declined/expired, payment succeeded/failed |

## 5. A custom dashboard (optional, looks great in the demo)

**Dashboards → Create a dashboard → Add widget → Query (NRQL).** Useful tiles:

```sql
-- p50/p95/p99 latency per endpoint
SELECT percentile(duration, 50, 95, 99) FROM Transaction
WHERE appName = 'goride' FACET name SINCE 30 minutes ago

-- throughput (requests/min)
SELECT rate(count(*), 1 minute) FROM Transaction
WHERE appName = 'goride' TIMESERIES SINCE 30 minutes ago

-- error rate
SELECT percentage(count(*), WHERE error IS true) FROM Transaction
WHERE appName = 'goride' SINCE 30 minutes ago

-- matching offer latency (custom metric)
SELECT average(newrelic.timeslice.value) FROM Metric
WHERE metricTimesliceName = 'Custom/Matching/OfferLatencyMs'
AND appName = 'goride' TIMESERIES SINCE 30 minutes ago

-- payment outcomes
SELECT count(*) FROM Metric
WHERE metricTimesliceName IN ('Custom/Payments/Succeeded','Custom/Payments/Failed')
FACET metricTimesliceName SINCE 30 minutes ago
```

## 6. Alerts (assignment §3: "alerts for slow response times")

**Alerts → Alert policies → Create policy** (`goride latency`), then **Create a
condition → APM → Web transactions response time**:

- Signal: **Web transaction percentiles → 95th percentile**
- Threshold: **critical > 250 ms for at least 5 minutes** (our hot p95 is ~7 ms,
  so this only fires on a genuine regression — mention that framing in the demo)
- Add a notification channel (email is fine for the demo).

A second condition on **error rate > 5%** rounds it out.

## 7. Capture for the performance report

Screenshot into `docs/performance-report.md`: the APM Summary (latency + throughput),
the Transactions "most time consuming" list, one transaction trace waterfall showing
DB segments, and the alert-condition config. Those four images complete the
"Performance report with New Relic" deliverable.

## Security note

The ingest key was shared in plaintext during setup and briefly lived in a tracked
`.env.example` (removed before any commit — the public repo never contained it).
It now lives only in gitignored `.env`. If you want to be thorough, **rotate the key**
in New Relic (API keys → the ingest key → rotate) after the assignment; the app
picks up the new value on the next `make run`.
