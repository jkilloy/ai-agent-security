"""Write test WAV files: a speech-like signal, clean or watermarked.

    docker compose run --rm -v "$PWD:/out" detectors \
        python -m detsvc.tools.make_sample --mark audioseal --rate 8000 --out /out/audioseal_8k.wav

--mark: none | audioseal | perth.  The file is 16-bit PCM mono at --rate
(downsampled through G.711 mu-law when --rate 8000 and --telephony is set).
"""

from __future__ import annotations

import argparse
import os

import numpy as np

from ..audio import MULAW, TARGET_RATE, mulaw_encode, resample, to_wav_bytes
from ..testsignal import speechlike


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--mark", choices=["none", "audioseal", "perth"], default="none")
    ap.add_argument("--seconds", type=float, default=12.0)
    ap.add_argument("--rate", type=int, default=8000)
    ap.add_argument("--telephony", action="store_true", help="pass through G.711 mu-law (8 kHz only)")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    os.environ.setdefault("TORCHDYNAMO_DISABLE", "1")
    os.environ.setdefault("AUDIOSEAL_CACHE_DIR", "/opt/release0/cache")
    x = speechlike(args.seconds, seed=11)
    if args.mark == "audioseal":
        import torch
        from audioseal import AudioSeal

        gen = AudioSeal.load_generator("audioseal_wm_16bits")
        t = torch.from_numpy(x)[None, None]
        with torch.inference_mode():
            x = (t + gen.get_watermark(t, TARGET_RATE))[0, 0].numpy()
    elif args.mark == "perth":
        import perth

        x = perth.PerthImplicitWatermarker().apply_watermark(x, sample_rate=TARGET_RATE).astype(np.float32)

    y = resample(x, TARGET_RATE, args.rate)
    if args.telephony:
        if args.rate != 8000:
            raise SystemExit("--telephony requires --rate 8000")
        y = MULAW[mulaw_encode(y)]
    with open(args.out, "wb") as f:
        f.write(to_wav_bytes(y, args.rate))
    print(f"wrote {args.out}: {args.mark}, {args.seconds}s at {args.rate} Hz")


if __name__ == "__main__":
    main()
