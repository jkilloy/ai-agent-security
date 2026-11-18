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
