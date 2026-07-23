# New Relic screenshots

Drop the captured PNGs here with **exactly these filenames** — they are already
referenced (embedded) from [`docs/performance-report.md`](../../performance-report.md),
so once the files exist they render on GitHub with no further edits.

| Filename | What it shows |
|---|---|
| `apm-summary.png` | APM → `goride` → Summary: response-time percentiles, throughput (rpm), error rate |
| `transactions.png` | APM → Transactions, sorted "Most time consuming" — route-pattern names, not raw UUIDs |
| `trace-waterfall.png` | One transaction trace: Postgres (nrpgx5) vs Redis vs app time within a request |
| `dashboard.png` | The imported **GoRide — API Performance** dashboard (from `docs/newrelic-dashboard.json`) |
| `alert-condition.png` | Alerts → the `goride latency` policy condition (95th percentile > 250ms / 5 min) |
| `logs-in-context.png` | *(optional)* A transaction with its correlated slog lines (`nrslog`) |

**Format:** PNG, ideally ≤ ~1600px wide. Confirm no license/ingest key is visible
in any image before committing.
