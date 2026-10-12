"use strict";
const $ = (s) => document.querySelector(s);
const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; };
const esc = (s) => s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
const reEsc = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
const HANDLE = /<([A-Z][A-Z_]*)_\d+>/g;
const KNOWN = new Set(["PERSON", "EMAIL_ADDRESS", "PHONE_NUMBER", "SECRET", "ORGANIZATION"]);

const S = { sid: null, rows: [], mapVer: null, reqs: [], reqSig: "", sel: null, pinned: false, auditSig: "", busy: false, realRe: null, byReal: new Map() };

// ---- chips
const SHORT = { EMAIL_ADDRESS: "EMAIL", PHONE_NUMBER: "PHONE", ORGANIZATION: "ORG" };
function typeAttrs(type) {
  if (KNOWN.has(type)) return `class="chip t-${type}"`;
  let h = 0; for (const c of type) h = (h * 31 + c.charCodeAt(0)) % 360;
  return `class="chip" style="--h:${h}"`;
}
function chipFor(type, handle, text) { return `<span ${typeAttrs(type)} data-handle="${esc(handle)}">${esc(text)}</span>`; }

function rebuildRealRe() {
  S.byReal = new Map(S.rows.map((r) => [r.real.toLowerCase(), r]));
  const parts = S.rows.map((r) => r.real).filter((r) => r.length > 1).sort((a, b) => b.length - a.length).map(reEsc);
  S.realRe = parts.length ? new RegExp(parts.join("|"), "giu") : null;
}
// agent view: real values -> chip of their handle's type
function hlReal(text) {
  if (!S.realRe) return esc(text);
  let out = "", last = 0;
  for (const m of text.matchAll(S.realRe)) {
    const r = S.byReal.get(m[0].toLowerCase());
    out += esc(text.slice(last, m.index));
    out += r ? chipFor(r.type, r.handle, m[0]) : esc(m[0]);
    last = m.index + m[0].length;
  }
  return out + esc(text.slice(last));
}
// provider view: handles -> chips; any real value still present -> leak marker
function hlProvider(text) {
  const reals = S.realRe ? S.realRe.source + "|" : "";
  const re = new RegExp(reals + HANDLE.source, "giu");
  let out = "", last = 0;
  for (const m of text.matchAll(re)) {
    out += esc(text.slice(last, m.index));
    const hm = /^<([A-Z][A-Z_]*)_\d+>$/.exec(m[0]);
    out += hm ? chipFor(hm[1], m[0], m[0]) : `<span class="leakv">${esc(m[0])}</span>`;
    last = m.index + m[0].length;
  }
  return out + esc(text.slice(last));
}

