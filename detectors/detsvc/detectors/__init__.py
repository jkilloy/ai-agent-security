"""Detector adapters. These run inside a release's worker process only.

Each adapter wraps one detection method behind the same interface:

    analyze(x)  x is mono float32 at 16 kHz -> {"score": 0..1, ...extras}
    selftest()  -> {"passed": bool, ...details}

`score` is the probability-like strength of the evidence: for watermark
detectors, that a watermark is present; for synthetic-speech classifiers,
that the speech is machine-generated.
"""

from __future__ import annotations

import importlib.metadata
from typing import Any

import numpy as np

from ..releases import Release


class Detector:
    def __init__(self, spec: dict, release: Release):
        self.spec = spec
        self.release = release
        self.id: str = spec["id"]
        self.kind: str = spec["kind"]
        self.threshold: float = float(spec.get("threshold", 0.5))
        self.options: dict = spec.get("options") or {}

    @property
    def version(self) -> str:
        pkg = self.spec.get("package")
        if pkg:
            try:
                return importlib.metadata.version(pkg)
            except importlib.metadata.PackageNotFoundError:
                return "unknown"
        model = self.spec.get("model")
        if model:
            return self.release.versions.get("hf", {}).get(model, "unknown")
        return "builtin"

    def info(self) -> dict[str, Any]:
        return {"id": self.id, "kind": self.kind, "adapter": self.spec["adapter"],
                "version": self.version, "threshold": self.threshold}

    def analyze(self, x: np.ndarray) -> dict[str, Any]:
        raise NotImplementedError

    def selftest(self) -> dict[str, Any]:
        raise NotImplementedError


def build(spec: dict, release: Release) -> Detector:
    adapter = spec["adapter"]
    if adapter == "audioseal":
        from .audioseal import AudioSealDetector as cls
    elif adapter == "perth":
        from .perth import PerthDetector as cls
    elif adapter == "hf_audio_classifier":
        from .hf_classifier import HFClassifier as cls
    elif adapter == "dummy":
        from .dummy import DummyDetector as cls
    else:
        raise ValueError(f"unknown adapter {adapter!r}")
    return cls(spec, release)
