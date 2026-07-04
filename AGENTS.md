# clawtel agent guide

## What this project is

clawtel is a single-binary Go CLI that reads token usage counts from a local [Tapes](https://github.com/papercomputeco/tapes) SQLite database and reports them as heartbeats to [claw.tech](https://claw.tech) for leaderboard tracking.

The entire application is one file: `main.go` (~390 lines).

The CLI has one subcommand: `clawtel reset`. It POSTs `{claw_id}` to `https://ingest.claw.tech/v1/reset` to shift the uptime baseline on claw.tech, then deletes the local cursor file so the next daemon run starts from "now". Reset is non-destructive server-side — heartbeat history and token totals are preserved.

## Architecture

```
tapes.sqlite (nodes table)  -->  clawtel  -->  POST https://ingest.claw.tech/v1/heartbeat
        (local, read-only)       (poll loop)          (claw.tech Astro endpoint)
```

- **Read side:** 5 columns from `nodes`: `created_at`, `model`, `prompt_tokens`, `completion_tokens`, `stop_reason` (enum values only — `end_turn`, `error`, etc. — no user content; read only when the column is present, so old tapes schemas degrade gracefully)
- **Send side:** one heartbeat per distinct model in the window. Each heartbeat carries `claw_id`, `window_start`, `window_end`, `model`, `input_tokens`, `output_tokens`, `message_count`, and — optionally — `context_tokens`, `error_count`, `gateway_process_up`, `gateway_health_ok`. Skills are attached to the first heartbeat only; the observability fields are attached to every heartbeat in the window, presence pings included.
- **Polling:** every 5 minutes. Idle windows still send one presence-ping heartbeat with empty model and zero tokens.
- **Cursor:** timestamp file next to the DB tracks last-seen row

When `CLAWTEL_CLAWHUB_LOCKS` is set, clawtel also reads `.clawhub/lock.json` files at the configured paths and adds an optional `clawhub_skills` array (slug + version only) to the heartbeat. The field is omitted when unchanged since the last successful send.

When `CLAWTEL_GATEWAY_HEALTH_URL` is set, clawtel also probes the local OpenClaw gateway (HTTP GET to that URL, plus a `pgrep -f` check against `CLAWTEL_GATEWAY_PROC`) and adds `gateway_process_up`/`gateway_health_ok` booleans to every heartbeat. Omitted entirely when the URL is not configured.

## Scope boundary

clawtel sends **heartbeats only** (`/v1/heartbeat`). It is not involved in the claw.tech activity journal:

- The journal is a separate ingest endpoint (`/v1/journal`) with its own table (`claw_journal`).
- Journal entries are posted by the standalone [`claw-journal`](https://github.com/bdougie/claw.tech/tree/main/skills/claw-journal) skill — a script that summarizes agent activity locally and POSTs one line per turn.
- `claw-journal` reuses `CLAW_ID` and `CLAW_INGEST_KEY` but shares no code with clawtel.

Do not add journal-posting — or any non-heartbeat endpoint — to `main.go`. A request to "post to the journal" or "wire up the journal" belongs in the `claw-journal` skill, not here.

## Security constraints

This is the most important section. clawtel runs on users' machines next to their private conversation data.

- **Never read or access** `content`, `bucket`, `project`, or `agent_name` columns from tapes
- **Never read** any field from `.clawhub/lock.json` other than top-level `version` and `skills.<slug>.version`. Never read `installedAt`, never read `SKILL.md` content from the workdir
- **Never add** session IDs, file paths, hostnames, IP addresses, or any PII to the heartbeat payload
- **Never change** the `heartbeat` struct fields without explicit review — this is the network contract. The `context_tokens`, `error_count`, `gateway_process_up`, `gateway_health_ok` fields (clawtel >= 0.2.0) were added under exactly this kind of review — see `docs/superpowers/plans/2026-07-03` in the claw.tech repo — and the field names (`context_tokens`, `error_count`, `gateway_process_up`, `gateway_health_ok`) must match what claw.tech's ingest endpoint accepts (server migration 023 (`023_claw_health.sql`))
- **Never change** the `resetRequest` struct fields without explicit review — `{claw_id}` is the entire reset payload, by design
- **`assertSchema`** must fail hard if required columns are missing and warn about sensitive columns
- **Read-only DB access** — the SQLite connection uses `?mode=ro`
- **No key, no network** — if `CLAW_INGEST_KEY` is unset, exit immediately with no network calls

If you're modifying what clawtel reads or sends, update the security model comment at the top of `main.go` to match.

**Deploy order:** clawtel >= 0.2.0 sends `context_tokens`, `error_count`, `gateway_process_up`, `gateway_health_ok` on the heartbeat payload. claw.tech's ingest endpoint must have run migration 023 (`023_claw_health.sql`) (which adds these columns to the heartbeats table) **before** any 0.2.0 clawtel release goes out — otherwise the server will reject or silently drop the new fields. Check the claw.tech repo's migration status before tagging a clawtel release that includes this change.

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

- **[claw.tech](https://github.com/bdougie/claw.tech)** — Astro frontend + Supabase backend that receives heartbeats. Ingest routes: `src/pages/v1/{heartbeat,reset,journal}.ts` (Astro endpoints — not `netlify/functions/`, which is dead code shadowed by the `*.tech` redirect). Schema: `supabase/migrations/`.
- **[claw-journal](https://github.com/bdougie/claw.tech/tree/main/skills/claw-journal)** — Standalone skill (lives in the claw.tech repo) that posts a sanitized activity feed to `/v1/journal`. A sibling to clawtel, not a dependency — see Scope boundary above.
- **[tapes](https://github.com/papercomputeco/tapes)** — Agentic telemetry system. Defines the `nodes` table schema in `pkg/storage/sqlite/migrations/001_baseline_schema.sql`.
- **[openclaw-in-a-box](https://github.com/papercomputeco/openclaw-in-a-box)** — Orchestrator skill that sets up claw agents with tapes and clawtel.

## Environment variables

| Variable | Required | Description |
|---|---|---|
| `CLAW_INGEST_KEY` | Yes (or silent exit) | Bearer token for claw.tech ingest (`ik_...` format) |
| `CLAW_ID` | Yes (when key is set) | Your claw identifier on the leaderboard. **Appears in startup logs** (public identifier by design) |
| `TAPES_DB` | No | Override path to tapes.sqlite |
| `CLAWTEL_CLAWHUB_LOCKS` | No | Comma-separated absolute paths to `.clawhub/lock.json` files. When set, slug+version of each installed clawhub skill is added to the heartbeat |
| `CLAWTEL_GATEWAY_HEALTH_URL` | No | OpenClaw gateway health endpoint (e.g. `http://127.0.0.1:18789/health`). When set, each heartbeat carries `gateway_process_up`/`gateway_health_ok` so claw.tech can flag a wedged gateway as RESTART NEEDED |
| `CLAWTEL_GATEWAY_PROC` | No | pgrep pattern for the gateway process (default `openclaw-gateway`) |
| `CLAWTEL_VERSION` | No | Pin the install script to a specific release tag (e.g. `v0.1.12`). Defaults to `latest` |
| `CLAWTEL_INSTALL_DIR` | No | Override the install directory for the install script. Defaults to `/usr/local/bin` |

## Releases

Tag-driven via GoReleaser. Workflow: `.github/workflows/release.yml`.

```bash
git tag v0.x.x && git push origin v0.x.x
```

Produces: `clawtel_{linux,darwin}_{amd64,arm64}.tar.gz` + `checksums.txt`.

Install script: `scripts/install.sh` — detects OS/arch and downloads from GitHub Releases.
