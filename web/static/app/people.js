// people.js — device→person mapping and per-person footprint (Phase 5, the most
// sensitive layer). Opt-in: the page is inert until profiling is enabled in
// Privacy. Mapping profiles a NAMED household member who did not consent, so the
// UI is honest about it and every mapping is one click from purged.
import { getJSON, send } from "./api.js";
import { fmtNum } from "./format.js";
import { promptDialog, toast } from "./modal.js";

const status = document.getElementById("pp-status");
const peopleBox = document.getElementById("pp-people");
const body = document.getElementById("pp-body");
const empty = document.getElementById("pp-empty");

function escapeHTML(s) { return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }

function deviceLabel(c) {
    const name = c.displayName || c.name || "—";
    return c.shared
        ? `${escapeHTML(name)} <span class="chip cc-server" title="Shared / NAT address — traffic here belongs to many devices, not one person.">⚠ shared</span>`
        : escapeHTML(name);
}

// setPerson maps (person set) or clears (person "") a device, then reloads.
async function setPerson(client, person) {
    try {
        await send("PUT", `/api/ui/clients/persons/${encodeURIComponent(client)}`, { person });
        toast(person.trim() ? `Mapped → ${person.trim()}` : "Mapping cleared");
        load();
    } catch (err) { toast("Could not save: " + err.message, { type: "error" }); }
}

function unmapButton(client) {
    const b = document.createElement("button");
    b.className = "btn-sub";
    b.textContent = "Un-map";
    b.addEventListener("click", () => setPerson(client, ""));
    return b;
}

function assignButton(c) {
    const b = document.createElement("button");
    b.className = "btn-sub";
    b.textContent = "Assign to person";
    if (c.shared) { b.disabled = true; b.title = "A shared / NAT address can't be mapped to one person."; return b; }
    b.addEventListener("click", async () => {
        const person = await promptDialog("Household member for this device", {
            title: "Map device to person", placeholder: "e.g. Alex",
        });
        if (person === null) return; // cancelled
        setPerson(c.name, person);
    });
    return b;
}

function personCard(fp) {
    const card = document.createElement("div");
    card.className = "fp-card";
    const head = document.createElement("div");
    head.className = "fp-head";
    head.innerHTML = `<span class="fp-guess">${escapeHTML(fp.person)}</span>` +
        `<span class="fp-count">${fmtNum(fp.queries)} queries · ${fmtNum(fp.blocked)} blocked</span>`;
    card.append(head);

    const list = document.createElement("div");
    list.className = "mix";
    for (const c of fp.clients || []) {
        const row = document.createElement("div");
        row.className = "mix-row";
        const meta = document.createElement("span");
        meta.className = "mix-name";
        meta.innerHTML = deviceLabel(c);
        const right = document.createElement("span");
        right.className = "mix-count";
        right.textContent = `${fmtNum(c.queries)} · ${fmtNum(c.blocked)} blocked`;
        row.append(meta, right, unmapButton(c.name));
        list.append(row);
    }
    card.append(list);
    return card;
}

async function load() {
    let data;
    try { data = await getJSON("/api/ui/people"); }
    catch (err) { status.hidden = false; status.textContent = "Could not load: " + err.message; return; }

    if (!data.enabled) {
        status.hidden = false;
        status.textContent = "Profiling is off. Person mapping is the most sensitive feature — turn on profiling in Privacy to use it.";
        peopleBox.innerHTML = "";
        body.innerHTML = "";
        empty.hidden = true;
        return;
    }
    status.hidden = true;

    const people = data.people || [];
    peopleBox.innerHTML = "";
    if (!people.length) {
        peopleBox.innerHTML = '<p class="empty">No one mapped yet. Assign a device below.</p>';
    } else {
        for (const fp of people) peopleBox.append(personCard(fp));
    }

    const rows = data.unassigned || [];
    body.innerHTML = "";
    empty.hidden = rows.length > 0;
    for (const c of rows) {
        const tr = document.createElement("tr");
        const dev = document.createElement("td");
        dev.innerHTML = deviceLabel(c);
        const q = document.createElement("td");
        q.className = "num";
        q.textContent = fmtNum(c.queries);
        const b = document.createElement("td");
        b.className = "num";
        b.textContent = fmtNum(c.blocked);
        const act = document.createElement("td");
        act.append(assignButton(c));
        tr.append(dev, q, b, act);
        body.append(tr);
    }
}

load();
