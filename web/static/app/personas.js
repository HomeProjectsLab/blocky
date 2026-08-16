// personas.js — the household personas dashboard (plan Phases 0-2, MVP).
// One warm-snapshot GET (/api/ui/personas) feeds a client-side fact table; every
// chart is a group-by/reduce over it, so group-by / cross-filter / hour-brush are
// all zero-fetch (Pi3-light). Only the range control and the live SSE tick touch
// anything beyond the one payload. The whole surface is gated on
// privacy.profiling.enable — off => the endpoint returns {enabled:false} and we
// render the locked state. Ported from the personas mockup, wired to live data.
import { getJSON, send, onQuery } from "./api.js";
import { fmtNum } from "./format.js";
import { confirmDialog, toast } from "./modal.js";

/* ============ state ============ */
const S = {
    scope: "person",       // person | class | identity | none
    filters: {},           // {class, person, identity, hour:[a,b]}
    range: "24h",
    active: new Map(),     // client name -> lastSeen ms (SSE)
};
let DATA = null;           // last payload
let CLIENTS = [];          // fact table
let personIndex = new Map();

const RANGES = { "7d": 7 * 86400, "30d": 30 * 86400, "60d": 60 * 86400 };

/* ============ tiny helpers ============ */
const el = (t, c, html) => { const e = document.createElement(t); if (c) e.className = c; if (html != null) e.innerHTML = html; return e; };
const sum = (a) => a.reduce((x, y) => x + y, 0);
const pct = (a, b) => (b ? Math.round((a / b) * 100) : 0);
const argmax = (a) => a.indexOf(Math.max(...a));
const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

const personColors = ["--c-resolved", "--c-cached", "--c-blocked", "--c-other", "--c-error", "--accent"];
function pColor(name) {
    const i = personIndex.has(name) ? personIndex.get(name) : -1;
    return `var(${personColors[(i < 0 ? 5 : i) % personColors.length]})`;
}
const CLASS_COLORS = {
    iot: "--c-resolved", iotdev: "--c-resolved", server: "--c-blocked",
    workstation: "--c-cached", wsfp: "--c-cached", unknown: "--muted",
};
function classColor(cl) {
    if (CLASS_COLORS[cl]) return `var(${CLASS_COLORS[cl]})`;
    let h = 0; for (let i = 0; i < (cl || "").length; i++) h = (h * 31 + cl.charCodeAt(i)) | 0;
    return `hsl(${((h % 360) + 360) % 360} 55% 52%)`;
}
const IDENTITY_COLORS = { recognized: "var(--c-cached)", unknown: "var(--muted)", shared: "var(--c-other)" };

// identity bucket derived from ClientRow flags (plan chart 20): shared / has-facet / neither.
const recognized = (c) => !c.shared && ((c.os && c.os !== "" && c.os !== "—") || (c.vendor && c.vendor.length));
const identityBucket = (c) => (c.shared ? "shared" : recognized(c) ? "recognized" : "unknown");

// current hour in the payload's tz (for the "now" presence marker).
function tzHour(tz) {
    try { return +new Intl.DateTimeFormat("en-GB", { hour: "2-digit", hour12: false, timeZone: tz }).format(new Date()) % 24; }
    catch { return new Date().getHours(); }
}
let NOW_HOUR = new Date().getHours();

/* ============ derive ============ */
function applyFilters(rows) {
    const f = S.filters;
    return rows.filter((c) => {
        if (f.class && c.class !== f.class) return false;
        if (f.person && (c.person || "") !== f.person) return false;
        if (f.identity && identityBucket(c) !== f.identity) return false;
        return true;
    });
}
function hourSum(hist) {
    const f = S.filters.hour;
    if (!f) return sum(hist);
    let s = 0; const [a, b] = f;
    for (let h = 0; h < 24; h++) if (a <= b ? h >= a && h <= b : h >= a || h <= b) s += hist[h];
    return s;
}
const hourQ = (c) => (S.filters.hour ? hourSum(c.hourLocal) : Number(c.queries));
function groupKey(c) {
    if (S.scope === "person") return c.person || "Unassigned";
    if (S.scope === "class") return c.class || "unknown";
    if (S.scope === "identity") return identityBucket(c);
    return "All clients";
}
const filterDim = () => ({ person: "person", class: "class", identity: "identity", none: "person" }[S.scope]);
const scopeWord = () => ({ person: "person", class: "class", identity: "identity", none: "fleet" }[S.scope]);
function derive() {
    const rows = applyFilters(CLIENTS);
    const groups = new Map();
    rows.forEach((c) => { const k = groupKey(c); (groups.get(k) || groups.set(k, []).get(k)).push(c); });
    return { rows, groups };
}
function groupColor(k) { return S.scope === "class" ? classColor(k) : S.scope === "identity" ? (IDENTITY_COLORS[k] || "var(--accent)") : pColor(k); }

