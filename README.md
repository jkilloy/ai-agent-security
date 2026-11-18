# agent-security — call audio analysis core

The core of a security system that analyzes phone-call audio. A client streams a call over a WebSocket. The core attaches the call's variables and audio to a **Call** object and runs every registered **analysis plugin** against the audio live, as it arrives. Each plugin's JSON output is attached to the Call, and the Call is stored in Postgres, with the raw audio on disk, for a configurable retention period.

## Plugins

| Plugin | What it does |
|---|---|
| `audiostats` | An example plugin: frame and byte counts, plus the audio duration estimated from `encoding` and `sample_rate`. |
| `aivoice` | Detects AI-generated voices, using watermarks (AudioSeal, Perth), synthetic-speech classifiers and, optionally, commercial APIs. It streams the call to the Python **detector service** in `detectors/`, which keeps its detectors up to date from upstream. See [detectors/README.md](detectors/README.md). It is enabled when `AIVOICE_URL` is set, which Compose does. |

## Quick start

```sh
cp .env.example .env          # optional; adjust port / retention / password
docker compose up -d --build   # first build downloads models (~2 GB) and self-tests them
go run ./cmd/streamcall -url ws://localhost:8080/ws/call \
  -file some.wav -vars '{"caller":"+15551234567","encoding":"pcm16","sample_rate":8000}'
curl localhost:8080/calls/<call_id>
```

Run the tests with `go test -race ./...`. They need no database. The Postgres query tests run only when `TEST_DATABASE_URL` points at a scratch database, which they truncate.

## Admin UI

Open **http://localhost:8080/admin/**. It is served by the core itself (embedded in the binary, no build step) and lists every stored call, newest first:

- **Call list.** Each call shows its verdict, the caller, and an *evidence strip*: a bar the length of the call, marked where AI-voice evidence was found (magenta for a watermark, amber for a machine-generated-sounding voice), with one row per audio channel.
  - Search by any variable value or by call ID prefix.
  - Filter to flagged calls, a single verdict, or calls where a plugin errored.
  - The first page refreshes every 15 seconds.
- **Call page.**
  - A plain-language verdict, the risk score, and the call audio. The audio is converted to WAV from mu-law, A-law or PCM, and can be seeked.
  - A timeline with one lane per detector per channel. It follows the audio, and you can click a lane to play from that point.
  - Per-detector results, including AudioSeal's watermark payload, and the caveats.
  - Detector versions, call details and variables.
  - Every other plugin's output, rendered generically, plus each plugin's raw JSON.

Set `ADMIN_PASSWORD` to require HTTP basic auth (user `ADMIN_USER`, default `admin`) for `/admin/` and `/api/`. **Without it the UI is open to anyone who can reach the port**, and the core logs a warning at startup. All call data is rendered as text (never as HTML) under a strict Content-Security-Policy, because call variables come from outside callers.

Read-only JSON API behind the UI:

| Endpoint | |
|---|---|
| `GET /api/stats` | Counts of stored calls: total, last 24 h, flagged, plugin errors, and each AI-voice verdict |
| `GET /api/calls` | Newest first. Takes `limit` (≤200), `before` (the cursor from `next`), `q` (search) and `verdict` (`flagged`, `errors`, or an aivoice verdict). |
| `GET /api/calls/{id}` | One call with all plugin results |
| `GET /api/calls/{id}/audio` | Call audio as 16-bit PCM WAV, with Range support. This needs `encoding` and `sample_rate` in the call's variables. |

## WebSocket protocol — `GET /ws/call`

| Step | Direction | Frame | Content |
|---|---|---|---|
| 1 | client → server | text | A JSON **object** of call variables (any JSON values). |
| 2 | server → client | text | `{"type":"accepted","call_id":"<uuid>"}` |
| 3 | client → server | binary | Audio frames, passed through unchanged. |
| 4 | client → server | text | `{"type":"end"}` — **or just disconnect** (a hangup also ends the call). |
| 5 | server → client | text | `{"type":"complete","call_id":"<uuid>"}` after the results are saved (only if the client is still connected), then a normal close. |

- If the first message is not a JSON object, or does not arrive within `FIRST_MESSAGE_TIMEOUT`, the connection is closed with code **1008**.
- The core does not interpret the audio. Audio-format keys such as `encoding`, `sample_rate` and `channels` are ordinary variables that plugins read.
- When a call reaches `MAX_CALL_DURATION`, or the server shuts down, the call ends as if the caller hung up. The results are still saved, but the connection may drop before `complete` is sent.
- Text frames other than `{"type":"end"}` are ignored.

## HTTP API

- `GET /calls/{id}` returns the call's variables, `results` (keyed by plugin name), `audio_bytes`, `started_at`, `ended_at` and `expires_at`. It returns 404 for unknown or expired calls and 400 for an ID that is not a UUID.
- `GET /healthz` returns `{"status":"ok"}`, or 503 if the database is unreachable.

## Writing a plugin

