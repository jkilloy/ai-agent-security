"""Audio decoding (G.711 mu-law/A-law, PCM) and resampling, numpy only."""

from __future__ import annotations

from math import gcd

import numpy as np
from scipy.signal import resample_poly

TARGET_RATE = 16000

# Accepted encoding names -> canonical name.
ENCODINGS = {
    "pcm16": "pcm16", "linear16": "pcm16", "s16le": "pcm16", "l16": "pcm16",
    "mulaw": "mulaw", "ulaw": "mulaw", "pcmu": "mulaw", "g711u": "mulaw",
    "alaw": "alaw", "pcma": "alaw", "g711a": "alaw",
    "f32le": "f32le", "float32": "f32le",
    "pcm8": "u8", "u8": "u8",
}
_WIDTH = {"pcm16": 2, "mulaw": 1, "alaw": 1, "f32le": 4, "u8": 1}


def _mulaw_table() -> np.ndarray:
    u = (~np.arange(256, dtype=np.uint8)).astype(np.int32)
    sign, exp, mant = u & 0x80, (u >> 4) & 0x07, u & 0x0F
    mag = (((mant << 3) + 0x84) << exp) - 0x84
    return (np.where(sign, -mag, mag) / 32768.0).astype(np.float32)


def _alaw_table() -> np.ndarray:
    a = np.arange(256, dtype=np.int32) ^ 0x55
    sign, exp, mant = a & 0x80, (a >> 4) & 0x07, a & 0x0F
    mag = np.where(exp == 0, (mant << 4) + 8, ((mant << 4) + 0x108) << np.maximum(exp - 1, 0))
    return (np.where(sign, mag, -mag) / 32768.0).astype(np.float32)


MULAW = _mulaw_table()
ALAW = _alaw_table()


def normalize_encoding(name: str) -> str:
    try:
        return ENCODINGS[name.strip().lower()]
    except (KeyError, AttributeError):
        raise ValueError(f"unsupported encoding {name!r}; supported: {sorted(set(ENCODINGS))}") from None


class Decoder:
    """Incrementally decodes a byte stream into float32 frames of shape
    (samples, channels). Partial samples split across messages are kept."""

    def __init__(self, encoding: str, channels: int):
        self.encoding = normalize_encoding(encoding)
        if not 1 <= channels <= 8:
            raise ValueError("channels must be between 1 and 8")
        self.channels = channels
        self.frame_bytes = _WIDTH[self.encoding] * channels
        self._rest = b""

    def feed(self, data: bytes) -> np.ndarray:
        buf = self._rest + data
        usable = len(buf) - len(buf) % self.frame_bytes
        self._rest = buf[usable:]
        raw = buf[:usable]
        if self.encoding == "pcm16":
            x = np.frombuffer(raw, dtype="<i2").astype(np.float32) / 32768.0
        elif self.encoding == "mulaw":
            x = MULAW[np.frombuffer(raw, dtype=np.uint8)]
        elif self.encoding == "alaw":
            x = ALAW[np.frombuffer(raw, dtype=np.uint8)]
        elif self.encoding == "u8":
            x = (np.frombuffer(raw, dtype=np.uint8).astype(np.float32) - 128.0) / 128.0
        else:  # f32le
            x = np.frombuffer(raw, dtype="<f4").astype(np.float32)
        return x.reshape(-1, self.channels)


def resample(x: np.ndarray, src: int, dst: int = TARGET_RATE) -> np.ndarray:
    if src == dst:
        return x.astype(np.float32, copy=False)
    g = gcd(src, dst)
    return resample_poly(x, dst // g, src // g).astype(np.float32)


def rms(x: np.ndarray) -> float:
    return float(np.sqrt(np.mean(np.square(x, dtype=np.float64)))) if x.size else 0.0


def mulaw_encode(x: np.ndarray) -> np.ndarray:
    """Float [-1,1] -> G.711 mu-law bytes (used by self-tests to simulate telephony)."""
    s = np.clip(np.round(x * 32768.0), -32768, 32767).astype(np.int32)
    sign = np.where(s < 0, 0x80, 0)
    mag = np.minimum(np.abs(s), 32635) + 0x84
    exp = np.floor(np.log2(mag)).astype(np.int32) - 7
    exp = np.clip(exp, 0, 7)
    mant = (mag >> (exp + 3)) & 0x0F
    return (~(sign | (exp << 4) | mant) & 0xFF).astype(np.uint8)


def telephony_roundtrip(x16k: np.ndarray) -> np.ndarray:
    """Simulate a phone call path: 16 kHz -> 8 kHz G.711 mu-law -> 16 kHz."""
    narrow = resample(x16k, TARGET_RATE, 8000)
    return resample(MULAW[mulaw_encode(narrow)], 8000, TARGET_RATE)


def to_wav_bytes(x: np.ndarray, rate: int) -> bytes:
    """Mono float32 -> 16-bit PCM WAV bytes."""
    import io
    import wave

    pcm = np.clip(np.round(x * 32767.0), -32768, 32767).astype("<i2").tobytes()
    bio = io.BytesIO()
    with wave.open(bio, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(pcm)
    return bio.getvalue()
