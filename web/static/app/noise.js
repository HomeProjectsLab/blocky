// noise.js — the decoy dashboard: mirrors the real dashboard, scoped to chaff.
// Stat tiles + stacked decoy-QPS-by-source chart + source mix + top fake domains,
// plus a live "noise wire" (SSE filtered to decoy=1) so you watch the machine work.
import { getJSON, onQuery } from "./api.js";
import { fmtNum, fmtClock, band } from "./format.js";
import { stackedArea } from "./chart.js";

const RANGES = { "1h": 3600, "6h": 6 * 3600, "24h": 24 * 3600, "7d": 7 * 24 * 3600 };
const STEPS = { "1h": 60, "6h": 300, "24h": 900, "7d": 3600 };

// Fixed source order + colors — the 8 provenance labels the engine stamps.
const SOURCES = [
    { key: "replay", color: "#3987e5" },
    { key: "corpus", color: "#199e70" },
    { key: "list", color: "#c98500" },
    { key: "companion", color: "#9085e9" },
    { key: "cohort", color: "#39b5c9" },
    { key: "chatter", color: "#d06fb3" },
    { key: "miss", color: "#8fa1b3" },
    { key: "fail", color: "#e66767" },
];
const SRC_COLOR = Object.fromEntries(SOURCES.map((s) => [s.key, s.color]));

let range = "24h";
let chart = null;
let reqSeq = 0; // bumped per loadAll; loaders ignore responses from stale ranges

function window_() {
    const to = new Date();
    const from = new Date(to.getTime() - RANGES[range] * 1000);
    return { from: from.toISOString(), to: to.toISOString() };
}

async function loadOverview(token) {
    const o = await getJSON("/api/ui/noise/overview", window_());
    if (token !== reqSeq) return;
    setText("n-decoys", fmtNum(o.decoys));
    setText("n-distinct", fmtNum(o.distinctDomains));
    const mix = Object.entries(o.bySource || {}).sort((a, b) => b[1] - a[1]);
    setText("n-topsrc", mix.length ? mix[0][0] : "—");
    setText("n-srccount", String(mix.length));
}

async function loadBuckets(token) {
    const w = window_();
    const res = await getJSON("/api/ui/noise/buckets", { ...w, step: STEPS[range] });
    if (token !== reqSeq) return;
    const buckets = res.buckets || [];
    const el = document.getElementById("noise-chart");
    const empty = document.getElementById("noise-empty");

    if (chart) { chart.destroy(); chart = null; }
    rebuildChart = null;
    el.innerHTML = "";

    if (buckets.length === 0) { empty.hidden = false; return; }
    empty.hidden = true;

    const xs = buckets.map((b) => b.ts);
    const rows = SOURCES.map((s) => buckets.map((b) => (b.counts || {})[s.key] || 0));

    // closure so a theme toggle can rebuild the canvas-baked axis/grid tokens.
    rebuildChart = () => {
        if (chart) chart.destroy();
        el.innerHTML = "";
        chart = stackedArea(el, {
            labels: SOURCES.map((s) => s.key),
            colors: SOURCES.map((s) => s.color),
            data: [xs, ...rows],
            xRange: [Math.floor(Date.parse(w.from) / 1000), Math.floor(Date.parse(w.to) / 1000)],
            fmtVal: fmtNum,
        });
    };
    rebuildChart();
}
let rebuildChart = null;
addEventListener("themechange", () => { if (rebuildChart) rebuildChart(); });

async function loadBars(path, elID, token) {
    const res = await getJSON(path, window_());
    if (token !== reqSeq) return;
    const items = res.items || [];
    const ol = document.getElementById(elID);
    if (items.length === 0) {
        ol.innerHTML = `<p class="empty">No decoys in this window.</p>`;
        return;
    }
    const max = Math.max(...items.map((i) => i.count), 1);
    ol.innerHTML = items.map((i) => {
        const color = SRC_COLOR[i.name] || "var(--accent)";
        return `<li>
            <div class="bar-row"><span class="bar-name" title="${esc(i.name)}">${esc(i.name)}</span><span class="bar-count">${fmtNum(i.count)}</span></div>
            <div class="bar-track"><div class="bar-fill" style="width:${(i.count / max * 100).toFixed(1)}%;background:${color}"></div></div>
        </li>`;
    }).join("");
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
        loadOverview(token), loadBuckets(token),
        loadBars("/api/ui/noise/sourcemix", "n-sourcemix", token),
        loadBars("/api/ui/noise/top", "n-topdomains", token),
    ];
    for (const j of jobs) j.catch((err) => {
        console.error(err);
        if (token !== reqSeq) return; // a newer range is already loading
        const empty = document.getElementById("noise-empty");
        empty.textContent = "Couldn't load noise data — the backend may be unavailable (query log not in sqlite mode, or a config apply in progress).";
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

// ---- live noise wire: SSE filtered to decoy=1 ----
const MAX_ROWS = 200;
const wireBody = document.getElementById("noise-wire-body");
const wireEmpty = document.getElementById("noise-wire-empty");

function addNoiseRow(item) {
    wireEmpty.hidden = true;
    const tr = document.createElement("tr");
    tr.dataset.band = band(item);
    tr.dataset.decoy = "1";
    const src = item.decoySource || "?";
    const cells = [
        "", fmtClock(item.ts), src, item.client, item.question, item.qtype, item.rtype,
        String(item.durationMs),
    ];
    tr.innerHTML = `<td class="rail"></td>` + cells.slice(1).map((c, i) =>
        `<td class="${i === 1 ? "src" : i === 3 ? "q" : i === 5 ? "rt" : i === 6 ? "num" : ""}"></td>`).join("");
    const tds = tr.querySelectorAll("td");
    cells.slice(1).forEach((c, i) => {
        if (i === 1) { tds[i + 1].innerHTML = `<span class="src-tag" style="border-color:${SRC_COLOR[src] || "var(--grid)"}">${esc(c)}</span>`; }
        else { tds[i + 1].textContent = c; }
    });
    wireBody.prepend(tr);
    while (wireBody.children.length > MAX_ROWS) wireBody.lastElementChild.remove();
}

// Coalesce rendering: the noise machine emits decoys constantly (and far more
// under load), so rendering one row per SSE event saturates the main thread and
// crashes the tab. Buffer (bounded), drain on an animation frame — DOM touched
// at most ~60x/sec; under a burst only the newest MAX_ROWS can be visible.
const wireBuffer = [];
let wireRaf = 0;
function scheduleWireFlush() {
    if (wireRaf) return;
    wireRaf = requestAnimationFrame(() => {
        wireRaf = 0;
        if (wireBuffer.length > MAX_ROWS) wireBuffer.splice(0, wireBuffer.length - MAX_ROWS);
        while (wireBuffer.length > 0) addNoiseRow(wireBuffer.shift());
    });
}

onQuery((item) => {
    if (!item.decoy) return;
    wireBuffer.push(item);
    if (wireBuffer.length > MAX_ROWS) wireBuffer.shift();
    scheduleWireFlush();
});

loadAll();
