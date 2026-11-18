"""Build a detector release: install pinned package versions, download pinned
model snapshots, then run the self-tests.

    python -m detsvc.release_build --base /opt/release0
builds the image's baked-in base release from the installed packages and the
current upstream model revisions (run at docker build time).
"""

from __future__ import annotations

import argparse
import importlib.metadata
import json
import logging
import subprocess
import sys
import time
from pathlib import Path

from . import catalog as cat
from .pool import run_selftest_sync
from .releases import Release, write_meta

log = logging.getLogger(__name__)

# Only these file types are fetched from model repos: no pickles, no code.
HF_ALLOW = ["*.json", "*.safetensors", "*.txt", "*.model", "*.yaml"]
# AudioSeal ships pickled .pth weights only; they are fetched at a pinned commit.
AUDIOSEAL_FILES = ["detector_base.pth", "generator_base.pth"]


class BuildError(RuntimeError):
    pass


def download_models(rel_path: Path, specs: list[dict], hf_versions: dict[str, str]) -> None:
    from huggingface_hub import hf_hub_download, snapshot_download

    for spec in specs:
        if spec.get("model"):
            repo = spec["model"]
            dest = rel_path / "models" / cat.safe_name(repo)
            snapshot_download(repo, revision=hf_versions[repo], local_dir=dest, allow_patterns=HF_ALLOW)
            if not list(dest.glob("*.safetensors")):
                raise BuildError(f"{repo}@{hf_versions[repo]} has no safetensors weights; refusing pickle formats")
        if spec.get("adapter") == "audioseal" and spec.get("weights_repo"):
            repo = spec["weights_repo"]
            for fn in AUDIOSEAL_FILES:
                # AudioSeal looks for <cache>/audioseal/<file> before downloading.
                hf_hub_download(repo, fn, revision=hf_versions[repo], local_dir=rel_path / "cache" / "audioseal")


def install_packages(rel_path: Path, pypi_versions: dict[str, str], constraints: Path) -> str:
    """Create a venv layered over the image's site-packages and install the
    pinned detector packages into it. Returns the venv's python."""
    venv = rel_path / "venv"
    subprocess.run([sys.executable, "-m", "venv", "--system-site-packages", str(venv)], check=True)
    py = str(venv / "bin" / "python")
    pins = [f"{p}=={v}" for p, v in sorted(pypi_versions.items())]
    cmd = [py, "-m", "pip", "install", "--no-cache-dir", "--disable-pip-version-check",
           "--only-binary=:all:", "-c", str(constraints), *pins]
    p = subprocess.run(cmd, capture_output=True, text=True)
    if p.returncode != 0:
        raise BuildError("pip install failed:\n" + (p.stdout + p.stderr)[-3000:])
    return py


def build(rel_path: Path, specs: list[dict], versions: dict, *, app_dir: Path, constraints: Path | None,
          threads: int = 2, base: bool = False, fingerprint: str = "") -> tuple[Release, dict]:
    rel_path.mkdir(parents=True, exist_ok=True)
    (rel_path / "cache").mkdir(exist_ok=True)
    rid = rel_path.name if not base else "base"
    if base:
        python = sys.executable
    else:
        if constraints is None:
            raise BuildError("constraints file required")
        python = install_packages(rel_path, versions["pypi"], constraints)
    download_models(rel_path, specs, versions["hf"])
    meta = {
        "id": rid,
        "created": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "python": python,
        "catalog": specs,
        "versions": versions,
        "fingerprint": fingerprint or cat.fingerprint(specs, versions),
    }
    write_meta(rel_path, meta)
    rel = Release(rel_path, meta)
    result = run_selftest_sync(rel, app_dir, threads)
    result["telephony_robust"] = {
        k: bool(v.get("telephony_robust")) for k, v in result.get("detectors", {}).items()
        if "telephony_robust" in v
    }
    meta["selftest"] = result
    write_meta(rel_path, meta)
    return rel, result


def latest_hf_revisions(repos: list[str]) -> dict[str, str]:
    from huggingface_hub import HfApi

    api = HfApi()
    return {r: api.model_info(r).sha for r in repos}


def main() -> None:
    logging.basicConfig(level=logging.INFO)
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True, help="directory for the base release")
    ap.add_argument("--catalog", default="/app/catalog.yaml")
    ap.add_argument("--app-dir", default="/app")
    args = ap.parse_args()

    specs = cat.load_file(Path(args.catalog))
    tr = cat.tracked(specs)
    versions = {
        "pypi": {p: importlib.metadata.version(p) for p in tr["pypi"]},
        "hf": latest_hf_revisions(tr["hf"]),
    }
    _, result = build(Path(args.base), specs, versions, app_dir=Path(args.app_dir), constraints=None, base=True)
    print(json.dumps(result, indent=2))
    if not result.get("passed"):
        sys.exit("base release self-test failed")


if __name__ == "__main__":
    main()
