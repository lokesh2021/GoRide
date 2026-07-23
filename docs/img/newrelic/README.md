# New Relic screenshots

Drop the captured PNGs here with **exactly these filenames** — they are already
referenced (embedded) from [`docs/performance-report.md`](../../performance-report.md),
so once the files exist they render on GitHub with no further edits.

| Filename | What it shows |
|---|---|
| `apm-summary.png` | APM → `goride` → Summary: response-time percentiles, throughput (rpm), error rate |
| `transactions.png` | APM → Transactions, sorted "Most time consuming" — route-pattern names, not raw UUIDs |
| `dashboard.png` | The imported **GoRide — API Performance** dashboard (from `docs/newrelic-dashboard.json`) |
| `logs-in-context.png` | A transaction with its correlated slog lines (`nrslog`) |

**Format:** PNG, ideally ≤ ~1600px wide. Confirm no license/ingest key is visible
in any image before committing.
