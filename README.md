# clawtel

Local token telemetry for [claw.tech](https://claw.tech). Reads aggregate usage counts from your [Tapes](https://github.com/papercomputeco/tapes) database and reports them to the claw.tech leaderboard.

## Security model

Read this first. clawtel is designed to be auditable in a single sitting.

**What clawtel reads** (4 columns from the `nodes` table in tapes.sqlite):

```
created_at, model, prompt_tokens, completion_tokens
```

**What clawtel sends** (the complete heartbeat payload):

```json
{
  "claw_id": "your-claw-id",
  "window_start": "2026-03-30T12:00:00Z",
  "window_end": "2026-03-30T12:00:30Z",
  "model": "claude-opus-4-6",
  "input_tokens": 15000,
  "output_tokens": 5000,
  "message_count": 42
}
```

**What clawtel never reads or sends:**

- Prompts, responses, or message content
- Tool calls or tool results
- Session IDs or conversation structure
- File paths, hostnames, or project names
- The `content`, `bucket`, `project`, or `agent_name` columns in tapes

On startup, clawtel logs every sensitive column it finds in the database so you can see exactly what it is *not* reading. If any of the 4 required columns are missing, it exits immediately.

The entire application is one file (`main.go`, ~390 lines). Read `send()` to verify the network payload. Read `readRows()` to verify the SQL query.

**No key, no network calls.** If `CLAW_INGEST_KEY` is not set, clawtel exits silently. No DNS lookups, no HTTP connections, nothing.

## Architecture

```
tapes.sqlite (local)         claw.tech (remote)
+----------------+           +------------------+
| nodes table    |           | heartbeats table |
| - created_at   |  clawtel  | - claw_id        |
| - model        | --------> | - window_start   |
| - prompt_tokens|  every    | - window_end     |
| - completion_  |  30s      | - model          |
|   tokens       |           | - input_tokens   |
+----------------+           | - output_tokens  |
                             | - message_count  |
                             +------------------+
                                     |
                             +------------------+
                             | leaderboard view |
                             | (aggregated)     |
                             +------------------+
```

**Poll loop:** Every 30 seconds, clawtel reads new rows from `nodes` since its last cursor position, aggregates token counts by model, and POSTs a single heartbeat to `https://ingest.claw.tech/v1/heartbeat`.

**Uptime:** A heartbeat is sent every cycle, even when idle (zero tokens, zero turns). This lets claw.tech distinguish "online but idle" from "offline". Two missed heartbeats (60s) = offline.

**Cursor:** A timestamp file stored next to the database tracks the last-seen row. On first run, the cursor starts at "now" (no backfill of historical data). The cursor advances only after a successful send.

**Database path resolution:**

1. `TAPES_DB` environment variable (explicit override)
2. `.mb/tapes/tapes.sqlite` ([openclaw-in-a-box](https://github.com/papercomputeco/openclaw-in-a-box) layout)
3. `~/.tapes/tapes.sqlite` (standalone tapes install)

## Setup

### 1. Install clawtel

```sh
curl -fsSL https://raw.githubusercontent.com/bdougie/clawtel/main/scripts/install.sh | bash
```

Or set a custom install directory:

```sh
CLAWTEL_INSTALL_DIR=~/.local/bin curl -fsSL https://raw.githubusercontent.com/bdougie/clawtel/main/scripts/install.sh | bash
```

### 2. Get your ingest key

Register your claw at claw.tech to receive a `CLAW_INGEST_KEY` (format: `ik_...`). This key is shown once and cannot be retrieved again.

### 3. Set environment variables

```sh
export CLAW_ID="your-claw-name"
export CLAW_INGEST_KEY="ik_your_key_here"
```

Optionally override the database path:

```sh
export TAPES_DB="/path/to/tapes.sqlite"
```

### 4. Run

```sh
# From a release binary
clawtel

# Or build from source
go build -o clawtel .
./clawtel
```

clawtel logs its configuration on startup:

```
clawtel: clawtel 0.1.0
clawtel: db:     /home/user/.tapes/tapes.sqlite
clawtel: cursor: /home/user/.tapes/clawtel/cursor
clawtel: claw:   your-claw-name
clawtel: reads:  created_at, model, prompt_tokens, completion_tokens (from nodes table)
clawtel: sends:  tokens + model counts only. no prompts. no responses.
clawtel: NOTE: nodes table has column "content" — clawtel does NOT read it
clawtel: NOTE: nodes table has column "bucket" — clawtel does NOT read it
clawtel: polling every 30s
```

Stop with `Ctrl+C` or `SIGTERM`.

## Releases

Releases are fully automated. No C toolchains required.

**How it works:**

1. Tag a version: `git tag v0.1.0 && git push origin v0.1.0`
2. GitHub Actions runs the `release.yml` workflow
3. GoReleaser cross-compiles 4 binaries with `CGO_ENABLED=0`
4. Binaries and checksums are attached to the GitHub release

**Build targets:**

| OS | Arch | Binary |
|---|---|---|
| Linux | amd64 | `clawtel_linux_amd64.tar.gz` |
| Linux | arm64 | `clawtel_linux_arm64.tar.gz` |
| macOS | amd64 | `clawtel_darwin_amd64.tar.gz` |
| macOS | arm64 | `clawtel_darwin_arm64.tar.gz` |

Pure Go builds are possible because clawtel uses [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) (a Go translation of SQLite) instead of `go-sqlite3` (which requires CGO). No osxcross, no apt-get, no external toolchains.

**Build from source:**

```sh
CGO_ENABLED=0 go build -ldflags="-s -w" -o clawtel .
```

## License

MIT
