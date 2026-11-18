# HTTP API

The core serves the WebSocket call endpoint ([websocket-api.md](websocket-api.md)), the HTTP endpoints below, and the admin UI, all on one port (`LISTEN_ADDR`, `:8080` by default).

| Endpoint | Auth | Purpose |
|---|---|---|
| `GET /ws/call` | none | Stream a call ([websocket-api.md](websocket-api.md)) |
| `GET /calls/{id}` | none | Fetch one call's variables and results (integration API) |
| `GET /healthz` | none | Liveness and database check |
| `GET /admin/` | admin | The admin UI |
| `GET /api/stats` | admin | Counts for the admin dashboard |
| `GET /api/calls` | admin | List and search calls |
| `GET /api/calls/{id}` | admin | One call (same body as `/calls/{id}`) |
| `GET /api/calls/{id}/audio` | admin | The call audio as WAV |

"admin" means HTTP basic auth is required when `ADMIN_PASSWORD` is set. When it isn't set, those endpoints are open (see [§4](#4-authentication-and-headers)).

All JSON errors have the shape `{"error": "<message>"}`.

---

## 1. Integration endpoints

Implemented in [`internal/server/server.go`](../internal/server/server.go).

### `GET /calls/{id}`

Returns one stored call.

```sh
curl -s http://localhost:8080/calls/8f0c2b4e-6a3d-4b1e-9c7a-2d5f1e0b3a91
```

```json
{
  "id": "8f0c2b4e-6a3d-4b1e-9c7a-2d5f1e0b3a91",
  "variables": { "caller": "+15551234567", "encoding": "mulaw", "sample_rate": 8000 },
  "results": {
    "audiostats": { "frames": 3000, "bytes": 480000, "largest_frame": 160, "duration_seconds": 60 },
    "aivoice": { "verdict": "no_evidence", "risk_score": 0.36, "channels": [], "audio": {}, "detectors": {}, "caveats": [] }
  },
  "audio_bytes": 480000,
  "started_at": "2026-11-18T14:02:11.402Z",
  "ended_at": "2026-11-18T14:03:12.118Z",
  "expires_at": "2026-12-18T14:03:12.118Z"
}
```

| Field | Type | Meaning |
|---|---|---|
| `id` | UUID string | The call ID, as returned in `accepted` |
| `variables` | object | The first WebSocket message, as sent |
| `results` | object | Each plugin's JSON output, keyed by plugin name. A failed plugin has `{"error": "…"}`. |
| `audio_bytes` | integer | Bytes of audio received |
| `started_at` | RFC 3339 UTC | When the first message was accepted |
| `ended_at` | RFC 3339 UTC | When the audio ended (`end`, hangup, max duration or shutdown) |
| `expires_at` | RFC 3339 UTC | `ended_at + RETENTION`. After this time the call returns 404, and the cleanup task deletes it. |

| Status | When |
|---|---|
| 200 | Found |
| 400 | `{"error":"invalid call id"}`: the ID is not a UUID |
| 404 | `{"error":"not found"}`: unknown, not yet saved (still in progress), or expired |
| 500 | `{"error":"internal error"}`: database failure (details in the core log) |

The audio itself is not included. Use `/api/calls/{id}/audio`.

### `GET /healthz`

```json
{"status": "ok"}
```

Returns 503 with `{"status":"db unavailable"}` if a database ping fails within 2 s. Use it for load-balancer and orchestrator health checks. It does not check the detector service, which has its own `/healthz`.

## 2. Admin API

Implemented in [`internal/admin/admin.go`](../internal/admin/admin.go). It is read-only, and it is what the admin UI uses. It is a reasonable API for dashboards and exports too.

### `GET /api/stats`

```json
{
  "total": 1287,
  "last_24h": 112,
  "flagged": 9,
  "errors": 2,
  "verdicts": { "no_evidence": 1201, "insufficient_audio": 75, "likely_synthetic": 7, "watermark_detected": 2 }
}
```

| Field | Meaning (stored, unexpired calls only) |
|---|---|
| `total` | All calls |
| `last_24h` | Calls that started in the last 24 hours |
| `flagged` | `results.aivoice.verdict` is `watermark_detected` or `likely_synthetic` |
| `errors` | Calls where at least one plugin result is an object with an `error` key |
| `verdicts` | Count per `results.aivoice.verdict`. Calls without an `aivoice` verdict are not counted. |

