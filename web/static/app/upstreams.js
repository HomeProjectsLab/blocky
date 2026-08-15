// upstreams.js — upstream group management (list, strategy, entries, apply).
import { getJSON, send, action } from "./api.js";
import { confirmDialog, promptDialog } from "./modal.js";

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
            if (!(await confirmDialog(`Delete upstream group "${g.name}"?`, { danger: true }))) return;
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

// ── Conditional forwarding ────────────────────────────────────────────────
// conditional.mapping.<domain> = <resolver(s)>. Own panel, own PUT endpoint.
const condEl = el("div");
groupsEl.after(condEl);

function condRow(domain, upstreams) {
    const row = el("div", { class: "entry-row" });
    const label = el("span", { class: "group-name", text: domain });
    const value = el("span", { class: "strat-desc", text: upstreams.join(", ") });
    const edit = el("button", { type: "button", class: "btn-icon", title: "edit", text: "✎" });
    const rm = el("button", { type: "button", class: "btn-icon", title: "remove", text: "✕" });
    edit.addEventListener("click", () => putCond(domain, upstreams.join(", ")));
    rm.addEventListener("click", async () => {
        if (!(await confirmDialog(`Remove conditional mapping for "${domain}"?`, { danger: true }))) return;
        await sendCond({ domain, upstreams: [] });
    });
    row.append(label, el("span", { class: "spacer" }), value, edit, rm);
    return row;
}

async function sendCond(body) {
    try {
        const r = await send("PUT", "/api/ui/upstreams/conditional", body);
        if (r.needsApply) showApply(true);
        loadCond();
    } catch (err) { flash(err.message, true); }
}

async function putCond(domain, currentUpstreams) {
    const dom = domain ?? (await promptDialog("Domain to forward (e.g. fritz.box, home)", {}))?.trim();
    if (!dom) return;
    const val = await promptDialog(`Resolver(s) for "${dom}", comma-separated`, { value: currentUpstreams || "" });
    if (val == null) return;
    const upstreams = val.split(",").map((s) => s.trim()).filter(Boolean);
    if (!upstreams.length) { flash("Enter at least one resolver (or delete the mapping).", true); return; }
    await sendCond({ domain: dom, upstreams });
}

async function loadCond() {
    condEl.textContent = "";
    let data;
    try { data = await getJSON("/api/ui/upstreams/conditional"); }
    catch (err) { flash(`Could not load conditional forwarding: ${err.message}`, true); return; }
    const mapping = data.mapping || {};
    const panel = el("section", { class: "panel" });
    panel.append(el("h2", { text: "Conditional forwarding", class: "sub-h" }));
    const domains = Object.keys(mapping).sort();
    if (!domains.length) {
        panel.append(el("p", { class: "empty", text: "No conditional mappings — queries for a matching domain go to a specific resolver instead of the normal upstreams." }));
    } else {
        for (const d of domains) panel.append(condRow(d, mapping[d]));
    }
    const add = el("button", { type: "button", class: "btn-sub", text: "+ Add mapping" });
    add.addEventListener("click", () => putCond());
    const router = el("button", { type: "button", class: "btn-sub", text: "Send reverse-DNS + local names to my router" });
    router.addEventListener("click", routerHelper);
    panel.append(el("div", { class: "ctl-row" }, add, router));
    condEl.append(panel);
}

// One click: point in-addr.arpa, ip6.arpa and a local domain at the router's
// resolver, so reverse lookups and bare LAN hostnames resolve.
async function routerHelper() {
    const ip = (await promptDialog("Router IP (its DNS resolver)", { value: "192.168.1.1" }))?.trim();
    if (!ip) return;
    const local = (await promptDialog("Local domain for LAN hostnames", { value: "fritz.box" }))?.trim();
    if (!local) return;
    for (const dom of ["in-addr.arpa", "ip6.arpa", local]) {
        await sendCond({ domain: dom, upstreams: [ip] });
    }
    flash(`Reverse DNS + "${local}" now forward to ${ip}.`);
}

loadCond();

document.getElementById("add-group-btn").addEventListener("click", async () => {
    const name = await promptDialog("New group name (client IP, CIDR or name)", {});
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
