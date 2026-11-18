"use strict";

// Call review console. Call variables and plugin output come from outside
// callers, so everything is rendered with text nodes - never innerHTML.

const app = document.getElementById("app");

// ---------- DOM helpers ----------

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k === "style") Object.assign(el.style, v);
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? "" : String(v));
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

function clear(el) { el.replaceChildren(); return el; }

// ---------- formatting ----------

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
const dtf = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "medium" });

function ago(iso) {
  const s = (new Date(iso) - Date.now()) / 1000;
  const units = [["year", 31536000], ["month", 2592000], ["day", 86400], ["hour", 3600], ["minute", 60]];
  for (const [u, n] of units) if (Math.abs(s) >= n) return rtf.format(Math.round(s / n), u);
  return "just now";
}

function clock(sec) {
  if (sec == null || !isFinite(sec)) return "–";
  sec = Math.max(0, Math.round(sec));
  const hh = Math.floor(sec / 3600), mm = Math.floor((sec % 3600) / 60), ss = sec % 60;
  const p = (n) => String(n).padStart(2, "0");
  return hh ? `${hh}:${p(mm)}:${p(ss)}` : `${mm}:${p(ss)}`;
}

function bytes(n) {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB"];
  const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  return `${(n / 1024 ** i).toFixed(i ? 1 : 0)} ${u[i]}`;
}

const pct = (x) => `${Math.round((x || 0) * 100)}%`;
const shortId = (id) => id.slice(0, 8);

function callSeconds(c) {
  const av = aivoice(c);
  const ch = av && !av.error && av.channels;
  if (ch && ch.length) return Math.max(...ch.map((x) => x.seconds_received || 0));
  const st = c.results && c.results.audiostats;
  if (st && typeof st.duration_seconds === "number") return st.duration_seconds;
  return (new Date(c.ended_at) - new Date(c.started_at)) / 1000;
}

// ---------- vocabulary ----------

const VERDICT = {
  watermark_detected: "AI watermark",
  likely_synthetic: "Likely AI voice",
  no_evidence: "No evidence",
  insufficient_audio: "Too little speech",
  error: "Analysis failed",
  none: "Not analyzed",
};

const DETECTOR_NAMES = { audioseal: "AudioSeal", perth: "Perth", "wav2vec2-deepfake": "wav2vec2 classifier" };
const API_NAMES = { resemble: "Resemble Detect", reality_defender: "Reality Defender" };
const detName = (id) => DETECTOR_NAMES[id] || id;

function aivoice(c) { return c.results && c.results.aivoice; }

function verdictOf(c) {
  const av = aivoice(c);
  if (!av) return "none";
  if (av.error) return "error";
  return av.verdict || "none";
}

function badge(v) { return h("span", { class: `badge v-${v}` }, VERDICT[v] || v); }

const WHO_KEYS = ["caller", "from", "ani", "caller_id", "phone", "number", "customer", "name"];

function who(vars) {
  vars = vars || {};
  for (const k of WHO_KEYS) {
    const v = vars[k];
    if (typeof v === "string" || typeof v === "number") return { primary: String(v), key: k };
  }
  const FORMAT_KEYS = new Set(["encoding", "sample_rate", "channels", "channel_labels"]);
  const first = Object.entries(vars).find(([k, v]) => !FORMAT_KEYS.has(k) && typeof v === "string" && v.length < 60);
  return first ? { primary: first[1], key: first[0] } : null;
}

function channelName(ch) { return ch.label || `Channel ${ch.channel + 1}`; }

// Segments of evidence per channel: [{start,end,kind,score,detector}]
function evidence(ch) {
  // Classifier segments first so watermark segments (stronger evidence) draw on top.
  const out = [];
  for (const [id, s] of Object.entries(ch.synthetic_speech || {}))
    for (const g of s.segments || []) out.push({ ...g, kind: "syn", detector: id });
  for (const [id, s] of Object.entries(ch.watermarks || {}))
    for (const g of s.segments || []) out.push({ ...g, kind: "wm", detector: id });
  return out;
}

function segEl(g, total, cls) {
  const left = Math.min(100, (g.start / total) * 100);
  const width = Math.max(0, Math.min(100 - left, ((g.end - g.start) / total) * 100));
  return h("span", {
    class: `seg ${g.kind} ${cls || ""}`,
    style: { left: `${left}%`, width: `${width}%` },
    title: `${detName(g.detector)}: ${clock(g.start)}–${clock(g.end)}, score ${pct(g.max_score)}`,
  });
}

