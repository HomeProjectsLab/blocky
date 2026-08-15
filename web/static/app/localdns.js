// localdns.js — structured editor for local DNS records (customDNS.zone).
// The server is authoritative; client-side checks are lenient hints only.
import { getJSON, send } from "./api.js";

const TYPES = ["A", "AAAA", "CNAME", "MX", "TXT", "SRV", "NS", "PTR", "CAA"];
const PLACEHOLDER = {
    A: "10.0.0.5", AAAA: "fd00::5", CNAME: "target.lan", MX: "10 mail.lan",
    TXT: "v=spf1 -all", SRV: "10 5 5060 sip.lan", NS: "ns1.lan", PTR: "host.lan",
    CAA: `0 issue "letsencrypt.org"`,
};

const statusEl = document.getElementById("ld-status");
const applyBtn = document.getElementById("ld-apply");
const tbody = document.getElementById("ld-body");
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

// ---- row rendering ----
function typeSelect(selected) {
    const sel = el("select");
    for (const t of TYPES) {
        const o = el("option", { text: t });
        if (t === selected) o.setAttribute("selected", "selected");
        sel.append(o);
    }
    return sel;
}

function addRow(rec = { name: "", type: "A", ttl: 3600, value: "" }) {
    const tr = el("tr");
    const name = el("input", { type: "text", value: rec.name || "", placeholder: "host.lan" });
    const type = typeSelect(rec.type || "A");
    const ttl = el("input", { type: "number", min: "0", value: String(rec.ttl ?? 3600), style: "width:5rem" });
    const value = el("input", { type: "text", value: rec.value || "", placeholder: PLACEHOLDER[rec.type] || "" });
    const err = el("div", { class: "err-line" });
    err.hidden = true;

    const syncPh = () => { value.placeholder = PLACEHOLDER[type.value] || ""; };
    type.addEventListener("change", syncPh);

    const del = el("button", { type: "button", class: "btn-icon btn-danger", title: "remove", text: "✕" });
    del.addEventListener("click", () => tr.remove());

    tr.append(
        el("td", {}, name),
        el("td", {}, type),
        el("td", {}, ttl),
        el("td", {}, value, err),
        el("td", {}, del),
    );
    tr._fields = { name, type, ttl, value, err };
    tbody.append(tr);
    return tr;
}

// ---- lenient client-side validation (server is authoritative) ----
function checkRow(f) {
    const name = f.name.value.trim();
    const type = f.type.value;
    const val = f.value.value.trim();
    if (!name || /\s/.test(name)) return "name required, no spaces";
    if (!/^\d+$/.test(f.ttl.value.trim() || "0")) return "ttl must be an integer ≥ 0";
    if (type === "A" && !/^\d+\.\d+\.\d+\.\d+$/.test(val)) return "A wants an IPv4 address";
    if (type === "AAAA" && !val.includes(":")) return "AAAA wants an IPv6 address";
    if (type === "MX" && val.split(/\s+/).length < 2) return "MX wants: <pref> <host>";
    if (type === "SRV" && val.split(/\s+/).length < 4) return "SRV wants: <pri> <weight> <port> <host>";
    if (type === "CAA" && val.split(/\s+/).length < 3) return "CAA wants: <flags> <tag> <value>";
    return "";
}

function collect() {
    const rows = [];
    let ok = true;
    for (const tr of tbody.querySelectorAll("tr")) {
        const f = tr._fields;
        const msg = checkRow(f);
        f.err.hidden = !msg;
        f.err.textContent = msg;
        if (msg) { ok = false; continue; }
        rows.push({
            name: f.name.value.trim(),
            type: f.type.value,
            ttl: parseInt(f.ttl.value.trim() || "3600", 10),
            value: f.value.value.trim(),
        });
    }
    return ok ? rows : null;
}

// ---- load / save ----
async function load() {
    let data;
    try { data = await getJSON("/api/ui/localdns"); }
    catch (err) { flash(`Could not load local DNS records: ${err.message}`, true); return; }
    tbody.textContent = "";
    for (const rec of data.records || []) addRow(rec);
    document.getElementById("ld-raw").value = data.zone || "";
}

document.getElementById("ld-add").addEventListener("click", () => addRow());

document.getElementById("ld-save").addEventListener("click", async () => {
    const records = collect();
    if (!records) { flash("Fix the highlighted rows first.", true); return; }
    try {
        const r = await send("PUT", "/api/ui/localdns", { records });
        if (r.needsApply) showApply(true);
        flash("Saved. Apply to activate.");
        load();
    } catch (err) { flash(err.message, true); }
});

document.getElementById("ld-raw-save").addEventListener("click", async () => {
    try {
        const r = await send("PUT", "/api/ui/localdns", { zone: document.getElementById("ld-raw").value });
        if (r.needsApply) showApply(true);
        flash("Saved raw zone. Apply to activate.");
        load();
    } catch (err) { flash(err.message, true); }
});

applyBtn.addEventListener("click", async () => {
    try {
        await send("POST", "/api/ui/config/apply");
        needsApply = false; applyBtn.hidden = true;
        flash("Applying — DNS resolution restarts briefly.");
    } catch (err) { flash(err.message, true); }
});

load();
