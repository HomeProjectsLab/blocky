// blocking.js — blocking status/toggle, list refresh, cache flush, read-only lists.
import { action, getJSON, getText } from "./api.js";

const dot = document.getElementById("bl-dot");
const state = document.getElementById("bl-state");
const actionMsg = document.getElementById("bl-action");

function say(el, msg, isErr) {
    el.textContent = msg;
    el.style.color = isErr ? "var(--c-error)" : "var(--c-cached)";
}

async function refreshStatus() {
    try {
        const s = await getJSON("/api/blocking/status");
        dot.className = "status-dot " + (s.enabled ? "on" : "off");
        if (s.enabled) {
            state.textContent = "Blocking is enabled.";
        } else {
            const grp = s.disabledGroups?.length ? ` (${s.disabledGroups.join(", ")})` : "";
            const secs = s.autoEnableInSec ? ` — re-enables in ${s.autoEnableInSec}s` : "";
            state.textContent = `Blocking is disabled${grp}${secs}.`;
        }
    } catch (err) { state.textContent = "Status unavailable: " + err.message; }
}

document.getElementById("bl-disable").addEventListener("click", async () => {
    try {
        await action("GET", "/api/blocking/disable", { duration: document.getElementById("bl-duration").value });
        say(actionMsg, "Blocking disabled.");
        refreshStatus();
    } catch (err) { say(actionMsg, err.message, true); }
});

document.getElementById("bl-enable").addEventListener("click", async () => {
    try { await action("GET", "/api/blocking/enable"); say(actionMsg, "Blocking enabled."); refreshStatus(); }
    catch (err) { say(actionMsg, err.message, true); }
});

document.getElementById("lists-refresh").addEventListener("click", async (ev) => {
    const btn = ev.currentTarget; btn.disabled = true;
    say(actionMsg, "Refreshing blocklists…");
    try { await action("POST", "/api/lists/refresh"); say(actionMsg, "Blocklists refreshed."); }
    catch (err) { say(actionMsg, err.message, true); }
    finally { btn.disabled = false; }
});

document.getElementById("cache-flush").addEventListener("click", async () => {
    try { await action("POST", "/api/cache/flush"); say(actionMsg, "DNS cache flushed."); }
    catch (err) { say(actionMsg, err.message, true); }
});

// Read-only view of the blocking config: extract the blocking: block from raw YAML.
(async () => {
    const box = document.getElementById("bl-lists");
    try {
        const yaml = await getText("/api/ui/config/raw");
        const lines = yaml.split("\n");
        const start = lines.findIndex((l) => /^blocking:/.test(l));
        if (start < 0) { box.innerHTML = '<p class="empty">No blocking section configured.</p>'; return; }
        const block = [lines[start]];
        for (let i = start + 1; i < lines.length; i++) {
            if (/^\S/.test(lines[i])) break; // next top-level key
            block.push(lines[i]);
        }
        const pre = document.createElement("pre");
        pre.className = "yaml-view";
        pre.textContent = block.join("\n").replace(/\s+$/, "");
        box.innerHTML = "";
        box.append(pre, Object.assign(document.createElement("p"), { className: "empty", textContent: "Read-only — edit lists in Settings → raw YAML." }));
    } catch (err) { box.innerHTML = `<p class="empty">Could not read config: ${err.message}</p>`; }
})();

refreshStatus();
