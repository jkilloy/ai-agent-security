"""Resemble AI Detect (https://docs.resemble.ai/detect/create).

Requests deepfake detection plus watermark analysis (Resemble's watermark
check covers Perth and, where available, Google SynthID) and audio source
tracing. Uses zero-retention mode so Resemble does not keep the audio."""

from __future__ import annotations

import asyncio
import time

import httpx

BASE = "https://app.resemble.ai/api/v2/detect"


def _f(v):
    try:
        return float(v)
    except (TypeError, ValueError):
        return None


def parse(item: dict) -> dict:
    m = item.get("metrics") or {}
    out = {
        "label": m.get("label"),
        "score": _f(m.get("aggregated_score")),
        "consistency": _f(m.get("consistency")),
        "window_scores": [_f(s) for s in (m.get("score") or [])],
        "uuid": item.get("uuid"),
    }
    wm = (item.get("watermark") or {}).get("metrics")
    if wm:
        out["watermark"] = {
            "status": wm.get("overall_status"),
            "confidence": wm.get("confidence"),
            "detected_model_versions": wm.get("detected_model_versions"),
            "synthid": wm.get("synthid"),
        }
    for key in ("audio_source_tracing", "source_tracing"):
        if item.get(key) is not None:
            out["source_tracing"] = item[key]
    return out


def _done(item: dict) -> bool:
    status = (item.get("status") or "").lower()
    return bool((item.get("metrics") or {}).get("label")) or status in ("completed", "failed", "error")


async def detect(wav: bytes, api_key: str, timeout: float) -> dict:
    headers = {"Authorization": f"Bearer {api_key}"}
    deadline = time.monotonic() + timeout
    async with httpx.AsyncClient(timeout=timeout) as c:
        r = await c.post(
            BASE,
            headers={**headers, "Prefer": "wait"},
            files={"file": ("call.wav", wav, "audio/wav")},
            data={"detect_watermark": "true", "audio_source_tracing": "true", "zero_retention_mode": "true"},
        )
        r.raise_for_status()
        body = r.json()
        item = body.get("item") or {}
        # Prefer: wait normally returns the finished result; poll if not.
        while not _done(item) and item.get("uuid") and time.monotonic() < deadline:
            await asyncio.sleep(1.0)
            g = await c.get(f"{BASE}/{item['uuid']}", headers=headers)
            g.raise_for_status()
            item = g.json().get("item") or item
        if not _done(item):
            return {"error": "result not ready before timeout", "uuid": item.get("uuid")}
        return parse(item)
