// logs.js — the CONSOLE: live application log, level + text filter, pause, tail.
import { getJSON } from "./api.js";

const MAX_LINES = 1000;

const pre = document.getElementById("log-console");
const emptyMsg = document.getElementById("log-empty");
const levelSel = document.getElementById("log-level");
const textInput = document.getElementById("log-filter");
const pauseBtn = document.getElementById("log-pause");
const clearBtn = document.getElementById("log-clear");
const scrollBox = document.querySelector(".console-scroll");

// logrus levels, most severe first (index = severity rank, lower = worse).
const LEVELS = ["panic", "fatal", "error", "warning", "info", "debug", "trace"];
function rank(level) {
    const i = LEVELS.indexOf(level);
    return i === -1 ? LEVELS.length : i;
}

let paused = false;      // explicit pause (button)
let hoverPaused = false; // auto-pause on hover / scroll-up
const buffer = [];

function isPaused() { return paused || hoverPaused; }

function setPaused(p) {
    paused = p;
    pauseBtn.setAttribute("aria-pressed", String(p));
    pauseBtn.textContent = p ? "Resume" : "Pause";
    if (!isPaused()) flush();
}

// A line passes when it's at or above the selected level and matches the text.
function matches(item) {
    if (rank(item.level) > rank(levelSel.value)) return false;
    const q = textInput.value.trim().toLowerCase();
    if (!q) return true;
    return renderText(item).toLowerCase().includes(q);
}

function renderText(item) {
    let s = `${item.ts}  ${item.level.toUpperCase().padEnd(7)} `;
    if (item.prefix) s += `[${item.prefix}] `;
    s += item.msg;
    if (item.fields) {
        for (const [k, v] of Object.entries(item.fields)) s += `  ${k}=${v}`;
    }
    return s;
}

function nearBottom() {
    return scrollBox.scrollHeight - scrollBox.scrollTop - scrollBox.clientHeight < 40;
}

function addLine(item) {
    emptyMsg.hidden = true;
    const stick = nearBottom(); // decide before we mutate the DOM
    const span = document.createElement("span");
    span.className = "log-line";
    span.dataset.level = item.level;
    span._item = item;

    // ts + level as plain text; prefix and each field in their own span for colour.
    span.append(document.createTextNode(`${item.ts}  ${item.level.toUpperCase().padEnd(7)} `));
    if (item.prefix) {
        const p = document.createElement("span");
        p.className = "prefix";
        p.textContent = `[${item.prefix}] `;
        span.append(p);
    }
    span.append(document.createTextNode(item.msg));
    if (item.fields) {
        for (const [k, v] of Object.entries(item.fields)) {
            const f = document.createElement("span");
            f.className = "field";
            f.textContent = `  ${k}=${v}`;
            span.append(f);
        }
    }

    span.hidden = !matches(item);
    pre.append(span);
    while (pre.children.length > MAX_LINES) pre.firstElementChild.remove();
    if (stick && !isPaused()) scrollBox.scrollTop = scrollBox.scrollHeight;
}

function flush() {
    while (buffer.length > 0 && !isPaused()) addLine(buffer.shift());
}

// Coalesce rendering: a flood could push thousands of lines/sec; drain the
// buffer on an animation frame so the DOM is touched at most ~60x/sec.
let rafPending = 0;
function scheduleFlush() {
    if (rafPending || isPaused()) return;
    rafPending = requestAnimationFrame(() => {
        rafPending = 0;
        if (isPaused()) return;
        if (buffer.length > MAX_LINES) buffer.splice(0, buffer.length - MAX_LINES);
        while (buffer.length > 0) addLine(buffer.shift());
    });
}

// Client-side re-filter of existing lines (level or text changed).
function refilter() {
    for (const span of pre.children) span.hidden = !matches(span._item);
}

// Load the ring snapshot for the current level, replacing what's shown.
async function loadRecent() {
    try {
        const lines = await getJSON("/api/ui/logs/recent", { level: levelSel.value });
        pre.textContent = "";
        buffer.length = 0;
        for (const item of lines) addLine(item); // server returns oldest→newest
        if (pre.children.length === 0) emptyMsg.hidden = false;
        scrollBox.scrollTop = scrollBox.scrollHeight;
    } catch (err) {
        console.error("log snapshot failed:", err);
    }
}

// ---- own EventSource (Console shares no stream with the ticker/wire singleton).
// ponytail: own EventSource, not the shared api.js singleton — different stream.
let source = null;
let reconnectDelay = 1000;

function connect() {
    source = new EventSource("/api/ui/logs");
    source.addEventListener("log", (ev) => {
        let item;
        try { item = JSON.parse(ev.data); } catch { return; }
        buffer.push(item);
        if (buffer.length > MAX_LINES) buffer.shift();
        scheduleFlush();
    });
    source.addEventListener("open", () => { reconnectDelay = 1000; });
    // EventSource auto-reconnects on transient drops only; on an HTTP error it
    // goes CLOSED and stays dead. Re-open ourselves on a capped backoff.
    source.addEventListener("error", () => {
        if (source && source.readyState === EventSource.CLOSED) {
            source.close();
            source = null;
            setTimeout(connect, reconnectDelay);
            reconnectDelay = Math.min(reconnectDelay * 2, 30000);
        }
    });
}

// ---- wiring
levelSel.addEventListener("change", () => { loadRecent(); }); // history must match the new floor
textInput.addEventListener("input", refilter);
pauseBtn.addEventListener("click", () => setPaused(!paused));
clearBtn.addEventListener("click", () => {
    pre.textContent = "";
    buffer.length = 0;
    emptyMsg.hidden = false;
});

// Auto-pause tailing while the pointer is over the console or scrolled up.
scrollBox.addEventListener("mouseenter", () => { hoverPaused = true; });
scrollBox.addEventListener("mouseleave", () => { hoverPaused = false; flush(); });
scrollBox.addEventListener("scroll", () => {
    hoverPaused = !nearBottom();
    if (!hoverPaused) flush();
}, { passive: true });

loadRecent();
connect();
