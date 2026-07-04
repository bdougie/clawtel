# clawtel

Local token telemetry for [claw.tech](https://claw.tech). Reads aggregate usage counts from your [Tapes](https://github.com/papercomputeco/tapes) database and reports them to the claw.tech leaderboard.

## Security model

Read this first. clawtel is designed to be auditable in a single sitting.

**What clawtel reads** (5 columns from the `nodes` table in tapes.sqlite):

```
created_at, model, prompt_tokens, completion_tokens, stop_reason
```

`stop_reason` is enum-like (`end_turn`, `error`, ...) — no user content. It is read only when present; clawtel degrades gracefully on tapes schemas that predate the column.

When `CLAWTEL_GATEWAY_HEALTH_URL` is set, clawtel additionally probes the local OpenClaw gateway: an HTTP GET to that URL, and a `pgrep -f` check for the gateway process. Only a boolean up/down result is read from each probe — no response bodies, no process command lines.

**What clawtel sends** (the complete heartbeat payload):

```json
{
  "claw_id": "your-claw-id",
  "window_start": "2026-03-30T12:00:00Z",
  "window_end": "2026-03-30T12:00:30Z",
  "model": "claude-opus-4-6",
  "input_tokens": 15000,
  "output_tokens": 5000,
  "message_count": 42,
  "context_tokens": 84000,
  "error_count": 1,
  "gateway_process_up": true,
  "gateway_health_ok": false
}
```

`context_tokens`, `error_count`, `gateway_process_up`, and `gateway_health_ok` are all optional — omitted entirely (not `null`) when the underlying probe has nothing to report. The `gateway_*` fields are omitted unless `CLAWTEL_GATEWAY_HEALTH_URL` is configured.

**What clawtel never reads or sends:**

- Prompts, responses, or message content
- Tool calls or tool results
- Session IDs or conversation structure
- File paths, hostnames, or project names
- The `content`, `bucket`, `project`, or `agent_name` columns in tapes
- Gateway health-check response bodies or process command lines

On startup, clawtel logs every sensitive column it finds in the database so you can see exactly what it is *not* reading. If any of the 4 required columns (`created_at`, `model`, `prompt_tokens`, `completion_tokens`) are missing, it exits immediately.

The entire application is one file (`main.go`, ~950 lines). Read `send()` to verify the network payload. Read `readRows()` to verify the SQL query.

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

Full reference:

| Variable | Required | Description |
|---|---|---|
| `CLAW_INGEST_KEY` | Yes (or silent exit) | Bearer token for claw.tech ingest |
| `CLAW_ID` | Yes (when key is set) | Your claw identifier on the leaderboard |
| `TAPES_DB` | No | Override path to tapes.sqlite |
| `CLAWTEL_CLAWHUB_LOCKS` | No | Comma-separated paths to `.clawhub/lock.json` files |
| `CLAWTEL_GATEWAY_HEALTH_URL` | No | OpenClaw gateway health endpoint (e.g. `http://127.0.0.1:18789/health`). When set, each heartbeat carries `gateway_process_up`/`gateway_health_ok` so claw.tech can flag a wedged gateway as RESTART NEEDED |
| `CLAWTEL_GATEWAY_PROC` | No | pgrep pattern for the gateway process (default `openclaw-gateway`) |

### Skills reporting (OpenClaw)

To report installed skills to the leaderboard, set `CLAWTEL_CLAWHUB_LOCKS` to the paths of your `.clawhub/lock.json` files:

```sh
# Find all lock.json files in your OpenClaw workspace
export CLAWTEL_CLAWHUB_LOCKS=$(find ~/.openclaw -name "lock.json" 2>/dev/null | tr '\n' ',')
```

Or set paths explicitly:

```sh
export CLAWTEL_CLAWHUB_LOCKS="/home/user/.openclaw/workspace/.clawhub/lock.json"
```

clawtel reads only `version` and `skills.<slug>.version` from these files. No file paths, timestamps, or SKILL.md content is transmitted.

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
clawtel: clawtel 0.2.0
clawtel: db:     /home/user/.tapes/tapes.sqlite
clawtel: cursor: /home/user/.tapes/clawtel/cursor
clawtel: claw:   your-claw-name
clawtel: reads:  created_at, model, prompt_tokens, completion_tokens, stop_reason (from nodes table)
clawtel: sends:  tokens + model counts, context/error/gateway health (optional). no prompts. no responses.
clawtel: NOTE: nodes table has column "content" — clawtel does NOT read it
clawtel: NOTE: nodes table has column "bucket" — clawtel does NOT read it
clawtel: polling every 5m0s
```

Stop with `Ctrl+C` or `SIGTERM`.

### Running as a systemd service

For persistent operation, run clawtel as a systemd service:

1. Create `/etc/clawtel.env` (mode 0600):

```sh
CLAW_ID=your-claw-id
CLAW_INGEST_KEY=ik_your_key_here
TAPES_DB=/home/user/.tapes/tapes.sqlite
CLAWTEL_CLAWHUB_LOCKS=/home/user/.openclaw/workspace/.clawhub/lock.json
```

2. Create `/etc/systemd/system/clawtel.service`:

```ini
[Unit]
Description=clawtel — token telemetry for claw.tech
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
EnvironmentFile=/etc/clawtel.env
ExecStart=/usr/local/bin/clawtel
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
```

3. Enable and start:

```sh
sudo chmod 600 /etc/clawtel.env
sudo systemctl daemon-reload
sudo systemctl enable --now clawtel
```

4. Check status:

```sh
sudo journalctl -u clawtel -f
```

## Using clawtel with OpenClaw agents

If your workload is an OpenClaw agent (clawchief, staffchief, openclaw-in-a-box), setting `ANTHROPIC_BASE_URL=http://localhost:8080` is **not enough**. OpenClaw instantiates its Anthropic client with `baseURL: model.baseUrl`, which clobbers the SDK's normal `readEnv("ANTHROPIC_BASE_URL")` fallback. The gateway will hold a direct TLS connection to `api.anthropic.com` and `nodes` will stay empty forever — clawtel will send heartbeats, but every one will be `model=""`, `input_tokens=0`, `output_tokens=0`, and claw.tech will show the claw pinned at 0% uptime.

Set the provider's base URL explicitly in `~/.openclaw/openclaw.json`:

```json
{
  "models": {
    "providers": {
      "anthropic": {
        "baseUrl": "http://localhost:8080",
        "models": [
          { "id": "claude-opus-4-7",   "name": "Claude Opus 4.7"   },
          { "id": "claude-sonnet-4-6", "name": "Claude Sonnet 4.6" },
          { "id": "claude-haiku-4-5",  "name": "Claude Haiku 4.5"  }
        ]
      }
    }
  }
}
```

Restart the OpenClaw gateway, then verify it's talking to tapes (not Anthropic's edge directly):

