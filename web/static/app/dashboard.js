// dashboard.js — stat tiles, stacked QPS-by-outcome chart, latency tiles, top lists.
import { getJSON } from "./api.js";
import { fmtNum, fmtMs, fmtPct } from "./format.js";
import { stackedArea } from "./chart.js";

const RANGES = { "1h": 3600, "6h": 6 * 3600, "24h": 24 * 3600, "7d": 7 * 24 * 3600 };
const STEPS = { "1h": 60, "6h": 300, "24h": 900, "7d": 3600 };

// Fixed series order + status colors — never cycled, never re-ranked.
const SERIES = [
    { key: "RESOLVED", label: "resolved", color: cssTok("--c-resolved") },
    { key: "BLOCKED", label: "blocked", color: cssTok("--c-blocked") },
    { key: "CACHED", label: "cached", color: cssTok("--c-cached") },
    { key: "OTHER", label: "other", color: cssTok("--c-other") },
];

function cssTok(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

let range = "24h";
let chart = null;
let reqSeq = 0; // bumped per loadAll; loaders ignore responses from stale ranges

function window_() {
    const to = new Date();
    const from = new Date(to.getTime() - RANGES[range] * 1000);
    return { from: from.toISOString(), to: to.toISOString() };
}

async function loadOverview(token) {
    const w = window_();
    // Noise impact reuses this loader's real-query count as its denominator —
    // no separate stats/overview fetch. .catch keeps the tiles rendering if
    // the noise engine endpoint is unavailable.
    const [o, noise] = await Promise.all([
        getJSON("/api/ui/stats/overview", w),
        getJSON("/api/ui/noise/overview", w).catch(() => null),
    ]);
    if (token !== reqSeq) return;
    setText("t-queries", fmtNum(o.queries));
    setText("t-blocked", fmtPct(o.blocked, o.queries));
    setText("t-cached", fmtPct(o.cached, o.queries));
    setText("t-clients", fmtNum(o.clients));
    setText("t-p95", fmtMs(o.p95Ms));
    renderNoiseImpact(noise, o.queries);
}

// Noise-engine impact tile: injected once (no shell.html markup), reuses the
// .tile + .status-dot classes. decoys===0 reads "Cover traffic off", never "0×".
function ensureNoiseTile() {
    let tile = document.getElementById("noise-tile");
    if (tile) return tile;
    tile = document.createElement("div");
    tile.className = "tile";
    tile.id = "noise-tile";
    tile.innerHTML = `<span class="tile-label">Noise cover</span>` +
        `<span class="tile-value" style="display:flex;align-items:center;gap:0.4rem">` +
        `<span class="status-dot"></span><span class="nt-text">—</span></span>`;
    document.getElementById("tiles").append(tile);
    return tile;
}

function renderNoiseImpact(noise, realQueries) {
    const tile = ensureNoiseTile();
    const dot = tile.querySelector(".status-dot");
    const text = tile.querySelector(".nt-text");
    const decoys = noise ? noise.decoys : 0;
    if (!decoys) {
        dot.className = "status-dot off";
        text.textContent = "Cover traffic off";
        return;
    }
    dot.className = "status-dot on";
    // ponytail: naive ratio, denominator is total real in-range queries.
    const ratio = realQueries ? decoys / realQueries : 0;
    text.textContent = `${fmtNum(decoys)} decoys · ${ratio.toFixed(1)}×`;
}

// "Resolving recursively from root" badge: shown when any upstream group runs
// the recursive strategy. Injected into .page-head; toggled off if none do.
async function loadUpstreamBadge(token) {
    const data = await getJSON("/api/ui/upstreams").catch(() => null);
    if (token !== reqSeq || !data) return;
    const recursive = (data.groups || []).some((g) => g.strategy === "recursive");
    let badge = document.getElementById("recursive-badge");
    if (!recursive) { if (badge) badge.remove(); return; }
    if (!badge) {
        badge = document.createElement("span");
        badge.id = "recursive-badge";
        badge.className = "cat-badge";
        badge.textContent = "Resolving recursively from root";
        document.querySelector(".page-head").append(badge);
    }
}

async function loadLatency(token) {
    const w = window_();
    const l = await getJSON("/api/ui/stats/latency", w);
    if (token !== reqSeq) return;
    setText("t-lp50", fmtMs(l.p50));
    setText("t-lp90", fmtMs(l.p90));
    setText("t-lp95", fmtMs(l.p95));
    setText("t-lp99", fmtMs(l.p99));
}

async function loadBuckets(token) {
    const w = window_();
    const res = await getJSON("/api/ui/stats/buckets", { ...w, step: STEPS[range] });
    if (token !== reqSeq) return;
    const buckets = res.buckets || [];
    const el = document.getElementById("qps-chart");
    const empty = document.getElementById("qps-empty");

    if (chart) { chart.destroy(); chart = null; }
    el.innerHTML = "";

    if (buckets.length === 0) { empty.hidden = false; return; }
    empty.hidden = true;

    const xs = buckets.map((b) => b.ts);
    const known = new Set(["RESOLVED", "BLOCKED", "CACHED"]);
    const rows = SERIES.map((s) => buckets.map((b) => {
        const counts = b.counts || {};
        if (s.key !== "OTHER") return counts[s.key] || 0;
        let sum = 0;
        for (const [k, v] of Object.entries(counts)) if (!known.has(k)) sum += v;
        return sum;
    }));

    const toSec = Math.floor(Date.parse(w.to) / 1000);
    const fromSec = Math.floor(Date.parse(w.from) / 1000);

    chart = stackedArea(el, {
        labels: SERIES.map((s) => s.label),
        colors: SERIES.map((s) => s.color),
        data: [xs, ...rows],
        xRange: [fromSec, toSec],
        fmtVal: fmtNum,
    });
}

// TOP_PANELS maps each top-N column to its list element id.
const TOP_PANELS = { domain: "top-domain", blocked: "top-blocked", client: "top-client", transport: "top-transport" };

function renderTop(elID, items, device) {
    const ol = document.getElementById(elID);
    if (!items || items.length === 0) {
        ol.innerHTML = `<p class="empty">No queries in this window — widen the time range.</p>`;
        return;
    }
    const max = Math.max(...items.map((i) => i.count), 1);
    ol.innerHTML = items.map((i) => {
        const guess = device && device[i.name];
        const chip = guess ? ` <span class="chip cc-iot" title="Device class (best guess)">${esc(guess)}</span>` : "";
        return `
        <li>
            <div class="bar-row"><span class="bar-name" title="${esc(i.name)}">${esc(i.name)}${chip}</span><span class="bar-count">${fmtNum(i.count)}</span></div>
            <div class="bar-track"><div class="bar-fill" style="width:${(i.count / max * 100).toFixed(1)}%"></div></div>
        </li>`;
    }).join("");
}

// One request for all four panels: keeps the dashboard under the browser's
// 6-connections-per-origin cap (the SSE stream already holds one slot).
async function loadTop(token) {
    const cols = Object.keys(TOP_PANELS);
    // Fetch clients alongside top so the name→deviceGuess map is ready when the
    // client panel renders (same await = no ordering race). /api/ui/clients is
    // range-independent; .catch keeps the panels rendering if it fails.
    const [res, clients] = await Promise.all([
        getJSON("/api/ui/stats/top", { ...window_(), col: cols.join(","), n: 10 }),
        getJSON("/api/ui/clients").catch(() => ({ clients: [] })),
    ]);
    if (token !== reqSeq) return;
    const device = {};
    for (const c of clients.clients || []) if (c.deviceGuess) device[c.name] = c.deviceGuess;
    const columns = res.columns || {};
    for (const col of cols) renderTop(TOP_PANELS[col], columns[col], col === "client" ? device : null);
}

function esc(s) {
    return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

function setText(id, text) {
    document.getElementById(id).textContent = text;
}

function loadAll() {
    const token = ++reqSeq;
    const jobs = [
        loadOverview(token), loadLatency(token), loadBuckets(token), loadTop(token),
        loadUpstreamBadge(token),
    ];
    for (const j of jobs) j.catch((err) => {
        console.error(err);
        if (token !== reqSeq) return; // a newer range is already loading
        const empty = document.getElementById("qps-empty");
        empty.textContent = "Couldn't load dashboard data — the backend may be unavailable (query log not in sqlite mode, or a config apply in progress).";
        empty.hidden = false;
    });
}

document.getElementById("range-row").addEventListener("click", (ev) => {
    const btn = ev.target.closest("button[data-range]");
    if (!btn) return;
    range = btn.dataset.range;
    for (const b of document.querySelectorAll("#range-row button")) {
        b.setAttribute("aria-pressed", b === btn ? "true" : "false");
    }
    loadAll();
});

loadAll();
