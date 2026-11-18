"""A release is a self-contained set of detector code versions and model files.

Layout of a release directory:
    release.json   id, versions, catalog snapshot, python executable, self-test
    venv/          (non-base releases) venv layered over the image's packages
    models/        Hugging Face snapshots, one dir per repo
    cache/         AudioSeal weight cache (AUDIOSEAL_CACHE_DIR)
"""

from __future__ import annotations

import json
import os
import shutil
from dataclasses import dataclass
from pathlib import Path

from .catalog import safe_name


@dataclass
class Release:
    path: Path
    meta: dict

    @property
    def id(self) -> str:
        return self.meta["id"]

    @property
    def python(self) -> str:
        return self.meta["python"]

    @property
    def catalog(self) -> list[dict]:
        return self.meta["catalog"]

    @property
    def versions(self) -> dict:
        return self.meta["versions"]

    def model_dir(self, repo: str) -> Path:
        return self.path / "models" / safe_name(repo)

    @property
    def cache_dir(self) -> Path:
        return self.path / "cache"

    def worker_env(self, offline: bool = True) -> dict[str, str]:
        env = dict(os.environ)
        env.update(
            AUDIOSEAL_CACHE_DIR=str(self.cache_dir),
            HF_HOME=str(self.path / "hf"),
            TORCHDYNAMO_DISABLE="1",
            TOKENIZERS_PARALLELISM="false",
        )
        if offline:
            env["HF_HUB_OFFLINE"] = "1"
            env["TRANSFORMERS_OFFLINE"] = "1"
        # Never hand API keys to detector code.
        for k in ("RESEMBLE_API_KEY", "REALITY_DEFENDER_API_KEY"):
            env.pop(k, None)
        return env

    def summary(self) -> dict:
        return {
            "id": self.id,
            "created": self.meta.get("created"),
            "versions": self.versions,
            "telephony_robust": self.meta.get("selftest", {}).get("telephony_robust", {}),
        }


def load(path: Path) -> Release:
    return Release(path, json.loads((path / "release.json").read_text()))


def write_meta(path: Path, meta: dict) -> None:
    tmp = path / "release.json.tmp"
    tmp.write_text(json.dumps(meta, indent=2, sort_keys=True))
    os.replace(tmp, path / "release.json")


class Store:
    """Tracks the active release and rejected candidates under DATA_DIR."""

    def __init__(self, data_dir: Path, base: Path, keep: int):
        self.data_dir = data_dir
        self.base = base
        self.keep = keep
        self.dir = data_dir / "releases"
        self.dir.mkdir(parents=True, exist_ok=True)
        self._state_path = data_dir / "state.json"

    def _state(self) -> dict:
        try:
            return json.loads(self._state_path.read_text())
        except (FileNotFoundError, json.JSONDecodeError):
            return {}

    def _save(self, st: dict) -> None:
        tmp = self._state_path.with_suffix(".tmp")
        tmp.write_text(json.dumps(st, indent=2, sort_keys=True))
        os.replace(tmp, self._state_path)

    def active(self) -> Release:
        st = self._state()
        p = st.get("active")
        if p and (Path(p) / "release.json").exists():
            return load(Path(p))
        return load(self.base)

    def base_release(self) -> Release:
        return load(self.base)

    def history(self) -> list[str]:
        return self._state().get("history", [])

    def promote(self, rel: Release) -> None:
        st = self._state()
        hist = [h for h in st.get("history", []) if h != str(rel.path)]
        prev = st.get("active") or str(self.base)
        if prev != str(rel.path):
            hist.insert(0, prev)
        st["active"] = str(rel.path)
        st["history"] = hist
        self._save(st)
        self._prune(st)

    def rollback(self) -> Release | None:
        st = self._state()
        for i, p in enumerate(st.get("history", [])):
            if (Path(p) / "release.json").exists():
                bad = st.get("active")
                st["active"] = p
                st["history"] = st["history"][i + 1:]
                if bad:
                    # Don't auto-reinstall the release we just rolled back from.
                    st.setdefault("rejected", {})[load(Path(bad)).meta.get("fingerprint", "")] = "rolled back"
                self._save(st)
                return load(Path(p))
        return None

    def is_rejected(self, fp: str) -> bool:
        return fp in self._state().get("rejected", {})

    def reject(self, fp: str, reason: str) -> None:
        st = self._state()
        st.setdefault("rejected", {})[fp] = reason[:500]
        self._save(st)

    def rejected(self) -> dict:
        return self._state().get("rejected", {})

    def _prune(self, st: dict) -> None:
        keep = {st.get("active")} | set(st.get("history", [])[: max(self.keep - 1, 0)])
        st["history"] = [h for h in st.get("history", []) if h in keep]
        self._save(st)
        for d in self.dir.iterdir():
            if d.is_dir() and str(d) not in keep:
                shutil.rmtree(d, ignore_errors=True)
