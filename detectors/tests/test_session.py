import asyncio

import numpy as np
import pytest

from detsvc.session import Session, StartError
from tests.conftest import dummy_specs


class FakeRelease:
    id = "fake"
    catalog = dummy_specs()
    meta = {}

    def summary(self):
        return {"id": "fake", "telephony_robust": {}}


class FakePool:
    def __init__(self, delay=0.0):
        self.release = FakeRelease()
        self.detectors = []
        self.calls = []
        self.delay = delay

    async def analyze(self, x, dets):
        self.calls.append((x.size, tuple(dets)))
        await asyncio.sleep(self.delay)
        s = float(np.abs(x).max())
        return {"type": "result", "results": {d: {"score": s} for d in dets}, "errors": {}}


def pcm16(x):
    return (np.clip(x, -1, 1) * 32767).astype("<i2").tobytes()


def test_start_validation(cfg):
    for bad in [{"encoding": "opus", "sample_rate": 8000}, {"encoding": "pcm16"},
                {"encoding": "pcm16", "sample_rate": 8000, "channels": 9}]:
        with pytest.raises(StartError):
            Session(cfg, FakePool(), {}, bad)


def test_windows_strides_silence_and_report(cfg):
    async def run():
        pool = FakePool()
        s = Session(cfg, pool, {}, {"call_id": "c1", "encoding": "pcm16", "sample_rate": 8000, "channels": 2,
                                    "channel_labels": ["caller", "agent"]})
        # 5 s: channel 0 loud (0.9), channel 1 silent; fed in odd-sized chunks.
        loud = np.full(8000 * 5, 0.9, np.float32)
        inter = np.stack([loud, np.zeros_like(loud)], axis=1).reshape(-1)
        data = pcm16(inter)
        for i in range(0, len(data), 333):
            s.feed(data[i:i + 333])
        return pool, await s.finish()

    pool, rep = asyncio.run(run())
    ch0, ch1 = rep["channels"]
    assert ch0["label"] == "caller" and ch1["label"] == "agent"
    assert ch0["windows"]["speech"] == 5 and ch1["windows"]["silent"] == 5
    # every window is resampled to 16 kHz before analysis
    assert all(n == 16000 for n, _ in pool.calls)
    # "wm" runs on every window; "syn" every 2nd window, capped at 2
    assert ch0["watermarks"]["wm"]["windows_analyzed"] == 5
    assert ch0["synthetic_speech"]["syn"]["windows_analyzed"] == 2
    assert ch0["verdict"] == "watermark_detected" and ch1["verdict"] == "insufficient_audio"
    assert rep["verdict"] == "watermark_detected" and rep["audio"]["sample_rate"] == 8000


def test_backlog_skips_windows(cfg):
    cfg.max_backlog_windows = 2

    async def run():
        s = Session(cfg, FakePool(delay=0.05), {}, {"encoding": "pcm16", "sample_rate": 8000})
        s.feed(pcm16(np.full(8000 * 6, 0.5, np.float32)))
        return await s.finish()

    rep = asyncio.run(run())
    ch = rep["channels"][0]
    assert ch["windows"]["skipped_backlog"] == 4
    assert any("fell behind" in c for c in rep["caveats"])
