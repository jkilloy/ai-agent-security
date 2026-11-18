"""Turns per-window detector scores into the per-call report."""

from __future__ import annotations

from collections import Counter
from dataclasses import dataclass, field

VERDICT_RANK = {"insufficient_audio": 0, "no_evidence": 1, "likely_synthetic": 2, "watermark_detected": 3}
MIN_ANALYZED_SECONDS = 2.0


@dataclass
class Window:
    start: float
    end: float
    score: float
    extra: dict = field(default_factory=dict)


def _r(x: float | None) -> float | None:
    return None if x is None else round(float(x), 4)


def segments(windows: list[Window], threshold: float) -> list[dict]:
    """Merge consecutive positive windows into time segments."""
    out: list[dict] = []
    for w in sorted(windows, key=lambda w: w.start):
        if w.score < threshold:
            continue
        if out and w.start - out[-1]["end"] <= 0.01:
            out[-1]["end"] = round(w.end, 2)
            out[-1]["max_score"] = max(out[-1]["max_score"], _r(w.score))
        else:
            out.append({"start": round(w.start, 2), "end": round(w.end, 2), "max_score": _r(w.score)})
    return out


def watermark_summary(windows: list[Window], threshold: float) -> dict:
    pos = [w for w in windows if w.score >= threshold]
    out = {
        "detected": bool(pos),
        "max_score": _r(max((w.score for w in windows), default=0.0)),
        "windows_analyzed": len(windows),
        "windows_positive": len(pos),
        "detected_seconds": round(sum(w.end - w.start for w in pos), 2),
        "segments": segments(windows, threshold),
    }
    msgs = Counter(w.extra["message"] for w in pos if w.extra.get("message"))
    if msgs:
        out["message"] = msgs.most_common(1)[0][0]
    return out


def synthetic_summary(windows: list[Window], threshold: float) -> dict:
    scores = [w.score for w in windows]
    mean = sum(scores) / len(scores) if scores else None
    return {
        "flagged": mean is not None and mean >= threshold,
        "mean_score": _r(mean),
        "max_score": _r(max(scores, default=None) if scores else None),
        "windows_analyzed": len(scores),
        "flagged_fraction": _r(sum(s >= threshold for s in scores) / len(scores)) if scores else None,
        "segments": segments(windows, threshold),
    }


def api_signals(apis: dict) -> tuple[bool, float]:
    """(flagged, score) from commercial API results."""
    flagged, score = False, 0.0
    r = apis.get("resemble") or {}
    if r.get("label"):
        if str(r["label"]).lower() == "fake":
            flagged = True
            score = max(score, r.get("score") or 0.0)
        if (r.get("watermark") or {}).get("status") not in (None, "absent"):
            flagged = True
    rd = apis.get("reality_defender") or {}
    if str(rd.get("status", "")).upper() in ("MANIPULATED", "FAKE"):
        flagged = True
        score = max(score, float(rd.get("score") or 0.0))
    return flagged, score


def channel_report(ch_index: int, label: str | None, specs: list[dict], scores: dict[str, list[Window]],
                   stats: dict, apis: dict, errors: dict) -> dict:
    watermarks, synthetic = {}, {}
    risk = 0.0
    for spec in specs:
        wins = scores.get(spec["id"], [])
        thr = float(spec.get("threshold", 0.5))
        if spec["kind"] == "watermark":
            s = watermark_summary(wins, thr)
            watermarks[spec["id"]] = s
            if s["detected"]:
                risk = max(risk, s["max_score"])
        else:
            s = synthetic_summary(wins, thr)
            synthetic[spec["id"]] = s
            if s["mean_score"] is not None:
                risk = max(risk, s["mean_score"])
    api_flag, api_score = api_signals(apis)
    risk = max(risk, api_score)

    if any(s["detected"] for s in watermarks.values()):
        verdict = "watermark_detected"
    elif any(s["flagged"] for s in synthetic.values()) or api_flag:
        verdict = "likely_synthetic"
    elif stats["speech_seconds_analyzed"] < MIN_ANALYZED_SECONDS and not apis:
        verdict = "insufficient_audio"
    else:
        verdict = "no_evidence"

    out = {
        "channel": ch_index,
        "verdict": verdict,
        "risk_score": _r(risk),
        **stats,
        "watermarks": watermarks,
        "synthetic_speech": synthetic,
    }
    if label:
        out["label"] = label
    if apis:
        out["apis"] = apis
    if errors:
        out["errors"] = errors
    return out


def call_report(channels: list[dict], audio: dict, release: dict, notes: list[str]) -> dict:
    verdict = max((c["verdict"] for c in channels), key=VERDICT_RANK.__getitem__, default="insufficient_audio")
    risk = max((c["risk_score"] or 0.0 for c in channels), default=0.0)
    caveats = list(notes)
    if audio["sample_rate"] < 16000:
        robust = release.get("telephony_robust") or {}
        survive = sorted(k for k, v in robust.items() if v)
        lost = sorted(k for k, v in robust.items() if not v)
        msg = f"Audio is {audio['sample_rate']} Hz narrowband; it is upsampled to 16 kHz for analysis."
        if survive:
            msg += f" Self-tests show these watermarks survive an 8 kHz phone path: {', '.join(survive)}."
        if lost:
            msg += f" These did not, so they may miss watermarks on this call: {', '.join(lost)}."
        caveats.append(msg)
    if verdict in ("no_evidence", "insufficient_audio"):
        caveats.append("No watermark does not mean a human voice: most voice-cloning tools do not watermark, "
                       "and watermarks can be removed.")
    if any(c.get("synthetic_speech") for c in channels):
        caveats.append("Synthetic-speech classifier scores are probabilistic; validate thresholds on your own call audio.")
    return {
        "verdict": verdict,
        "risk_score": _r(risk),
        "channels": channels,
        "audio": audio,
        "detectors": release,
        "caveats": caveats,
    }
