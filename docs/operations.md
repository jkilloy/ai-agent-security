# Operations

How to run, configure, deploy, size, monitor, test and troubleshoot the system.

Contents:

- [Running locally](#running-locally)
- [Configuration](#configuration)
- [Deploying](#deploying)
- [Capacity](#capacity)
- [Logs and monitoring](#logs-and-monitoring)
- [Data management](#data-management)
- [Testing](#testing)
- [Troubleshooting](#troubleshooting)

---

## Running locally

Requirements: Docker with Compose v2. Go 1.25+ is needed only for `streamcall` and the tests.

```sh
cp .env.example .env              # then edit: at least POSTGRES_PASSWORD and ADMIN_PASSWORD
                                  # (set POSTGRES_PASSWORD before the first `up`: it only applies to a new volume)
docker compose up -d --build      # the first build downloads the detector models (~2 GB) and self-tests them
docker compose ps                 # wait until detectors is "healthy" (up to ~3 min on first start)

# stream a test call
go run ./cmd/streamcall -file some.wav \
  -vars '{"caller":"+15551234567","encoding":"pcm16","sample_rate":8000}'

# look at it
open http://localhost:8080/admin/             # or: curl localhost:8080/calls/<call_id>
```

To generate test audio with a real AI watermark:

```sh
docker compose run --rm -v "$PWD:/out" detectors \
  python -m detsvc.tools.make_sample --mark audioseal --rate 8000 --telephony --out /out/audioseal.wav
go run ./cmd/streamcall -file audioseal.wav -vars '{"encoding":"pcm16","sample_rate":8000}' -interval 20ms
```

If port 8080 is taken, set `CORE_PORT` in `.env` and use that port in the URLs above.

Useful commands:

| | |
|---|---|
| `docker compose logs -f core` | Core logs (JSON) |
| `docker compose logs -f detectors` | Detector service logs |
| `docker compose up -d --build core` | Rebuild and restart only the core, for example after adding a plugin |
| `docker compose down` | Stop everything, keeping data |
| `docker compose down -v` | Stop and **delete all calls, audio and detector releases** |

## Configuration

All configuration is through environment variables. Compose reads overrides from `.env` (see [`.env.example`](../.env.example)).

### Core

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | *(required)* | Postgres connection string. Compose sets it from the `POSTGRES_*` values. |
| `LISTEN_ADDR` | `:8080` | HTTP listen address inside the container |
| `AUDIO_DIR` | `/data/audio` | Where `<call_id>.raw` files are written. Created with mode 0700 if missing. |
| `RETENTION` | `720h` | How long a call is kept after it ends (Go duration: `90m`, `72h`, `2160h`) |
| `CLEANUP_INTERVAL` | `1h` | How often expired calls are deleted. Must be positive. |
| `PLUGIN_TIMEOUT` | `30s` (Compose: `30m`) | How long plugins may run after the audio ends. With `aivoice` it must cover the longest time a call's analysis can wait in the detector queue. |
| `FIRST_MESSAGE_TIMEOUT` | `10s` | Upgrade → variables message |
| `MAX_CALL_DURATION` | `4h` | Longest call. Longer calls are ended and saved. |
| `MAX_FRAME_BYTES` | `1048576` | Largest WebSocket message |
| `SHUTDOWN_TIMEOUT` | `60s` | How long shutdown waits for in-flight calls to save. Keep it below the container stop grace period (90 s in Compose). |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `ADMIN_USER` | `admin` | Basic-auth user for `/admin/` and `/api/` |
| `ADMIN_PASSWORD` | *(unset)* | Basic-auth password. **Unset means no login**, and a warning is logged at startup. |
| `AIVOICE_URL` | *(unset; Compose: `ws://detectors:8000/v1/stream`)* | Enables the `aivoice` plugin |
| `AIVOICE_DEFAULT_ENCODING` | *(unset)* | Encoding to assume when a call doesn't send `encoding` |
| `AIVOICE_DEFAULT_SAMPLE_RATE` | *(unset)* | Sample rate to assume when a call doesn't send `sample_rate` |

### Compose-only

| Variable | Default | Meaning |
|---|---|---|
| `CORE_PORT` | `8080` | Host port published for the core |
| `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB` | `callsec` | Database credentials. **Change the password.** Postgres is not published to the host. |
| `DETECTOR_*` | | Passed to the detector service as the variable without the prefix, for example `DETECTOR_WORKERS` → `WORKERS` |
| `RESEMBLE_API_KEY`, `REALITY_DEFENDER_API_KEY` | *(unset)* | Enable those vendor APIs. **This sends call audio to the vendor.** |

### Detector service

See [detectors/README.md § Configuration](../detectors/README.md#configuration): `WORKERS`, `TORCH_THREADS`, `WINDOW_SECONDS`, `SILENCE_RMS`, `FINISH_TIMEOUT`, `UPDATE_ENABLED`, `UPDATE_INTERVAL`, `CATALOG_URL`, `RELEASES_KEEP`, `API_MAX_SECONDS` and `API_TIMEOUT`.

### Timeouts that must line up

Each timeout in this chain must be shorter than the one after it:

| Inner | must be shorter than | Outer | Defaults (Compose) |
|---|---|---|---|
| detector queue wait (+ `API_TIMEOUT` if APIs are on) | < | core `PLUGIN_TIMEOUT` | unbounded (`FINISH_TIMEOUT=0`) < 30m |
| core `SHUTDOWN_TIMEOUT` | < | container `stop_grace_period` | 60s < 90s |
| client's read timeout for `complete` | ≥ | the time until results are saved | your value |

**The detector service never drops work, so results can lag.** Each call's AI-voice analysis waits behind earlier calls in the detector queue, and the core saves a call only once every plugin has returned. With enough hardware this is seconds; after a busy period it can be minutes. Three consequences:

- `PLUGIN_TIMEOUT` caps the wait. A call still queued after 30 minutes is saved with `results.aivoice = {"error": "timed out…"}`.
- Clients waiting for `complete` wait for the analysis too. Either hang up after sending `end` (the call is still saved), or set a long read timeout.
- **Shutdown waits only `SHUTDOWN_TIMEOUT` (60 s).** Calls whose analysis is still queued at that point are lost, so drain traffic before restarting the core.

## Deploying

The Compose file is a complete single-host deployment. For anything beyond a lab:

1. **Put a TLS reverse proxy in front of the core,** and publish only that. Example for nginx:

   ```nginx
   location /ws/call {
       proxy_pass http://core:8080;
       proxy_http_version 1.1;
       proxy_set_header Upgrade $http_upgrade;
       proxy_set_header Connection "upgrade";
       proxy_read_timeout 4h;          # >= MAX_CALL_DURATION; calls can be silent for long stretches
       proxy_send_timeout 4h;
       client_max_body_size 0;
       # authenticate integrations here (mTLS, IP allowlist, or auth_request) until the core does
   }
   location / {
       proxy_pass http://core:8080;   # /admin/, /api/, /calls/, /healthz
   }
   ```

2. **Authenticate `/ws/call` and `/calls/{id}`** at the proxy (mTLS or an IP allowlist for your media servers). The core doesn't authenticate them yet ([architecture.md §11](architecture.md#11-security)).
3. **Set `ADMIN_PASSWORD`,** or restrict `/admin/` and `/api/` to an SSO-protected path at the proxy.
4. **Set strong `POSTGRES_PASSWORD`** and keep Postgres unpublished.
5. **Pin images and back up volumes** (below).
6. **Decide on detector updates:** automatic (the default), `UPDATE_ENABLED=false` with image rebuilds, or a reviewed catalog at `CATALOG_URL`. See the detector [trust model](../detectors/README.md#trust-model).

### Images

| Image | Base | Notes |
|---|---|---|
| core ([`Dockerfile`](../Dockerfile)) | `gcr.io/distroless/static-debian12:nonroot` | Static Go binary, runs as UID 65532, no shell. About 14 MB. |
| detectors ([`detectors/Dockerfile`](../detectors/Dockerfile)) | `python:3.12-slim` | CPU torch, plus a baked base release that was self-tested at build time. About 3.2 GB. |

### Scaling

- **One core process handles many concurrent calls.** Each call is a few goroutines plus its queued frames.
- **Several core replicas** behind a load balancer work as-is: calls are independent, and all state is in Postgres and the audio volume. The audio volume must be **shared** (all replicas must see every file) for admin playback and cleanup to work. A volume per replica also works if every replica's cleaner only ever sees its own files: rows are shared, and a missing file is tolerated, but playback then works only through the replica that recorded the call. Shared storage is simpler.
- **The detector service is the bottleneck** for `aivoice`. Scale it with `DETECTOR_WORKERS` and CPU first. Running several detector containers behind one URL also works, because each call is one WebSocket and is handled wholly by one instance.

## Capacity

Per concurrent call, roughly:

| Resource | Cost |
|---|---|
| Core memory | Queued frames per plugin. Near zero when plugins keep up; up to the remaining call audio for a stalled plugin. |
| Core CPU | Negligible (copying bytes) |
| Disk | The audio bit rate (below) |
| Network to the detector service | The audio bit rate again, one WebSocket |
| Detector CPU | See below |

Audio bit rates:

| Format | Bytes/s | Per hour |
|---|---|---|
| 8 kHz mono mulaw/alaw | 8,000 | 28.8 MB |
| 8 kHz mono pcm16 | 16,000 | 57.6 MB |
| 8 kHz stereo pcm16 | 32,000 | 115 MB |
| 16 kHz stereo pcm16 | 64,000 | 230 MB |

Disk needed ≈ calls per day × average call length × bit rate × retention in days. For example, 1,000 five-minute mono mulaw calls a day kept for 30 days is 1000 × 300 s × 8 KB/s × 30 ≈ **72 GB**. Postgres adds about 1–5 KB per call.

**Detector throughput.** Each speech window (4 s per channel) runs the watermark detectors, and every second window runs the wav2vec2 classifier, at most 15 per channel. On CPU the classifier takes about 1–5 s per window, and the watermark detectors less. To keep up in real time you need roughly `concurrent channels × (work seconds per window ÷ 4 s)` workers. Nothing is dropped when the hardware falls behind; reports just arrive later. Measure on your hardware: stream real-time-paced calls (`-interval 20ms`) at your expected concurrency, and watch `analysis.seconds_after_call` in the reports. If it keeps growing, the queue isn't draining. Increase `DETECTOR_WORKERS` and CPU until it stays at a few seconds. Each worker is a separate process that holds every model in memory, so check the memory per worker with `docker stats` before adding more.

## Logs and monitoring

The core logs JSON lines to stdout (`log/slog`). Useful messages:

| `msg` | Level | Fields | Meaning |
|---|---|---|---|
| `plugins loaded` | info | `plugins` | At startup: which plugins are active. Check that `aivoice` is listed. |
| `listening` | info | `addr`, `retention`, `admin_ui` | Ready |
| `admin UI has no password…` | warn | | `ADMIN_PASSWORD` is unset |
| `call started` | info | `call_id`, `variables` | Accepted |
| `call ended` | info | `call_id`, `reason`, `audio_bytes`, `duration` | Audio over. `reason` is one of `end message`, `client closed`, `hangup`, `max call duration`, `server shutdown`, `storage error`. |
| `call saved` | info | `call_id`, `plugins` | Stored |
| `rejected call` | info | `remote`, `err` | Bad first message (1008) |
| `plugin failed` | warn | `plugin`, `call_id`, `err` | A plugin returned an error or timed out |
| `plugin panicked` | error | `plugin`, `call_id`, `panic`, `stack` | |
| `plugin ignored cancellation; abandoning` | error | `plugin` | A plugin bug: it doesn't honor `ctx` |
| `save call` | error | `call_id`, `err` | **The call was lost.** Investigate Postgres. |
| `retention sweep` / `retention sweep failed` | info / error | `deleted`, `err` | Cleanup |

Health checks:

- Core: `GET /healthz` returns 200 if the database answers.
- Detectors: `GET /healthz` on port 8000 (internal) returns 200 once workers are ready. `GET /v1/status` shows the active release, versions, self-test results and update history.

What to alert on:

- any `save call` error;
- `plugin failed` rate by plugin;
- calls with plugin errors (`/api/calls?errors=1`) appearing;
- growing `analysis.seconds_after_call` in AI-voice reports (the detector queue isn't keeping up);
- disk usage of the audio volume;
- `/healthz` failures.

## Data management

- **Find a call:** in the admin UI, search by any variable value or by call ID prefix. With the API, use `GET /api/calls?q=…`.
- **Delete a call now** (for example for a data-subject request): there is no API yet. Set its expiry and let the cleaner remove it, which deletes the file and then the row:

  ```sh
  docker compose exec postgres psql -U callsec -c \
    "UPDATE calls SET expires_at = now() WHERE id = '<call_id>';"
  ```

  It disappears from the API immediately, and from disk at the next sweep (within `CLEANUP_INTERVAL`, or restart the core to sweep now).
- **Change retention:** set `RETENTION` and restart. This applies to calls saved afterwards. To apply it to existing calls: `UPDATE calls SET expires_at = ended_at + interval '7 days';`.
- **Backups:** back up the `pgdata` volume (`pg_dump`) and the `audio` volume together. A row without its audio still shows its results. Audio without its row is unreachable, and the cleaner never removes it.
- **Detector releases** live in the `detectors` volume. Deleting it is safe: the service falls back to the baked base release and updates again.

## Testing

```sh
go vet ./... && go test -race ./...                       # core, plugins, admin (no database needed)

# Postgres query tests: against a scratch database (it TRUNCATES calls)
docker compose exec postgres createdb -U callsec callsec_test
TEST_DATABASE_URL=postgres://callsec:<pw>@localhost:5432/callsec_test?sslmode=disable go test ./internal/store/
#   (publish 5432 temporarily, or run go test inside a container on the compose network)

# Detector service
docker build -t agent-security-detectors detectors
docker run --rm agent-security-detectors python -m pytest -q tests
```

End-to-end smoke test: bring the stack up, stream a watermarked sample (see [Running locally](#running-locally)), and confirm on the call's admin page that `results.aivoice.verdict` is `watermark_detected` and that the recording plays.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Connection closes immediately with 1008 | The first message is not a JSON object text frame, or was sent too late | Send `{"…": …}` as text within `FIRST_MESSAGE_TIMEOUT` |
| `results.aivoice = {"error":"call variables must include \"encoding\" and \"sample_rate\"…"}` | Format variables missing, or `sample_rate` sent as a string | Send them as JSON numbers, or set the `AIVOICE_DEFAULT_*` variables |
| `results.aivoice = {"error":"connect to detector service: …"}` | The detector service is down or still starting | `docker compose ps`, `docker compose logs detectors`. On first start it needs up to ~3 min. |
| `results.aivoice = {"error":"detector service rejected call: unsupported encoding …"}` | Unknown encoding name | Use `mulaw`, `alaw`, `pcm16`, `f32le` or `u8` |
| `aivoice` missing from results entirely | `AIVOICE_URL` unset | Check the `plugins loaded` log line |
| Calls take minutes to appear after hanging up; large `analysis.seconds_after_call` | The detector queue is behind; calls wait their turn | More `DETECTOR_WORKERS` or CPU |
| `results.aivoice = {"error":"timed out…"}` | The call's analysis was still queued after `PLUGIN_TIMEOUT` | More detector capacity, or a longer `PLUGIN_TIMEOUT` |
| Audio plays as loud noise | Wrong encoding or byte order (big-endian L16 sent as `pcm16`, or mulaw labeled `pcm16`) | Fix the variables or byte-swap at the source |
| Audio plays too fast or slow, or at the wrong pitch | Wrong `sample_rate` or `channels` | Fix the variables |
| Player says the call can't be played (422) | The variables don't describe the format | Send `encoding` and `sample_rate` |
| No `complete` message | The client timed out before the plugins finished, or the client closed first | Read timeout above `PLUGIN_TIMEOUT` |
| Every plugin result is `{"error":"timed out…"}` | `PLUGIN_TIMEOUT` too short for the slowest plugin | Raise it, keeping the [timeout chain](#timeouts-that-must-line-up) in order |
| Core exits at startup with `connect to database` | Postgres not reachable within 30 s | Check `DATABASE_URL` and the Postgres health. Compose restarts the core. |
| Postgres logs `password authentication failed for user "callsec"` and the core keeps restarting | `POSTGRES_PASSWORD` was changed after the `pgdata` volume was created. Postgres sets the password only when it first creates the volume. | Keep your data and set the new password: `docker compose exec -T postgres psql -U callsec -d callsec -c "ALTER ROLE callsec PASSWORD '<new password>'"`, then `docker compose up -d`. Or delete everything: `docker compose down -v`. |
| Calls missing after a core crash | In-flight calls are only saved at the end | Known limitation ([architecture.md §13](architecture.md#13-future-work)) |
| Browser connection to `/ws/call` gets 403 | Cross-origin browser connection rejected | Connect from a server, or from a page served by the same host |
