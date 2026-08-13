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

// The vendor-telemetry families the engine ships beacon domains for. Empty
// selection = the engine's built-in default set. Names only — the engine owns
// the actual beacon domains.
const VENDOR_FAMILIES = ["apple", "google", "amazon", "microsoft", "samsung", "tuya", "sonos"];

// section defs: [key, label, [ {field, type, label, help, min, max, step, options} ]]
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
    ["decoy", "Persona profile", "Sizes the cover-traffic volume curve to your network. The box's total DNS egress is shaped to a household/office diurnal curve so an eavesdropper can't read your activity level, not just which queries are real.", [
        { field: "personaProfile", type: "select", options: ["home", "enterprise", "auto"], label: "Profile", help: "Home (~40 peak / 6 quiet queries-per-min), Enterprise (~300 / 60, office-diurnal over a 24/7 IoT floor), or auto (home baseline, may escalate from observed device classes)." },
        { field: "targetQpmPeak", type: "number", step: "1", min: "0", label: "Busy-hour target (q/min)", help: "Total egress aimed for at the daily peak. Overrides the profile when set away from the home default (40)." },
        { field: "targetQpmTrough", type: "number", step: "1", min: "0", label: "Quiet-hour target (q/min)", help: "Total egress aimed for pre-dawn. Overrides the profile when set away from the home default (6)." },
    ]],
    ["deviceClass", "Device-class shaping", "Classifies each client from its DNS behaviour (IoT / workstation / server) and shapes its cover to match — IoT and servers beacon to fixed vendor telemetry, they don't browse, so browsing-shaped noise for them is itself a tell. Manage & override per-client classes on the Clients page.", [
        { field: "enable", type: "toggle", label: "Enable device-class shaping", help: "Off = every client gets the same browsing-shaped cover." },
        { field: "vendorTelemetry", type: "toggle", label: "Emit vendor-telemetry chaff", help: "Cover IoT/server clients with fixed vendor beacon lookups instead of browse companions." },
        { field: "vendorFamilies", type: "multi", options: VENDOR_FAMILIES, label: "Vendor families", help: "Which telemetry families to draw beacon domains from. Select none = the engine's built-in default set." },
        { field: "phantomDevicesPct", type: "number", min: "0", max: "100", label: "Phantom devices %", help: "Share of vendor-telemetry chaff drawn from families NOT in your real fleet, to obscure true fleet size and vendor mix (0–100)." },
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
        if (def.help) { const h = document.createElement("p"); h.className = "field-help"; h.textContent = def.help; wrap.append(h); }
    } else if (def.type === "select") {
        const label = document.createElement("label");
        label.className = "field-label"; label.htmlFor = id; label.textContent = def.label;
        const sel = document.createElement("select");
        sel.id = id; sel.dataset.section = sectionKey; sel.dataset.field = def.field; sel.dataset.kind = "str";
        for (const opt of def.options) {
            const o = document.createElement("option");
            o.value = opt; o.textContent = opt;
            if ((value ?? "") === opt) o.selected = true;
            sel.append(o);
        }
        wrap.append(label, sel);
        if (def.help) { const h = document.createElement("p"); h.className = "field-help"; h.textContent = def.help; wrap.append(h); }
    } else if (def.type === "multi") {
        const label = document.createElement("label");
        label.className = "field-label"; label.htmlFor = id; label.textContent = def.label;
        const sel = document.createElement("select");
        sel.id = id; sel.multiple = true; sel.size = Math.min(def.options.length, 8);
        sel.dataset.section = sectionKey; sel.dataset.field = def.field; sel.dataset.kind = "multi";
        const cur = new Set(value || []);
        for (const opt of def.options) {
            const o = document.createElement("option");
            o.value = opt; o.textContent = opt; o.selected = cur.has(opt);
            sel.append(o);
        }
        wrap.append(label, sel);
        if (def.help) { const h = document.createElement("p"); h.className = "field-help"; h.textContent = def.help; wrap.append(h); }
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
    const out = {};
    for (const [key] of SECTIONS) out[key] = out[key] || {};
    for (const input of form.querySelectorAll("[data-section]")) {
        const { section, field, kind } = input.dataset;
        if (kind === "bool") out[section][field] = input.checked;
        else if (kind === "num") out[section][field] = Number(input.value) || 0;
        else if (kind === "multi") out[section][field] = Array.from(input.selectedOptions).map((o) => o.value);
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
