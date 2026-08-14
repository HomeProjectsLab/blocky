// clients.js — client list + drill-down with the fingerprint panel, plus the
// device-class table (auto class + manual override).
import { getJSON, send } from "./api.js";
import { lineChart } from "./chart.js";
import { fmtNum, fmtDateTime, fmtPct } from "./format.js";

const body = document.getElementById("cl-body");
const empty = document.getElementById("cl-empty");
const drawer = document.getElementById("cl-drawer");
const detail = document.getElementById("cl-detail");
const title = document.getElementById("cl-drawer-title");

document.getElementById("cl-drawer-close").addEventListener("click", () => { drawer.hidden = true; });
document.addEventListener("keydown", (e) => { if (e.key === "Escape") drawer.hidden = true; });

// Resolver-software best-guess: a small heuristic over the fingerprint signals.
// Deliberately labelled "best guess" — a fingerprint is suggestive, not proof.
function guessSoftware(fp) {
    const t = (fp.transport || "").toLowerCase();
    const ua = (fp.dohUserAgent || "").toLowerCase();
    const codes = (fp.ednsOptCodes || "").split(",").filter(Boolean);
    const hasCookie = fp.hasCookie || codes.includes("10");

    if (t.includes("doh") || t.includes("https")) {
        if (ua.includes("firefox")) return "Firefox (DoH)";
        if (ua.includes("chrome") || ua.includes("chromium")) return "Chrome/Chromium (DoH)";
        if (ua.includes("cloudflared") || ua.includes("dnscrypt")) return "DoH forwarder (cloudflared/dnscrypt)";
        if (ua.includes("go-http") || ua.includes("okhttp")) return "App/library DoH client";
        return "DoH client";
    }
    if (t.includes("dot") || t.includes("tls")) {
        return "DoT stub — likely Android Private DNS or systemd-resolved";
    }
    // plain Do53
    if (!fp.hadEdns0) return "Minimal stub (no EDNS) — old/embedded device or musl libc";
    if (hasCookie && fp.ednsUdpSize === 1232) return "systemd-resolved / recent Unbound (EDNS cookie, 1232)";
    if (fp.ednsUdpSize === 1232) return "DNS-flag-day stub (1232) — systemd-resolved, Knot, recent Unbound";
    if (fp.ednsUdpSize === 4096 && fp.do) return "glibc / dig / BIND-style resolver (4096, DO)";
    if (fp.ednsUdpSize === 512) return "Conservative stub (512 UDP)";
    return "Generic EDNS-capable stub";
}

function transportMix(transports, total) {
    const wrap = document.createElement("div");
    wrap.className = "mix";
    for (const t of transports) {
        const row = document.createElement("div");
        row.className = "mix-row";
        row.innerHTML =
            `<span class="mix-name">${t.name ? escapeHTML(t.name) : "—"}</span>` +
            `<span class="mix-count">${fmtNum(t.count)} · ${fmtPct(t.count, total)}</span>`;
        const track = document.createElement("div");
        track.className = "bar-track";
        const fill = document.createElement("div");
        fill.className = "bar-fill";
        fill.style.width = fmtPct(t.count, total);
        track.append(fill);
        wrap.append(row, track);
    }
    return wrap;
}

function fpPanel(fingerprints) {
    const wrap = document.createElement("div");
    if (!fingerprints.length) {
        wrap.innerHTML = '<p class="empty">No fingerprint captured for this client yet.</p>';
        return wrap;
    }
    for (const fp of fingerprints) {
        const card = document.createElement("div");
        card.className = "fp-card";
        const feats = [
            fp.transport,
            fp.hadEdns0 ? `EDNS0 ${fp.ednsUdpSize || 0}` : "no EDNS0",
            fp.do ? "DO" : null,
            fp.hasCookie ? "cookie" : null,
            fp.ednsOptCodes ? `opts ${fp.ednsOptCodes}` : null,
            fp.dohUserAgent || null,
        ].filter(Boolean);
        card.innerHTML =
            `<div class="fp-head"><span class="fp-guess">${guessSoftware(fp)}</span>` +
            `<span class="fp-count">${fmtNum(fp.count)}</span></div>` +
            `<div class="fp-hash">${escapeHTML(fp.fpHash || "")}</div>` +
            `<div class="fp-feats">${feats.map((f) => `<span class="chip">${escapeHTML(f)}</span>`).join("")}</div>`;
        wrap.append(card);
    }
    const note = document.createElement("p");
    note.className = "empty";
    note.textContent = "Resolver software is a best guess from the query fingerprint, not a definitive identification.";
    wrap.append(note);
    return wrap;
}

function escapeHTML(s) { return String(s).replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c])); }

function section(heading) {
    const h = document.createElement("h2");
    h.className = "sub-h";
    h.textContent = heading;
    return h;
}

