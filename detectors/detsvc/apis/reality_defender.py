"""Reality Defender (https://pypi.org/project/realitydefender/)."""

from __future__ import annotations

import os
import tempfile


def parse(result: dict) -> dict:
    return {
        "status": result.get("status"),  # e.g. MANIPULATED / AUTHENTIC
        "score": result.get("score"),
        "models": [
            {"name": m.get("name"), "status": m.get("status"), "score": m.get("score")}
            for m in result.get("models") or []
        ],
    }


async def detect(wav: bytes, api_key: str, timeout: float) -> dict:
    from realitydefender import RealityDefender

    fd, path = tempfile.mkstemp(suffix=".wav")
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(wav)
        rd = RealityDefender(api_key=api_key)
        up = await rd.upload(file_path=path)
        result = await rd.get_result(up["request_id"])
        return {"request_id": up["request_id"], **parse(result)}
    finally:
        os.unlink(path)