```sh
ss -tnp | awk -v pid="$(pgrep -f openclaw-gateway | head -1)" '$0 ~ "pid="pid'
# Expect a line with Peer Address  127.0.0.1:8080
# If you see a Cloudflare IP or 160.79.*.*, tapes is still being bypassed.
```

Then confirm tapes is actually recording:

```sh
sqlite3 ~/.tapes/tapes.sqlite 'SELECT count(*), max(created_at) FROM nodes;'
```

Non-zero count = clawtel will have something to send on the next poll.

## Restart detection

When `CLAWTEL_GATEWAY_HEALTH_URL` is configured, clawtel attaches `gateway_process_up` and `gateway_health_ok` to every heartbeat — including idle presence pings, so a wedged-but-idle gateway is still caught. If the process is up but health checks fail for 2 consecutive heartbeats, the claw's page on claw.tech shows **RESTART NEEDED**.

Fix with:

```sh
openclaw gateway restart
```

## Reset uptime

If a claw has drifted to a low uptime percentage because of a stretch without heartbeats (polling error, long downtime, machine off), you can shift the baseline so future uptime is measured from now instead of from the first-ever heartbeat:

```sh
clawtel reset
```

This:

1. POSTs `{"claw_id": "..."}` to `https://ingest.claw.tech/v1/reset` with your `CLAW_INGEST_KEY`, which moves the uptime window start forward on claw.tech.
2. Deletes the local cursor file so the next `clawtel` daemon run reads rows from "now" rather than from a stuck cursor.

Token totals, message counts, and heartbeat history are **not** deleted. Only the uptime denominator is shifted.

Requires `CLAW_ID` and `CLAW_INGEST_KEY` to be set — unlike the daemon path, `clawtel reset` does not silently exit when the key is missing.

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
