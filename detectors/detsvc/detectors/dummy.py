"""Dependency-free detector for tests: score is the window's peak amplitude."""

from __future__ import annotations

import numpy as np

from . import Detector


class DummyDetector(Detector):
    def analyze(self, x: np.ndarray) -> dict:
        if self.options.get("crash"):
            raise RuntimeError("dummy crash")
        return {"score": float(np.clip(np.abs(x).max(initial=0.0), 0, 1))}

    def selftest(self) -> dict:
        return {"passed": not self.options.get("fail_selftest"), "telephony_robust": True}
