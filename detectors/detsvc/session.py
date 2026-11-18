"""One call's analysis: decodes the incoming stream, cuts it into windows per
channel, schedules detector runs on the worker pool, and builds the report."""

from __future__ import annotations

import asyncio
import logging
import time
from dataclasses import dataclass, field

import numpy as np

from . import apis as api_mod
from .audio import Decoder, normalize_encoding, resample, rms
from .report import Window, call_report, channel_report

log = logging.getLogger(__name__)


class StartError(ValueError):
    pass


@dataclass
class Channel:
    index: int
    label: str | None
    buf: list[np.ndarray] = field(default_factory=list)
    buffered: int = 0
    consumed: int = 0
    speech_windows: int = 0
    silent_windows: int = 0
    speech_seconds: float = 0.0
    scheduled: dict[str, int] = field(default_factory=dict)
    scores: dict[str, list[Window]] = field(default_factory=dict)
    errors: dict[str, str] = field(default_factory=dict)
    api_audio: list[np.ndarray] = field(default_factory=list)
    api_samples: int = 0


def parse_start(msg: dict) -> tuple[str, int, int, list]:
    try:
        enc = normalize_encoding(str(msg.get("encoding", "")))
    except ValueError as e:
        raise StartError(str(e)) from None
    sr = msg.get("sample_rate")
    if not isinstance(sr, int) or not 4000 <= sr <= 96000:
        raise StartError("sample_rate must be an integer between 4000 and 96000")
    ch = msg.get("channels", 1)
    if not isinstance(ch, int) or not 1 <= ch <= 8:
        raise StartError("channels must be an integer between 1 and 8")
    labels = msg.get("channel_labels") or []
    if not isinstance(labels, list):
        labels = []
    return enc, sr, ch, [str(x) for x in labels]


class Session:
    def __init__(self, cfg, pool, apis: dict, start: dict):
        self.cfg = cfg
        self.pool = pool
        self.apis = apis
        self.call_id = str(start.get("call_id", ""))[:64]
        self.encoding, self.rate, nch, labels = parse_start(start)
        self.decoder = Decoder(self.encoding, nch)
        self.win = int(cfg.window_seconds * self.rate)
        self.min_win = int(cfg.min_window_seconds * self.rate)
        self.api_max = int(cfg.api_max_seconds * self.rate) if apis else 0
        self.channels = [Channel(i, labels[i] if i < len(labels) else None) for i in range(nch)]
        self.specs = pool.release.catalog
        self.pending: set[asyncio.Task] = set()
        # Queue position of this call: earlier calls are analyzed first.
        self.job = pool.next_job()

    # --- ingest -----------------------------------------------------------

    def feed(self, data: bytes) -> None:
        frames = self.decoder.feed(data)
        if not frames.size:
            return
        for ch in self.channels:
            x = frames[:, ch.index]
            ch.buf.append(x)
            ch.buffered += x.size
            if ch.api_samples < self.api_max:
                take = x[: self.api_max - ch.api_samples]
                ch.api_audio.append(take)
                ch.api_samples += take.size
            while ch.buffered >= self.win:
                self._window(ch, self._take(ch, self.win))

    def _take(self, ch: Channel, n: int) -> np.ndarray:
        data = np.concatenate(ch.buf) if len(ch.buf) > 1 else ch.buf[0]
        out, rest = data[:n], data[n:]
        ch.buf = [rest] if rest.size else []
        ch.buffered = rest.size
        return out

    def _due(self, ch: Channel) -> list[str]:
        ids = []
        for spec in self.specs:
            stride = int(spec.get("stride", 1)) or 1
            cap = spec.get("max_windows")
            if (ch.speech_windows - 1) % stride:
                continue
            if cap is not None and ch.scheduled.get(spec["id"], 0) >= int(cap):
                continue
            ids.append(spec["id"])
        return ids

    def _window(self, ch: Channel, x: np.ndarray) -> None:
        start = ch.consumed / self.rate
        ch.consumed += x.size
        end = ch.consumed / self.rate
        if rms(x) < self.cfg.silence_rms:
            ch.silent_windows += 1
            return
        ch.speech_windows += 1
        dets = self._due(ch)
        if not dets:
            return
        for d in dets:
            ch.scheduled[d] = ch.scheduled.get(d, 0) + 1
        x16 = resample(x, self.rate)
        t = asyncio.create_task(self._analyze(ch, start, end, x16, dets))
        self.pending.add(t)
        t.add_done_callback(self.pending.discard)

    async def _analyze(self, ch: Channel, start: float, end: float, x: np.ndarray, dets: list[str]) -> None:
        try:
            res = await self.pool.analyze(x, dets, (self.job, ch.index, start))
        except Exception as e:  # noqa: BLE001
            for d in dets:
                ch.errors[d] = f"{type(e).__name__}: {e}"[:300]
            return
        ch.speech_seconds += end - start
        for did, r in res.get("results", {}).items():
            extra = {k: v for k, v in r.items() if k != "score"}
            ch.scores.setdefault(did, []).append(Window(start, end, float(r["score"]), extra))
        for did, err in res.get("errors", {}).items():
            ch.errors[did] = err[:300]

    # --- finish -----------------------------------------------------------

    async def finish(self) -> dict:
        """Analyze any remaining audio, wait for every queued window (no window
        is dropped; with FINISH_TIMEOUT > 0, give up after that long), and
        build the report."""
        for ch in self.channels:
            if ch.buffered >= self.min_win:
                self._window(ch, self._take(ch, ch.buffered))
        notes = []
        ended = time.monotonic()
        queued = len(self.pending)
        api_task = asyncio.create_task(self._run_apis())
        if self.pending:
            timeout = self.cfg.finish_timeout or None
            done, not_done = await asyncio.wait(set(self.pending), timeout=timeout)
            for t in not_done:
                t.cancel()
            if not_done:
                notes.append(f"{len(not_done)} analysis windows did not finish within {self.cfg.finish_timeout:.0f}s.")
        api_results = await api_task
        waited = time.monotonic() - ended

        reports = []
        for ch in self.channels:
            stats = {
                "seconds_received": round(ch.consumed / self.rate + ch.buffered / self.rate, 2),
                "speech_seconds_analyzed": round(ch.speech_seconds, 2),
                "windows": {"speech": ch.speech_windows, "silent": ch.silent_windows},
            }
            reports.append(channel_report(ch.index, ch.label, self.specs, ch.scores, stats,
                                          api_results.get(ch.index, {}), ch.errors))
        audio = {"encoding": self.encoding, "sample_rate": self.rate, "channels": len(self.channels),
                 "analysis_rate": 16000, "window_seconds": self.cfg.window_seconds}
        rel = self.pool.release.summary()
        rel["detectors"] = self.pool.detectors
        rep = call_report(reports, audio, rel, notes)
        # How far analysis lagged the call: windows still queued when the call
        # ended, and how long the report took after that.
        rep["analysis"] = {"windows_queued_at_end": queued, "seconds_after_call": round(waited, 2)}
        return rep

    async def _run_apis(self) -> dict[int, dict]:
        if not self.apis:
            return {}
        out = {}
        for ch in self.channels:
            audio = np.concatenate(ch.api_audio) if ch.api_audio else np.zeros(0, np.float32)
            if ch.speech_windows == 0 or audio.size < self.min_win:
                continue
            out[ch.index] = await api_mod.run_all(self.apis, audio, self.rate, self.cfg.api_timeout)
        return out
