import asyncio

from detsvc import catalog as cat
from detsvc.releases import Store
from detsvc.updater import Updater
from tests.conftest import dummy_specs, make_release

SPECS = dummy_specs()


def setup(cfg, latest, passed=True):
    cfg.catalog_path.write_text("detectors:\n" + "".join(
        f"  - {{id: {s['id']}, kind: {s['kind']}, adapter: dummy}}\n" for s in SPECS))
    specs = cat.load_file(cfg.catalog_path)
    store = Store(cfg.data_dir, cfg.base_release, keep=2)
    promoted, builds = [], []

    async def fetch(tracked):
        return latest

    def builder(path, specs, versions, **kw):
        builds.append(path.name)
        rel = make_release(path, specs, fingerprint=kw["fingerprint"], rid=path.name)
        return rel, {"passed": passed}

    async def on_promote(rel):
        promoted.append(rel.id)

    return specs, store, Updater(cfg, store, on_promote, fetch=fetch, builder=builder), promoted, builds


def test_promotes_new_versions_then_up_to_date(cfg):
    latest = {"pypi": {}, "hf": {"org/model": "abc"}}
    specs, store, up, promoted, builds = setup(cfg, latest)
    r1 = asyncio.run(up.check())
    assert r1["result"] == "promoted" and promoted == [r1["release"]]
    assert store.active().id == r1["release"] and store.history() == [str(cfg.base_release)]
    r2 = asyncio.run(up.check())
    assert r2["result"] == "up_to_date" and len(builds) == 1


def test_failed_selftest_is_rejected_and_not_retried(cfg):
    latest = {"pypi": {}, "hf": {"org/model": "bad"}}
    _, store, up, promoted, builds = setup(cfg, latest, passed=False)
    assert asyncio.run(up.check())["result"] == "selftest_failed"
    assert asyncio.run(up.check())["result"] == "candidate_previously_rejected"
    assert promoted == [] and len(builds) == 1 and store.active().id == "base"
    assert not list(store.dir.iterdir())  # failed candidate removed


def test_returns_to_base_when_base_matches(cfg):
    latest = {"pypi": {}, "hf": {}}
    specs, store, up, promoted, builds = setup(cfg, latest)
    # base release fingerprint matches what upstream now offers
    import json
    meta = json.loads((cfg.base_release / "release.json").read_text())
    meta["fingerprint"] = cat.fingerprint(specs, latest)
    (cfg.base_release / "release.json").write_text(json.dumps(meta))
    store.promote(make_release(store.dir / "rold", specs, fingerprint="old", rid="rold"))
    r = asyncio.run(up.check())
    assert r["result"] == "promoted" and r["release"] == "base" and builds == []


def test_rollback(cfg):
    store = Store(cfg.data_dir, cfg.base_release, keep=3)
    store.promote(make_release(store.dir / "r1", SPECS, fingerprint="f1", rid="r1"))
    store.promote(make_release(store.dir / "r2", SPECS, fingerprint="f2", rid="r2"))
    assert store.rollback().id == "r1"
    assert store.is_rejected("f2")


def test_catalog_validation():
    import pytest
    with pytest.raises(cat.CatalogError):
        cat.validate({"detectors": [{"id": "a", "kind": "watermark", "adapter": "evil"}]})
    with pytest.raises(cat.CatalogError):
        cat.validate({"detectors": [{"id": "a", "kind": "synthetic_speech", "adapter": "hf_audio_classifier"}]})
    t = cat.tracked([{"package": "audioseal", "weights_repo": "facebook/audioseal"}, {"model": "o/m"}])
    assert t == {"pypi": ["audioseal"], "hf": ["facebook/audioseal", "o/m"]}
