// app.js — shell boot: ticker, footer, page module dispatch.
import { getJSON, onQuery } from "./api.js";
import { fmtClock, fmtUptime, band } from "./format.js";

const page = document.body.dataset.page;

const pageModules = {
    dashboard: "./dashboard.js",
    live: "./live.js",
    queries: "./queries.js",
    noise: "./noise.js",
    upstreams: "./upstreams.js",
    blocking: "./blocking.js",
    localdns: "./localdns.js",
    clients: "./clients.js",
    privacy: "./privacy.js",
    settings: "./settings.js",
    system: "./system.js",
};

if (pageModules[page]) {
    import(pageModules[page]).catch((err) => console.error("page module failed:", err));
}

// ---- footer: version + uptime from /api/ui/system ----
(async () => {
    try {
        const sys = await getJSON("/api/ui/system");
        document.getElementById("foot-version").textContent = `blocky ${sys.version}`;
        document.getElementById("foot-uptime").textContent =
            `${fmtUptime(sys.uptimeSeconds)} · ${sys.queriesTotal} queries logged`;
    } catch { /* footer stays with server-rendered version */ }
})();

// ---- header system-usage strip: per-core CPU / RAM / disk + R/W, 2s poll ----
const sysbar = document.getElementById("sysbar");

function sysBytes(n) {
    n = Number(n) || 0;
    const u = ["B", "K", "M", "G", "T"];
    let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return `${n < 10 && i > 0 ? n.toFixed(1) : Math.round(n)}${u[i]}`;
}

function sysPct(used, total) { return total ? Math.round((100 * used) / total) : 0; }

function cpuColor(p) {
    return p >= 85 ? "var(--c-error)" : p >= 50 ? "var(--c-blocked)" : "var(--c-cached)";
}

function renderSysbar(s) {
    const bars = s.cpuPerCore
        .map((p) => `<span class="cpu-bar" title="${Math.round(p)}%" style="height:${Math.max(6, Math.round(p))}%;background:${cpuColor(p)}"></span>`)
        .join("");
    const item = (label, value) =>
        `<span class="sysbar-item"><span class="sysbar-label">${label}</span>${value}</span>`;
    return [
        item("CPU", `<span class="cpu-bars">${bars}</span><span class="sysbar-value">${Math.round(s.cpuTotal)}%</span>`),
        item("MEM", `<span class="sysbar-value">${sysBytes(s.memUsed)}/${sysBytes(s.memTotal)} · ${sysPct(s.memUsed, s.memTotal)}%</span>`),
        item("DISK", `<span class="sysbar-value">${sysBytes(s.diskUsed)}/${sysBytes(s.diskTotal)} · ${sysPct(s.diskUsed, s.diskTotal)}%</span>`),
        item("I/O", `<span class="sysbar-value">↓${sysBytes(s.diskReadBps)}/s ↑${sysBytes(s.diskWriteBps)}/s</span>`),
    ].join("");
}

async function pollSys() {
    try {
        const s = await getJSON("/api/ui/system");
        if (!Array.isArray(s.cpuPerCore)) return; // unsupported host (non-linux / pre-first-sample): stay hidden
        sysbar.innerHTML = renderSysbar(s);
        sysbar.hidden = false;
    } catch { /* transient error: keep the last render */ }
}

if (sysbar) { pollSys(); setInterval(pollSys, 2000); }

// ---- the ticker: last query one-liner on every page ----
const tickerText = document.getElementById("ticker-text");
const tickerRail = document.getElementById("ticker-rail");
const reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
const bandColors = {
    resolved: "var(--c-resolved)",
    cached: "var(--c-cached)",
    blocked: "var(--c-blocked)",
    error: "var(--c-error)",
    other: "var(--c-other)",
};

let tickerPaused = false;
tickerText.parentElement.addEventListener("mouseenter", () => { tickerPaused = true; });
tickerText.parentElement.addEventListener("mouseleave", () => { tickerPaused = false; });

onQuery((item) => {
    if (tickerPaused) return;
    const b = band(item);
    tickerRail.style.background = bandColors[b];
    tickerText.textContent =
        `${fmtClock(item.ts)}  ${item.client} → ${item.question}  [${item.qtype} ${item.rtype} ${item.durationMs}ms]`;
    if (!reduceMotion) {
        tickerText.classList.remove("tick");
        void tickerText.offsetWidth; // restart animation
        tickerText.classList.add("tick");
    }
});
