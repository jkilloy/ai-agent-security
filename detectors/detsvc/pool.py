"""Pool of detector worker subprocesses for one release."""

from __future__ import annotations

import asyncio
import itertools
import json
import logging
import subprocess
from pathlib import Path

import numpy as np

from .releases import Release

log = logging.getLogger(__name__)

READY_TIMEOUT = 600.0  # loading large models can take a while
REQUEST_TIMEOUT = 120.0


class WorkerError(RuntimeError):
    pass


def worker_cmd(release: Release, threads: int) -> list[str]:
    return [release.python, "-m", "detsvc.worker", "--release", str(release.path), "--threads", str(threads)]


class Worker:
    def __init__(self, release: Release, app_dir: Path, threads: int, name: str):
        self.release = release
        self.app_dir = app_dir
        self.threads = threads
        self.name = name
        self.proc: asyncio.subprocess.Process | None = None
        self.info: dict = {}
        self.lock = asyncio.Lock()

    async def start(self) -> dict:
        env = self.release.worker_env(offline=True)
        env["PYTHONPATH"] = str(self.app_dir)
        self.proc = await asyncio.create_subprocess_exec(
            *worker_cmd(self.release, self.threads),
            stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE, stderr=None,
            env=env, cwd=str(self.app_dir), limit=1 << 20,
        )
        msg = await self._read(READY_TIMEOUT)
        if msg.get("type") != "ready":
            await self.stop()
            raise WorkerError(f"worker failed to start: {msg.get('error', msg)}")
        self.info = msg
        log.info("worker %s ready (release %s)", self.name, self.release.id)
        return msg

    async def _read(self, timeout: float) -> dict:
        assert self.proc and self.proc.stdout
        line = await asyncio.wait_for(self.proc.stdout.readline(), timeout)
        if not line:
            raise WorkerError("worker exited")
        return json.loads(line)

    async def request(self, header: dict, payload: bytes = b"", timeout: float = REQUEST_TIMEOUT) -> dict:
        async with self.lock:
            if not self.alive:
                await self.start()
            assert self.proc and self.proc.stdin
            try:
                self.proc.stdin.write((json.dumps({**header, "bytes": len(payload)}) + "\n").encode() + payload)
                await self.proc.stdin.drain()
                return await self._read(timeout)
            except (WorkerError, asyncio.TimeoutError, BrokenPipeError, ConnectionResetError) as e:
                # A crashed or hung worker is replaced on the next request.
                log.error("worker %s failed: %r; restarting", self.name, e)
                await self.stop()
                raise WorkerError(f"worker failed: {e!r}") from e

    @property
    def alive(self) -> bool:
        return self.proc is not None and self.proc.returncode is None

    async def stop(self) -> None:
        if self.proc and self.proc.returncode is None:
            self.proc.kill()
            await self.proc.wait()


class Pool:
    """Runs analysis requests on N workers of one release. Sessions hold a
    reference so a pool replaced by an update is closed only when drained.

    Requests wait in a priority queue and are never dropped. The priority a
    session passes is (job, channel, position): calls are analyzed in the order
    they started, and within a call channel 0 before channel 1, oldest audio
    first. A later call's windows run only when no earlier call has any queued.
    """

    def __init__(self, release: Release, app_dir: Path, size: int, threads: int):
        self.release = release
        self.workers = [Worker(release, app_dir, threads, f"{release.id}/{i}") for i in range(size)]
        self.queue: asyncio.PriorityQueue = asyncio.PriorityQueue()
        self._seq = itertools.count()   # tie-breaker: FIFO within one priority
        self._jobs = itertools.count()
        self.tasks: list[asyncio.Task] = []
        self.refs = 0
        self.retired = False
        self.info: dict = {}

    async def start(self) -> None:
        infos = await asyncio.gather(*(w.start() for w in self.workers))
        self.info = infos[0]
        self.tasks = [asyncio.create_task(self._run(w)) for w in self.workers]

    @property
    def detectors(self) -> list[dict]:
        return self.info.get("detectors", [])

    async def _run(self, w: Worker) -> None:
        while True:
            _, _, header, payload, fut = await self.queue.get()
            if fut.cancelled():
                continue
            try:
                res = await w.request(header, payload)
                if not fut.cancelled():
                    fut.set_result(res)
            except Exception as e:  # noqa: BLE001
                if not fut.cancelled():
                    fut.set_exception(e)

    def next_job(self) -> int:
        """A job number for a new call; lower numbers are analyzed first."""
        return next(self._jobs)

    @property
    def queued(self) -> int:
        return self.queue.qsize()

    async def analyze(self, x: np.ndarray, detectors: list[str], priority: tuple = ()) -> dict:
        fut = asyncio.get_running_loop().create_future()
        item = (priority, next(self._seq), {"op": "analyze", "detectors": detectors}, x.astype("<f4").tobytes(), fut)
        await self.queue.put(item)
        res = await fut
        if res.get("type") != "result":
            raise WorkerError(res.get("error", "analysis failed"))
        return res

    def ref(self) -> "Pool":
        self.refs += 1
        return self

    async def unref(self) -> None:
        self.refs -= 1
        if self.retired and self.refs <= 0:
            await self.close()

    async def retire(self) -> None:
        self.retired = True
        if self.refs <= 0:
            await self.close()

    async def close(self) -> None:
        for t in self.tasks:
            t.cancel()
        await asyncio.gather(*(w.stop() for w in self.workers), return_exceptions=True)
        log.info("pool for release %s closed", self.release.id)


def run_selftest_sync(release: Release, app_dir: Path, threads: int, timeout: float = 1800.0) -> dict:
    """Start a worker for `release` (network allowed, so model weights can be
    fetched into the release's cache) and run its self-test."""
    env = release.worker_env(offline=False)
    env["PYTHONPATH"] = str(app_dir)
    req = json.dumps({"op": "selftest", "bytes": 0}) + "\n"
    try:
        p = subprocess.run(worker_cmd(release, threads), input=req.encode(), capture_output=True,
                           env=env, cwd=str(app_dir), timeout=timeout)
    except subprocess.TimeoutExpired:
        return {"passed": False, "error": "self-test timed out"}
    lines = [json.loads(line) for line in p.stdout.decode().splitlines() if line.strip()]
    if not lines or lines[0].get("type") != "ready":
        err = lines[0].get("error") if lines else p.stderr.decode()[-2000:]
        return {"passed": False, "error": f"worker failed to start: {err}"}
    if len(lines) < 2:
        return {"passed": False, "error": "no self-test result: " + p.stderr.decode()[-2000:]}
    res = lines[1]
    res.pop("type", None)
    res["detector_info"] = lines[0].get("detectors", [])
    return res
