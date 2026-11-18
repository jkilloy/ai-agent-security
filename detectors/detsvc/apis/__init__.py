"""Optional commercial detection APIs. Each is enabled only when its API key
is configured. They receive a WAV of the first API_MAX_SECONDS of a channel at
the end of the call. Note: this sends call audio to a third party."""

from __future__ import annotations

import asyncio
import logging
from typing import Awaitable, Callable

import numpy as np

log = logging.getLogger(__name__)

ApiFn = Callable[[bytes, float], Awaitable[dict]]


def enabled(cfg) -> dict[str, ApiFn]:
    out: dict[str, ApiFn] = {}
    if cfg.resemble_api_key:
        from .resemble import detect as resemble

        out["resemble"] = lambda wav, t: resemble(wav, cfg.resemble_api_key, t)
    if cfg.reality_defender_api_key:
        from .reality_defender import detect as rd

        out["reality_defender"] = lambda wav, t: rd(wav, cfg.reality_defender_api_key, t)
    return out


async def run_all(apis: dict[str, ApiFn], audio: np.ndarray, rate: int, timeout: float) -> dict:
    from ..audio import to_wav_bytes

    if not apis or audio.size == 0:
        return {}
    wav = to_wav_bytes(audio, rate)

    async def one(name: str, fn: ApiFn) -> tuple[str, dict]:
        try:
            return name, await asyncio.wait_for(fn(wav, timeout), timeout)
        except asyncio.TimeoutError:
            return name, {"error": f"timed out after {timeout:.0f}s"}
        except Exception as e:  # noqa: BLE001
            log.warning("api %s failed: %r", name, e)
            return name, {"error": f"{type(e).__name__}: {e}"[:300]}

    return dict(await asyncio.gather(*(one(n, f) for n, f in apis.items())))
