# Analysis plugins

A plugin is a Go type compiled into the core. It receives each call live, reads the audio as it arrives, and returns a free-form JSON value that the core stores under the plugin's name in the call's `results`. This document is the contract between the core and plugins, and a guide to writing them.

- **Interface:** [`internal/plugin/plugin.go`](../internal/plugin/plugin.go)
- **Audio stream:** [`internal/audio/broadcast.go`](../internal/audio/broadcast.go)
- **Runner:** [`internal/engine/engine.go`](../internal/engine/engine.go)
- **Examples:** [`plugins/audiostats`](../plugins/audiostats/audiostats.go) (minimal) and [`plugins/aivoice`](../plugins/aivoice/aivoice.go) (streams to an external service)

Contents:

1. [The contract](#1-the-contract)
2. [Lifecycle and timing](#2-lifecycle-and-timing)
3. [Input](#3-input)
4. [Reading audio: `audio.Stream`](#4-reading-audio-audiostream)
5. [Output](#5-output)
6. [Errors, timeouts and panics](#6-errors-timeouts-and-panics)
7. [Concurrency rules](#7-concurrency-rules)
8. [Configuration](#8-configuration)
9. [Registering a plugin](#9-registering-a-plugin)
10. [Patterns](#10-patterns)
11. [Testing a plugin](#11-testing-a-plugin)
12. [How results appear in the admin UI](#12-how-results-appear-in-the-admin-ui)
13. [Limits of the current design](#13-limits-of-the-current-design)
14. [Checklist](#14-checklist)

---

## 1. The contract

```go
package plugin

type Plugin interface {
	// Name is the key the result is stored under in call.results.
	// It must be unique and non-empty, and should be a stable lowercase identifier.
	Name() string

	// Analyze is called once per call, when the call starts. It should read
	// in.Audio until io.EOF (or until it has seen enough) and return JSON.
	Analyze(ctx context.Context, in *Input) (json.RawMessage, error)
}

type Input struct {
	CallID    string          // the core's UUID for the call
	Variables map[string]any  // the call's first message; this plugin's own deep copy
	Audio     *audio.Stream   // this plugin's own live subscription to the call audio
}

func Register(p Plugin) // call from init(); panics on an empty or duplicate name
func All() []Plugin     // registered plugins, sorted by name
```

In short: **one `Analyze` call per plugin per phone call. It starts when the call starts, sees every audio byte live, and returns once at the end.**

## 2. Lifecycle and timing

```mermaid
sequenceDiagram
    participant WS as WebSocket handler
    participant E as Engine
    participant P as Plugin.Analyze (one goroutine per plugin)
    participant B as Broadcaster
    WS->>E: Start(callID, variables, broadcaster)
    E->>B: Subscribe() for each plugin
    E->>P: go Analyze(ctx, input)
    WS-->>WS: send "accepted" to the client
    loop during the call
        WS->>B: Publish(frame)
        B->>P: frame queued; Next() returns it
    end
    WS->>B: Close()  (end message, hangup, max duration or shutdown)
    B->>P: Next() returns io.EOF once the queue drains
    WS->>E: Wait()
    Note over E,P: PLUGIN_TIMEOUT starts now
    P-->>E: return JSON (or error)
    E-->>WS: results map
    WS->>WS: save call to Postgres
```

Key timing facts:

1. **Subscription happens before `accepted`.** The client can't send audio before every plugin is subscribed, so every plugin sees the call from byte 0.
2. **`Analyze` starts immediately,** before any audio has arrived. Use the time for setup, such as opening a connection to a model server, while the frames queue up for you.
3. **There is no deadline during the call.** A call may last up to `MAX_CALL_DURATION` (4 h by default). `ctx` is *not* cancelled when the caller hangs up; a hangup closes the stream, so you see `io.EOF` and can still finish.
4. **After the audio ends, `PLUGIN_TIMEOUT` starts** (30 s by default, 30 min under Compose). If you haven't returned by then, `ctx` is cancelled.
5. **Then a 2-second grace period.** A plugin that still hasn't returned is recorded as `{"error":"timed out"}` and *abandoned*: its goroutine keeps running, but its result is discarded. Don't ignore `ctx`.
6. The call is saved only after **every** plugin has returned or been abandoned. Your slowness delays the save and the client's `complete` message, but doesn't block other plugins.

## 3. Input

### 3.1 `CallID`

The core's UUID for the call, the same one the client receives and `GET /calls/{id}` uses. Use it for logs and for correlating with external services.

### 3.2 `Variables`

The first WebSocket message, decoded with `encoding/json` into `map[string]any`:

| JSON | Go |
|---|---|
| string | `string` |
| number | `float64` (even for integers: `8000` is `float64(8000)`) |
| true / false | `bool` |
| null | `nil` |
| object | `map[string]any` |
| array | `[]any` |

- Each plugin gets a **deep copy**, so mutating it can't affect other plugins or what gets stored. Treat it as read-only anyway.
- Always use the two-value type assertion. Variables come from outside the system, so any key can be missing or have an unexpected type:

```go
rate, _ := in.Variables["sample_rate"].(float64)  // 0 if missing or not a number
enc, _ := in.Variables["encoding"].(string)
labels, _ := in.Variables["channel_labels"].([]any)
```

The audio-format variables (`encoding`, `sample_rate`, `channels`, `channel_labels`) are the shared convention for describing the audio. See [websocket-api.md §4](websocket-api.md#4-audio-format). If your plugin needs other variables, document them in your package comment.

### 3.3 `Audio`

This plugin's own subscription to the call's audio (next section).

## 4. Reading audio: `audio.Stream`

```go
// Next returns the next frame, blocking until one arrives.
// It returns io.EOF after the call ends and every queued frame has been read,
// or ctx.Err() if ctx is cancelled first.
func (s *Stream) Next(ctx context.Context) ([]byte, error)

// Read implements io.Reader over the concatenated frames. It blocks without a
// context, so prefer Next when cancellation matters.
func (s *Stream) Read(p []byte) (int, error)

// Release stops delivery and frees queued frames. The engine calls it for you
// when Analyze returns; call it yourself only to free memory before returning.
func (s *Stream) Release()
```

The semantics:

- **A frame is one client WebSocket message,** exactly as sent. Frames have no guaranteed size or alignment. A 2-byte `pcm16` sample, or a stereo frame, may be split across two frames. If you decode samples, carry the leftover bytes over (see [§10.2](#102-decoding-samples)), or use `Read` with `io.ReadFull`.
- **Every frame arrives once, in order.** Frames published before you subscribed are not delivered, but subscription always happens before `accepted`, so you get them all.
- **Frames are shared, read-only slices.** The same backing array is delivered to every plugin. **Never modify a frame.** Copy it if you need to change it or keep it past your next read.
- **Your queue is unbounded.** The publisher never waits for you, so a slow plugin can't slow the client or other plugins. The cost is memory: if you stop reading, frames pile up for the rest of the call (8 kHz mulaw is about 29 MB per hour, and 16 kHz stereo `pcm16` about 230 MB per hour). Keep reading, or return.
- **One goroutine per stream.** A `Stream` is not safe for concurrent readers. If you fan the audio out internally, read in one goroutine and hand frames to others.
- **Returning early is allowed.** If you've seen enough, for example the first 30 seconds, return. The engine releases your subscription, and later frames are dropped for you only.

Choosing between `Next` and `Read`:

| Use `Next(ctx)` when | Use `Read` (`io.Reader`) when |
|---|---|
| You need cancellation (always, in practice) | You hand the audio to a library that wants an `io.Reader` |
| You forward frames as they are (to a socket or a gRPC stream) | You need fixed-size blocks (`io.ReadFull`) |
| You process frame-by-frame | The library returns at EOF on its own |

`Read` ignores `ctx`. If you use it, make sure the stream reaches EOF: the call always ends eventually, at the latest at `MAX_CALL_DURATION`.

## 5. Output

Return a `json.RawMessage`, usually from `json.Marshal`. The rules:

- **It must be valid JSON.** Invalid JSON is replaced with `{"error":"plugin returned invalid JSON"}`.
- **Any JSON value is allowed,** but **return an object.** Objects can grow new fields compatibly, and the admin UI renders them well.
- **Returning `nil, nil` stores `null`.** Use it to mean "nothing to say". An object with an explicit status is usually clearer.
- **The top-level `error` key is reserved.** The core, and the admin UI and API's "plugin errors" filter (`/api/calls?errors=1`), treat a result object with an `error` key as a failure. Return an `error` value from `Analyze` for failures. Don't put `error` at the top level of a successful result. Nested keys are fine.
- **Keep it reasonably small.** Results are stored as `jsonb` in the same row as the call and returned by every list query in the admin API. Aim for kilobytes. Put per-frame or high-volume data elsewhere and store a reference, or summarize it, for example as segments.

### 5.1 Recommended conventions

These aren't enforced, but following them makes results consistent and easy to read in the UI:

| Field | Purpose |
|---|---|
| `verdict` | A short machine-readable outcome, for example `"no_evidence"` or `"flagged"` |
| `score` / `risk_score` | 0–1, higher means more concerning |
| `segments` | `[{"start": 1.5, "end": 4.0, ...}]` in seconds from the start of the audio. Compute time from the byte offset: `offset / (bytes_per_sample × channels × sample_rate)`. |
| `channels` | One entry per audio channel, with `channel` (index) and `label` |
| `version` | Your plugin's or model's version, so stored results can be interpreted later |
| `caveats` | A list of plain-language limitations for reviewers |

### 5.2 Changing your output format

Stored results outlive deployments: they are kept for `RETENTION`, 30 days by default. Add fields freely, but avoid renaming or removing fields that consumers read. When a breaking change can't be avoided, include a `schema` or `version` number.

## 6. Errors, timeouts and panics

The engine isolates plugins from each other and from the call:

| Plugin does | Stored under its name | Logged |
|---|---|---|
| Returns `(data, nil)` with valid JSON | `data` | debug |
| Returns `(nil, nil)` | `null` | debug |
| Returns invalid JSON | `{"error":"plugin returned invalid JSON"}` | |
| Returns `(_, err)` | `{"error": err.Error()}` | warn |
| Returns `context.Canceled` or `DeadlineExceeded` (wrapped or not) | `{"error":"timed out: context canceled"}` | warn |
| Panics | `{"error":"panic: <value>"}` | error, with stack |
| Ignores cancellation for 2 s after timeout | `{"error":"timed out"}`, goroutine abandoned | error |

Nothing a plugin does fails the call or affects other plugins' results.

- **Error messages are stored and shown in the admin UI.** Make them useful to an operator (`connect to detector service: dial tcp 10.0.0.5:8000: connection refused`). Don't include secrets such as API keys or tokens in URLs.
- **Partial results.** If you have useful partial results when something fails, consider returning them with `nil` error and a status field (`{"status":"partial","reason":"…", …}`). Returning an error discards everything except the message.

## 7. Concurrency rules

- **One plugin value serves every call.** `Register` stores a single instance, and the engine calls `Analyze` on it concurrently for every active call. **`Analyze` must be safe for concurrent use.** Keep per-call state in local variables, not in struct fields. Shared resources (HTTP clients, connection pools, loaded models) are fine if they are themselves concurrency-safe.
- **Each call runs every plugin in its own goroutine.** With N concurrent calls and P plugins, there are N×P `Analyze` calls in flight. If your plugin uses a scarce resource (a GPU, a rate-limited API, a licensed engine), add your own limiter, such as a semaphore channel or `golang.org/x/sync/semaphore`. Keep draining the stream while waiting, or buffer deliberately.
- **CPU-heavy work** blocks only your goroutine. The Go scheduler spreads goroutines across cores, but a CPU-bound plugin with many concurrent calls will compete with the WebSocket handlers. Heavy ML belongs in a separate service, like `aivoice` and the [detector service](../detectors/README.md).
- **Don't start goroutines that outlive `Analyze`.** Anything you start must stop when `Analyze` returns. Use `ctx` plus a `sync.WaitGroup` or an `errgroup`.

## 8. Configuration

Plugins read their own configuration. The convention (see `aivoice`) is:

- Read environment variables in `init()`, prefixed with your plugin's name in capitals (`AIVOICE_URL`, `AIVOICE_DEFAULT_SAMPLE_RATE`).
- **Register conditionally** if the plugin can't work without configuration. For example, `aivoice` registers only when `AIVOICE_URL` is set. The startup log line `plugins loaded` shows which plugins are active.
- Pass the variables to the `core` service in `docker-compose.yml`, and document them in the plugin's package comment, [`.env.example`](../.env.example) and [operations.md](operations.md#configuration).
- Per-call options come from call variables. Document which variables you read.

```go
func init() {
	url := os.Getenv("MYPLUGIN_URL")
	if url == "" {
		return // disabled
	}
	plugin.Register(&Plugin{URL: url})
}
```

`Register` panics on a duplicate or empty name. That is deliberate: a mistake shows up at startup, not at the first call.

## 9. Registering a plugin

1. Create a package under `plugins/`, for example `plugins/myplugin/`.
2. Call `plugin.Register` from its `init()`.
3. Add a blank import to [`plugins/all.go`](../plugins/all.go):

   ```go
   import (
   	_ "agent-security/plugins/aivoice"
   	_ "agent-security/plugins/audiostats"
   	_ "agent-security/plugins/myplugin"
   )
   ```

4. Rebuild the core (`docker compose up -d --build core`). The log line `plugins loaded` lists it.

Plugins are started in name order, but they run concurrently, so the order has no effect on results.

## 10. Patterns

### 10.1 Minimal: consume the stream and summarize

This is the shape of [`audiostats`](../plugins/audiostats/audiostats.go):

```go
package myplugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"agent-security/internal/plugin"
)

func init() { plugin.Register(Plugin{}) }

type Plugin struct{}

func (Plugin) Name() string { return "myplugin" }

func (Plugin) Analyze(ctx context.Context, in *plugin.Input) (json.RawMessage, error) {
	var bytes int64
	for {
		frame, err := in.Audio.Next(ctx)
		if errors.Is(err, io.EOF) {
			break // the call ended and every frame has been read
		}
		if err != nil {
			return nil, err // ctx cancelled: PLUGIN_TIMEOUT passed after the call ended
		}
		bytes += int64(len(frame)) // read-only: never modify frame
	}
	return json.Marshal(map[string]any{"bytes": bytes})
}
```

### 10.2 Decoding samples

This example turns the byte stream into `int16` samples per channel while handling frames that split samples. It finds the loudest moment per channel and reports clipping. It supports `pcm16` and `mulaw`; extend the switch for other encodings.

```go
package levels

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"agent-security/internal/plugin"
)

func init() { plugin.Register(Plugin{}) }

type Plugin struct{}

func (Plugin) Name() string { return "levels" }

type channelStats struct {
	Channel    int     `json:"channel"`
	Label      string  `json:"label,omitempty"`
	Peak       int     `json:"peak"`
	PeakAt     float64 `json:"peak_at_seconds"`
	ClippedPct float64 `json:"clipped_percent"`
	samples    int64
	clipped    int64
}

func (Plugin) Analyze(ctx context.Context, in *plugin.Input) (json.RawMessage, error) {
	enc, _ := in.Variables["encoding"].(string)
	rate, _ := in.Variables["sample_rate"].(float64)
	nch := 1
	if c, ok := in.Variables["channels"].(float64); ok && c >= 1 && c <= 8 {
		nch = int(c)
	}
	var width int
	var decode func([]byte) int16
	switch enc {
	case "pcm16":
		width, decode = 2, func(b []byte) int16 { return int16(binary.LittleEndian.Uint16(b)) }
	case "mulaw":
		width, decode = 1, func(b []byte) int16 { return mulaw[b[0]] }
	default:
		return nil, fmt.Errorf("unsupported encoding %q (want pcm16 or mulaw)", enc)
	}
	if rate <= 0 {
		return nil, errors.New(`variable "sample_rate" is required`)
	}

	labels, _ := in.Variables["channel_labels"].([]any)
	stats := make([]channelStats, nch)
	for i := range stats {
		stats[i].Channel = i
		if i < len(labels) {
			stats[i].Label, _ = labels[i].(string)
		}
	}

	frameBytes := width * nch
	var carry []byte // bytes of an incomplete multi-channel frame
	var n int64      // multi-channel frames decoded so far
	for {
		chunk, err := in.Audio.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		buf := chunk
		if len(carry) > 0 {
			buf = append(carry, chunk...) // copy: never modify chunk itself
			carry = nil
		}
		usable := len(buf) - len(buf)%frameBytes
		for off := 0; off < usable; off += frameBytes {
			for ch := range nch {
				s := int(decode(buf[off+ch*width:]))
				if s < 0 {
					s = -s
				}
				st := &stats[ch]
				st.samples++
				if s >= 32000 {
					st.clipped++
				}
				if s > st.Peak {
					st.Peak, st.PeakAt = s, float64(n)/rate
				}
			}
			n++
		}
		if usable < len(buf) {
			carry = append([]byte(nil), buf[usable:]...)
		}
	}
	for i := range stats {
		if stats[i].samples > 0 {
			stats[i].ClippedPct = 100 * float64(stats[i].clipped) / float64(stats[i].samples)
		}
	}
	return json.Marshal(map[string]any{"channels": stats, "seconds": float64(n) / rate})
}

var mulaw [256]int16

func init() {
	for i := range 256 {
		u := ^uint8(i)
		exp := (u >> 4) & 7
		mag := ((int32(u&0x0F)<<3)+0x84)<<exp - 0x84
		if u&0x80 != 0 {
			mag = -mag
		}
		mulaw[i] = int16(mag)
	}
}
```

The same G.711 tables, and the format parsing, also exist in [`internal/admin/wav.go`](../internal/admin/wav.go). If several plugins need decoding, move them into a shared package such as `internal/audio/pcm` rather than copying them.

### 10.3 Streaming to an external service

Heavy analysis (ML models, ASR, vendor APIs) belongs outside the core process. [`aivoice`](../plugins/aivoice/aivoice.go) is the reference: it opens a WebSocket to the Python detector service, forwards every frame, sends `end`, and returns the service's report as its result. The outline:

```go
func (p *Plugin) Analyze(ctx context.Context, in *plugin.Input) (json.RawMessage, error) {
	// 1. Validate inputs first, so misconfiguration becomes a clear error result.
	start, err := p.start(in) // reads encoding / sample_rate / channels from in.Variables
	if err != nil {
		return nil, err
	}
	// 2. Connect with its own short timeout. Frames queue in in.Audio meanwhile.
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dctx, p.URL, nil)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("connect to detector service: %w", err)
	}
	defer conn.CloseNow()
	// 3. Handshake, then forward frames as they arrive.
	// ...send start, wait for {"type":"ready"}...
	for {
		frame, err := in.Audio.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if err := conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
			return nil, fmt.Errorf("stream audio: %w", err)
		}
	}
	// 4. Signal the end, wait for the report (bounded by ctx / PLUGIN_TIMEOUT).
	// ...send {"type":"end"}, read {"type":"report","report":{...}}...
	return report, nil
}
```

Points that apply to any service-backed plugin:

- **Validate what you send.** Send the service only the variables it needs. `aivoice` sends the call ID and the audio-format variables, not the caller's number.
- **Size `PLUGIN_TIMEOUT` for the service's worst-case lag.** The detector service queues work rather than dropping it, so after a busy period a report can arrive minutes after the call ends. Compose sets `PLUGIN_TIMEOUT` to 30 minutes so `aivoice` can wait its turn.
- **Writing to a slow service blocks only your goroutine.** The frames queue in your stream, not in the client's connection.
- **Set a read limit** on responses (`conn.SetReadLimit`) that fits the largest report you expect.

The detector service's protocol is documented in [`detectors/detsvc/app.py`](../detectors/detsvc/app.py), and its report in [detectors/README.md](../detectors/README.md#report-resultsaivoice).

### 10.4 Looking at only part of the call

```go
const maxSeconds = 30
limit := int64(maxSeconds * rate * bytesPerSample * channels)
var seen int64
for seen < limit {
	f, err := in.Audio.Next(ctx)
	if errors.Is(err, io.EOF) {
		break
	}
	if err != nil {
		return nil, err
	}
	seen += int64(len(f))
	// analyze f (or the part of it below limit)
}
return json.Marshal(result) // returning releases the stream; later frames are not queued for you
```

### 10.5 Using variables only

A plugin doesn't have to read audio. A plugin that checks the caller number against a blocklist can return immediately. Its stream is released as soon as it returns.

```go
func (p *Blocklist) Analyze(ctx context.Context, in *plugin.Input) (json.RawMessage, error) {
	caller, _ := in.Variables["caller"].(string)
	hit, err := p.lookup(ctx, caller) // must honor ctx
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"caller": caller, "listed": hit})
}
```

## 11. Testing a plugin

Plugins are plain Go. You can drive them with a real `audio.Broadcaster`, without the server or a database:

```go
package levels

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agent-security/internal/audio"
	"agent-security/internal/plugin"
)

func TestLevels(t *testing.T) {
	b := audio.NewBroadcaster(nil) // nil sink: no file
	in := &plugin.Input{
		CallID:    "test",
		Variables: map[string]any{"encoding": "pcm16", "sample_rate": float64(8000)},
		Audio:     b.Subscribe(), // subscribe before publishing
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type out struct {
		data json.RawMessage
		err  error
	}
	done := make(chan out, 1)
	go func() { d, err := (Plugin{}).Analyze(ctx, in); done <- out{d, err} }()

	_ = b.Publish([]byte{0x00, 0x7f, 0x00}) // one sample (32512) plus half of the next
	_ = b.Publish([]byte{0x40})             // completes the second sample (16384)
	b.Close()                               // the call ends: Next returns io.EOF

	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	var got struct {
		Channels []struct{ Peak int } `json:"channels"`
	}
	if err := json.Unmarshal(r.data, &got); err != nil || got.Channels[0].Peak != 32512 {
		t.Fatalf("got %s (%v)", r.data, err)
	}
}
```

Things worth covering:

- frames that split samples;
- an empty call, where `Close` comes with no frames;
- missing or wrongly typed variables;
- cancellation: cancel `ctx` without closing the broadcaster, and check that you return promptly;
- concurrent calls: run `Analyze` for several inputs in parallel under `go test -race`.

To run a plugin through the whole engine, including its timeout and panic handling, see [`internal/engine/engine_test.go`](../internal/engine/engine_test.go). For an external service, start an `httptest.Server` that fakes it, as [`plugins/aivoice/aivoice_test.go`](../plugins/aivoice/aivoice_test.go) does.

For an end-to-end check, run the stack, stream a file with `streamcall`, and open the call in the admin UI.

## 12. How results appear in the admin UI

The admin UI is plugin-agnostic. The call page shows each plugin's result under its name, as pretty-printed JSON, sorted by name. A result object with a top-level `error` key is marked as an error, both on the call page and in the call list. The UI needs no changes when plugins are added or removed.

## 13. Limits of the current design

These are known, deliberate simplifications. See [architecture.md §13](architecture.md#13-future-work) for where they could go.

- **Compiled in.** Adding or upgrading a plugin means rebuilding the core. There is no dynamic loading and no out-of-process plugin protocol. Use the service-backed pattern (§10.3) for anything heavy, or anything in another language.
- **One result per call, at the end.** There is no way to raise an alert mid-call. A plugin that detects something at second 10 reports it when the call ends.
- **No plugin dependencies.** Plugins can't see each other's results. Each one gets the same raw audio and variables.
- **Results are immutable after save.** There is no re-analysis of stored calls, although the raw audio is kept for `RETENTION` and could be replayed through `streamcall`.

## 14. Checklist

- [ ] `Name()` is unique, lowercase and stable. It is a storage key.
- [ ] `Analyze` is safe for concurrent calls, with no per-call state in struct fields.
- [ ] The stream is read until `io.EOF`, or the plugin returns as soon as it has enough.
- [ ] Every blocking call honors `ctx`.
- [ ] Frames are never modified or retained without copying.
- [ ] Sample decoding handles frames that split samples.
- [ ] Variables are read with checked type assertions, and numbers as `float64`.
- [ ] The result is a JSON object, small, with no top-level `error` key on success.
- [ ] Error messages are useful to operators and contain no secrets.
- [ ] Configuration comes from env vars read in `init()`, and the plugin registers conditionally if it needs them.
- [ ] Tests cover split frames, an empty call, bad variables and cancellation, and pass under `-race`.
- [ ] The plugin is added to `plugins/all.go`, documented in the package comment, and listed in the README's plugin table.
