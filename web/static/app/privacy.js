// privacy.js — decoy / TTL-jitter / EDNS-padding config, edited via /api/ui/privacy.
import { getJSON, send } from "./api.js";

const form = document.getElementById("pv-form");
const statusEl = document.getElementById("pv-status");
const saveBtn = document.getElementById("pv-save");
const applyBtn = document.getElementById("pv-apply");

function flash(msg, isErr) {
    statusEl.hidden = false;
    statusEl.textContent = msg;
    statusEl.style.color = isErr ? "var(--c-error)" : "var(--c-cached)";
}

// section defs: [key, label, [ {field, type, label, help, min, max, step} ]]
const SECTIONS = [
    ["decoy", "Decoy queries", "Injects believable extra DNS lookups so an eavesdropper can't tell your real traffic from noise.", [
        { field: "enable", type: "toggle", label: "Enable decoys" },
        { field: "queriesPerMinute", type: "number", step: "0.1", min: "0", label: "Queries per minute", help: "How many fake lookups to mix in each minute." },
        { field: "replayWeight", type: "number", min: "0", label: "Replay weight", help: "How much louder your real past queries are in the noise mix vs the popular-domains list." },
        { field: "listWeight", type: "number", min: "0", label: "List weight", help: "How much the popular-domains list contributes to the noise mix." },
        { field: "activeHoursStart", type: "number", min: "0", max: "23", label: "Active from (hour)", help: "Only inject noise starting at this hour (0–23)." },
        { field: "activeHoursEnd", type: "number", min: "1", max: "24", label: "Active until (hour)", help: "Stop injecting noise at this hour (1–24)." },
        { field: "refreshURL", type: "text", label: "Popular-domains URL", help: "Optional source list of popular domains to draw decoys from." },
    ]],
    ["ttlJitter", "TTL jitter", "Randomly nudges cached record lifetimes so your cache timing can't be used to profile you.", [
        { field: "enable", type: "toggle", label: "Enable TTL jitter" },
        { field: "percent", type: "number", min: "0", max: "90", label: "Jitter percent", help: "Maximum +/- percentage applied to each TTL (0–90)." },
    ]],
    ["ednsPadding", "EDNS padding", "Pads encrypted queries to a uniform size so their length leaks nothing about the domain.", [
        { field: "enable", type: "toggle", label: "Enable EDNS padding" },
    ]],
];

function field(sectionKey, def, value) {
    const wrap = document.createElement("div");
    wrap.className = "field";
    const id = `pv-${sectionKey}-${def.field}`;
    if (def.type === "toggle") {
        const input = document.createElement("input");
        input.type = "checkbox"; input.id = id; input.checked = !!value;
        input.dataset.section = sectionKey; input.dataset.field = def.field; input.dataset.kind = "bool";
        const label = document.createElement("label");
        label.className = "toggle-label"; label.htmlFor = id;
        label.append(input, document.createTextNode(" " + def.label));
        wrap.append(label);
    } else {
        const label = document.createElement("label");
        label.className = "field-label"; label.htmlFor = id; label.textContent = def.label;
        const input = document.createElement("input");
        input.type = def.type; input.id = id;
        if (def.min !== undefined) input.min = def.min;
        if (def.max !== undefined) input.max = def.max;
        if (def.step !== undefined) input.step = def.step;
        input.value = value ?? "";
        input.dataset.section = sectionKey; input.dataset.field = def.field;
        input.dataset.kind = def.type === "number" ? "num" : "str";
        wrap.append(label, input);
        if (def.help) { const h = document.createElement("p"); h.className = "field-help"; h.textContent = def.help; wrap.append(h); }
    }
    return wrap;
}

function render(data) {
    form.innerHTML = "";
    for (const [key, heading, blurb, defs] of SECTIONS) {
        const panel = document.createElement("section");
        panel.className = "panel";
        const h = document.createElement("h2"); h.textContent = heading; panel.append(h);
        const desc = document.createElement("p"); desc.className = "empty"; desc.textContent = blurb; panel.append(desc);
        for (const def of defs) panel.append(field(key, def, data[key]?.[def.field]));
        form.append(panel);
    }
}

function collect() {
    const out = { decoy: {}, ttlJitter: {}, ednsPadding: {} };
    for (const input of form.querySelectorAll("[data-section]")) {
        const { section, field, kind } = input.dataset;
        if (kind === "bool") out[section][field] = input.checked;
        else if (kind === "num") out[section][field] = Number(input.value) || 0;
        else out[section][field] = input.value;
    }
    return out;
}

saveBtn.addEventListener("click", async () => {
    try {
        await send("PUT", "/api/ui/privacy", collect());
        flash("Saved. Apply to activate the new privacy settings.");
        applyBtn.hidden = false;
    } catch (err) { flash(err.message, true); }
});

applyBtn.addEventListener("click", async () => {
    try { await send("POST", "/api/ui/config/apply"); applyBtn.hidden = true; flash("Applying — the resolver is rebuilding."); }
    catch (err) { flash(err.message, true); }
});

(async () => {
    try { render(await getJSON("/api/ui/privacy")); }
    catch (err) { flash("Could not load privacy settings: " + err.message, true); }
})();