function domainList(domains) {
    const ol = document.createElement("ol");
    ol.className = "bar-list";
    const maxc = Math.max(1, ...domains.map((d) => d.count));
    for (const d of domains) {
        const li = document.createElement("li");
        li.innerHTML = `<div class="bar-row"><span class="bar-name">${escapeHTML(d.name)}</span><span class="bar-count">${fmtNum(d.count)}</span></div>` +
            `<div class="bar-track"><div class="bar-fill" style="width:${(d.count / maxc) * 100}%"></div></div>`;
        ol.append(li);
    }
    return ol;
}

async function openDetail(name) {
    title.textContent = name;
    detail.innerHTML = '<p class="empty">Loading…</p>';
    drawer.hidden = false;
    let d;
    try { d = await getJSON(`/api/ui/clients/${encodeURIComponent(name)}`); }
    catch (err) { detail.innerHTML = `<p class="empty">Could not load: ${err.message}</p>`; return; }

    detail.innerHTML = "";
    const stat = document.createElement("p");
    stat.className = "empty";
    stat.textContent = `${fmtNum(d.queries)} queries · ${fmtNum(d.blocked)} blocked (last 24h)`;
    detail.append(stat);

    if (d.history && d.history.length > 1) {
        detail.append(section("Activity"));
        const chartEl = document.createElement("div");
        chartEl.className = "chart spark";
        detail.append(chartEl);
        const xs = d.history.map((h) => h.ts);
        const ys = d.history.map((h) => (h.counts && h.counts.queries) || 0);
        // defer so the element has a width
        requestAnimationFrame(() => lineChart(chartEl, {
            labels: ["queries"], colors: ["var(--c-resolved)"], data: [xs, ys], fmtVal: fmtNum,
        }));
    }

    detail.append(section("Fingerprint · who this client is"));
    detail.append(fpPanel(d.fingerprints || []));

    detail.append(section("Transport mix"));
    detail.append(transportMix(d.transports || [], d.queries || 1));

    detail.append(section("Top domains"));
    detail.append((d.topDomains && d.topDomains.length) ? domainList(d.topDomains) : Object.assign(document.createElement("p"), { className: "empty", textContent: "No domains recorded." }));
}

// ---- device-class table ----
const ccBody = document.getElementById("cc-body");
const ccEmpty = document.getElementById("cc-empty");
const ccMsg = document.getElementById("cc-msg");
const CLASS_OPTS = ["auto", "iot", "workstation", "server", "unknown"];

function classChip(cls) {
    return `<span class="chip cc-${cls || "unknown"}">${escapeHTML(cls || "unknown")}</span>`;
}

function overrideSelect(client, override) {
    const sel = document.createElement("select");
    sel.className = "cc-override";
    for (const opt of CLASS_OPTS) {
        const o = document.createElement("option");
        o.value = opt === "auto" ? "" : opt;
        o.textContent = opt;
        if ((override || "") === o.value) o.selected = true;
        sel.append(o);
    }
    sel.addEventListener("change", async () => {
        try {
            await send("PUT", `/api/ui/clients/classes/${encodeURIComponent(client)}`, { class: sel.value || "auto" });
            ccMsg.textContent = `Set ${client} → ${sel.value || "auto"}.`;
            loadClasses();
        } catch (err) { ccMsg.textContent = "Could not save: " + err.message; }
    });
    return sel;
}

async function loadClasses() {
    let data;
    try { data = await getJSON("/api/ui/clients/classes"); }
    catch (err) { ccEmpty.hidden = false; ccEmpty.textContent = "Could not load classes: " + err.message; return; }
    const rows = data.classes || [];
    if (!rows.length) { ccEmpty.hidden = false; ccBody.innerHTML = ""; return; }
    ccEmpty.hidden = true;
    ccBody.innerHTML = "";
    for (const c of rows) {
        const tr = document.createElement("tr");
        tr.innerHTML =
            `<td>${escapeHTML(c.client || "—")}</td>` +
            `<td>${classChip(c.class)}</td>` +
            `<td>${classChip(c.effective)}</td>`;
        const td = document.createElement("td");
        td.append(overrideSelect(c.client, c.override));
        tr.append(td);
        ccBody.append(tr);
    }
}

async function load() {
    let data;
    try { data = await getJSON("/api/ui/clients"); }
    catch (err) { empty.hidden = false; empty.textContent = "Could not load clients: " + err.message; return; }
    const rows = data.clients || [];
    if (!rows.length) { empty.hidden = false; return; }
    empty.hidden = true;
    body.innerHTML = "";
    for (const c of rows) {
        const tr = document.createElement("tr");
        tr.innerHTML =
            `<td>${escapeHTML(c.name || "—")}</td>` +
            `<td class="num">${fmtNum(c.queries)}</td>` +
            `<td class="num">${fmtNum(c.blocked)}</td>` +
            `<td>${c.lastSeen ? fmtDateTime(c.lastSeen) : "—"}</td>`;
        tr.addEventListener("click", () => openDetail(c.name));
        body.append(tr);
    }
}

load();
loadClasses();
