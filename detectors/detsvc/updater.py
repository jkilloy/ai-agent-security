"""Tracks the upstream detector packages (PyPI) and models (Hugging Face) named
in the catalog. When any has a newer release, builds a candidate release,
self-tests it, and promotes it only if the self-tests pass."""

from __future__ import annotations

import asyncio
import logging
import shutil
import time
from typing import Awaitable, Callable

import httpx

from . import catalog as cat
from . import release_build
from .releases import Release, Store

log = logging.getLogger(__name__)

Versions = dict[str, dict[str, str]]


async def fetch_latest(tracked: dict[str, list[str]], client: httpx.AsyncClient) -> Versions:
    async def pypi(pkg: str) -> str:
        r = await client.get(f"https://pypi.org/pypi/{pkg}/json")
        r.raise_for_status()
        return r.json()["info"]["version"]

    async def hf(repo: str) -> str:
        r = await client.get(f"https://huggingface.co/api/models/{repo}")
        r.raise_for_status()
        return r.json()["sha"]

    p = await asyncio.gather(*(pypi(x) for x in tracked["pypi"]))
    h = await asyncio.gather(*(hf(x) for x in tracked["hf"]))
    return {"pypi": dict(zip(tracked["pypi"], p)), "hf": dict(zip(tracked["hf"], h))}


class Updater:
    def __init__(self, cfg, store: Store, on_promote: Callable[[Release], Awaitable[None]],
                 fetch: Callable[[dict], Awaitable[Versions]] | None = None,
                 builder: Callable[..., tuple[Release, dict]] | None = None):
        self.cfg = cfg
        self.store = store
        self.on_promote = on_promote
        self._fetch = fetch
        self._build = builder or release_build.build
        self.lock = asyncio.Lock()
        self.status: dict = {"last_check": None, "last_result": None}

    async def _catalog(self) -> list[dict]:
        if self.cfg.catalog_url:
            async with httpx.AsyncClient(timeout=30, follow_redirects=True) as c:
                r = await c.get(self.cfg.catalog_url)
                r.raise_for_status()
                return cat.load_text(r.text)
        return cat.load_file(self.cfg.catalog_path)

    async def _latest(self, tracked: dict) -> Versions:
        if self._fetch:
            return await self._fetch(tracked)
        async with httpx.AsyncClient(timeout=30, follow_redirects=True) as c:
            return await fetch_latest(tracked, c)

    async def check(self) -> dict:
        """Run one update check. Returns a status dict."""
        async with self.lock:
            res = await self._check()
            self.status = {"last_check": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "last_result": res}
            log.info("update check: %s", res)
            return res

    async def _check(self) -> dict:
        try:
            specs = await self._catalog()
            latest = await self._latest(cat.tracked(specs))
        except Exception as e:  # noqa: BLE001
            return {"result": "check_failed", "error": f"{type(e).__name__}: {e}"}

        active = self.store.active()
        fp = cat.fingerprint(specs, latest)
        if fp == active.meta.get("fingerprint"):
            return {"result": "up_to_date", "release": active.id, "versions": latest}
        base = self.store.base_release()
        if fp == base.meta.get("fingerprint"):
            # A newer image already ships these versions: switch back to it.
            self.store.promote(base)
            await self.on_promote(base)
            return {"result": "promoted", "release": base.id, "changes": _diff(active.versions, latest)}
        if self.store.is_rejected(fp):
            return {"result": "candidate_previously_rejected", "versions": latest}

        rid = "r" + time.strftime("%Y%m%d-%H%M%S", time.gmtime())
        path = self.store.dir / rid
        changes = _diff(active.versions, latest)
        log.info("building candidate release %s: %s", rid, changes)
        try:
            rel, result = await asyncio.to_thread(
                self._build, path, specs, latest, app_dir=self.cfg.app_dir,
                constraints=self.cfg.app_dir / "constraints.txt", threads=self.cfg.torch_threads,
                fingerprint=fp)
        except Exception as e:  # noqa: BLE001
            shutil.rmtree(path, ignore_errors=True)
            self.store.reject(fp, f"build failed: {e}")
            return {"result": "build_failed", "changes": changes, "error": str(e)[-1000:]}

        if not result.get("passed"):
            shutil.rmtree(path, ignore_errors=True)
            self.store.reject(fp, "self-test failed")
            return {"result": "selftest_failed", "changes": changes, "selftest": result}

        self.store.promote(rel)
        await self.on_promote(rel)
        return {"result": "promoted", "release": rel.id, "changes": changes,
                "telephony_robust": result.get("telephony_robust")}

    async def run(self) -> None:
        while True:
            await self.check()
            await asyncio.sleep(self.cfg.update_interval)


def _diff(old: Versions, new: Versions) -> dict:
    out = {}
    for src in ("pypi", "hf"):
        for k, v in new.get(src, {}).items():
            if old.get(src, {}).get(k) != v:
                out[k] = {"from": old.get(src, {}).get(k), "to": v}
    return out
