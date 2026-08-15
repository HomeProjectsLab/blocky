// app.js — shell boot: ticker, footer, page module dispatch.
import { getJSON, onQuery, send } from "./api.js";
import { fmtClock, fmtUptime, band } from "./format.js";

const page = document.body.dataset.page;

// ---- sign out ----
const logoutBtn = document.getElementById("logout-btn");
if (logoutBtn) {
    logoutBtn.addEventListener("click", async () => {
        try { await send("POST", "/api/ui/auth/logout"); } catch { /* ignore */ }
        location = "/login";
    });
}

// ---- theme toggle: flip data-theme, persist; no-flash init is in shell <head> ----
const themeBtn = document.getElementById("theme-btn");
if (themeBtn) {
    themeBtn.addEventListener("click", () => {
        const root = document.documentElement;
        const dark = root.dataset.theme
            ? root.dataset.theme === "dark"
            : matchMedia("(prefers-color-scheme: dark)").matches;
        root.dataset.theme = localStorage.theme = dark ? "light" : "dark";
    });
}

const pageModules = {
    dashboard: "./dashboard.js",
    live: "./live.js",
    queries: "./queries.js",
    noise: "./noise.js",
    upstreams: "./upstreams.js",
    blocking: "./blocking.js",
    groups: "./groups.js",
    localdns: "./localdns.js",
    clients: "./clients.js",
    privacy: "./privacy.js",
    settings: "./settings.js",
    system: "./system.js",
    login: "./login.js",
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

function fmtQps(v) {
    v = Number(v) || 0;
    if (v >= 1000) return (v / 1000).toFixed(1) + "k";
    if (v >= 100) return String(Math.round(v));
    return v.toFixed(v >= 10 ? 0 : 1);
}

function cpuColor(p) {
    return p >= 85 ? "var(--c-error)" : p >= 50 ? "var(--c-blocked)" : "var(--c-cached)";
}

function renderSysbar(s) {
    const bars = s.cpuPerCore
        .map((p) => `<span class="cpu-bar" title="${Math.round(p)}%" style="height:${Math.max(6, Math.round(p))}%;background:${cpuColor(p)}"></span>`)
        .join("");
    const item = (label, value) =>
        `<span class="sysbar-item"><span class="sysbar-label">${label}</span>${value}</span>`;
    // rolling QPS (10s/1m/5m/10m/1h) — present only on boxes new enough to report it
    const qps = typeof s.qps10s === "number"
        ? item("QPS", `<span class="sysbar-value">${fmtQps(s.qps10s)}<sub>10s</sub> ${fmtQps(s.qps1m)}<sub>1m</sub> ${fmtQps(s.qps5m)}<sub>5m</sub> ${fmtQps(s.qps10m)}<sub>10m</sub> ${fmtQps(s.qps1h)}<sub>1h</sub></span>`)
        : "";
    return [
        qps,
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
