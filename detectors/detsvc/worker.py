"""Detector worker: a subprocess that loads one release's detectors and serves
analysis requests over stdin/stdout.

Protocol (one request at a time):
    request:  one JSON line, then `bytes` bytes of payload
              {"op": "analyze", "bytes": N, "detectors": ["id", ...]}  payload = float32 LE mono 16 kHz
              {"op": "info"} | {"op": "selftest"}
    response: one JSON line

The first line written after start is {"type": "ready", ...} or
{"type": "fatal", "error": ...}.

Run as:  <release python> -m detsvc.worker --release DIR [--threads N]
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import traceback
import warnings
from pathlib import Path

import numpy as np


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--release", required=True)
    ap.add_argument("--threads", type=int, default=2)
    args = ap.parse_args()

    # Keep stdout clean for the protocol: anything libraries print goes to stderr.
    out = os.fdopen(os.dup(1), "w", buffering=1)
    os.dup2(2, 1)
    sys.stdout = sys.stderr
    warnings.filterwarnings("ignore")
    inp = sys.stdin.buffer

    def send(obj: dict) -> None:
        out.write(json.dumps(obj) + "\n")
        out.flush()

    try:
        import torch

        torch.set_num_threads(max(1, args.threads))
        from . import detectors as det
        from .releases import load

        release = load(Path(args.release))
        dets = {s["id"]: det.build(s, release) for s in release.catalog}
    except Exception as e:  # noqa: BLE001
        traceback.print_exc()
        send({"type": "fatal", "error": f"{type(e).__name__}: {e}"})
        sys.exit(1)

    # Warm up: the first inference after loading is much slower (lazy init),
    # so pay that cost before accepting real calls.
    from .testsignal import speechlike

    warm = speechlike(2.0)
    for d in dets.values():
        try:
            d.analyze(warm)
        except Exception:  # noqa: BLE001 - surfaced on real requests instead
            traceback.print_exc()

    info = {"release": release.id, "detectors": [d.info() for d in dets.values()]}
    send({"type": "ready", **info})

    while True:
        line = inp.readline()
        if not line:
            return
        try:
            req = json.loads(line)
            payload = inp.read(int(req.get("bytes", 0)))
            op = req.get("op")
            if op == "analyze":
                x = np.frombuffer(payload, dtype="<f4")
                results, errors = {}, {}
                for did in req.get("detectors") or list(dets):
                    d = dets.get(did)
                    if d is None:
                        errors[did] = "unknown detector"
                        continue
                    try:
                        results[did] = d.analyze(x)
                    except Exception as e:  # noqa: BLE001
                        errors[did] = f"{type(e).__name__}: {e}"
                send({"type": "result", "results": results, "errors": errors})
            elif op == "info":
                send({"type": "result", **info})
            elif op == "selftest":
                report, passed = {}, True
                for d in dets.values():
                    try:
                        r = d.selftest()
                    except Exception as e:  # noqa: BLE001
                        traceback.print_exc()
                        r = {"passed": False, "error": f"{type(e).__name__}: {e}"}
                    report[d.id] = r
                    passed = passed and bool(r.get("passed"))
                send({"type": "result", "passed": passed, "detectors": report})
            else:
                send({"type": "error", "error": f"unknown op {op!r}"})
        except Exception as e:  # noqa: BLE001
            traceback.print_exc()
            send({"type": "error", "error": f"{type(e).__name__}: {e}"})


if __name__ == "__main__":
    main()
