"""Resemble AI Perth implicit watermark detector (https://github.com/resemble-ai/Perth).

Used by Resemble AI and by models built on it (e.g. Chatterbox TTS)."""

from __future__ import annotations

import numpy as np

from ..audio import TARGET_RATE, telephony_roundtrip
from ..testsignal import speechlike
from . import Detector


class PerthDetector(Detector):
    def __init__(self, spec, release):
        super().__init__(spec, release)
        import perth

        if perth.PerthImplicitWatermarker is None:
            # perth hides its import error; surface the real one.
            from perth.perth_net.perth_net_implicit.perth_watermarker import PerthImplicitWatermarker  # noqa: F401
        self.wm = perth.PerthImplicitWatermarker()

    def analyze(self, x: np.ndarray) -> dict:
        v = self.wm.get_watermark(x, sample_rate=TARGET_RATE, round=False)
        return {"score": float(np.clip(np.mean(v), 0.0, 1.0))}

    def selftest(self) -> dict:
        x = speechlike()
        marked = self.wm.apply_watermark(x, sample_rate=TARGET_RATE).astype(np.float32)
        clean = self.analyze(x)["score"]
        wm = self.analyze(marked)["score"]
        phone = self.analyze(telephony_roundtrip(marked))["score"]
        return {
            "passed": clean < self.threshold and wm >= 0.9,
            "clean_score": round(clean, 4),
            "watermarked_score": round(wm, 4),
            "telephony_score": round(phone, 4),
            "telephony_robust": phone >= self.threshold,
        }
