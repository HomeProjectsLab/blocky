// groups.js — household groups: bundle categories, add member devices, toggle
// live. Composes blocky's native client-groups; changes take effect on Apply.
import { getJSON, send } from "./api.js";
import { confirmDialog, promptDialog } from "./modal.js";

const applyBtn = document.getElementById("gr-apply");
const statusEl = document.getElementById("gr-status");
const listEl = document.getElementById("gr-list");
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

applyBtn.addEventListener("click", async () => {
    try {
        await send("POST", "/api/ui/config/apply");
        needsApply = false; applyBtn.hidden = true;
        flash("Applying — the resolver is rebuilding with the new groups. This can take a few seconds.");
    } catch (err) { flash(err.message, true); }
});

let data = { groups: [], categories: [] };

async function load() {
    try { data = await getJSON("/api/ui/blocking/groups"); }
    catch (err) { flash(`Could not load groups: ${err.message}`, true); return; }
    render();
}

// split a pasted textarea into member tokens: whitespace/comma separated.
const splitMembers = (s) => s.split(/[\s,]+/).map((t) => t.trim()).filter(Boolean);

function groupCard(g) {
    const card = el("section", { class: "panel" + (g.enabled ? "" : " off") });

    const enable = el("input", { type: "checkbox", title: "enabled" });
    enable.checked = g.enabled;
    enable.addEventListener("change", async () => {
        enable.disabled = true;
        try {
            const r = await send("PUT", `/api/ui/blocking/groups/${encodeURIComponent(g.name)}/enabled`, { enable: enable.checked });
            g.enabled = enable.checked;
            card.classList.toggle("off", !g.enabled);
            if (r.needsApply) showApply(true);
            flash(`Group "${g.name}" ${g.enabled ? "enabled" : "disabled"}. Apply to activate.`);
        } catch (err) { enable.checked = !enable.checked; flash(err.message, true); }
        finally { enable.disabled = false; }
    });

    const del = el("button", { type: "button", class: "btn-icon btn-danger", title: "remove group", text: "✕" });
    del.addEventListener("click", async () => {
        if (!(await confirmDialog(`Remove group "${g.name}"? Its members fall back to the global categories.`, { danger: true }))) return;
        try {
            const r = await send("DELETE", `/api/ui/blocking/groups/${encodeURIComponent(g.name)}`);
            if (r.needsApply) showApply(true);
            load();
        } catch (err) { flash(err.message, true); }
    });

    const head = el("div", { class: "ctl-row" },
        el("label", { class: "dt-label" }, enable, " ", el("strong", { text: g.name })),
        el("span", { class: "spacer" }), del);

    // category picker
    const selected = new Set(g.categories);
    const chips = el("div", { class: "seg-chips" });
    for (const name of data.categories) {
        const chip = el("button", { type: "button", class: "chip-toggle", text: name });
        chip.setAttribute("aria-pressed", selected.has(name) ? "true" : "false");
        chip.addEventListener("click", async () => {
            const on = chip.getAttribute("aria-pressed") !== "true";
            if (on) selected.add(name); else selected.delete(name);
            chip.setAttribute("aria-pressed", on ? "true" : "false");
            try {
                const r = await send("PUT", `/api/ui/blocking/groups/${encodeURIComponent(g.name)}`, { categories: [...selected] });
                g.categories = [...selected];
                if (r.needsApply) showApply(true);
            } catch (err) {
                if (on) selected.delete(name); else selected.add(name);
                chip.setAttribute("aria-pressed", on ? "false" : "true");
                flash(err.message, true);
            }
        });
        chips.append(chip);
    }

    // members textarea + save
    const ta = el("textarea", { rows: "3", spellcheck: "false", autocomplete: "off",
        placeholder: "one device per line — client name, IP or small CIDR (e.g. kids-tablet, 192.168.1.50, 192.168.1.0/24)" });
    ta.value = g.members.join("\n");
    const save = el("button", { type: "button", class: "btn-sub", text: "Save members" });
    save.addEventListener("click", async () => {
        const members = splitMembers(ta.value);
        try {
            const r = await send("PUT", `/api/ui/blocking/groups/${encodeURIComponent(g.name)}/members`, { members });
            g.members = members; ta.value = members.join("\n");
            if (r.needsApply) showApply(true);
            flash(`Members of "${g.name}" saved. Apply to activate.`);
        } catch (err) { flash(err.message, true); }
    });

    card.append(head,
        el("p", { class: "empty", text: "Categories to block for this group's members:" }), chips,
        el("p", { class: "empty", text: "Member devices:" }), ta,
        el("div", { class: "ctl-row" }, save));
    return card;
}

function render() {
    listEl.textContent = "";
    if (!data.groups.length) {
        listEl.append(el("p", { class: "empty", text: "No groups yet — add one below." }));
        return;
    }
    for (const g of [...data.groups].sort((a, b) => a.name.localeCompare(b.name))) listEl.append(groupCard(g));
}

document.getElementById("gr-add").addEventListener("click", async () => {
    const name = await promptDialog("Group name (e.g. Kids, Guest, Office):", { placeholder: "Kids" });
    if (!name || !name.trim()) return;
    try {
        // create with no categories yet; the picker fills them in
        const r = await send("PUT", `/api/ui/blocking/groups/${encodeURIComponent(name.trim())}`, { categories: [] });
        if (r.needsApply) showApply(true);
        load();
    } catch (err) { flash(err.message, true); }
});

load();