### `GET /api/calls`

Lists calls newest first, with cursor paging.

| Query parameter | Default | Meaning |
|---|---|---|
| `limit` | 50 | Page size, 1–200. Out-of-range values become 50. |
| `before` | (none) | The opaque cursor from the previous page's `next` |
| `q` | (none) | Search, up to 200 characters. Matches a **call ID prefix**, or any text anywhere in the **variables** (case-insensitive substring of their JSON). |
| `verdict` | (none) | `flagged`, `errors`, or one AI-voice verdict: `watermark_detected`, `likely_synthetic`, `no_evidence`, `insufficient_audio`. Anything else returns 400. |

```sh
curl -s -u admin:$ADMIN_PASSWORD 'http://localhost:8080/api/calls?limit=2&verdict=flagged'
```

```json
{
  "calls": [ { "id": "…", "variables": {}, "results": {}, "audio_bytes": 0, "started_at": "…", "ended_at": "…", "expires_at": "…" } ],
  "next": "eyJ0IjoiMjAyNi0xMS0xOFQxNDowMjoxMS40MDJaIiwiaWQiOiI4ZjBjLi4uIn0"
}
```

- Each element of `calls` has the same shape as `GET /calls/{id}`.
- `next` is `null` on the last page. To get the next page, pass it as `before`, with the same `q` and `verdict`.
- The order is `started_at DESC, id DESC`, so paging stays stable while new calls arrive.

Errors: 400 `unknown verdict filter`, 400 `invalid cursor`.

Note that the search matches the variables' JSON text, keys included. For example, `q=caller` matches every call that has a `caller` variable.

### `GET /api/calls/{id}`

Same body and errors as `GET /calls/{id}`, except the 404 message is `call not found or expired`.

### `GET /api/calls/{id}/audio`

The call's raw audio, converted on the fly to a **16-bit PCM WAV**, with the original sample rate and channel count.

- It uses the call's `encoding`, `sample_rate` and `channels` variables. See [websocket-api.md §4](websocket-api.md#4-audio-format) for the accepted values.
- `mulaw`, `alaw`, `u8` and `f32le` are converted to 16-bit, and `pcm16` is passed through.
- **Range requests are supported** (`206 Partial Content`), so browsers can seek. Conversion is streamed, so the whole file is never held in memory.
- A trailing partial frame (fewer bytes than one sample of every channel) is ignored.

| Status | When |
|---|---|
| 200 / 206 | `Content-Type: audio/wav` |
| 400 | Invalid call ID |
| 404 | Unknown or expired call, or the audio file is missing |
| 422 | `cannot play this call: unknown audio encoding "…"` / `missing or invalid sample_rate` / `invalid channels`: the variables don't describe the audio |

```sh
curl -s -u admin:$ADMIN_PASSWORD -o call.wav http://localhost:8080/api/calls/<id>/audio
```

## 3. The admin UI

`GET /admin/` serves a single-page app embedded in the binary from [`internal/admin/static/`](../internal/admin/static/). `GET /admin` redirects there. The app uses only the `/api/*` endpoints above. Its routes are hash-based:

| URL | Page |
|---|---|
| `/admin/#/` | Call list: stats, search, verdict filter, evidence strips, paging. It refreshes every 15 s. |
| `/admin/#/calls/<id>` | Call page: verdict headline, risk, audio player, per-detector timeline, detector results, variables, other plugins and raw JSON |

Link to a call from your own tools with `http://<core>/admin/#/calls/<call_id>`.

## 4. Authentication and headers

| Endpoints | Protection |
|---|---|
| `/ws/call`, `/calls/{id}`, `/healthz` | **None.** Restrict them at the network level. |
| `/admin/*`, `/api/*` | HTTP basic auth when `ADMIN_PASSWORD` is set. The user is `ADMIN_USER` (default `admin`), and credentials are compared in constant time. Without a password these are open, and the core logs a warning at startup. |

Basic auth sends the password with every request, so use it only over TLS (a reverse proxy) on any network you don't fully control.

Every admin response carries:

```
Content-Security-Policy: default-src 'self'; img-src 'self' data:; media-src 'self'; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
Cache-Control: no-store
```

Call variables and plugin output come from outside the system. The UI inserts them into the page only as text nodes, never as HTML, and the CSP blocks inline and third-party scripts as a second line of defense. `no-store` keeps call data out of browser and proxy caches.