/* ============ cross-filter ============ */
function toggleFilter(dim, val) {
    if (S.filters[dim] === val) delete S.filters[dim]; else S.filters[dim] = val;
    render();
}
function clearFilter(dim) { delete S.filters[dim]; render(); }

/* ============ tooltip ============ */
const tip = document.getElementById("pd-tip");
function moveTip(e) {
    let x = e.clientX + 14, y = e.clientY + 14; const r = tip.getBoundingClientRect();
    if (x + r.width > innerWidth) x = e.clientX - r.width - 10;
    if (y + r.height > innerHeight) y = e.clientY - r.height - 10;
    tip.style.left = x + "px"; tip.style.top = y + "px";
}
function tipify(node, html) {
    node.addEventListener("pointerenter", (e) => { tip.innerHTML = html; tip.style.opacity = 1; moveTip(e); });
    node.addEventListener("pointermove", moveTip);
    node.addEventListener("pointerleave", () => { tip.style.opacity = 0; });
}

/* ============ primitives ============ */
function barList(mount, items, { color, onClick, dim } = {}) {
    mount.innerHTML = ""; const max = Math.max(1, ...items.map((i) => i.val));
    const wrap = el("div", "pd-barlist");
    items.forEach((it) => {
        const row = el("div", "pd-bar");
        if (dim && dim(it)) row.classList.add("dim");
        const c = typeof color === "function" ? color(it) : color || "var(--accent)";
        row.innerHTML = `<span class="lbl">${esc(it.label)}</span><div class="track"><div class="fill"></div></div><span class="val">${it.disp != null ? it.disp : fmtNum(it.val)}</span>`;
        const fill = row.querySelector(".fill"); fill.style.background = c;
        requestAnimationFrame(() => { fill.style.width = ((it.val / max) * 100).toFixed(1) + "%"; });
        if (onClick) row.onclick = () => onClick(it);
        if (it.tip) tipify(row, it.tip);
        wrap.appendChild(row);
    });
    mount.appendChild(wrap);
}
function mixRows(mount, rows) {
    mount.innerHTML = ""; const wrap = el("div", "pd-mix");
    rows.forEach((r) => {
        const row = el("div", "pd-mixrow");
        row.innerHTML = `<span class="lbl" title="${esc(r.label)}">${esc(r.label)}</span><div class="pd-mixtrack"></div>${r.tail ? `<span class="tail">${r.tail}</span>` : ""}`;
        const track = row.querySelector(".pd-mixtrack");
        const total = sum(r.segs.map((s) => s.v)) || 1;
        r.segs.forEach((s) => {
            const seg = el("div", "pd-seg"); seg.style.background = s.c;
            if (S.filters[s.dim] && S.filters[s.dim] !== s.val) seg.classList.add("dim");
            requestAnimationFrame(() => { seg.style.width = ((s.v / total) * 100).toFixed(2) + "%"; });
            if (s.dim && s.val != null) seg.onclick = () => toggleFilter(s.dim, s.val);
            tipify(seg, `<b>${esc(s.name)}</b> · ${fmtNum(s.v)}`);
            track.appendChild(seg);
        });
        wrap.appendChild(row);
    });
    mount.appendChild(wrap);
}
function presenceStrip(hist, { micro, ramp, showNow } = {}) {
    const strip = el("div", "pstrip" + (micro ? " micro" : ""));
    const max = Math.max(1, ...hist); const col = ramp || "var(--c-resolved)";
    const hb = S.filters.hour;
    for (let h = 0; h < 24; h++) {
        const cell = el("div", "cell");
        cell.style.background = `color-mix(in srgb, ${col} ${Math.round((hist[h] / max) * 100)}%, transparent)`;
        if (showNow && h === NOW_HOUR) cell.classList.add("now");
        if (hb) { const [a, b] = hb; if (a <= b ? h >= a && h <= b : h >= a || h <= b) cell.classList.add("hb"); }
        tipify(cell, `<b>${String(h).padStart(2, "0")}:00</b> · ${hist[h]} queries`);
        cell.style.cursor = "pointer"; cell.onclick = () => brushHour(h);
        strip.appendChild(cell);
    }
    return strip;
}
let brushStart = null;
function brushHour(h) {
    if (brushStart == null) { brushStart = h; S.filters.hour = [h, h]; }
    else { S.filters.hour = [Math.min(brushStart, h), Math.max(brushStart, h)]; brushStart = null; }
    render();
}

