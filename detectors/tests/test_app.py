import numpy as np
from fastapi.testclient import TestClient

from detsvc.app import create_app


def test_stream_protocol(cfg):
    with TestClient(create_app(cfg)) as client:
        assert client.get("/healthz").json() == {"ready": True}
        with client.websocket_connect("/v1/stream") as ws:
            ws.send_json({"call_id": "c1", "encoding": "mulaw", "sample_rate": 8000})
            assert ws.receive_json() == {"type": "ready", "release": "base"}
            ws.send_bytes(bytes([0x80]) * 8000 * 3)  # loud mu-law samples
            ws.send_json({"type": "end"})
            msg = ws.receive_json()
        assert msg["type"] == "report"
        rep = msg["report"]
        assert rep["verdict"] == "watermark_detected"
        assert rep["channels"][0]["watermarks"]["wm"]["windows_analyzed"] == 3
        assert rep["detectors"]["id"] == "base"

        with client.websocket_connect("/v1/stream") as ws:
            ws.send_json({"encoding": "opus", "sample_rate": 8000})
            assert ws.receive_json()["type"] == "error"

        st = client.get("/v1/status").json()
        assert st["ready"] and st["release"]["id"] == "base" and st["apis_enabled"] == []
