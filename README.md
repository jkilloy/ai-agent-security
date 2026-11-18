# agent-security: call audio analysis

The core of a security system that analyzes phone-call audio.

- A client streams a call over a WebSocket. The core attaches the call's variables and audio to a **Call** object.
- Every registered **analysis plugin** runs against the audio live, as it arrives.
- Each plugin's JSON output is attached to the Call.
- The Call is stored in Postgres, with the raw audio on disk, for a configurable retention period.
- An admin UI built into the core shows every call and its analysis.

```
phone system ──WS /ws/call──▶ core ──▶ plugins (live) ──▶ Postgres + audio files ──▶ /calls/{id}, admin UI
  JSON variables, then audio        └─ aivoice ──WS──▶ detector service (AI-voice watermarks & classifiers)
```

## Documentation

Full documentation is in **[`docs/`](docs/README.md)**:

| | |
|---|---|
| **[WebSocket call API](docs/websocket-api.md)** | How to integrate a phone system: the protocol, the first message and identifiers, audio formats, sending audio, ending calls, errors, and examples in Go, Python and Node, plus Twilio and SIP bridges |
| **[Plugins](docs/plugins.md)** | The plugin interface and how to write, configure, register and test a plugin |
| **[HTTP API](docs/http-api.md)** | `GET /calls/{id}`, `/healthz`, and the admin `/api/*` |
| **[Architecture and code design](docs/architecture.md)** | Components, data model, call lifecycle, concurrency, failure handling, security, design decisions, future work |
| **[Operations](docs/operations.md)** | Configuration reference, deployment, capacity, logs, data management, testing, troubleshooting |
| **[Detector service](detectors/README.md)** | AI-voice detection: detectors, report format, automatic updates, trust model |

## Plugins

| Plugin | What it does |
|---|---|
| `audiostats` | An example plugin: frame and byte counts, plus the audio duration estimated from `encoding` and `sample_rate`. |
| `aivoice` | Detects AI-generated voices, using watermarks (AudioSeal, Perth), synthetic-speech classifiers and, optionally, commercial APIs. It streams the call to the Python **detector service** in `detectors/`, which keeps its detectors up to date from upstream. It is enabled when `AIVOICE_URL` is set, which Compose does. |

## Quick start

```sh
cp .env.example .env           # set POSTGRES_PASSWORD and ADMIN_PASSWORD; adjust port / retention
docker compose up -d --build   # first build downloads models (~2 GB) and self-tests them
go run ./cmd/streamcall -url ws://localhost:8080/ws/call \
  -file some.wav -vars '{"caller":"+15551234567","encoding":"pcm16","sample_rate":8000}'
curl localhost:8080/calls/<call_id>
```

Then open **http://localhost:8080/admin/** to see the call.

Run the tests with `go test -race ./...`. They need no database. See [operations.md § Testing](docs/operations.md#testing) for the Postgres and detector-service tests.

## The protocol in brief

| Step | Direction | Frame | Content |
|---|---|---|---|
| 1 | client → core | text | A JSON **object** of call variables, for example `{"caller":"+15551234567","pbx_call_id":"abc","encoding":"mulaw","sample_rate":8000,"channels":2,"channel_labels":["caller","agent"]}` |
| 2 | core → client | text | `{"type":"accepted","call_id":"<uuid>"}` |
| 3 | client → core | binary | Raw audio bytes: no headers, channels interleaved frame by frame, about 20 ms per message |
| 4 | client → core | text | `{"type":"end"}`, **or just disconnect** |
| 5 | core → client | text | `{"type":"complete","call_id":"<uuid>"}` once the results are saved |

**Recommended audio:** G.711 μ-law at 8000 Hz, 2 channels with both sides of the call on one connection (channel 0 = caller/remote, channel 1 = agent/local), 320-byte (20 ms) binary messages. PCM16 little-endian and A-law are also supported. Compressed codecs (G.722, G.729, Opus) must be decoded to PCM16 first.

Audio format details, handling both sides of a call, limits, error codes and examples: [docs/websocket-api.md](docs/websocket-api.md).

## Writing a plugin in brief

```go
func init() { plugin.Register(P{}) }       // and import the package in plugins/all.go

type P struct{}

func (P) Name() string { return "myplugin" } // key under call.results

func (P) Analyze(ctx context.Context, in *plugin.Input) (json.RawMessage, error) {
	for {
		frame, err := in.Audio.Next(ctx) // live audio; io.EOF when the call ends
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		_ = frame // read-only
	}
	return json.Marshal(map[string]any{"verdict": "ok"})
}
```

Lifecycle, rules and patterns: [docs/plugins.md](docs/plugins.md).

## Admin UI

**http://localhost:8080/admin/** is served by the core itself, embedded in the binary with no build step.

- **Call list.** Shows each call's verdict, the caller, and an *evidence strip* per audio channel marking where AI-voice evidence was found. You can search by any variable or call ID, and filter by verdict or by plugin errors.
- **Call page.** Shows a plain-language verdict, the risk score, the call audio (converted to WAV, and seekable), a per-detector timeline, detector results, variables, every other plugin's output, and the raw JSON.

Set `ADMIN_PASSWORD` to require basic auth for `/admin/` and `/api/`. **Without it the UI is open to anyone who can reach the port.**

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
docs/                documentation
```

## Status

The core, the two plugins, the detector service and the admin UI work end to end under Docker Compose. **`/ws/call` and `/calls/{id}` have no authentication yet.** Keep them on a private network or behind an authenticating proxy until they do. The rest of the roadmap is in [architecture.md § Future work](docs/architecture.md#13-future-work).