// ---------- API ----------

class HttpError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

async function api(path) {
  const r = await fetch(path, { headers: { Accept: "application/json" } });
  let body = null;
  try { body = await r.json(); } catch { /* not JSON */ }
  if (!r.ok) throw new HttpError(r.status, (body && body.error) || `Request failed (${r.status})`);
  return body;
}

// ---------- list page ----------

const listState = { q: "", verdict: "", calls: [], next: null, stats: null, timer: null, paged: false };

const FILTERS = [
  ["", "All calls"],
  ["flagged", "Flagged"],
  ["watermark_detected", "AI watermark"],
  ["likely_synthetic", "Likely AI voice"],
  ["no_evidence", "No evidence"],
  ["errors", "Plugin errors"],
];

function listQuery(before) {
  const p = new URLSearchParams({ limit: "50" });
  if (listState.q) p.set("q", listState.q);
  if (listState.verdict) p.set("verdict", listState.verdict);
  if (before) p.set("before", before);
  return `/api/calls?${p}`;
}

async function loadList({ append: more = false } = {}) {
  const [page, stats] = await Promise.all([
    api(listQuery(more ? listState.next : null)),
    more ? Promise.resolve(listState.stats) : api("/api/stats"),
  ]);
  listState.calls = more ? listState.calls.concat(page.calls || []) : page.calls || [];
  listState.next = page.next;
  listState.stats = stats;
  listState.paged = more || listState.paged;
}

function setFilter(v) {
  listState.verdict = v;
  listState.paged = false;
  refreshList();
}

async function refreshList() {
  try {
    await loadList();
    renderListBody();
  } catch (e) {
    showError(e);
  }
}

function renderList() {
  clear(app);
  document.title = "Call review";
  const summary = h("div", { id: "summary" });
  let debounce;
  const search = h("input", {
    class: "search", type: "search", placeholder: "Search by caller, variable or call ID",
    "aria-label": "Search calls", value: listState.q,
    oninput: (e) => {
      clearTimeout(debounce);
      debounce = setTimeout(() => { listState.q = e.target.value.trim(); listState.paged = false; refreshList(); }, 250);
    },
  });
  const filters = h("div", { class: "filters", role: "group", "aria-label": "Show" },
    FILTERS.map(([v, label]) =>
      h("button", { type: "button", "aria-pressed": String(listState.verdict === v), "data-v": v, onclick: () => setFilter(v) }, label)));
  append(app, [summary, h("div", { class: "controls" }, search, filters), h("div", { id: "rows" })]);
  refreshList();

  clearInterval(listState.timer);
  listState.timer = setInterval(() => {
    // Keep the first page live; leave the list alone once the user loads more.
    if (location.hash.startsWith("#/calls/") || listState.paged || document.hidden) return;
    refreshList();
  }, 15000);
}

function renderSummary() {
  const s = listState.stats;
  const el = document.getElementById("summary");
  if (!el || !s) return;
  const calls = s.total === 1 ? "call" : "calls";
  const sub = [`${s.last_24h} in the last 24 hours.`];
  if (s.errors) sub.push(` ${s.errors} with a plugin error.`);
  append(clear(el), [
    h("h1", { class: "summary" },
      s.flagged
        ? [h("strong", {}, `${s.flagged} of ${s.total} ${calls}`), " show signs of an AI voice."]
        : s.total
          ? [h("strong", {}, `${s.total} ${calls}`), " stored, none flagged for an AI voice."]
          : "No calls stored yet."),
    h("p", { class: "summary-sub" }, sub,
      s.flagged && listState.verdict !== "flagged"
        ? [" ", h("button", { type: "button", onclick: () => setFilter("flagged") }, "Show flagged calls")]
        : null),
  ]);
  for (const b of document.querySelectorAll(".filters button"))
    b.setAttribute("aria-pressed", String(b.dataset.v === listState.verdict));
}

