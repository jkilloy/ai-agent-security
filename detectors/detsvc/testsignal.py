"""Deterministic speech-like test signal used by detector self-tests."""

from __future__ import annotations

import numpy as np

from .audio import TARGET_RATE


def speechlike(seconds: float = 4.0, seed: int = 7) -> np.ndarray:
    """A voiced, vowel-like signal: harmonic series with a wandering pitch,
    a syllable-rate amplitude envelope and a little breath noise."""
    rng = np.random.default_rng(seed)
    sr = TARGET_RATE
    t = np.arange(int(sr * seconds)) / sr
    f0 = 120 + 20 * np.sin(2 * np.pi * 0.7 * t)
    phase = 2 * np.pi * np.cumsum(f0) / sr
    x = sum((1.0 / k) * np.sin(k * phase) for k in range(1, 30))
    x = x * np.clip(0.5 + 0.5 * np.sin(2 * np.pi * 3 * t), 0, 1)
    x = x + 0.02 * rng.standard_normal(t.size)
    return (x / np.abs(x).max() * 0.5).astype(np.float32)
