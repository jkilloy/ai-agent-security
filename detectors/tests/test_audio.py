import numpy as np
import pytest

from detsvc.audio import ALAW, MULAW, Decoder, mulaw_encode, resample, telephony_roundtrip


def test_g711_tables_known_values():
    assert MULAW[0xFF] == 0.0 and MULAW[0x7F] == 0.0
    assert MULAW[0x00] == pytest.approx(-32124 / 32768)
    assert MULAW[0x80] == pytest.approx(32124 / 32768)
    assert ALAW[0xD5] == pytest.approx(8 / 32768)
    assert ALAW[0x55] == pytest.approx(-8 / 32768)
    assert ALAW[0xAA] == pytest.approx(32256 / 32768)


def test_mulaw_roundtrip_error_is_small():
    x = np.linspace(-0.9, 0.9, 2001, dtype=np.float32)
    y = MULAW[mulaw_encode(x)]
    assert np.max(np.abs(y - x) / np.maximum(np.abs(x), 0.01)) < 0.07


def test_decoder_keeps_partial_samples_and_deinterleaves():
    d = Decoder("pcm16", 2)
    raw = np.array([1000, -1000, 2000, -2000], dtype="<i2").tobytes()
    a = d.feed(raw[:3])          # 1.5 samples
    b = d.feed(raw[3:])
    out = np.concatenate([a, b])
    assert out.shape == (2, 2)
    assert out[:, 0] == pytest.approx([1000 / 32768, 2000 / 32768])
    assert out[:, 1] == pytest.approx([-1000 / 32768, -2000 / 32768])


def test_decoder_rejects_unknown_encoding():
    with pytest.raises(ValueError):
        Decoder("opus", 1)


def test_resample_and_telephony_lengths():
    x = np.zeros(16000, np.float32)
    assert resample(x, 16000, 8000).size == 8000
    assert telephony_roundtrip(x).size == 16000