function renderListBody() {
  renderSummary();
  const rows = document.getElementById("rows");
  if (!rows) return;
  clear(rows);
  if (!listState.calls.length) {
    const filtered = listState.q || listState.verdict;
    rows.append(h("p", { class: "empty" }, filtered
      ? "No calls match these filters. Clear the search or pick All calls."
      : "No calls yet. Stream a call to /ws/call and it appears here when it ends."));
    return;
  }
  const tbody = h("tbody", {}, listState.calls.map(callRow));
  rows.append(h("table", { class: "calls" },
    h("thead", {}, h("tr", {},
      h("th", { scope: "col" }, "Verdict"), h("th", { scope: "col" }, "Call"),
      h("th", { scope: "col" }, "Where evidence was found"), h("th", { scope: "col", class: "num" }, "Length"),
      h("th", { scope: "col" }, "Started"))),
    tbody));
  if (listState.next) {
    rows.append(h("p", { class: "more" }, h("button", {
      type: "button",
      onclick: async (e) => {
        e.target.disabled = true;
        try { await loadList({ append: true }); renderListBody(); } catch (err) { showError(err); }
      },
    }, "Load older calls")));
  }
}

function callRow(c) {
  const v = verdictOf(c);
  const w = who(c.variables);
  const href = `#/calls/${c.id}`;
  const secs = callSeconds(c);
  return h("tr", { onclick: (e) => { if (!e.target.closest("a")) location.hash = href; } },
    h("td", {}, badge(v)),
    h("td", { class: "who" },
      h("a", { href }, w ? w.primary : shortId(c.id)),
      h("span", { class: "sub" }, w ? h("span", { class: "mono" }, shortId(c.id)) : `${Object.keys(c.variables || {}).length} variables`)),
    h("td", { class: "strip-cell" }, miniStrip(c, secs)),
    h("td", { class: "num" }, clock(secs)),
    h("td", { class: "when", title: dtf.format(new Date(c.started_at)) }, ago(c.started_at)));
}

function miniStrip(c, total) {
  const av = aivoice(c);
  if (!av || av.error || !av.channels) {
    return h("div", { class: "strip", "aria-label": "Not analyzed for AI voice" },
      h("div", { class: "lane unanalyzed" }));
  }
  const lanes = av.channels.map((ch) => {
    const segs = evidence(ch);
    return h("div", { class: "lane", title: channelName(ch) }, segs.map((g) => segEl(g, total || 1)));
  });
  const found = av.channels.flatMap(evidence);
  const label = found.length
    ? `Evidence in ${found.length} segment${found.length > 1 ? "s" : ""}`
    : "No evidence segments";
  return h("div", { class: "strip", role: "img", "aria-label": label }, lanes);
}

// ---------- detail page ----------

async function renderCall(id) {
  clear(app).append(h("p", { class: "loading" }, "Loading call…"));
  let c;
  try {
    c = await api(`/api/calls/${encodeURIComponent(id)}`);
  } catch (e) {
    clear(app).append(backLink(), h("h1", { class: "headline" },
      e.status === 404 ? "This call is gone" : "Couldn't load this call"),
      h("p", { class: "lede" }, e.status === 404
        ? "It doesn't exist or passed its retention period and was deleted."
        : e.message));
    return;
  }
  const w = who(c.variables);
  document.title = `${w ? w.primary : shortId(c.id)} – Call review`;
  const v = verdictOf(c);
  const av = aivoice(c);
  const secs = callSeconds(c);

  clear(app).append(
    backLink(),
    h("h1", { class: `headline v-${v}` }, headline(v, av)),
    h("p", { class: "lede" }, lede(c, w, secs)),
    riskMeter(av),
    playerBlock(c, av, secs),
    av ? aivoiceBlock(av) : null,
    detailsBlock(c, secs),
    otherPlugins(c),
  );
  window.scrollTo(0, 0);
  app.focus({ preventScroll: true });
}

function backLink() { return h("a", { class: "back", href: "#/" }, "All calls"); }

function headline(v, av) {
  if (v === "none") return "This call wasn't checked for AI voices";
  if (v === "error") return "AI-voice analysis failed for this call";
  const chs = av.channels || [];
  const named = (pred) => chs.filter(pred).map(channelName);
  const list = (xs) => xs.length <= 1 ? xs.join("") : `${xs.slice(0, -1).join(", ")} and ${xs.at(-1)}`;
  if (v === "watermark_detected") {
    const dets = new Set();
    for (const ch of chs) for (const [id, s] of Object.entries(ch.watermarks || {})) if (s.detected) dets.add(detName(id));
    const where = named((ch) => ch.verdict === "watermark_detected");
    return `${list([...dets])} watermark found${chs.length > 1 ? ` on ${list(where)}` : ""}`;
  }
  if (v === "likely_synthetic") {
    const where = named((ch) => ch.verdict === "likely_synthetic");
    return `The voice${chs.length > 1 ? ` on ${list(where)}` : ""} sounds machine-generated`;
  }
  if (v === "no_evidence") return "No sign of an AI voice";
  return "Too little speech to check";
}

