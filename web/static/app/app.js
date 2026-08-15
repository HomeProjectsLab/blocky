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