/* ============ renderers ============ */
function renderKPIs() {
    const box = document.getElementById("pd-kpis");
    const rows = applyFilters(CLIENTS);
    const people = new Set(rows.filter((c) => c.person).map((c) => c.person));
    const q = sum(rows.map(hourQ)), b = sum(rows.map((c) => Number(c.blocked)));
    const rec = rows.filter(recognized).length, shr = rows.filter((c) => c.shared).length;
    const defs = [
        { k: "People", v: people.size, d: (DATA.people || []).length + " mapped", dim: null },
        { k: "Devices", v: rows.length, d: "of " + CLIENTS.length + " total", dim: null },
        { k: "Recognized", v: pct(rec, rows.length) + "%", d: shr + " shared/NAT", dim: "identity", val: "recognized" },
        { k: "Queries", v: fmtNum(q), d: S.range + " window", dim: null },
        { k: "Blocked", v: pct(b, q) + "%", d: fmtNum(b) + " queries", dim: null },
        { k: "Active now", v: S.active.size, d: "last 5 min", dim: null, live: true },
    ];
    box.innerHTML = "";
    defs.forEach((x) => {
        const t = el("div", "tile" + (x.live ? " live" : "") + (x.dim ? "" : " static"));
        t.innerHTML = `<span class="tile-label">${x.k}</span><span class="tile-value">${x.v}</span><span class="tile-sub">${x.d}</span>`;
        if (x.dim) t.onclick = () => toggleFilter(x.dim, x.val);
        box.appendChild(t);
    });
}

function renderDonut(mount) {
    const d = derive();
    const groups = [...d.groups.entries()].map(([k, rows]) => ({ k, q: sum(rows.map(hourQ)) })).filter((g) => g.q > 0).sort((a, b) => b.q - a.q);
    const total = sum(groups.map((g) => g.q)) || 1;
    const R = 64, cx = 80, cy = 80, C = 2 * Math.PI * R; let off = 0;
    const dim = filterDim();
    const arcs = groups.map((g) => {
        const frac = g.q / total;
        const faded = S.filters[dim] && S.filters[dim] !== g.k ? ' opacity="0.28"' : "";
        const seg = `<circle cx="${cx}" cy="${cy}" r="${R}" fill="none" stroke="${groupColor(g.k)}" stroke-width="20" stroke-dasharray="${(frac * C).toFixed(1)} ${C}" stroke-dashoffset="${(-off * C).toFixed(1)}" transform="rotate(-90 ${cx} ${cy})" data-k="${esc(g.k)}"${faded}></circle>`;
        off += frac; return seg;
    });
    mount.innerHTML = `<div class="pd-donut">
        <svg viewBox="0 0 160 160" width="160" height="160" role="img" aria-label="query share by ${scopeWord()}">
            ${arcs.join("")}
            <text x="${cx}" y="${cy - 4}" text-anchor="middle" font-size="22" font-weight="700" fill="var(--ink)">${fmtNum(total)}</text>
            <text x="${cx}" y="${cy + 13}" text-anchor="middle" font-size="10" fill="var(--muted)">queries · ${groups.length} ${scopeWord()}s</text>
        </svg>
        <div class="pd-barlist" style="flex:1;min-width:150px" id="pd-donutleg"></div></div>`;
    const leg = mount.querySelector("#pd-donutleg");
    groups.forEach((g) => {
        const row = el("div", "pd-bar"); row.style.gridTemplateColumns = "14px 1fr auto";
        row.innerHTML = `<span class="swatch" style="background:${groupColor(g.k)}"></span><span class="lbl">${esc(g.k)}</span><span class="val">${pct(g.q, total)}%</span>`;
        row.onclick = () => toggleFilter(dim, g.k);
        leg.appendChild(row);
    });
    mount.querySelectorAll("circle[data-k]").forEach((c) => {
        const g = groups.find((x) => x.k === c.dataset.k);
        c.onclick = () => toggleFilter(dim, c.dataset.k);
        tipify(c, `<b>${esc(g.k)}</b> · ${fmtNum(g.q)} queries · ${pct(g.q, total)}%`);
    });
}

