"""HTTP/WebSocket API of the detector service.

WebSocket /v1/stream:
    client -> text  {"call_id", "encoding", "sample_rate", "channels"?, "channel_labels"?}
    server -> text  {"type": "ready", "release": ...}   or {"type": "error", "error": ...} + close 1008
    client -> binary audio frames (interleaved if channels > 1)
    client -> text  {"type": "end"}
    server -> text  {"type": "report", "report": {...}} then close
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import logging

from fastapi import FastAPI, WebSocket, WebSocketDisconnect
from fastapi.responses import JSONResponse

from . import apis as api_mod
from .config import Config
from .pool import Pool
from .releases import Release, Store
from .session import Session, StartError
from .updater import Updater

log = logging.getLogger(__name__)


class Service:
    def __init__(self, cfg: Config):
        self.cfg = cfg
        self.store = Store(cfg.data_dir, cfg.base_release, cfg.releases_keep)
        self.pool: Pool | None = None
        self.apis = api_mod.enabled(cfg)
        self.updater = Updater(cfg, self.store, self.swap)
        self._swap_lock = asyncio.Lock()
        self._tasks: list[asyncio.Task] = []

    async def start(self) -> None:
        rel = self.store.active()
        try:
            self.pool = await self._start_pool(rel)
        except Exception:
            log.exception("active release %s failed to start; falling back to base release", rel.id)
            base = self.store.base_release()
            self.store.reject(rel.meta.get("fingerprint", rel.id), "failed to start")
            self.store.promote(base)
            self.pool = await self._start_pool(base)
        log.info("serving release %s; commercial APIs enabled: %s", self.pool.release.id, sorted(self.apis) or "none")
        if self.cfg.update_enabled:
            self._tasks.append(asyncio.create_task(self.updater.run()))

    async def _start_pool(self, rel: Release) -> Pool:
        pool = Pool(rel, self.cfg.app_dir, self.cfg.workers, self.cfg.torch_threads)
        try:
            await pool.start()
        except Exception:
            await pool.close()
            raise
        return pool

    async def swap(self, rel: Release) -> None:
        """Start workers for `rel`, route new calls to them, and drain the old pool."""
        async with self._swap_lock:
            new = await self._start_pool(rel)
            old, self.pool = self.pool, new
            log.info("now serving release %s", rel.id)
            if old:
                await old.retire()

    async def stop(self) -> None:
        for t in self._tasks:
            t.cancel()
        if self.pool:
            await self.pool.close()

    def status(self) -> dict:
        p = self.pool
        return {
            "ready": p is not None,
            "release": p.release.summary() if p else None,
            "detectors": p.detectors if p else [],
            "selftest": p.release.meta.get("selftest") if p else None,
            "apis_enabled": sorted(self.apis),
            "updates": {"enabled": self.cfg.update_enabled, "interval_seconds": self.cfg.update_interval,
                        **self.updater.status},
            "history": self.store.history(),
            "rejected": self.store.rejected(),
        }


def create_app(cfg: Config) -> FastAPI:
    svc = Service(cfg)

    @contextlib.asynccontextmanager
    async def lifespan(app: FastAPI):
        await svc.start()
        yield
        await svc.stop()

    app = FastAPI(title="detector service", lifespan=lifespan)
    app.state.svc = svc

    @app.get("/healthz")
    async def healthz():
        return JSONResponse({"ready": svc.pool is not None}, status_code=200 if svc.pool else 503)

    @app.get("/v1/status")
    async def status():
        return svc.status()

    @app.post("/v1/update")
    async def update():
        if svc.updater.lock.locked():
            return JSONResponse({"result": "already_running"}, status_code=409)
        asyncio.create_task(svc.updater.check())
        return JSONResponse({"result": "started"}, status_code=202)

    @app.post("/v1/rollback")
    async def rollback():
        rel = svc.store.rollback()
        if rel is None:
            return JSONResponse({"error": "no previous release"}, status_code=409)
        await svc.swap(rel)
        return {"result": "rolled_back", "release": rel.id}

    @app.websocket("/v1/stream")
    async def stream(ws: WebSocket):
        await ws.accept()
        try:
            start = json.loads(await asyncio.wait_for(ws.receive_text(), cfg.max_start_wait))
            if not isinstance(start, dict):
                raise StartError("start message must be a JSON object")
            pool = svc.pool
            if pool is None:
                raise StartError("service not ready")
            sess = Session(cfg, pool, svc.apis, start)
        except (StartError, ValueError, asyncio.TimeoutError, KeyError) as e:
            msg = str(e) or type(e).__name__
            with contextlib.suppress(Exception):
                await ws.send_json({"type": "error", "error": msg})
                await ws.close(code=1008)
            return
        except WebSocketDisconnect:
            return

        pool.ref()
        disconnected = False
        try:
            await ws.send_json({"type": "ready", "release": pool.release.id})
            while True:
                m = await ws.receive()
                if m["type"] == "websocket.disconnect":
                    disconnected = True
                    break
                if m.get("bytes") is not None:
                    sess.feed(m["bytes"])
                elif m.get("text") is not None:
                    with contextlib.suppress(ValueError):
                        if json.loads(m["text"]).get("type") == "end":
                            break
            if disconnected:
                for t in list(sess.pending):
                    t.cancel()
                return
            report = await sess.finish()
            await ws.send_json({"type": "report", "report": report})
            await ws.close()
        except WebSocketDisconnect:
            for t in list(sess.pending):
                t.cancel()
        except Exception as e:  # noqa: BLE001
            log.exception("session %s failed", sess.call_id)
            with contextlib.suppress(Exception):
                await ws.send_json({"type": "error", "error": f"{type(e).__name__}: {e}"})
                await ws.close(code=1011)
        finally:
            await pool.unref()

    return app