function lede(c, w, secs) {
  let text = `${w ? `${w.primary}, ` : ""}${clock(secs)} call, started ${dtf.format(new Date(c.started_at))}.`;
  const av = aivoice(c);
  if (av && av.verdict === "watermark_detected" && av.channels && av.channels.length > 1) {
    const syn = av.channels.filter((ch) => ch.verdict === "likely_synthetic").map(channelName);
    if (syn.length) text += ` The voice on ${syn.join(" and ")} also sounds machine-generated.`;
  }
  return text;
}

function riskMeter(av) {
  if (!av || av.error || av.risk_score == null) return null;
  const r = av.risk_score;
  const color = av.verdict === "watermark_detected" ? "var(--wm)" : av.verdict === "likely_synthetic" ? "var(--syn)" : "var(--clear)";
  const fill = h("span", { class: "risk-fill", style: { width: "0%", background: color } });
  requestAnimationFrame(() => { fill.style.width = pct(r); });
  return h("div", { class: "risk" },
    h("div", { class: "risk-bar", role: "meter", "aria-valuemin": "0", "aria-valuemax": "100", "aria-valuenow": String(Math.round(r * 100)), "aria-label": "Risk score" }, fill),
    h("span", { class: "risk-label" }, `Risk score ${pct(r)}`));
}

function playerBlock(c, av, total) {
  const audio = h("audio", { controls: true, preload: "metadata", src: `/api/calls/${c.id}/audio` });
  const note = h("p", { class: "note", hidden: true });
  audio.addEventListener("error", async () => {
    audio.hidden = true;
    note.hidden = false;
    try {
      await api(`/api/calls/${c.id}/audio`);
      note.textContent = "The audio can't be played.";
    } catch (e) {
      note.textContent = e.status === 422
        ? `The audio can't be played: ${e.message.replace(/^cannot play this call: /, "")}. Send "encoding" and "sample_rate" in the call's first message.`
        : e.status === 404 ? "The audio file is no longer available." : e.message;
    }
  });

  const block = h("div", { class: "player" }, audio, note);
  if (!av || av.error || !av.channels) {
    block.append(h("p", { class: "note" }, av && av.error
      ? "The AI-voice plugin returned an error, so there's no timeline."
      : "No AI-voice timeline: this call wasn't analyzed."));
    return block;
  }

  const tl = h("div", { class: "timeline" });
  const tracks = [];
  for (const ch of av.channels) {
    if (av.channels.length > 1) tl.append(h("div", { class: "ch-head" }, channelName(ch)));
    const lanes = [
      ...Object.entries(ch.watermarks || {}).map(([id, s]) => [id, s, "wm"]),
      ...Object.entries(ch.synthetic_speech || {}).map(([id, s]) => [id, s, "syn"]),
    ];
    for (const [id, s, kind] of lanes) {
      const track = h("div", { class: "track", title: `Click to play from here` },
        (s.segments || []).map((g) => segEl({ ...g, kind, detector: id }, total || 1)));
      track.addEventListener("click", (e) => {
        const r = track.getBoundingClientRect();
        if (!isFinite(audio.duration)) return;
        audio.currentTime = ((e.clientX - r.left) / r.width) * audio.duration;
        audio.play().catch(() => {});
      });
      tracks.push(track);
      tl.append(h("div", { class: "lane-name", title: detName(id) }, detName(id)), track);
    }
  }
  tl.append(h("div", { class: "lane-name" }), h("div", { class: "axis", "aria-hidden": "true" },
    axisTicks(total).map((t) => h("span", { style: { left: `${(t / total) * 100}%` } }, clock(t)))));

  const head = h("div", { class: "playhead" });
  tl.append(head);
  audio.addEventListener("timeupdate", () => {
    if (!tracks.length || !isFinite(audio.duration) || !audio.duration) return;
    const first = tracks[0].getBoundingClientRect();
    const box = tl.getBoundingClientRect();
    head.style.display = "block";
    head.style.left = `${first.left - box.left + (audio.currentTime / audio.duration) * first.width}px`;
  });

  block.append(tl, h("div", { class: "legend" },
    h("span", {}, h("i", { style: { background: "var(--wm)" } }), "Watermark found"),
    h("span", {}, h("i", { style: { background: "var(--syn)" } }), "Sounds machine-generated"),
    h("span", {}, h("i", { style: { background: "var(--track)" } }), "Nothing found")));
  return block;
}