function renderClassDist(mount) {
    const d = derive(); const by = {};
    d.rows.forEach((c) => { const cl = c.class || "unknown"; by[cl] = (by[cl] || 0) + 1; });
    const segs = Object.entries(by).sort((a, b) => b[1] - a[1]).map(([k, v]) => ({ name: k, v, c: classColor(k), dim: "class", val: k }));
    if (!segs.length) { mount.innerHTML = '<div class="pd-empty-note">classes appear after ~20 queries per device</div>'; return; }
    mixRows(mount, [{ label: "Fleet", segs, tail: d.rows.length + " dev" }]);
    const leg = el("div", "pd-legend");
    segs.forEach((s) => { leg.innerHTML += `<span><i style="background:${s.c}"></i>${esc(s.name)} ${s.v}</span>`; });
    mount.appendChild(leg);
}

function renderHeatmap(mount) {
    const d = derive();
    const groups = [...d.groups.entries()].map(([k, rows]) => {
        const h = Array(24).fill(0); rows.forEach((c) => c.hourLocal.forEach((v, i) => { h[i] += v; }));
        return { k, h, tot: sum(h) };
    }).filter((g) => g.tot > 0).sort((a, b) => b.tot - a.tot);
    mount.innerHTML = "";
    if (!groups.length) { mount.innerHTML = '<div class="pd-empty-note">no presence recorded yet</div>'; return; }
    const heat = el("div", "pd-heat");
    const gmax = Math.max(1, ...groups.map((g) => Math.max(...g.h)));
    const dim = filterDim();
    groups.forEach((g) => {
        const row = el("div", "pd-heatrow");
        if (S.filters[dim] && S.filters[dim] !== g.k) row.classList.add("dim");
        row.innerHTML = `<span class="rl">${esc(g.k)}</span>`;
        const strip = el("div", "pstrip"); strip.style.height = "18px";
        for (let h = 0; h < 24; h++) {
            const cell = el("div", "cell");
            cell.style.background = `color-mix(in srgb, ${groupColor(g.k)} ${Math.round((g.h[h] / gmax) * 100)}%, transparent)`;
            if (h === NOW_HOUR) cell.classList.add("now");
            if (S.filters.hour) { const [a, b] = S.filters.hour; if (a <= b ? h >= a && h <= b : h >= a || h <= b) cell.classList.add("hb"); }
            tipify(cell, `<b>${esc(g.k)}</b> · ${String(h).padStart(2, "0")}:00 · ${g.h[h]}`);
            strip.appendChild(cell);
        }
        row.appendChild(strip); row.onclick = () => toggleFilter(dim, g.k);
        heat.appendChild(row);
    });
    mount.appendChild(heat);
}

