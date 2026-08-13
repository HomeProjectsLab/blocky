// settings.js — read-only config summary + raw YAML editor (validate/save/apply).
import { getText, putText, send } from "./api.js";

const yamlEl = document.getElementById("set-yaml");
const errEl = document.getElementById("set-error");
const msgEl = document.getElementById("set-msg");
const summaryEl = document.getElementById("set-summary");

function msg(text, isErr) {
    msgEl.textContent = text;
    msgEl.style.color = isErr ? "var(--c-error)" : "var(--c-cached)";
}

function showErr(text) {
    if (!text) { errEl.hidden = true; return; }
    errEl.hidden = false;
    errEl.textContent = text;
}

// display-only: pull a handful of scalars out of the raw YAML by indentation.
// A miss just shows "—"; the raw editor is the source of truth.
function scalar(yaml, section, key) {
    const lines = yaml.split("\n");
    const start = lines.findIndex((l) => new RegExp(`^${section}:`).test(l));
    if (start < 0) return null;
    for (let i = start + 1; i < lines.length; i++) {
        if (/^\S/.test(lines[i])) break;
        const m = lines[i].match(new RegExp(`^\\s+${key}:\\s*(.+?)\\s*$`));
        if (m) return m[1].replace(/["']/g, "");
    }
    return null;
}

function renderSummary(yaml) {
    const rows = [
        ["DNS port", scalar(yaml, "ports", "dns")],
        ["HTTP port", scalar(yaml, "ports", "http")],
        ["Caching min TTL", scalar(yaml, "caching", "minTime")],
        ["Caching prefetching", scalar(yaml, "caching", "prefetching")],
        ["Query log type", scalar(yaml, "queryLog", "type")],
        ["Query log retention (days)", scalar(yaml, "queryLog", "logRetentionDays")],
    ];
    summaryEl.innerHTML = "";
    for (const [k, v] of rows) {
        const dt = document.createElement("dt"); dt.textContent = k;
        const dd = document.createElement("dd"); dd.textContent = v ?? "—";
        summaryEl.append(dt, dd);
    }
}

async function reload() {
    showErr(null); msg("");
    try {
        const yaml = await getText("/api/ui/config/raw");
        yamlEl.value = yaml;
        renderSummary(yaml);
    } catch (err) { msg("Could not load config: " + err.message, true); }
}

document.getElementById("set-validate").addEventListener("click", async () => {
    showErr(null);
    try {
        // validate the current textarea content (empty body would validate the stored blob)
        const res = await fetch("/api/ui/config/validate", { method: "POST", body: yamlEl.value });
        const data = await res.json();
        if (data.valid) { showErr(null); msg("Configuration is valid."); }
        else { showErr(data.error || "invalid configuration"); msg("Validation failed.", true); }
    } catch (err) { msg("Validate failed: " + err.message, true); }
});

document.getElementById("set-save").addEventListener("click", async () => {
    showErr(null);
    const err = await putText("/api/ui/config/raw", yamlEl.value);
    if (err) { showErr(err); msg("Save rejected — fix the error above.", true); }
    else { msg("Saved. Apply to activate."); renderSummary(yamlEl.value); }
});

document.getElementById("set-apply").addEventListener("click", async () => {
    try { await send("POST", "/api/ui/config/apply"); msg("Applying — the resolver is rebuilding."); }
    catch (err) { msg("Apply failed: " + err.message, true); }
});

document.getElementById("set-reload").addEventListener("click", reload);

reload();
