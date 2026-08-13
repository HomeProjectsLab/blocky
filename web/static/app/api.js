// api.js — fetch + shared EventSource helpers.

export async function getJSON(path, params) {
    const url = new URL(path, window.location.origin);
    if (params) {
        for (const [k, v] of Object.entries(params)) {
            if (v !== undefined && v !== null && v !== "") url.searchParams.set(k, v);
        }
    }
    const res = await fetch(url);
    if (!res.ok) throw new Error(`${path}: HTTP ${res.status}`);
    return res.json();
}

// One EventSource per page, shared between ticker and the live wire.
const subscribers = new Set();
let source = null;

function ensureSource() {
    if (source) return;
    source = new EventSource("/api/ui/stream");
    source.addEventListener("query", (ev) => {
        let item;
        try { item = JSON.parse(ev.data); } catch { return; }
        for (const fn of subscribers) fn(item);
    });
    // EventSource auto-reconnects; nothing else to do.
}

export function onQuery(fn) {
    subscribers.add(fn);
    ensureSource();
    return () => subscribers.delete(fn);
}