function renderRoster(mount) {
    const d = derive();
    const rows = [...d.rows].sort((a, b) => Number(b.queries) - Number(a.queries));
    mount.innerHTML = `<div class="pd-scroll"><table class="pd-rtable">
        <thead><tr><th>Device</th><th>Person</th><th>Class</th><th>Identity</th><th class="num">Queries</th><th class="num">Blocked</th><th style="width:150px">Presence (typical day)</th></tr></thead>
        <tbody></tbody></table></div>`;
    const tb = mount.querySelector("tbody");
    if (!rows.length) { tb.innerHTML = '<tr><td colspan="7" class="pd-empty-note">no devices match the current filters</td></tr>'; return; }
    rows.forEach((c) => {
        const tr = el("tr", "rrow"); tr.dataset.name = c.name;
        const on = S.active.has(c.name);
        const idb = identityBucket(c);
        tr.innerHTML = `<td class="name"><span class="pd-dot${on ? " on" : ""}"></span><div>${esc(c.displayName || c.name)}<small>${esc(c.name)}</small></div></td>
            <td>${c.person ? esc(c.person) : '<span style="color:var(--muted)">—</span>'}</td>
            <td><span class="pd-tag" style="color:${classColor(c.class || "unknown")}">${esc(c.class || "unknown")}</span></td>
            <td><span style="font-size:12px;color:var(--ink2)">${idb}${c.shared ? ` · ${c.fpCount} fp` : ""}</span></td>
            <td class="num">${fmtNum(Number(c.queries))}</td>
            <td class="num">${pct(Number(c.blocked), Number(c.queries))}%</td>
            <td class="pcell"></td>`;
        tr.querySelector(".pcell").appendChild(presenceStrip(c.hourLocal, { micro: true, ramp: pColor(c.person || "x") }));
        tr.onclick = (e) => { if (e.target.closest(".pstrip")) return; openDrawer(c); };
        tb.appendChild(tr);
    });
}

/* deferred charts — clean placeholder until the aggregations are exposed. */
function renderDeferred(mount, note) {
    mount.innerHTML = `<div class="pd-empty-note">${esc(note)}</div>`;
}

/* ============ drawer (drill) ============ */
const drawer = document.getElementById("pd-drawer");
function openDrawer(c) {
    const body = document.getElementById("pd-detail");
    const peak = argmax(c.hourLocal);
    body.innerHTML = `
        <div style="color:var(--muted);font-size:12px;margin-bottom:0.5rem">${esc(c.name)} · owned by <b style="color:var(--ink)">${esc(c.person || "unassigned")}</b></div>
        <div style="display:flex;gap:0.4rem;flex-wrap:wrap;margin-bottom:0.8rem">
            <span class="pd-tag" style="color:${classColor(c.class || "unknown")}">${esc(c.class || "unknown")}</span>
            <span class="pd-tag" style="color:var(--ink2)">${identityBucket(c)}</span>
            ${c.os && c.os !== "—" ? `<span class="pd-tag" style="color:var(--ink2)">${esc(c.os)}</span>` : ""}
            ${(c.vendor || []).map((v) => `<span class="pd-tag" style="color:var(--ink2)">${esc(v)}</span>`).join("")}
            ${c.shared ? `<span class="pd-tag" style="color:var(--c-other)">${c.fpCount} fingerprints</span>` : ""}
        </div>
        <section class="tiles tiles-small">
            <div class="tile"><span class="tile-label">Queries</span><span class="tile-value">${fmtNum(Number(c.queries))}</span></div>
            <div class="tile"><span class="tile-label">Blocked</span><span class="tile-value">${pct(Number(c.blocked), Number(c.queries))}%</span></div>
            <div class="tile"><span class="tile-label">Peak hr</span><span class="tile-value">${String(peak).padStart(2, "0")}</span></div>
        </section>
        <h3 style="font-size:13px;margin:1rem 0 0.5rem">Presence — typical day</h3>
        <div id="pd-dpres"></div>
        <div class="paxis">${Array.from({ length: 24 }, (_, h) => `<span>${h}</span>`).join("")}</div>
        <p class="pd-empty-note" style="margin-top:1rem">First seen ${c.firstSeen ? new Date(c.firstSeen).toLocaleDateString() : "—"} · last seen ${c.lastSeen ? new Date(c.lastSeen).toLocaleString() : "—"}</p>`;
    document.getElementById("pd-dpres").appendChild(presenceStrip(c.hourLocal, { ramp: pColor(c.person || "x"), showNow: true }));
    document.getElementById("pd-drawer-title").textContent = c.displayName || c.name;
    drawer.hidden = false;
}
document.getElementById("pd-drawer-close").onclick = () => { drawer.hidden = true; };
addEventListener("keydown", (e) => { if (e.key === "Escape") drawer.hidden = true; });

