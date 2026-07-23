# CLAUDE.md

This project's agent guide is the vendor-neutral **[AGENTS.md](AGENTS.md)** —
read it first. It covers commands, the package map, a "where things live"
index, a common-tasks cookbook, and the hard-won gotchas.

@AGENTS.md

## Claude Code notes

- **Binding contract:** `docs/SPEC.md` (LLD). Design rationale: `docs/HLD.md`.
- **Before finishing any change:** `make test` (unit, `-race`) and, when a
  change touches domain/store logic, `make test-integration` (needs live
  Postgres + Redis). Frontend changes: `cd web && npm run build` must be
  type-clean.
- **Verifying UI changes:** the Vite dev server on `:5173` hot-reloads; use the
  browser preview tools. After a backend restart, bounce `npm run dev` if API
  calls start failing (dead proxy keep-alive — see AGENTS.md gotchas).
- **Secrets:** live in `.env` (gitignored, sourced by `make run`). Never print,
  log, or commit their values.
