import json
import sys
from pathlib import Path

import pytest

from detsvc.config import Config
from detsvc.releases import Release, write_meta

APP_DIR = Path(__file__).resolve().parent.parent


def dummy_specs(**opts):
    return [
        {"id": "wm", "kind": "watermark", "adapter": "dummy", "threshold": 0.5, "options": opts},
        {"id": "syn", "kind": "synthetic_speech", "adapter": "dummy", "threshold": 0.5, "stride": 2,
         "max_windows": 2},
    ]


def make_release(path: Path, specs=None, fingerprint="fp-base", rid="base") -> Release:
    path.mkdir(parents=True, exist_ok=True)
    meta = {"id": rid, "created": "now", "python": sys.executable, "catalog": specs or dummy_specs(),
            "versions": {"pypi": {}, "hf": {}}, "fingerprint": fingerprint,
            "selftest": {"passed": True, "telephony_robust": {"wm": True}}}
    write_meta(path, meta)
    return Release(path, meta)


@pytest.fixture
def cfg(tmp_path):
    base = make_release(tmp_path / "base")
    return Config(app_dir=APP_DIR, data_dir=tmp_path / "data", base_release=base.path,
                  catalog_path=tmp_path / "catalog.yaml", workers=1, torch_threads=1,
                  window_seconds=1.0, min_window_seconds=0.5, update_enabled=False,
                  finish_timeout=10.0)
