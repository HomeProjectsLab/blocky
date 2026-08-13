// system.js — /api/ui/system health tiles + cache flush.
import { getJSON, action } from "./api.js";
import { fmtNum, fmtUptime } from "./format.js";

function fmtBytes(n) {
    if (n == null) return "—";
    if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + " GB";
    if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
    if (n >= 1 << 10) return (n / (1 << 10)).toFixed(1) + " KB";
    return n + " B";
}

function tile(label, value) {
    const t = document.createElement("div");
    t.className = "tile";
    t.innerHTML = `<span class="tile-label">${label}</span><span class="tile-value">${value}</span>`;
    return t;
}

(async () => {
    const tiles = document.getElementById("sys-tiles");
    try {
        const s = await getJSON("/api/ui/system");
        tiles.append(
            tile("Version", s.version || "—"),
            tile("Uptime", fmtUptime(s.uptimeSeconds).replace(/^up /, "")),
            tile("Goroutines", fmtNum(s.goroutines)),
            tile("Heap", fmtBytes(s.heapAllocBytes)),
            tile("Config DB", fmtBytes(s.dbConfigBytes)),
            tile("Query-log DB", fmtBytes(s.dbQuerylogBytes)),
            tile("Queries logged", fmtNum(s.queriesTotal)),
        );
    } catch (err) {
        tiles.innerHTML = `<p class="empty">Could not load system info: ${err.message}</p>`;
    }
})();

document.getElementById("sys-cache-flush").addEventListener("click", async () => {
    const msg = document.getElementById("sys-msg");
    try { await action("POST", "/api/cache/flush"); msg.textContent = "DNS cache flushed."; msg.style.color = "var(--c-cached)"; }
    catch (err) { msg.textContent = err.message; msg.style.color = "var(--c-error)"; }
});