function axisTicks(total) {
  const steps = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600];
  const step = steps.find((s) => total / s <= 6) || 3600;
  const out = [];
  for (let t = 0; t <= total + 1e-6; t += step) out.push(t);
  return out;
}

function scoreCell(x, kind, positive) {
  if (x == null) return h("span", { class: "muted" }, "–");
  return h("span", { class: "score" },
    h("span", { class: "score-bar" }, h("b", { class: positive ? kind : "", style: { width: pct(x) } })),
    pct(x));
}

function aivoiceBlock(av) {
  const sec = h("section", { class: "block" }, h("h2", {}, "What the AI-voice detectors found"));
  if (av.error) {
    sec.append(h("div", { class: "errorbox" }, av.error));
    return sec;
  }
  sec.append(h("p", { class: "intro" },
    "Watermark detectors look for marks that AI voice generators embed in their audio. Classifiers judge whether the voice itself sounds machine-generated."));

  for (const ch of av.channels || []) {
    const box = h("div", { class: "channel" },
      h("h3", {}, channelName(ch), badge(ch.verdict)),
      h("p", { class: "facts" },
        `${clock(ch.speech_seconds_analyzed)} of speech analyzed out of ${clock(ch.seconds_received)}. `,
        ch.windows && ch.windows.skipped_backlog ? `${ch.windows.skipped_backlog} windows skipped because analysis fell behind. ` : "",
        ch.windows && ch.windows.silent ? `${ch.windows.silent} silent windows ignored.` : ""));

    const rows = [];
    for (const [id, s] of Object.entries(ch.watermarks || {})) {
      rows.push(h("tr", {},
        h("td", {}, detName(id)),
        h("td", { class: "hide-sm muted" }, "Watermark"),
        h("td", {}, s.detected
          ? h("span", { class: "found wm" }, "Found", s.message ? [" ", h("span", { class: "mono", title: "Embedded watermark payload" }, s.message)] : null)
          : h("span", { class: "muted" }, "Not found")),
        h("td", { class: "num" }, scoreCell(s.max_score, "wm", s.detected)),
        h("td", { class: "num hide-sm" }, `${s.windows_positive || 0} of ${s.windows_analyzed || 0}`)));
    }
    for (const [id, s] of Object.entries(ch.synthetic_speech || {})) {
      rows.push(h("tr", {},
        h("td", {}, detName(id)),
        h("td", { class: "hide-sm muted" }, "Voice classifier"),
        h("td", {}, s.flagged
          ? h("span", { class: "found syn" }, "Sounds machine-generated")
          : h("span", { class: "muted" }, s.windows_analyzed ? "Sounds human" : "Not run")),
        h("td", { class: "num" }, scoreCell(s.mean_score, "syn", s.flagged)),
        h("td", { class: "num hide-sm" }, s.windows_analyzed ? `${Math.round((s.flagged_fraction || 0) * s.windows_analyzed)} of ${s.windows_analyzed}` : "–")));
    }
    for (const [name, r] of Object.entries(ch.apis || {})) {
      const fake = r.label ? String(r.label).toLowerCase() === "fake" : String(r.status || "").toUpperCase() === "MANIPULATED";
      rows.push(h("tr", {},
        h("td", {}, API_NAMES[name] || name),
        h("td", { class: "hide-sm muted" }, "External service"),
        h("td", {}, r.error ? h("span", { class: "muted" }, `Failed: ${r.error}`)
          : h("span", { class: fake ? "found syn" : "muted" }, r.label || r.status || "–",
            r.watermark && r.watermark.status && r.watermark.status !== "absent" ? ` (watermark: ${r.watermark.status})` : "")),
        h("td", { class: "num" }, scoreCell(r.score, "syn", fake)),
        h("td", { class: "num hide-sm" }, "–")));
    }
    box.append(h("table", { class: "det" },
      h("thead", {}, h("tr", {},
        h("th", { scope: "col" }, "Detector"), h("th", { scope: "col", class: "hide-sm" }, "Type"),
        h("th", { scope: "col" }, "Result"), h("th", { scope: "col", class: "num" }, "Score"),
        h("th", { scope: "col", class: "num hide-sm" }, "Positive windows"))),
      h("tbody", {}, rows)));
    if (ch.errors) box.append(h("div", { class: "errorbox" }, Object.entries(ch.errors).map(([k, e]) => h("div", {}, `${detName(k)}: ${e}`))));
    sec.append(box);
  }

  if (av.caveats && av.caveats.length) sec.append(h("ul", { class: "caveats" }, av.caveats.map((c) => h("li", {}, c))));
  const d = av.detectors || {};
  sec.append(h("details", { class: "raw" }, h("summary", {}, `Detector versions (release ${d.id || "unknown"})`),
    h("dl", { class: "kv", style: { marginTop: "12px" } },
      (d.detectors || []).flatMap((x) => [h("dt", {}, detName(x.id)), h("dd", {}, h("span", { class: "mono" }, x.version), ` threshold ${x.threshold}`)]))),
    rawJSON(av, "Full AI-voice report"));
  return sec;
}