/* ============ chip bar ============ */
function renderChips() {
    const bar = document.getElementById("pd-chipbar"); bar.innerHTML = "";
    const f = S.filters; const dims = { class: "Class", person: "Person", identity: "Identity" };
    let any = false;
    Object.entries(dims).forEach(([d, lbl]) => {
        if (f[d]) { any = true; const c = el("button", "pd-chip", `${lbl}: ${esc(f[d])} <span class="x">✕</span>`); c.onclick = () => clearFilter(d); bar.appendChild(c); }
    });
    if (f.hour) { any = true; const c = el("button", "pd-chip", `Hours ${f.hour[0]}:00–${f.hour[1]}:00 <span class="x">✕</span>`); c.onclick = () => { brushStart = null; clearFilter("hour"); }; bar.appendChild(c); }
    if (!any) bar.innerHTML = '<span class="pd-hint">no filters — click any chart to cross-filter</span>';
    document.getElementById("pd-fclass").value = f.class || "";
    document.getElementById("pd-fperson").value = f.person || "";
    document.getElementById("pd-fidentity").value = f.identity || "";
}

/* ============ board ============ */
let CHARTS = [];
function card(span, title, sub) {
    const c = el("div", "pd-card " + span);
    c.innerHTML = `<div class="pd-hd"><div><h3>${title}</h3><div class="sub">${sub}</div></div></div>`;
    const b = el("div"); c.appendChild(b);
    return { card: c, body: b };
}
function section(t, s) { return el("div", "pd-section", `<h2>${esc(t)}</h2><span>${esc(s)}</span>`); }
function buildBoard() {
    const board = document.getElementById("pd-board");
    board.innerHTML = ""; CHARTS = [];
    board.appendChild(section("Household composition", "who's on your network — click any slice or bar to cross-filter every chart"));
    let c;
    c = card("span6", "Query share", "by " + scopeWord() + " · click to filter"); board.appendChild(c.card); CHARTS.push({ body: c.body, fn: renderDonut });
    c = card("span6", "Device-class distribution", "how the fleet breaks down"); board.appendChild(c.card); CHARTS.push({ body: c.body, fn: renderClassDist });

    board.appendChild(section("When — presence", "hour-of-day rhythm · click a row to filter, drag cells to brush hours"));
    c = card("span12", "Presence heatmap", "group × hour-of-day (typical day, localized)"); board.appendChild(c.card); CHARTS.push({ body: c.body, fn: renderHeatmap });

    board.appendChild(section("Roster", "every device across every heuristic — the accessible table equivalent of the charts above"));
    c = card("span12", "Persona roster", "sortable by activity · click a row for the full profile"); board.appendChild(c.card); CHARTS.push({ body: c.body, fn: renderRoster });

    board.appendChild(section("Coming soon", "charts awaiting server-side aggregations not yet exposed"));
    c = card("span6", "Services & usage", "apps · websites · services, format-tagged"); board.appendChild(c.card);
    CHARTS.push({ body: c.body, fn: (m) => renderDeferred(m, "Deferred — needs the per-client usage[] fold (ServiceUsageByClient, Phase 4).") });
    c = card("span6", "Weekly usage calendar", "hour × weekday, what runs when"); board.appendChild(c.card);
    CHARTS.push({ body: c.body, fn: (m) => renderDeferred(m, "Deferred — needs the dow_hour_service aggregation (Phase 5).") });
}

/* ============ master render ============ */
function render() {
    renderKPIs(); renderChips();
    CHARTS.forEach((c) => { try { c.fn(c.body); } catch (e) { c.body.innerHTML = '<div class="pd-empty-note">—</div>'; console.error(e); } });
}

