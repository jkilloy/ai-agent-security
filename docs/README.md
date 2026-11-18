# Documentation

| Document | Read it if you are… |
|---|---|
| [WebSocket call API](websocket-api.md) | **Integrating a phone system.** Covers the protocol, the first message and identifiers, the audio formats, how to send audio, ending a call, errors, client examples in Go, Python and Node, and bridges for Twilio and SIP. |
| [Plugins](plugins.md) | **Writing an analysis plugin.** Covers the interface, lifecycle and timing, the audio stream API, output conventions, error isolation, concurrency rules, patterns, testing, and how results appear in the UI. |
| [HTTP API](http-api.md) | **Reading results or building on them.** Covers `GET /calls/{id}`, the admin `/api/*` (list, search, stats, audio as WAV), authentication and headers. |
| [Architecture and code design](architecture.md) | **Working on the core.** Covers goals, components, the data model, the call lifecycle, the concurrency model, failure handling, retention, security, design decisions and future work. |
| [Operations](operations.md) | **Running it.** Covers configuration, deployment behind TLS, capacity planning, logs, data management, testing and troubleshooting. |
| [Detector service](../detectors/README.md) | **Working on AI-voice detection.** Covers the detectors, the report format, the analysis pipeline, automatic upstream updates and the trust model. |

To get started quickly, see the [project README](../README.md#quick-start).