function detailsBlock(c, secs) {
  const vars = Object.entries(c.variables || {});
  return h("section", { class: "block" }, h("h2", {}, "Call details"),
    h("div", { class: "cols", style: { marginTop: "16px" } },
      h("dl", { class: "kv" },
        h("dt", {}, "Call ID"), h("dd", { class: "mono" }, c.id),
        h("dt", {}, "Started"), h("dd", {}, dtf.format(new Date(c.started_at))),
        h("dt", {}, "Ended"), h("dd", {}, dtf.format(new Date(c.ended_at))),
        h("dt", {}, "Length"), h("dd", {}, clock(secs)),
        h("dt", {}, "Audio stored"), h("dd", {}, bytes(c.audio_bytes)),
        h("dt", {}, "Deleted on"), h("dd", {}, `${dtf.format(new Date(c.expires_at))} (${ago(c.expires_at)})`)),
      vars.length
        ? h("dl", { class: "kv" }, vars.flatMap(([k, v]) => [h("dt", {}, k), h("dd", {}, valueView(v, 1))]))
        : h("p", { class: "muted" }, "No variables were sent with this call.")));
}

function otherPlugins(c) {
  const others = Object.entries(c.results || {}).filter(([k]) => k !== "aivoice");
  if (!others.length) return null;
  return h("section", { class: "block" }, h("h2", {}, "Other analysis"),
    others.map(([name, r]) => h("div", { class: "plugin" }, h("h3", {}, name),
      r && r.error ? h("div", { class: "errorbox" }, `This plugin failed: ${r.error}`) : valueView(r, 0),
      rawJSON(r, "Raw output"))));
}

// Generic renderer for free-form plugin JSON.
function valueView(v, depth) {
  if (v === null) return h("span", { class: "muted" }, "null");
  if (typeof v !== "object") return String(v);
  if (depth > 3) return h("span", { class: "mono" }, JSON.stringify(v));
  if (Array.isArray(v)) {
    if (!v.length) return h("span", { class: "muted" }, "none");
    if (v.every((x) => x === null || typeof x !== "object")) return v.map(String).join(", ");
    const keys = [...new Set(v.flatMap((x) => (x && typeof x === "object" && !Array.isArray(x) ? Object.keys(x) : [])))];
    if (keys.length && keys.length <= 8 && v.every((x) => x && typeof x === "object" && !Array.isArray(x))) {
      return h("table", { class: "det" },
        h("thead", {}, h("tr", {}, keys.map((k) => h("th", { scope: "col" }, k)))),
        h("tbody", {}, v.map((row) => h("tr", {}, keys.map((k) => h("td", {}, k in row ? valueView(row[k], depth + 1) : ""))))));
    }
    return h("span", { class: "mono" }, JSON.stringify(v));
  }
  const entries = Object.entries(v);
  if (!entries.length) return h("span", { class: "muted" }, "empty");
  return h("dl", { class: "kv" }, entries.flatMap(([k, x]) => [h("dt", {}, k), h("dd", {}, valueView(x, depth + 1))]));
}

function rawJSON(v, label) {
  return h("details", { class: "raw" }, h("summary", {}, label), h("pre", {}, JSON.stringify(v, null, 2)));
}

// ---------- shell ----------

function showError(e) {
  const msg = e.status === 401 ? "Sign in to view calls." : `Couldn't load calls: ${e.message}`;
  const rows = document.getElementById("rows") || app;
  clear(rows).append(h("div", { class: "errorbox" }, msg));
}

function route() {
  const m = location.hash.match(/^#\/calls\/([0-9a-fA-F-]{36})$/);
  if (m) {
    clearInterval(listState.timer);
    renderCall(m[1]);
  } else {
    renderList();
  }
}

window.addEventListener("hashchange", route);
route();
