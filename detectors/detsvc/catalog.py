"""Detector catalog: which detectors run and which upstream sources to track."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

import yaml

KINDS = {"watermark", "synthetic_speech"}
ADAPTERS = {"audioseal", "perth", "hf_audio_classifier", "dummy"}


class CatalogError(ValueError):
    pass


def validate(data: Any) -> list[dict]:
    """Validate a parsed catalog and return its detector specs."""
    if not isinstance(data, dict) or not isinstance(data.get("detectors"), list):
        raise CatalogError("catalog must be a mapping with a 'detectors' list")
    seen = set()
    out = []
    for i, d in enumerate(data["detectors"]):
        if not isinstance(d, dict):
            raise CatalogError(f"detector #{i} is not a mapping")
        did = d.get("id")
        if not isinstance(did, str) or not did or did in seen:
            raise CatalogError(f"detector #{i}: missing or duplicate id")
        seen.add(did)
        if d.get("kind") not in KINDS:
            raise CatalogError(f"{did}: kind must be one of {sorted(KINDS)}")
        if d.get("adapter") not in ADAPTERS:
            raise CatalogError(f"{did}: unknown adapter {d.get('adapter')!r}")
        if d["adapter"] == "hf_audio_classifier":
            if not d.get("model") or not d.get("synthetic_labels"):
                raise CatalogError(f"{did}: hf_audio_classifier needs model and synthetic_labels")
        thr = d.get("threshold", 0.5)
        if not isinstance(thr, (int, float)) or not 0 < thr <= 1:
            raise CatalogError(f"{did}: threshold must be in (0, 1]")
        out.append(d)
    if not out:
        raise CatalogError("catalog has no detectors")
    return out


def load_file(path: Path) -> list[dict]:
    return validate(yaml.safe_load(path.read_text()))


def load_text(text: str) -> list[dict]:
    return validate(yaml.safe_load(text))


def tracked(specs: list[dict]) -> dict[str, list[str]]:
    """Upstream sources referenced by the catalog."""
    pypi = sorted({d["package"] for d in specs if d.get("package")})
    hf = sorted({r for d in specs for r in (d.get("model"), d.get("weights_repo")) if r})
    return {"pypi": pypi, "hf": hf}


def fingerprint(specs: list[dict], versions: dict) -> str:
    blob = json.dumps({"catalog": specs, "versions": versions}, sort_keys=True)
    return hashlib.sha256(blob.encode()).hexdigest()[:16]


def safe_name(repo: str) -> str:
    return repo.replace("/", "__")
