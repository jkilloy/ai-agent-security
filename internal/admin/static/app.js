"use strict";
// Admin view of stored calls. Generic: it shows the call object as stored,
// whatever plugins produced. Call data comes from outside callers, so it is
// only ever inserted as text (never innerHTML).

const app = document.getElementById("app");

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false) el.setAttribute(k, v === true ? "" : v);
  }
  for (const k of kids.flat(Infinity)) if (k !== null && k !== undefined) el.append(k);
  return el;
}

// API URLs are built from location.origin, which never contains credentials;
// fetch refuses URLs resolved against a page URL like http://user:pw@host/.
const url = (path) => location.origin + path;

async function api(path) {
  const r = await fetch(url(path), { headers: { Accept: "application/json" } });
  const body = await r.json().catch(() => null);
  if (!r.ok) throw new Error((body && body.error) || `HTTP ${r.status}`);
  return body;
}

const json = (v) => JSON.stringify(v, null, 2);
const time = (s) => new Date(s).toLocaleString();
function duration(c) {
  const s = Math.max(0, (new Date(c.ended_at) - new Date(c.started_at)) / 1000);
  return s < 60 ? `${s.toFixed(1)} s` : `${Math.floor(s / 60)} m ${Math.round(s % 60)} s`;
}
const hasError = (r) => r !== null && typeof r === "object" && !Array.isArray(r) && "error" in r;

// ---- list ---------------------------------------------------------------

const state = { q: "", errors: false };

async function showList() {
  const q = h("input", { type: "search", placeholder: "Search call ID or variables", value: state.q });
  const errors = h("input", { type: "checkbox", checked: state.errors });
  const rows = h("tbody");
  const more = h("button", { type: "button", hidden: true }, "Load more");
  const status = h("p", { class: "muted" });
  let next = null;

  async function load(reset) {
    const p = new URLSearchParams({ limit: "50" });
    if (state.q) p.set("q", state.q);
    if (state.errors) p.set("errors", "1");
    if (!reset && next) p.set("before", next);
    try {
      const page = await api(`/api/calls?${p}`);
      if (reset) rows.replaceChildren();
      for (const c of page.calls) rows.append(row(c));
      next = page.next;
      more.hidden = !next;
      status.textContent = rows.children.length ? "" : "No calls.";
    } catch (e) {
      status.textContent = `Could not load calls: ${e.message}`;
    }
  }

  q.addEventListener("change", () => { state.q = q.value.trim(); load(true); });
  errors.addEventListener("change", () => { state.errors = errors.checked; load(true); });
  more.addEventListener("click", () => load(false));

  app.replaceChildren(
    h("div", { class: "controls" },
      q,
      h("label", {}, errors, " Only calls with plugin errors"),
      h("button", { type: "button", onclick: () => load(true) }, "Refresh")),
    h("div", { class: "wrap" },
      h("table", { class: "calls" },
        h("thead", {}, h("tr", {},
          ["Started", "Connected", "Call ID", "Audio bytes", "Variables", "Results"].map((t) => h("th", {}, t)))),
        rows)),
    status, more);
  await load(true);
}

function row(c) {
  const results = Object.entries(c.results || {}).map(([name, r], i) =>
    [i ? ", " : "", hasError(r) ? h("span", { class: "err" }, `${name} (error)`) : name]);
  return h("tr", {},
    h("td", {}, time(c.started_at)),
    h("td", {}, duration(c)),
    h("td", { class: "mono" }, h("a", { href: `#/calls/${c.id}` }, c.id)),
    h("td", {}, String(c.audio_bytes)),
    h("td", { class: "vars mono", title: JSON.stringify(c.variables) }, JSON.stringify(c.variables)),
    h("td", {}, results));
}

// ---- call ---------------------------------------------------------------

async function showCall(id) {
  app.replaceChildren(h("p", {}, "Loading…"));
  let c;
  try {
    c = await api(`/api/calls/${id}`);
  } catch (e) {
    app.replaceChildren(h("p", { class: "err" }, `Could not load call ${id}: ${e.message}`), h("a", { href: "#/" }, "Back to calls"));
    return;
  }
  const audioURL = url(`/api/calls/${c.id}/audio`);
  const recording = h("div", {}, h("p", { class: "muted" }, "Checking recording…"));

  const attrs = [
    ["id", c.id],
    ["started_at", `${c.started_at}  (${time(c.started_at)})`],
    ["ended_at", `${c.ended_at}  (${time(c.ended_at)})`],
    ["connected", `${duration(c)}  (ended_at − started_at)`],
    ["expires_at", `${c.expires_at}  (${time(c.expires_at)})`],
    ["audio_bytes", String(c.audio_bytes)],
  ];

  const results = Object.entries(c.results || {}).sort(([a], [b]) => a.localeCompare(b));

  app.replaceChildren(
    h("p", {}, h("a", { href: "#/" }, "← Calls")),
    h("h1", { class: "mono" }, `Call ${c.id}`),
    h("table", {}, h("tbody", {}, attrs.map(([k, v]) => h("tr", {}, h("th", { class: "mono" }, k), h("td", { class: "mono" }, v))))),
    h("h2", {}, "Recording"),
    recording,
    h("h2", {}, "variables"),
    h("pre", {}, json(c.variables)),
    h("h2", {}, `results (${results.length} plugin${results.length === 1 ? "" : "s"})`),
    h("div", {}, results.length ? results.map(([name, r]) => [
      h("h3", { class: "mono" }, name, hasError(r) ? h("span", { class: "err" }, "  error") : null),
      h("pre", {}, json(r)),
    ]) : h("p", { class: "muted" }, "No plugin results.")),
    h("details", {}, h("summary", {}, "Full call object (JSON)"), h("pre", {}, json(c))));

  // The audio endpoint explains why a call can't be played (e.g. no encoding
  // variable); check before showing a player that would just fail silently.
  try {
    const r = await fetch(audioURL, { headers: { Range: "bytes=0-0" } });
    if (!r.ok) {
      const body = await r.json().catch(() => null);
      recording.replaceChildren(h("p", { class: "muted" }, (body && body.error) || `Not available (HTTP ${r.status})`));
      return;
    }
    recording.replaceChildren(
      h("audio", { controls: true, preload: "metadata", src: audioURL }),
      h("p", {}, h("a", { href: audioURL, download: `${c.id}.wav` }, "Download WAV")));
  } catch (e) {
    recording.replaceChildren(h("p", { class: "err" }, `Could not check recording: ${e.message}`));
  }
}

// ---- routing -------------------------------------------------------------

function route() {
  const m = location.hash.match(/^#\/calls\/([0-9a-f-]{36})$/i);
  window.scrollTo(0, 0);
  if (m) showCall(m[1]);
  else showList();
}
window.addEventListener("hashchange", route);
route();
