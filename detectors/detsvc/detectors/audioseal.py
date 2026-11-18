"""Meta AudioSeal watermark detector (https://github.com/facebookresearch/audioseal)."""

from __future__ import annotations

import numpy as np
import torch

from ..audio import TARGET_RATE, telephony_roundtrip
from ..testsignal import speechlike
from . import Detector


class AudioSealDetector(Detector):
    def __init__(self, spec, release):
        super().__init__(spec, release)
        from audioseal import AudioSeal

        self._AudioSeal = AudioSeal
        self.detector = AudioSeal.load_detector(self.options.get("detector_card", "audioseal_detector_16bits"))
        self.detector.eval()

    @torch.inference_mode()
    def analyze(self, x: np.ndarray) -> dict:
        t = torch.from_numpy(np.ascontiguousarray(x))[None, None]
        prob, message = self.detector.detect_watermark(t, TARGET_RATE)
        score = float(prob)
        out = {"score": score}
        if score >= self.threshold:
            bits = (message[0] > 0.5).int().tolist()
            out["message"] = "0x%04x" % int("".join(map(str, bits)), 2)
        return out

    @torch.inference_mode()
    def selftest(self) -> dict:
        gen = self._AudioSeal.load_generator(self.options.get("generator_card", "audioseal_wm_16bits"))
        x = speechlike()
        t = torch.from_numpy(x)[None, None]
        msg = torch.tensor([[1, 0, 1, 1, 0, 0, 1, 0, 1, 1, 1, 0, 0, 1, 0, 1]])
        wm = (t + gen.get_watermark(t, TARGET_RATE, message=msg))[0, 0].numpy()
        clean = self.analyze(x)["score"]
        marked = self.analyze(wm)
        phone = self.analyze(telephony_roundtrip(wm))
        passed = clean < self.threshold and marked["score"] >= 0.9 and marked.get("message") == "0xb2e5"
        return {
            "passed": passed,
            "clean_score": round(clean, 4),
            "watermarked_score": round(marked["score"], 4),
            "telephony_score": round(phone["score"], 4),
            "telephony_robust": phone["score"] >= self.threshold,
        }
