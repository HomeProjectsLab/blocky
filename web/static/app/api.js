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

// send a body-carrying request; returns parsed JSON, or {} for 204/empty.
// throws Error(message) using the server's {"error":...} body when present.
export async function send(method, path, body) {
    const opts = { method };
    if (body !== undefined) {
        opts.headers = { "Content-Type": "application/json" };
        opts.body = JSON.stringify(body);
    }
    const res = await fetch(path, opts);
    const text = await res.text();
    let data = {};
    if (text) { try { data = JSON.parse(text); } catch { data = { raw: text }; } }
    if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
    return data;
}

// GET a text/plain (or yaml) body.
export async function getText(path) {
    const res = await fetch(path);
    if (!res.ok) throw new Error(`${path}: HTTP ${res.status}`);
    return res.text();
}

// PUT a raw text body; returns null on success or the server error string.
export async function putText(path, text) {
    const res = await fetch(path, { method: "PUT", body: text });
    if (res.ok) return null;
    let msg = `HTTP ${res.status}`;
    try { const j = JSON.parse(await res.text()); if (j.error) msg = j.error; } catch { /* keep */ }
    return msg;
}

// bodyless request with query params (blocking enable/disable are GET;
// cache/flush and lists/refresh are POST). Returns the response text.
export async function action(method, path, params) {
    const url = new URL(path, window.location.origin);
    if (params) for (const [k, v] of Object.entries(params)) {
        if (v !== undefined && v !== null && v !== "") url.searchParams.set(k, v);
    }
    const res = await fetch(url, { method });
    if (!res.ok) throw new Error(`${path}: HTTP ${res.status}`);
    return res.text();
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
