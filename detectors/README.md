# Detector service: AI-voice detection

A Python service that analyzes call audio for AI-generated speech. The core's `aivoice` plugin streams each call to it, and the service returns a per-call report. That report is stored as `results.aivoice`.

It uses two kinds of evidence:

| Kind | Detectors (default) | What a positive means |
|---|---|---|
| **Watermarks** | [AudioSeal](https://github.com/facebookresearch/audioseal) (Meta), [Perth](https://github.com/resemble-ai/Perth) (Resemble AI, also used by Chatterbox) | The audio carries a known AI-generation watermark. This is strong evidence. |
| **Synthetic-speech classifiers** | [`garystafford/wav2vec2-deepfake-voice-detector`](https://huggingface.co/garystafford/wav2vec2-deepfake-voice-detector) (trained on ElevenLabs, Polly, Kokoro, Hume, and others) | A model judges the voice to be machine-generated. This is probabilistic. |
| **Commercial APIs** (optional) | Resemble Detect (its watermark check also covers **SynthID**), Reality Defender | The vendor's verdict. These run at the end of the call on the first `API_MAX_SECONDS` of each channel. |

**Read results with care.**
- *No watermark does not mean a human voice.* Most voice-cloning tools don't watermark, and research shows post-hoc watermarks [can be removed](https://arxiv.org/abs/2504.10782).
- Classifier scores vary a lot between models and audio conditions. Validate thresholds on your own call audio before acting on them.
- Phone audio is 8 kHz, while the detectors were trained on wideband audio. The self-test measures whether each watermark survives an 8 kHz G.711 mu-law path, and the report includes that result (`detectors.telephony_robust`). AudioSeal and Perth both passed with the current versions.

## Report (`results.aivoice`)

```jsonc
{
  "verdict": "watermark_detected",   // > likely_synthetic > no_evidence > insufficient_audio
  "risk_score": 1.0,                 // max of: detected watermark score, classifier mean, API score
  "channels": [{
    "channel": 0, "label": "caller",  // labels come from the channel_labels variable
    "verdict": "watermark_detected", "risk_score": 1.0,
    "seconds_received": 12.0, "speech_seconds_analyzed": 12.0,
    "windows": {"speech": 3, "silent": 0},
    "watermarks": {
      "audioseal": {"detected": true, "max_score": 1.0, "windows_analyzed": 3, "windows_positive": 3,
                    "detected_seconds": 12.0, "message": "0xae20",   // AudioSeal 16-bit payload
                    "segments": [{"start": 0, "end": 12, "max_score": 1.0}]},
      "perth": {"detected": false, ...}
    },
    "synthetic_speech": {
      "wav2vec2-deepfake": {"flagged": false, "mean_score": 0.36, "max_score": 0.41,
                            "windows_analyzed": 2, "flagged_fraction": 0.0, "segments": []}
    },
    "apis": {"resemble": {...}, "reality_defender": {...}}   // only when enabled
  }],
  "audio": {"encoding": "pcm16", "sample_rate": 8000, "channels": 1, "analysis_rate": 16000, "window_seconds": 4},
  "detectors": {"id": "base", "versions": {...}, "detectors": [...], "telephony_robust": {...}},
  "caveats": ["..."],
  "analysis": {"windows_queued_at_end": 0, "seconds_after_call": 1.8}   // how far analysis lagged the call
}
```

### Call variables used

| Variable | Required | Meaning |
|---|---|---|
| `encoding` | yes* | One of `pcm16`/`linear16`, `mulaw`/`ulaw`/`pcmu`, `alaw`/`pcma`, `f32le`, `u8` |
| `sample_rate` | yes* | Integer Hz, for example `8000` |
| `channels` | no | 1 to 8 interleaved channels, for example 2 for caller and agent. Each channel is analyzed separately. |
| `channel_labels` | no | For example `["caller","agent"]`, copied into the report |

\* Unless `AIVOICE_DEFAULT_ENCODING` and `AIVOICE_DEFAULT_SAMPLE_RATE` are set on the core. Only these variables and the call ID are sent to the service; the rest stay in the core.

## How analysis works

1. Audio is decoded and split per channel into `WINDOW_SECONDS` windows (4 s by default), then resampled to 16 kHz.
2. Silent windows, those below `SILENCE_RMS`, are skipped.
3. Each speech window is sent to a worker process:
   - Watermark detectors run on every window.
   - Slow classifiers run on every `stride`-th window, up to `max_windows` per channel.
4. **No window is ever dropped.** Windows wait in one priority queue shared by all calls, and are analyzed in this order:
   - calls in the order they started: a later call's windows run only once no earlier call has any queued;
   - within a call, every window of channel 0 before channel 1;
   - within a channel, in time order.

   If the hardware can't keep up in real time, the queue grows and reports arrive later, but every window is analyzed. On CPU, the default wav2vec2-large classifier takes about 1–5 s per window.
5. When the call ends, the service waits for all of that call's queued windows, runs any enabled APIs, and builds the report. `FINISH_TIMEOUT` (0 = no limit, the default) can cap this wait. The report's `analysis` field shows the lag: `windows_queued_at_end` and `seconds_after_call`. The core's `PLUGIN_TIMEOUT` (30 min in Compose) must be longer than the longest wait you expect.

## Staying up to date

`catalog.yaml` lists the detectors and the upstream sources they come from: PyPI packages, and Hugging Face model and weight repositories. Every `UPDATE_INTERVAL` (24 h by default, and once at startup) the updater:

1. Looks up the latest PyPI version and Hugging Face commit of every tracked source. The catalog itself can also come from `CATALOG_URL`.
2. If anything changed, builds a **candidate release** in `/data/releases/<id>`:
   - A venv layered over the image's packages, with the new detector package versions installed. Only wheels are allowed (`--only-binary`), and `constraints.txt` holds torch, transformers and the other base packages at the image's versions.
   - Model snapshots at the exact commit. Only safetensors and config files are fetched for Hugging Face classifiers.
3. **Self-tests** the candidate in a fresh worker:
   - Each watermark detector must find its own watermark in a test signal and not flag the clean signal. AudioSeal must also recover its message.
   - Each classifier must load and produce valid scores.
4. On a pass, **promotes** the candidate. New calls go to new workers, and calls in progress finish on the old ones.
5. On a failure, discards the candidate and remembers it, so the same versions are not retried.

The image includes a baked-in **base release** (`/opt/release0`), built and self-tested at `docker build` time. The service works offline from its first start, and falls back to the base release if the active one can't start.

To add a detector:
- **A new Hugging Face audio-classification model:** add a `hf_audio_classifier` entry to the catalog. No code is needed. Use the same entry format in a catalog served from `CATALOG_URL` to roll it out without rebuilding the image.
- **A new watermark scheme:** write a small adapter in `detsvc/detectors/` with `analyze` and `selftest`, and add it to the catalog.

### Trust model

Tracking upstream automatically means new upstream code and weights run in this service once they pass the self-test. **The self-test catches broken updates, not malicious ones.** Limits on what an update can do:
- Detectors run in separate worker processes as an unprivileged user.
- Workers receive no API keys and run with the Hugging Face hub set to offline mode.
- The base packages cannot be replaced.
- Classifier models load from safetensors with `trust_remote_code=False`.
- AudioSeal's official weights are pickle files, loaded by AudioSeal itself. They are fetched at a pinned commit when a release is built.

To control updates manually instead, set `UPDATE_ENABLED=false` and update by rebuilding the image, or point `CATALOG_URL` at a catalog you review.

## API

| Endpoint | |
|---|---|
| `WS /v1/stream` | Call stream. See the protocol in `detsvc/app.py`. |
| `GET /healthz` | 200 once workers are ready |
| `GET /v1/status` | Active release, detector versions, self-test results, update status, history and rejected candidates |
| `POST /v1/update` | Run an update check now |
| `POST /v1/rollback` | Switch to the previous release and mark the current one rejected |

The service is reachable only on the Compose network, not on the host. To use the endpoints, run for example: `docker compose exec detectors python -c "import urllib.request;print(urllib.request.urlopen('http://localhost:8000/v1/status').read().decode())"`.

## Configuration

| Variable | Default | |
|---|---|---|
| `WORKERS` / `TORCH_THREADS` | 2 / 2 | Worker processes, and torch threads per worker |
| `WINDOW_SECONDS` | 4 | Analysis window length |
| `SILENCE_RMS` | 0.003 | Windows quieter than this are skipped |
| `FINISH_TIMEOUT` | 0 | How long to wait for queued windows after the call ends. 0 means no limit, so every window is analyzed. |
| `UPDATE_ENABLED` / `UPDATE_INTERVAL` | true / 24h | Upstream tracking |
| `CATALOG_URL` | (empty) | Fetch the catalog from this URL instead of the built-in one |
| `RELEASES_KEEP` | 3 | Releases kept for rollback |
| `RESEMBLE_API_KEY` / `REALITY_DEFENDER_API_KEY` | (empty) | Enable those APIs. **This sends call audio to the vendor.** Resemble is called with `zero_retention_mode=true`. |
| `API_MAX_SECONDS` / `API_TIMEOUT` | 60 / 25s | Seconds of audio per channel sent to each API, and the time limit per API call |

## Development

```sh
docker build -t agent-security-detectors detectors
docker run --rm agent-security-detectors python -m pytest -q tests      # fast; uses a dummy detector

# Test samples (speech-like signal, optionally watermarked, through a phone path):
docker run --rm -v "$PWD:/out" agent-security-detectors \
  python -m detsvc.tools.make_sample --mark audioseal --rate 8000 --telephony --out /out/audioseal.wav
go run ./cmd/streamcall -file audioseal.wav -vars '{"encoding":"pcm16","sample_rate":8000}'
```