```go
package myplugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"agent-security/internal/plugin"
)

func init() { plugin.Register(P{}) }

type P struct{}

func (P) Name() string { return "myplugin" } // key under call.results

func (P) Analyze(ctx context.Context, in *plugin.Input) (json.RawMessage, error) {
	// in.CallID, in.Variables (your own deep copy), in.Audio (your own live stream)
	for {
		frame, err := in.Audio.Next(ctx) // or treat in.Audio as an io.Reader
		if errors.Is(err, io.EOF) {
			break // call ended
		}
		if err != nil {
			return nil, err // ctx cancelled: timed out
		}
		_ = frame // analyze; do not modify (frames are shared)
	}
	return json.Marshal(map[string]any{"verdict": "ok"})
}
```

Then add `_ "agent-security/plugins/myplugin"` to `plugins/all.go`.

How the engine runs plugins:
- Every plugin is subscribed **before** the client receives `accepted`, so it sees every frame.
- Each plugin has its own unbounded queue, so a slow plugin never blocks incoming audio or the other plugins.
- After the audio ends, plugins have `PLUGIN_TIMEOUT` to finish before their context is cancelled. A plugin that ignores cancellation is abandoned after a 2-second grace period.
- An error, panic, invalid JSON or timeout is recorded as `{"error":"..."}` under the plugin's name, and the other plugins are unaffected.
- A plugin may return early without reading the whole stream; its subscription is then released.

## Storage and retention

- Calls are stored in Postgres in the `calls` table: `id`, `variables` jsonb, `results` jsonb, `audio_path`, `audio_bytes`, `started_at`, `ended_at`, `expires_at`. Migrations in `internal/store/migrations/` are embedded and applied on startup.
- Raw audio is written as it arrives to `AUDIO_DIR/<call_id>.raw` (the `audio` volume under Compose).
- `expires_at = ended_at + RETENTION`.
- A cleanup task runs at startup and then every `CLEANUP_INTERVAL`. For each expired call it deletes the audio file first, then the row.

## Configuration (environment)

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | *(required)* | Postgres connection string (set automatically by Compose) |
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `AUDIO_DIR` | `/data/audio` | Where raw call audio is written |
| `RETENTION` | `720h` | How long a call is kept after it ends |
| `CLEANUP_INTERVAL` | `1h` | How often expired calls are deleted |
| `PLUGIN_TIMEOUT` | `30s` | How long plugins may run after the audio ends (Compose sets 75s for `aivoice`) |
| `AIVOICE_URL` | *(unset)* | Detector service stream URL; enables the `aivoice` plugin |
| `AIVOICE_DEFAULT_ENCODING` / `AIVOICE_DEFAULT_SAMPLE_RATE` | *(unset)* | Audio format to assume when a call's variables don't say |
| `FIRST_MESSAGE_TIMEOUT` | `10s` | How long to wait for the variables message |
| `MAX_CALL_DURATION` | `4h` | Longest allowed call |
| `MAX_FRAME_BYTES` | `1048576` | Largest accepted WebSocket message |
| `SHUTDOWN_TIMEOUT` | `60s` | How long shutdown waits for in-flight calls to save |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `ADMIN_USER` / `ADMIN_PASSWORD` | `admin` / *(unset)* | Basic auth for the admin UI and `/api/`. Unset means no login. |

Compose also reads `CORE_PORT` (the host port, default 8080) and the `POSTGRES_*` credentials from `.env`. Postgres is not published to the host.

## Layout

```
cmd/core/            service entrypoint: wiring, HTTP server, graceful shutdown
cmd/streamcall/      test client: streams a file (WAV header stripped) as a call
internal/config/     environment configuration
internal/call/       the Call object
internal/audio/      Broadcaster: fan-out of frames to plugin streams + disk sink
internal/plugin/     Plugin interface, Input, registry
internal/engine/     runs plugins per call: timeouts, panic recovery, result collection
internal/server/     WebSocket protocol, HTTP API
internal/admin/      admin UI (static/, embedded) + read-only /api, WAV conversion
internal/store/      Postgres persistence + embedded migrations
internal/retention/  expiry cleanup task
plugins/             analysis plugins + all.go registry imports
  audiostats/        example plugin
  aivoice/           AI-voice detection (client of the detector service)
detectors/           Python detector service: models, upstream updater, self-tests
```

## Shutdown

On SIGINT or SIGTERM, the service:
1. Stops accepting new connections.
2. Ends in-flight calls as if the callers hung up.
3. Waits up to `SHUTDOWN_TIMEOUT` for their plugins to finish and their results to save.
4. Stops the cleanup task.

Compose's `stop_grace_period` is set to 90s to allow for this.

## Not done yet

- Authentication on the WebSocket and `GET /calls/{id}`. The admin UI and `/api/` have optional basic auth. This is required before the service is exposed anywhere.
- Plugins that run in a separate process or are loaded dynamically (plugins are currently compiled in).
- Plugins pushing events or alerts during a call.
- Per-call retention, for example set from the variables.
