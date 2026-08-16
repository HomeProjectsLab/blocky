// live.js — the LIVE WIRE: streaming table, pause, filters, detail drawer.
import { onQuery } from "./api.js";
import { fmtClock, band } from "./format.js";

const MAX_ROWS = 500;

const body = document.getElementById("wire-body");
const emptyMsg = document.getElementById("wire-empty");
const pauseBtn = document.getElementById("pause-btn");
const fClient = document.getElementById("f-client");
const fDomain = document.getElementById("f-domain");
const fMode = document.getElementById("f-mode");
const drawer = document.getElementById("drawer");
const drawerBody = document.getElementById("drawer-body");

let paused = false;      // explicit pause (button)
let hoverPaused = false; // auto-pause on hover/scroll
const buffer = [];

function isPaused() { return paused || hoverPaused; }

function setPaused(p) {
    paused = p;
    pauseBtn.setAttribute("aria-pressed", String(p));
    pauseBtn.textContent = p ? "Resume stream" : "Pause stream";
    if (!isPaused()) flush();
}

function matchesFilter(item) {
    const mode = fMode.value;
    if (mode === "real" && item.decoy) return false;
    if (mode === "decoy" && !item.decoy) return false;
    const c = fClient.value.trim().toLowerCase();
    const d = fDomain.value.trim().toLowerCase();
    if (c && !(item.client || "").toLowerCase().includes(c) &&
        !(item.clientNames || []).some((n) => n.toLowerCase().includes(c))) return false;
    if (d && !(item.question || "").toLowerCase().includes(d)) return false;
    return true;
}

function addRow(item) {
    const tr = document.createElement("tr");
    tr.dataset.band = band(item);
    tr._item = item;
    const cells = [
        fmtClock(item.ts), item.client, item.question, item.qtype, item.rtype,
        String(item.durationMs),
    ];
    tr.innerHTML = `<td class="rail"></td>` + cells.map((c, i) =>
        `<td class="${i === 2 ? "q" : i === 4 ? "rt" : i === 5 ? "num" : ""}"></td>`).join("");
    const tds = tr.querySelectorAll("td");
    cells.forEach((c, i) => { tds[i + 1].textContent = c; });
    if (item.decoy) {
        tr.dataset.decoy = "1";
        const tag = document.createElement("span");
        tag.className = "src-tag";
        tag.textContent = item.decoySource || "decoy";
        tds[5].append(" ", tag); // outcome cell
    }
    if (!matchesFilter(item)) tr.hidden = true;
    else emptyMsg.hidden = true;
    body.prepend(tr);
    let trimmedVisible = false;
    while (body.children.length > MAX_ROWS) {
        if (!body.lastElementChild.hidden) trimmedVisible = true;
        body.lastElementChild.remove();
    }
    if (trimmedVisible) updateEmpty();
}

// Show the empty message when no row is visible: "no matches" if rows exist
// but the filters hide them all, the initial waiting text otherwise.
function updateEmpty() {
    const anyVisible = [...body.children].some((tr) => !tr.hidden);
    emptyMsg.textContent = body.children.length
        ? "No queries match the current filters."
        : "Waiting for the first query…";
    emptyMsg.hidden = anyVisible;
}

function flush() {
    while (buffer.length > 0 && !isPaused()) addRow(buffer.shift());
}

// Coalesce rendering. A flooded resolver can push ~1k stream events/sec;
// rendering one DOM row per event saturates the main thread and crashes the tab
// (the buffer bounds memory, but not the mutation RATE). So always buffer, and
// drain on an animation frame — the DOM is touched at most ~60x/sec regardless
// of the incoming rate, and under a burst only the newest MAX_ROWS can matter.
let rafPending = 0;
function scheduleFlush() {
    if (rafPending || isPaused()) return;
    rafPending = requestAnimationFrame(() => {
        rafPending = 0;
        if (isPaused()) return;
        if (buffer.length > MAX_ROWS) buffer.splice(0, buffer.length - MAX_ROWS);
        while (buffer.length > 0) addRow(buffer.shift());
    });
}

onQuery((item) => {
    buffer.push(item);
    if (buffer.length > MAX_ROWS) buffer.shift();
    scheduleFlush();
});

pauseBtn.addEventListener("click", () => setPaused(!paused));

// Auto-pause while the pointer is over the feed or while scrolling in it.
const scrollBox = document.querySelector(".wire-scroll");
scrollBox.addEventListener("mouseenter", () => { hoverPaused = true; });
scrollBox.addEventListener("mouseleave", () => { hoverPaused = false; flush(); });
let scrollTimer;
scrollBox.addEventListener("scroll", () => {
    hoverPaused = true;
    clearTimeout(scrollTimer);
    scrollTimer = setTimeout(() => { hoverPaused = false; flush(); }, 1500);
}, { passive: true });

// Client-side filters re-evaluate existing rows.
function refilter() {
    for (const tr of body.children) tr.hidden = !matchesFilter(tr._item);
    updateEmpty();
}
fClient.addEventListener("input", refilter);
fDomain.addEventListener("input", refilter);
fMode.addEventListener("change", refilter);

// Row click → detail drawer with all contract fields.
const FIELDS = [
    ["ts", "time"], ["client", "client"], ["clientNames", "client names"],
    ["question", "question"], ["qtype", "qtype"], ["rtype", "outcome"],
    ["rcode", "rcode"], ["answer", "answer"], ["durationMs", "duration ms"],
    ["transport", "transport"], ["fpHash", "fingerprint hash"],
    ["reason", "reason"], ["decoy", "decoy"], ["decoySource", "decoy source"],
];

body.addEventListener("click", (ev) => {
    const tr = ev.target.closest("tr");
    if (!tr || !tr._item) return;
    const item = tr._item;
    drawerBody.innerHTML = "";
    for (const [key, label] of FIELDS) {
        let v = item[key];
        if (Array.isArray(v)) v = v.join(", ");
        if (v === undefined || v === null || v === "") v = "—";
        const dt = document.createElement("dt");
        dt.textContent = label;
        const dd = document.createElement("dd");
        dd.textContent = String(v);
        drawerBody.append(dt, dd);
    }
    drawer.hidden = false;
});

document.getElementById("drawer-close").addEventListener("click", () => { drawer.hidden = true; });
document.addEventListener("keydown", (ev) => { if (ev.key === "Escape") drawer.hidden = true; });