// ---- chat
const chat = $("#chat");
function setRaw(node, raw, mono) { node._raw = raw; node.innerHTML = hlReal(raw); }
// minimal markdown on already-escaped HTML: **bold**, `code`
function md(html) { return html.replace(/\*\*([^*\n]+)\*\*/g, "<b>$1</b>").replace(/`([^`\n]+)`/g, "<code>$1</code>"); }
function paint(n) { n.innerHTML = n._md ? md(hlReal(n._raw)) : hlReal(n._raw); }
function rerenderChat() { chat.querySelectorAll("[data-raw-el]").forEach(paint); }
function addNode(n) {
  const e = chat.querySelector(".empty"); if (e) e.remove();
  const near = chat.scrollHeight - chat.scrollTop - chat.clientHeight < 60;
  chat.appendChild(n);
  if (near) chat.scrollTop = chat.scrollHeight;
}
function rawNode(tag, cls, raw) { const n = el(tag, cls); n.dataset.rawEl = "1"; n._raw = raw; n.innerHTML = hlReal(raw); return n; }
const callNodes = {};
function renderEvent(ev) {
  if (ev.type === "user" || ev.type === "assistant") {
    const n = rawNode("div", "msg " + ev.type, ev.text);
    if (ev.type === "assistant") { n._md = true; paint(n); }
    addNode(n);
  } else if (ev.type === "error") {
    addNode(el("div", "msg error", ev.text));
  } else if (ev.type === "tool_call" || ev.type === "tool_result") {
    const call = ev.type === "tool_call";
    const box = el("div", "tool " + (call ? "call" : "result"));
    const head = el("div", "head");
    head.append(el("span", "caret"), el("span", "name", call ? ev.name : "result"));
    const sum = el("span", "sum");
    const raw = call ? JSON.stringify(ev.args, null, 1) : ev.text;
    sum.dataset.rawEl = "1"; sum._raw = call ? Object.values(ev.args).map((v) => String(v).slice(0, 60)).join(" ") : ev.text.split("\n")[0].slice(0, 80);
    sum.innerHTML = hlReal(sum._raw);
    head.append(sum);
    head.addEventListener("click", () => box.classList.toggle("open"));
    box.append(head, rawNode("pre", "body mono", raw));
    addNode(box);
  }
}
function clearChat() { chat.replaceChildren(el("div", "empty", "No messages")); }

async function send(text) {
  S.busy = true; $("#send").disabled = true; $("#msg").disabled = true;
  const typing = el("div", "typing", "working"); addNode(typing);
  const poll = setInterval(refresh, 1000);
  try {
    const r = await fetch("/api/chat", { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ session: S.sid, message: text }) });
    const rd = r.body.getReader(), dec = new TextDecoder(); let buf = "";
    for (;;) {
      const { value, done } = await rd.read(); if (done) break;
      buf += dec.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf("\n\n")) >= 0) {
        const frame = buf.slice(0, i); buf = buf.slice(i + 2);
        if (frame.startsWith("data:")) { typing.remove(); const ev = JSON.parse(frame.slice(5)); if (ev.type !== "done") { renderEvent(ev); addNode(typing); } }
      }
    }
  } catch (e) { renderEvent({ type: "error", text: String(e) }); }
  clearInterval(poll); typing.remove();
  await refresh();
  S.busy = false; $("#send").disabled = false; $("#msg").disabled = false; $("#msg").focus();
  chat.dataset.done = String((+chat.dataset.done || 0) + 1);
}

// ---- mapping
function renderMapping() {
  const tb = $("#map tbody"); tb.replaceChildren();
  for (const r of S.rows) {
    const tr = el("tr"); tr.dataset.handle = r.handle;
    const a = el("td", "r"); a.textContent = r.real; a.title = r.real;
    const b = el("td", "h"); b.innerHTML = chipFor(r.type, r.handle, r.handle).replace(" data-handle", " data-nohl");
    const c = el("td", "t", SHORT[r.type] || r.type); c.title = r.type;
    tr.append(a, b, c);
    tr.addEventListener("mouseenter", () => hover(r.handle, tr, true));
    tr.addEventListener("mouseleave", () => hover(r.handle, tr, false));
    tb.append(tr);
  }
  $("#map-count").textContent = S.rows.length;
  $("#map-empty").hidden = S.rows.length > 0;
  $("#map").hidden = S.rows.length === 0;
}
function hover(handle, tr, on) {
  tr.classList.toggle("hover", on);
  document.querySelectorAll("#chat [data-handle], #payload [data-handle]").forEach((n) => { if (n.dataset.handle === handle) n.classList.toggle("hl", on); });
}

// ---- audit
const SKIP = new Set(["ts", "event", "session", "prev", "hash", "call_id", "principal"]);
function keyCounts(ev) {
  const out = [];
  for (const [k, v] of Object.entries(ev)) {
    if (SKIP.has(k) || v == null || typeof v === "boolean") continue;
    if (Array.isArray(v)) { if (v.length) out.push(`${k} ${v.length}`); }
    else if (typeof v === "object") { if (Object.keys(v).length) out.push(`${k} ` + Object.entries(v).map(([a, b]) => `${a}:${b}`).join(",")); }
    else if (typeof v === "number") out.push(`${k} ${v}`);
    else if (String(v).length <= 24) out.push(`${k}=${v}`);
  }
  return out.join("  ");
}
const hms = (d) => d.toLocaleTimeString([], { hour12: false });
function renderAudit(evs) {
  const box = $("#audit"); box.replaceChildren();
  if (!evs.length) { box.append(el("div", "empty", "No events")); return; }
  for (const ev of evs) {
    const row = el("div", "ev"); row.dataset.event = ev.event;
    const kc = el("span", "kc", keyCounts(ev)); kc.title = kc.textContent;
    row.append(el("span", "ts", hms(new Date(ev.ts))), el("span", "nm", ev.event), kc);
    box.append(row);
  }
}

// ---- provider
function strings(v, out) {
  if (typeof v === "string") {
    out.push(v);
    const t = v.trim();
    if (t[0] === "{" || t[0] === "[") { try { strings(JSON.parse(t), out); } catch (_) { /* not json */ } }
  } else if (Array.isArray(v)) v.forEach((x) => strings(x, out));
  else if (v && typeof v === "object") Object.values(v).forEach((x) => strings(x, out));
}
// whole-token, case-insensitive, Unicode-aware match (mirror of litellm/pii_leak.py _boundary_re):
// "Ivan" does not hit in "Ivanov", "contact" not in "contacts"; e-mails/phones are whole tokens.
function boundaryRe(real) {
  const body = real.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  if (real.includes("@")) return new RegExp("(?<![\\p{L}\\p{N}_@.+-])" + body + "(?![\\p{L}\\p{N}_@-]|\\.[\\p{L}\\p{N}_])", "iu");
  if (/^[+(\d]/.test(real)) return new RegExp("(?<![\\p{L}\\p{N}_+])" + body + "(?![\\p{L}\\p{N}_])", "iu");
  return new RegExp("(?<![\\p{L}\\p{N}_])" + body + "(?![\\p{L}\\p{N}_])", "iu");
}
function leaks() {
  const found = new Set();
  const reals = S.rows.map((r) => r.real.trim()).filter((r) => r.length > 1);
  for (const q of S.reqs) {
    const acc = []; strings(q.body, acc);
    const hay = acc.join("\n");
    const low = hay.toLowerCase();
    for (const r of reals) if (low.includes(r.toLowerCase()) && boundaryRe(r).test(hay)) found.add(r.toLowerCase());
  }
  return found.size;
}
function renderLeak() {
  const n = leaks(), b = $("#leak");
  b.textContent = "leaks " + n; b.className = "badge " + (n ? "bad" : "ok"); b.dataset.count = n;
}
function renderReqs() {
  const box = $("#reqs"); box.replaceChildren();
  $("#req-count").textContent = S.reqs.length;
  if (!S.reqs.length) { box.append(el("div", "empty", "No requests")); $("#payload").replaceChildren(); return; }
  S.reqs.forEach((q, i) => {
    const row = el("div", "req" + (q.n === S.sel ? " sel" : "")); row.dataset.n = q.n;
    row.append(el("span", "", hms(new Date(q.ts * 1000))), el("span", "p", q.path), el("span", "", q.status ?? "-"), el("span", "r", fmtSize(q.size)));
    row.addEventListener("click", () => { S.sel = q.n; S.pinned = i !== S.reqs.length - 1; renderReqs(); renderPayload(); });
    box.append(row);
  });
}
const fmtSize = (n) => n > 1024 ? (n / 1024).toFixed(1) + "k" : n + "b";
function renderPayload() {
  const q = S.reqs.find((x) => x.n === S.sel), box = $("#payload");
  if (!q) { box.replaceChildren(); return; }
  box.innerHTML = hlProvider(JSON.stringify(q.body, null, 2).replace(/\\n/g, "\n"));
  box.dataset.n = q.n;
}

// ---- data refresh
let refreshing = false;
async function refresh() {
  if (refreshing || !S.sid) return; refreshing = true;
  try {
    const [m, a, u] = await Promise.all(["mapping", "audit", "upstream"].map((k) => fetch(`/api/${k}?session=${S.sid}`).then((r) => r.json())));
    let mapChanged = false;
    if (m.version !== S.mapVer) { S.mapVer = m.version; S.rows = m.rows; rebuildRealRe(); renderMapping(); rerenderChat(); mapChanged = true; }
    const asig = a.events.map((e) => e.hash || e.ts).join();
    if (asig !== S.auditSig) { S.auditSig = asig; renderAudit(a.events); }
    const rsig = u.requests.map((r) => r.n).join();
    S.reqs = u.requests;
    if (rsig !== S.reqSig) {
      S.reqSig = rsig;
      if (!S.pinned || !S.reqs.some((r) => r.n === S.sel)) S.sel = S.reqs.length ? S.reqs[S.reqs.length - 1].n : null;
      renderReqs(); renderPayload();
    } else if (mapChanged) renderPayload();
    renderLeak();
  } catch (e) { console.warn(e); }
  refreshing = false;
}

// ---- session
function reset(sid) {
  S.sid = sid; S.rows = []; S.mapVer = null; S.reqs = []; S.reqSig = ""; S.sel = null; S.pinned = false; S.auditSig = "x"; rebuildRealRe();
  $("#sid").textContent = sid; $("#sid").dataset.sid = sid;
  clearChat(); renderMapping(); renderAudit([]); renderReqs(); renderLeak();
  try { localStorage.setItem("demo-sid", sid); } catch (_) { /* ignore */ }
}
async function newSession() {
  const s = await fetch("/api/session", { method: "POST" }).then((r) => r.json());
  reset(s.id);
}
async function init() {
  const cfg = await fetch("/api/config").then((r) => r.json());
  $("#mode").textContent = cfg.mode + " / " + cfg.model;
  let sid = null; try { sid = localStorage.getItem("demo-sid"); } catch (_) { /* ignore */ }
  const st = sid ? await fetch("/api/session?session=" + sid).then((r) => r.json()) : { exists: false };
  if (st.exists) { reset(sid); st.events.forEach(renderEvent); await refresh(); } else await newSession();
  setInterval(() => { if (!S.busy) refresh(); }, 3000);
}
$("#new").addEventListener("click", newSession);
$("#form").addEventListener("submit", (e) => {
  e.preventDefault();
  const t = $("#msg").value.trim(); if (!t || S.busy) return;
  $("#msg").value = ""; send(t);
});
init();
