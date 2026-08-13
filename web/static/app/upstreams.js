// upstreams.js — upstream group management (list, strategy, entries, apply).
import { getJSON, send, action } from "./api.js";

const STRATEGIES = [
    ["parallel_best", "Picks 2 random weighted resolvers, returns the fastest (default)."],
    ["strict", "Strict order; next tried only if the previous fails."],
    ["random", "One random weighted resolver per query."],
    ["round_robin", "Rotates through upstreams in order."],
    ["weighted_round_robin", "Rotates proportionally to weight."],
    ["weighted_random", "Random, weighted by each upstream's weight."],
    ["time_hop", "Sticks to one upstream for a random interval, then hops."],
    ["domain_shard", "Same domain always uses the same upstream."],
    ["recursive", "Resolves from the root servers instead of forwarding."],
];

const groupsEl = document.getElementById("up-groups");
const statusEl = document.getElementById("up-status");
const applyBtn = document.getElementById("apply-btn");
let needsApply = false;

function showApply(on) {
    needsApply = needsApply || on;
    applyBtn.hidden = !needsApply;
}

function flash(msg, isErr) {
    statusEl.hidden = false;
    statusEl.textContent = msg;
    statusEl.style.color = isErr ? "var(--c-error)" : "var(--c-cached)";
}

function el(tag, attrs = {}, ...kids) {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
        if (k === "class") n.className = v;
        else if (k === "text") n.textContent = v;
        else n.setAttribute(k, v);
    }
    for (const c of kids) n.append(c);
    return n;
}

function entryRow(e = { address: "", weight: 1, enabled: true }) {
    const row = el("div", { class: "entry-row" });
    const addr = el("input", { type: "text", value: e.address, placeholder: "1.1.1.1 or https://…", class: "entry-addr" });
    const weight = el("input", { type: "number", min: "0", value: String(e.weight ?? 1), class: "entry-weight", title: "weight" });
    const enabled = el("input", { type: "checkbox", title: "enabled" });
    enabled.checked = e.enabled !== false;
    const rm = el("button", { type: "button", class: "btn-icon", title: "remove", text: "✕" });
    rm.addEventListener("click", () => row.remove());
    row.append(addr, weight, el("label", { class: "chk-label" }, enabled, "on"), rm);
    row._read = () => ({ address: addr.value.trim(), weight: Number(weight.value) || 0, enabled: enabled.checked });
    return row;
}

function groupPanel(g, isDefault) {
    const panel = el("section", { class: "panel group-panel" });
    const head = el("div", { class: "ctl-row" });
    head.append(el("h2", { text: g.name, class: "group-name" }));
    if (!isDefault) {
        const del = el("button", { type: "button", class: "btn-danger", text: "Delete group" });
        del.addEventListener("click", async () => {
            if (!confirm(`Delete upstream group "${g.name}"?`)) return;
            try { await send("DELETE", `/api/ui/upstreams/groups/${encodeURIComponent(g.name)}`); showApply(true); load(); }
            catch (err) { flash(err.message, true); }
        });
        head.append(el("span", { class: "spacer" }), del);
    }
    panel.append(head);

    // strategy + hop meta
    const meta = el("div", { class: "ctl-row" });
    const sel = el("select", { class: "strat-sel" });
    for (const [val, desc] of STRATEGIES) {
        const o = el("option", { value: val, text: val });
        o.title = desc;
        if (val === g.strategy) o.selected = true;
        sel.append(o);
    }
    const hopWrap = el("span", { class: "hop-wrap" });
    const hopMin = el("input", { type: "text", value: g.hopMin, class: "hop-in", placeholder: "1m", title: "hopMin" });
    const hopMax = el("input", { type: "text", value: g.hopMax, class: "hop-in", placeholder: "5m", title: "hopMax" });
    hopWrap.append("hop ", hopMin, " – ", hopMax);
    const syncHop = () => { hopWrap.hidden = sel.value !== "time_hop"; };
    sel.addEventListener("change", syncHop);
    syncHop();
    const stratDesc = el("span", { class: "strat-desc empty" });
    const syncDesc = () => { stratDesc.textContent = STRATEGIES.find((s) => s[0] === sel.value)?.[1] || ""; };
    sel.addEventListener("change", syncDesc);
    syncDesc();
    const saveMeta = el("button", { type: "button", text: "Save group" });
    saveMeta.addEventListener("click", async () => {
        try {
            const r = await send("PUT", `/api/ui/upstreams/groups/${encodeURIComponent(g.name)}`, {
                strategy: sel.value, hopMin: hopMin.value || "0", hopMax: hopMax.value || "0",
            });
            if (r.needsApply) showApply(true);
            flash(`Group "${g.name}" saved.`);
        } catch (err) { flash(err.message, true); }
    });
    meta.append(el("label", { class: "dt-label" }, "strategy ", sel), hopWrap, el("span", { class: "spacer" }), saveMeta);
    panel.append(meta, stratDesc);

    // entries
    const entries = el("div", { class: "entries" });
    for (const e of g.entries) entries.append(entryRow(e));
    const addEntry = el("button", { type: "button", class: "btn-sub", text: "+ Add upstream" });
    addEntry.addEventListener("click", () => entries.append(entryRow()));
    const saveEntries = el("button", { type: "button", text: "Save upstreams" });
    saveEntries.addEventListener("click", async () => {
        const rows = [...entries.querySelectorAll(".entry-row")].map((r) => r._read()).filter((e) => e.address);
        if (!rows.length) { flash("A group needs at least one upstream.", true); return; }
        try {
            const r = await send("PUT", `/api/ui/upstreams/groups/${encodeURIComponent(g.name)}/entries`, { entries: rows });
            if (r.swapped) flash(`"${g.name}" swapped live — ${rows.length} upstream(s).`);
            else { showApply(true); flash(`"${g.name}" saved${r.reason ? " — " + r.reason : ""}. Apply to activate.`); }
        } catch (err) { flash(err.message, true); }
    });
    panel.append(el("h2", { text: "Upstreams", class: "sub-h" }), entries, el("div", { class: "ctl-row" }, addEntry, saveEntries));
    return panel;
}

async function load() {
    statusEl.hidden = true;
    groupsEl.textContent = "";
    let data;
    try { data = await getJSON("/api/ui/upstreams"); }
    catch (err) { flash(`Could not load upstreams: ${err.message}`, true); return; }
    const groups = data.groups || [];
    if (!groups.length) {
        groupsEl.append(el("section", { class: "panel" },
            el("p", { class: "empty", text: "No per-group overrides yet — upstreams are governed by the raw config (Settings). Adding a group here starts overriding it for that group." })));
        return;
    }
    for (const g of groups) groupsEl.append(groupPanel(g, g.name === "default"));
}

applyBtn.addEventListener("click", async () => {
    try { await send("POST", "/api/ui/config/apply"); needsApply = false; applyBtn.hidden = true; flash("Applying — the resolver is rebuilding."); }
    catch (err) { flash(err.message, true); }
});

document.getElementById("add-group-btn").addEventListener("click", async () => {
    const name = prompt("New group name (client IP, CIDR or name):");
    if (!name) return;
    const n = name.trim();
    try {
        // the group row must exist before entries can be attached
        await send("PUT", `/api/ui/upstreams/groups/${encodeURIComponent(n)}`, { strategy: "parallel_best", hopMin: "0", hopMax: "0" });
        await send("PUT", `/api/ui/upstreams/groups/${encodeURIComponent(n)}/entries`, { entries: [{ address: "1.1.1.1", weight: 1, enabled: true }] });
        showApply(true); load();
    } catch (err) { flash(err.message, true); }
});

load();
