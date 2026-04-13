# clawtel agent guide

## What this project is

clawtel is a single-binary Go CLI that reads token usage counts from a local [Tapes](https://github.com/papercomputeco/tapes) SQLite database and reports them as heartbeats to [claw.tech](https://claw.tech) for leaderboard tracking.

The entire application is one file: `main.go` (~390 lines).

## Architecture

```
tapes.sqlite (nodes table)  -->  clawtel  -->  POST https://ingest.claw.tech/v1/heartbeat
        (local, read-only)       (poll loop)          (claw.tech Supabase edge function)
```

- **Read side:** 4 columns from `nodes`: `created_at`, `model`, `prompt_tokens`, `completion_tokens`
- **Send side:** aggregated heartbeat: `claw_id`, `window_start`, `window_end`, `model`, `input_tokens`, `output_tokens`, `message_count`
- **Polling:** every 60 minutes, sends even when idle (presence ping)
- **Cursor:** timestamp file next to the DB tracks last-seen row

When `CLAWTEL_CLAWHUB_LOCKS` is set, clawtel also reads `.clawhub/lock.json` files at the configured paths and adds an optional `clawhub_skills` array (slug + version only) to the heartbeat. The field is omitted when unchanged since the last successful send.

## Security constraints

This is the most important section. clawtel runs on users' machines next to their private conversation data.

- **Never read or access** `content`, `bucket`, `project`, or `agent_name` columns from tapes
- **Never read** any field from `.clawhub/lock.json` other than top-level `version` and `skills.<slug>.version`. Never read `installedAt`, never read `SKILL.md` content from the workdir
- **Never add** session IDs, file paths, hostnames, IP addresses, or any PII to the heartbeat payload
- **Never change** the `heartbeat` struct fields without explicit review — this is the network contract
- **`assertSchema`** must fail hard if required columns are missing and warn about sensitive columns
- **Read-only DB access** — the SQLite connection uses `?mode=ro`
- **No key, no network** — if `CLAW_INGEST_KEY` is unset, exit immediately with no network calls

If you're modifying what clawtel reads or sends, update the security model comment at the top of `main.go` to match.

## Testing

All changes must include tests. Run the suite with:

```bash
go test -v ./...
```

Coverage report:

```bash
go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out
```

Requirements:
- All new functions must have test coverage
- Business logic functions (aggregate, readRows, assertSchema, cursor, etc.) must be at 100%
- Use in-memory SQLite (`":memory:"`) for database tests — no fixtures on disk
- Use `httptest.NewServer` for HTTP tests — never hit real endpoints
- Functions that need a configurable URL should accept it as a parameter (see `sendToURL`, `pollWithURL`) so tests can inject `httptest` servers
- `main()` is the orchestrator and uses `os.Exit`/`log.Fatal` — it is excluded from coverage targets

## Build

```bash
CGO_ENABLED=0 go build -ldflags="-s -w" -o clawtel .
```

Pure Go via `modernc.org/sqlite` — no CGO, no C toolchain needed.

## Related repositories

- **[claw.tech](https://github.com/bdougie/claw.tech)** — Astro frontend + Supabase backend that receives heartbeats. Ingest endpoint: `supabase/functions/ingest/index.ts`. Schema: `supabase/migrations/001_clawtel.sql`.
- **[tapes](https://github.com/papercomputeco/tapes)** — Agentic telemetry system. Defines the `nodes` table schema in `pkg/storage/sqlite/migrations/001_baseline_schema.sql`.
- **[openclaw-in-a-box](https://github.com/papercomputeco/openclaw-in-a-box)** — Orchestrator skill that sets up claw agents with tapes and clawtel.

## Environment variables

| Variable | Required | Description |
|---|---|---|
| `CLAW_INGEST_KEY` | Yes (or silent exit) | Bearer token for claw.tech ingest (`ik_...` format) |
| `CLAW_ID` | Yes (when key is set) | Your claw identifier on the leaderboard |
| `TAPES_DB` | No | Override path to tapes.sqlite |
| `CLAWTEL_CLAWHUB_LOCKS` | No | Comma-separated absolute paths to `.clawhub/lock.json` files. When set, slug+version of each installed clawhub skill is added to the heartbeat |

## Releases

Tag-driven via GoReleaser. Workflow: `.github/workflows/release.yml`.

```bash
git tag v0.x.x && git push origin v0.x.x
```

Produces: `clawtel_{linux,darwin}_{amd64,arm64}.tar.gz` + `checksums.txt`.

Install script: `scripts/install.sh` — detects OS/arch and downloads from GitHub Releases.
