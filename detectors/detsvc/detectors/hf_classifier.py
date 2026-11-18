"""Generic Hugging Face audio-classification model as a synthetic-speech detector.

Any `AutoModelForAudioClassification` checkpoint whose labels distinguish real
from synthetic speech works; list its synthetic label names in the catalog.
Models are loaded from the release's local snapshot, safetensors only, and
never with trust_remote_code."""

from __future__ import annotations

import numpy as np
import torch

from ..audio import TARGET_RATE
from ..testsignal import speechlike
from . import Detector


class HFClassifier(Detector):
    def __init__(self, spec, release):
        super().__init__(spec, release)
        from transformers import AutoFeatureExtractor, AutoModelForAudioClassification

        path = release.model_dir(spec["model"])
        self.fe = AutoFeatureExtractor.from_pretrained(path, trust_remote_code=False)
        self.model = AutoModelForAudioClassification.from_pretrained(
            path, trust_remote_code=False, use_safetensors=True
        ).eval()
        labels = {int(k): v for k, v in self.model.config.id2label.items()}
        wanted = {s.lower() for s in spec["synthetic_labels"]}
        self.synthetic_idx = [i for i, name in labels.items() if name.lower() in wanted]
        if not self.synthetic_idx:
            raise ValueError(f"{self.id}: none of {sorted(wanted)} in model labels {labels}")
        self.labels = labels
        if getattr(self.fe, "sampling_rate", TARGET_RATE) != TARGET_RATE:
            raise ValueError(f"{self.id}: model expects {self.fe.sampling_rate} Hz, service provides {TARGET_RATE}")

    @torch.inference_mode()
    def analyze(self, x: np.ndarray) -> dict:
        inputs = self.fe(x, sampling_rate=TARGET_RATE, return_tensors="pt")
        probs = self.model(**inputs).logits.softmax(-1)[0]
        return {"score": float(probs[self.synthetic_idx].sum())}

    def selftest(self) -> dict:
        out = self.analyze(speechlike())
        ok = 0.0 <= out["score"] <= 1.0
        # The test signal is synthetic, so a high score is expected but not
        # required: this only verifies the model loads and produces sane output.
        return {"passed": ok, "synthetic_test_signal_score": round(out["score"], 4),
                "labels": list(self.labels.values())}