/* ============ locked / off states ============ */
function showLocked() {
    document.getElementById("pd-controls").hidden = true;
    document.getElementById("pd-kpis").hidden = true;
    document.getElementById("pd-banner").hidden = true;
    const board = document.getElementById("pd-board");
    board.innerHTML = `<div class="pd-card span12 pd-locked">
        <div class="ico" aria-hidden="true">🔒</div>
        <h2>Household profiling is off</h2>
        <p>Persona analytics — presence rhythms, person mapping, and identity facets — are disabled. Enable <code>privacy.profiling.enable</code> in <a href="/privacy">Privacy</a> to compute them locally on your Pi. Nothing is sent off-device.</p></div>`;
}

/* ============ data load ============ */
async function load() {
    const status = document.getElementById("pd-status");
    let data;
    const params = RANGES[S.range] ? { from: new Date(Date.now() - RANGES[S.range] * 1000).toISOString(), to: new Date().toISOString() } : undefined;
    try { data = await getJSON("/api/ui/personas", params); }
    catch (err) { status.hidden = false; status.textContent = "Could not load personas: " + err.message; return; }

    if (!data.enabled) { showLocked(); return; }

    status.hidden = true;
    DATA = data;
    CLIENTS = data.clients || [];
    NOW_HOUR = tzHour(data.tz);
    personIndex = new Map((data.people || []).map((p, i) => [p.person, i]));

    document.getElementById("pd-controls").hidden = false;
    document.getElementById("pd-kpis").hidden = false;
    document.getElementById("pd-banner").hidden = false;

    // populate filter selects once
    const fclass = document.getElementById("pd-fclass");
    const classes = [...new Set(CLIENTS.map((c) => c.class || "unknown"))].sort();
    fclass.innerHTML = '<option value="">All</option>' + classes.map((c) => `<option>${esc(c)}</option>`).join("");
    fclass.value = S.filters.class || "";
    const fperson = document.getElementById("pd-fperson");
    fperson.innerHTML = '<option value="">All</option>' + (data.people || []).map((p) => `<option>${esc(p.person)}</option>`).join("");
    fperson.value = S.filters.person || "";

    buildBoard();
    render();
}

/* ============ controls wiring ============ */
document.getElementById("pd-group").onclick = (e) => {
    const b = e.target.closest("button"); if (!b) return;
    [...b.parentNode.children].forEach((x) => x.setAttribute("aria-pressed", x === b));
    S.scope = b.dataset.v; buildBoard(); render();
};
document.getElementById("pd-fclass").onchange = (e) => { e.target.value ? (S.filters.class = e.target.value) : delete S.filters.class; render(); };
document.getElementById("pd-fperson").onchange = (e) => { e.target.value ? (S.filters.person = e.target.value) : delete S.filters.person; render(); };
document.getElementById("pd-fidentity").onchange = (e) => { e.target.value ? (S.filters.identity = e.target.value) : delete S.filters.identity; render(); };
document.getElementById("pd-frange").onchange = (e) => { S.range = e.target.value; load(); };

document.getElementById("pd-purge").onclick = async () => {
    const ok = await confirmDialog("Erase all locally-computed profiling data (presence, class, person, identity)? This cannot be undone.", { title: "Purge profiles", danger: true, okText: "Purge" });
    if (!ok) return;
    try { await send("DELETE", "/api/ui/clients/profiles"); toast("Profiles purged"); load(); }
    catch (err) { toast("Could not purge: " + err.message, { type: "error" }); }
};

/* ============ live SSE — active-now (KPI tile + roster dots) ============ */
onQuery((item) => { if (DATA && DATA.enabled) S.active.set(item.client, Date.now()); });
setInterval(() => {
    if (!DATA || !DATA.enabled) return;
    const now = Date.now();
    for (const [k, t] of S.active) if (now - t > 5 * 60 * 1000) S.active.delete(k);
    // cheap live repaint: KPI active-now tile + roster dots, no full re-render
    renderKPIs();
    document.querySelectorAll(".pd-rtable tr.rrow").forEach((tr) => {
        const dot = tr.querySelector(".pd-dot"); if (dot) dot.classList.toggle("on", S.active.has(tr.dataset.name));
    });
}, 1000);

load();
