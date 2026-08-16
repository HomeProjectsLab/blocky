// blocking.js — ad-blocker management: runtime toggle, category grid,
// per-device profiles, manual allow/deny, block stats.
import { action, getJSON, send } from "./api.js";
import { confirmDialog, promptDialog } from "./modal.js";

const dot = document.getElementById("bl-dot");
const state = document.getElementById("bl-state");
const actionMsg = document.getElementById("bl-action");
const statusEl = document.getElementById("bl-status");
const applyBtn = document.getElementById("bl-apply");
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

function say(msg, isErr) {
    actionMsg.textContent = msg;
    actionMsg.style.color = isErr ? "var(--c-error)" : "var(--c-cached)";
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

const fmtCount = (n) => n >= 1e6 ? (n / 1e6).toFixed(1) + "M" : n >= 1e3 ? (n / 1e3).toFixed(0) + "k" : String(n);

applyBtn.addEventListener("click", async () => {
    try {
        await send("POST", "/api/ui/config/apply");
        needsApply = false; applyBtn.hidden = true;
        flash("Applying — the resolver is rebuilding with the new lists. This can take a few seconds.");
    } catch (err) { flash(err.message, true); }
});

// ---- runtime status (enable/disable is instant, no apply needed) ----
async function refreshStatus() {
    try {
        const s = await getJSON("/api/blocking/status");
        dot.className = "status-dot " + (s.enabled ? "on" : "off");
        if (s.enabled) {
            state.textContent = "Blocking is on.";
        } else {
            const grp = s.disabledGroups?.length ? ` (${s.disabledGroups.join(", ")})` : "";
            const secs = s.autoEnableInSec ? ` — back on in ${s.autoEnableInSec}s` : "";
            state.textContent = `Blocking is off${grp}${secs}.`;
        }
    } catch (err) { state.textContent = "Status unavailable: " + err.message; }
}

document.getElementById("bl-disable").addEventListener("click", async () => {
    try {
        await action("GET", "/api/blocking/disable", { duration: document.getElementById("bl-duration").value });
        say("Blocking disabled."); refreshStatus();
    } catch (err) { say(err.message, true); }
});

document.getElementById("bl-enable").addEventListener("click", async () => {
    try { await action("GET", "/api/blocking/enable"); say("Blocking enabled."); refreshStatus(); }
    catch (err) { say(err.message, true); }
});

document.getElementById("lists-refresh").addEventListener("click", async (ev) => {
    const btn = ev.currentTarget; btn.disabled = true;
    say("Reloading lists from the database…");
    try { await action("POST", "/api/lists/refresh"); say("Lists reloaded."); }
    catch (err) { say(err.message, true); }
    finally { btn.disabled = false; }
});

document.getElementById("cache-flush").addEventListener("click", async () => {
    try { await action("POST", "/api/cache/flush"); say("DNS cache flushed."); }
    catch (err) { say(err.message, true); }
});

// ---- main state ----
let data = { categories: [], segments: [], allow: [], deny: [], adlists: [] };
const catNames = () => data.categories.map((c) => c.name);

async function load() {
    try { data = await getJSON("/api/ui/blocking"); }
    catch (err) { flash(`Could not load blocking config: ${err.message}`, true); return; }
    renderCats();
    renderSegments();
    renderEntries();
    renderAdlists();
    renderTiles();
}

// ---- category grid ----
function renderCats() {
    const grid = document.getElementById("bl-cats");
    grid.textContent = "";
    if (!data.categories.length) {
        grid.append(el("p", { class: "empty", text: "No preloaded blocklists found — is the query log running in sqlite mode?" }));
        return;
    }
    for (const c of data.categories) {
        const cb = el("input", { type: "checkbox" });
        cb.checked = c.enabled;
        const card = el("label", { class: "cat-card" + (c.enabled ? " on" : "") }, cb,
            el("span", {},
                el("div", { class: "cat-name" }, c.name, " ", c.default ? el("span", { class: "cat-badge", text: "default" }) : ""),
                el("div", { class: "cat-count", text: `${fmtCount(c.domains)} domains` })),
        );
        cb.addEventListener("change", async () => {
            cb.disabled = true;
            try {
                const r = await send("PUT", `/api/ui/blocking/categories/${encodeURIComponent(c.name)}`, { enable: cb.checked });
                c.enabled = cb.checked;
                card.classList.toggle("on", c.enabled);
                if (r.needsApply) showApply(true);
                flash(`"${c.name}" ${c.enabled ? "enabled" : "disabled"}. Apply to activate.`);
                renderTiles();
            } catch (err) {
                cb.checked = !cb.checked;
                flash(err.message, true);
            } finally { cb.disabled = false; }
        });
        grid.append(card);
    }
}

// ---- per-device profiles (segmentation) ----
function segRow(seg) {
    const row = el("div", { class: "seg-row" });
    const del = el("button", { type: "button", class: "btn-icon btn-danger", title: "remove profile", text: "✕" });
    del.addEventListener("click", async () => {
        if (!(await confirmDialog(`Remove the profile for "${seg.client}"? It falls back to the global categories.`, { danger: true }))) return;
        try {
            const r = await send("PUT", `/api/ui/blocking/segments/${encodeURIComponent(seg.client)}`, { categories: [] });
            if (r.needsApply) showApply(true);
            load();
        } catch (err) { flash(err.message, true); }
    });
    const head = el("div", { class: "seg-head" },
        el("span", { class: "seg-client", text: seg.client }),
        el("span", { class: "spacer" }), del);
    const chips = el("div", { class: "seg-chips" });
    const selected = new Set(seg.categories);
    for (const name of catNames()) {
        const chip = el("button", { type: "button", class: "chip-toggle", text: name });
        chip.setAttribute("aria-pressed", selected.has(name) ? "true" : "false");
        chip.addEventListener("click", async () => {
            const on = chip.getAttribute("aria-pressed") !== "true";
            if (on) selected.add(name); else selected.delete(name);
            chip.setAttribute("aria-pressed", on ? "true" : "false");
            try {
                const r = await send("PUT", `/api/ui/blocking/segments/${encodeURIComponent(seg.client)}`,
                    { categories: [...selected] });
                if (r.needsApply) showApply(true);
                if (!selected.size) load(); // empty set removes the profile
            } catch (err) {
                // roll the chip back
                if (on) selected.delete(name); else selected.add(name);
                chip.setAttribute("aria-pressed", on ? "false" : "true");
                flash(err.message, true);
            }
        });
        chips.append(chip);
    }
    row.append(head, chips);
    return row;
}

function renderSegments() {
    const box = document.getElementById("bl-segments");
    box.textContent = "";
    if (!data.segments.length) {
        box.append(el("p", { class: "empty", text: "No device profiles yet — every client uses the global categories above." }));
        return;
    }
    for (const seg of [...data.segments].sort((a, b) => a.client.localeCompare(b.client))) box.append(segRow(seg));
}

document.getElementById("seg-add").addEventListener("click", async () => {
    const client = await promptDialog("Device to profile (client name, IP or CIDR — e.g. kids-tablet or 192.168.1.50):", { placeholder: "kids-tablet or 192.168.1.50" });
    if (!client) return;
    // start from the globally enabled categories so the profile is a tweak, not a reset
    const start = data.categories.filter((c) => c.enabled).map((c) => c.name);
    try {
        const r = await send("PUT", `/api/ui/blocking/segments/${encodeURIComponent(client.trim())}`, { categories: start });
        if (r.needsApply) showApply(true);
        load();
    } catch (err) { flash(err.message, true); }
});

// ---- manual allow/deny ----
// blocky list syntax → badge, derived (no stored type column):
// /re/ → regex, *.host → wildcard, else exact.
function entryType(d) {
    if (d.length > 1 && d.startsWith("/") && d.endsWith("/")) return "regex";
    if (d.startsWith("*.")) return "wildcard";
    return "exact";
}

function entryList(kind, items) {
    const ul = document.getElementById(`${kind}-list`);
    ul.textContent = "";
    if (!items.length) {
        ul.append(el("li", { class: "empty", text: "Nothing here yet." }));
        return;
    }
    for (const e of items) {
        const cb = el("input", { type: "checkbox", title: "enabled" });
        cb.checked = e.enabled;
        const badge = el("span", { class: "type-badge", text: entryType(e.domain) });
        const comment = el("span", { class: "entry-comment", title: "click to edit comment", text: e.comment || "add note…" });
        const rm = el("button", { type: "button", class: "btn-icon", title: "remove", text: "✕" });
        const li = el("li", { class: e.enabled ? "" : "off" },
            cb, el("span", { class: "entry-domain", text: e.domain }), badge, comment,
            el("span", { class: "spacer" }), rm);

        const put = async (enabled, cmt) => {
            const r = await send("PUT", `/api/ui/blocking/${kind}/${e.id}`, { enabled, comment: cmt });
            e.enabled = enabled; e.comment = cmt;
            li.classList.toggle("off", !enabled);
            comment.textContent = cmt || "add note…";
            if (r.needsApply) showApply(true);
        };
        cb.addEventListener("change", async () => {
            cb.disabled = true;
            try { await put(cb.checked, e.comment || ""); }
            catch (err) { cb.checked = !cb.checked; flash(err.message, true); }
            finally { cb.disabled = false; }
        });
        comment.addEventListener("click", async () => {
            const cmt = await promptDialog("Comment for this entry:", { value: e.comment || "" });
            if (cmt === null) return;
            try { await put(e.enabled, cmt.trim()); } catch (err) { flash(err.message, true); }
        });
        rm.addEventListener("click", async () => {
            try {
                const r = await send("DELETE", `/api/ui/blocking/${kind}/${e.id}`);
                if (r.needsApply) showApply(true);
                load();
            } catch (err) { flash(err.message, true); }
        });
        ul.append(li);
    }
}

function renderEntries() {
    entryList("allow", data.allow);
    entryList("deny", data.deny);
}

// ---- blocklist URLs (adlists) ----
function renderAdlists() {
    const ul = document.getElementById("adlist-list");
    ul.textContent = "";
    if (!data.adlists.length) {
        ul.append(el("li", { class: "empty", text: "No blocklist URLs yet." }));
        return;
    }
    for (const a of data.adlists) {
        const cb = el("input", { type: "checkbox", title: "enabled" });
        cb.checked = a.enabled;
        const comment = el("span", { class: "entry-comment", title: "click to edit comment", text: a.comment || "add note…" });
        const rm = el("button", { type: "button", class: "btn-icon", title: "remove", text: "✕" });
        const li = el("li", { class: a.enabled ? "" : "off" },
            cb, el("span", { class: "entry-domain", text: a.url }), comment,
            el("span", { class: "spacer" }), rm);

        cb.addEventListener("change", async () => {
            cb.disabled = true;
            try {
                const r = await send("PUT", `/api/ui/blocking/adlists/${a.id}`, { enabled: cb.checked });
                a.enabled = cb.checked; li.classList.toggle("off", !cb.checked);
                if (r.needsApply) showApply(true);
            } catch (err) { cb.checked = !cb.checked; flash(err.message, true); }
            finally { cb.disabled = false; }
        });
        comment.addEventListener("click", async () => {
            const cmt = await promptDialog("Comment for this list:", { value: a.comment || "" });
            if (cmt === null) return;
            try {
                const r = await send("PUT", `/api/ui/blocking/adlists/${a.id}`, { url: a.url, comment: cmt.trim() });
                a.comment = cmt.trim(); comment.textContent = a.comment || "add note…";
                if (r.needsApply) showApply(true);
            } catch (err) { flash(err.message, true); }
        });
        rm.addEventListener("click", async () => {
            if (!(await confirmDialog(`Remove blocklist URL "${a.url}"?`, { danger: true }))) return;
            try {
                const r = await send("DELETE", `/api/ui/blocking/adlists/${a.id}`);
                if (r.needsApply) showApply(true);
                load();
            } catch (err) { flash(err.message, true); }
        });
        ul.append(li);
    }
}

document.getElementById("adlist-add").addEventListener("click", async () => {
    const urlEl = document.getElementById("adlist-url");
    const commentEl = document.getElementById("adlist-comment");
    const url = urlEl.value.trim();
    if (!url) return;
    try {
        const r = await send("POST", "/api/ui/blocking/adlists", { url, comment: commentEl.value.trim() });
        urlEl.value = ""; commentEl.value = "";
        if (r.needsApply) showApply(true);
        load();
    } catch (err) { flash(err.message, true); }
});

function wireAdd(kind) {
    const input = document.getElementById(`${kind}-in`);
    const add = async () => {
        const domain = input.value.trim();
        if (!domain) return;
        try {
            // domain may be one entry or a whole pasted list (space/comma/newline
            // separated); the server splits it.
            const r = await send("POST", `/api/ui/blocking/${kind}`, { group: "manual", domain });
            input.value = "";
            if (r.added > 1 || (r.skipped && r.skipped.length)) {
                const msg = `Added ${r.added}` + (r.skipped && r.skipped.length ? `, skipped ${r.skipped.length} invalid` : "");
                flash(msg, r.skipped && r.skipped.length > 0);
            }
            if (r.needsApply) showApply(true);
            load();
        } catch (err) { flash(err.message, true); }
    };
    document.getElementById(`${kind}-add`).addEventListener("click", add);
    // Enter adds; Shift+Enter inserts a newline (so a multi-line list can be pasted/typed).
    input.addEventListener("keydown", (ev) => {
        if (ev.key === "Enter" && !ev.shiftKey) { ev.preventDefault(); add(); }
    });
}
wireAdd("allow");
wireAdd("deny");

// ---- stats: tiles, top blocked, blocks by list ----
function renderTiles() {
    const active = data.categories.filter((c) => c.enabled);
    document.getElementById("bt-lists").textContent = String(active.length);
    document.getElementById("bt-domains").textContent = fmtCount(active.reduce((n, c) => n + c.domains, 0));
}

function barList(elId, items) {
    const ol = document.getElementById(elId);
    ol.textContent = "";
    const max = Math.max(1, ...items.map((i) => i.count));
    for (const it of items) {
        ol.append(el("li", {},
            el("div", { class: "bar-row" }, el("span", { class: "bar-name", text: it.name }), el("span", { class: "bar-count", text: String(it.count) })),
            el("div", { class: "bar-track" }, el("div", { class: "bar-fill", style: `width:${(100 * it.count / max).toFixed(1)}%` }))));
    }
}

async function loadStats() {
    try {
        const o = await getJSON("/api/ui/stats/overview");
        document.getElementById("bt-blocked").textContent = fmtCount(o.blocked);
        document.getElementById("bt-rate").textContent = o.queries ? (100 * o.blocked / o.queries).toFixed(1) + "%" : "—";
    } catch { /* tiles stay em-dash without a sqlite query log */ }

    try {
        const top = await getJSON("/api/ui/stats/top", { col: "blocked", n: 10 });
        barList("bl-top", (top.items || []).map((i) => ({ name: i.name, count: i.count })));
    } catch { /* ignore */ }

    // group attribution comes from recent block reasons: "BLOCKED (ads, manual)"
    try {
        const q = await getJSON("/api/ui/queries", { rtype: "BLOCKED", limit: 200 });
        const byGroup = {};
        for (const item of q.items || []) {
            const m = /\(([^)]+)\)/.exec(item.reason || "");
            if (!m) continue;
            for (const g of m[1].split(",")) {
                const name = g.split(":")[0].trim();
                if (name) byGroup[name] = (byGroup[name] || 0) + 1;
            }
        }
        const items = Object.entries(byGroup).map(([name, count]) => ({ name, count }))
            .sort((a, b) => b.count - a.count).slice(0, 10);
        document.getElementById("bl-bygroup-empty").hidden = items.length > 0;
        barList("bl-bygroup", items);
    } catch { document.getElementById("bl-bygroup-empty").hidden = false; }
}

refreshStatus();
load();
loadStats();
