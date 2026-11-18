from detsvc.report import Window, api_signals, call_report, channel_report, segments, synthetic_summary, watermark_summary

SPECS = [{"id": "wm", "kind": "watermark", "threshold": 0.5},
         {"id": "syn", "kind": "synthetic_speech", "threshold": 0.7}]
STATS = {"speech_seconds_analyzed": 8.0}


def test_segments_merge_adjacent_positive_windows():
    w = [Window(0, 4, 0.9), Window(4, 8, 0.8), Window(8, 12, 0.1), Window(12, 16, 0.6)]
    assert segments(w, 0.5) == [{"start": 0, "end": 8, "max_score": 0.9}, {"start": 12, "end": 16, "max_score": 0.6}]


def test_watermark_summary_majority_message():
    w = [Window(0, 4, 0.99, {"message": "0xb2e5"}), Window(4, 8, 0.98, {"message": "0xb2e5"}),
         Window(8, 12, 0.97, {"message": "0x0001"}), Window(12, 16, 0.1)]
    s = watermark_summary(w, 0.5)
    assert s["detected"] and s["message"] == "0xb2e5" and s["windows_positive"] == 3 and s["detected_seconds"] == 12


def test_synthetic_summary_uses_mean():
    s = synthetic_summary([Window(0, 4, 0.9), Window(4, 8, 0.4)], 0.7)
    assert not s["flagged"] and s["mean_score"] == 0.65 and s["flagged_fraction"] == 0.5


def test_verdict_precedence_and_risk():
    wm = channel_report(0, "caller", SPECS, {"wm": [Window(0, 4, 0.95)], "syn": [Window(0, 4, 0.9)]}, STATS, {}, {})
    assert wm["verdict"] == "watermark_detected" and wm["risk_score"] == 0.95 and wm["label"] == "caller"
    syn = channel_report(1, None, SPECS, {"wm": [Window(0, 4, 0.1)], "syn": [Window(0, 4, 0.8)]}, STATS, {}, {})
    assert syn["verdict"] == "likely_synthetic" and syn["risk_score"] == 0.8
    clean = channel_report(0, None, SPECS, {"wm": [Window(0, 4, 0.1)], "syn": [Window(0, 4, 0.2)]}, STATS, {}, {})
    assert clean["verdict"] == "no_evidence"
    quiet = channel_report(0, None, SPECS, {}, {"speech_seconds_analyzed": 0.0}, {}, {})
    assert quiet["verdict"] == "insufficient_audio"
    rep = call_report([clean, syn], {"sample_rate": 8000}, {"telephony_robust": {"wm": True}}, [])
    assert rep["verdict"] == "likely_synthetic" and rep["risk_score"] == 0.8
    assert any("narrowband" in c for c in rep["caveats"])


def test_api_signals():
    assert api_signals({"resemble": {"label": "fake", "score": 0.91}}) == (True, 0.91)
    assert api_signals({"resemble": {"label": "real", "score": 0.1, "watermark": {"status": "absent"}}}) == (False, 0.0)
    assert api_signals({"resemble": {"label": "real", "watermark": {"status": "present"}}})[0]
    assert api_signals({"reality_defender": {"status": "MANIPULATED", "score": 0.8}}) == (True, 0.8)
