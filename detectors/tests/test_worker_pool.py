import asyncio

import numpy as np

from detsvc.pool import Pool, run_selftest_sync
from tests.conftest import APP_DIR, dummy_specs, make_release


def test_pool_analyze_and_errors(tmp_path):
    specs = dummy_specs() + [{"id": "broken", "kind": "watermark", "adapter": "dummy", "options": {"crash": True}}]
    rel = make_release(tmp_path / "r", specs)

    async def run():
        pool = Pool(rel, APP_DIR, size=2, threads=1)
        await pool.start()
        try:
            res = await pool.analyze(np.full(16000, 0.25, np.float32), ["wm", "broken", "nope"])
            return pool.detectors, res
        finally:
            await pool.close()

    dets, res = asyncio.run(run())
    assert [d["id"] for d in dets] == ["wm", "syn", "broken"]
    assert res["results"]["wm"]["score"] == 0.25
    assert "dummy crash" in res["errors"]["broken"] and res["errors"]["nope"] == "unknown detector"


def test_selftest_sync(tmp_path):
    ok = run_selftest_sync(make_release(tmp_path / "ok"), APP_DIR, 1)
    assert ok["passed"] and ok["detectors"]["wm"]["passed"]
    bad = run_selftest_sync(make_release(tmp_path / "bad", dummy_specs(fail_selftest=True)), APP_DIR, 1)
    assert not bad["passed"]


def test_worker_fatal_on_bad_adapter(tmp_path):
    rel = make_release(tmp_path / "r", [{"id": "x", "kind": "watermark", "adapter": "nope"}])
    res = run_selftest_sync(rel, APP_DIR, 1)
    assert not res["passed"] and "failed to start" in res["error"]


def test_queue_order_calls_then_channels(tmp_path):
    """With one worker busy, queued windows run oldest call first, then channel
    0 before channel 1, then in time order, regardless of arrival order."""
    rel = make_release(tmp_path / "r")
    order = []

    async def run():
        pool = Pool(rel, APP_DIR, size=1, threads=1)
        await pool.start()
        try:
            a, b = pool.next_job(), pool.next_job()
            x = np.zeros(16000, np.float32)

            async def win(job, ch, t):
                await pool.analyze(x, ["wm"], (job, ch, t))
                order.append((job, ch, t))

            blocker = asyncio.create_task(pool.analyze(x, ["wm"], (-1, 0, 0)))
            await asyncio.sleep(0)
            # Arrival order: interleaved across calls and channels.
            arrivals = [(b, 0, 0.0), (a, 1, 0.0), (a, 0, 4.0), (b, 1, 0.0), (a, 0, 0.0), (a, 1, 4.0)]
            tasks = [asyncio.create_task(win(*w)) for w in arrivals]
            await asyncio.gather(blocker, *tasks)
            return a, b
        finally:
            await pool.close()

    a, b = asyncio.run(run())
    assert order == [(a, 0, 0.0), (a, 0, 4.0), (a, 1, 0.0), (a, 1, 4.0), (b, 0, 0.0), (b, 1, 0.0)]
