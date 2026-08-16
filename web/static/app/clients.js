// clients.js — client list + drill-down with the fingerprint panel, plus the
// device-class table (auto class + manual override).
import { getJSON, send } from "./api.js";
import { lineChart, cssTok } from "./chart.js";
import { fmtNum, fmtDateTime, fmtPct } from "./format.js";
import { promptDialog, toast } from "./modal.js";

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

function escapeHTML(s) { return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }

// DNS-native identity chips: distinct IP(s) (deduped against the name/PTR) plus
// the multi-facet device recognition (OS badge + vendor/model/app chips). These
// are a *traffic heuristic* — a DNS signature, suggestive not proof — distinct
// from the behaviour-classifier device class shown in the classes table above.
// R3 NAT-gate: a shared/NAT identity NEVER shows per-device facets (the union of
// every device behind one address is meaningless) — it shows "shared / N devices".
const HEUR = "traffic heuristic — DNS signature, suggestive not proof";
function identityHTML(c) {
    const chips = [];
    const ips = (c.ips || []).filter((ip) => ip && ip !== c.name);
    for (const ip of ips) chips.push(`<span class="chip">${escapeHTML(ip)}</span>`);

    if (c.shared || c.natAggregate) {
        const label = c.sharedLabel || `shared / ${fmtNum(c.fpCount || 0)} devices`;
        chips.push(`<span class="chip cc-server" title="Many devices share this one address — traffic here can't be split per-device.">⚠ ${escapeHTML(label)}</span>`);
        return chips.join(" ");
    }

    if (c.os) chips.push(`<span class="chip os-badge" title="OS · ${HEUR}">${escapeHTML(c.os)}</span>`);
    for (const v of c.vendor || []) chips.push(`<span class="chip" title="Vendor · ${HEUR}">${escapeHTML(v)}</span>`);
    for (const m of c.model || []) chips.push(`<span class="chip" title="Model · ${HEUR}">${escapeHTML(m)}</span>`);
    for (const a of c.apps || []) chips.push(`<span class="chip" title="App · ${HEUR}">${escapeHTML(a)}</span>`);
    // Fallback: legacy single-label guess if no structured facet surfaced.
    if (!c.os && !(c.vendor || []).length && !(c.apps || []).length && c.deviceGuess) {
        chips.push(`<span class="chip os-badge" title="${HEUR}">${escapeHTML(c.deviceGuess)}</span>`);
    }
    if (c.fpCount) chips.push(`<span class="chip">${fmtNum(c.fpCount)} fingerprints</span>`);
    return chips.join(" ");
}

// presenceHeatmap: a 24-cell hour-of-day strip (local time) shaded by relative
// activity, so the device's wake/sleep/work rhythm reads at a glance. Data is the
// precomputed, opt-in presence histogram (absent unless profiling is enabled).
function presenceHeatmap(presence) {
    const hrs = presence.hourLocal || [];
    const max = Math.max(1, ...hrs);
    const wrap = document.createElement("div");
    wrap.className = "presence-grid";
    wrap.style.display = "grid";
    wrap.style.gridTemplateColumns = "repeat(24, 1fr)";
    wrap.style.gap = "2px";
    for (let h = 0; h < 24; h++) {
        const v = hrs[h] || 0;
        const cell = document.createElement("div");
        // intensity 0..1 → opacity of the accent colour; empty hours stay faint.
        const t = v / max;
        cell.style.background = v ? `color-mix(in srgb, var(--c-resolved) ${Math.round(15 + t * 85)}%, transparent)` : "var(--bg2)";
        cell.style.aspectRatio = "1";
        cell.style.borderRadius = "2px";
        cell.title = `${h.toString().padStart(2, "0")}:00 — ${fmtNum(v)} queries`;
        wrap.append(cell);
    }
    const box = document.createElement("div");
    box.append(wrap);
    const legend = document.createElement("p");
    legend.className = "empty";
    legend.textContent = `Local hour of day (${presence.tz || "UTC"}) — darker = more active. Opt-in, computed on the box, never exported.`;
    box.append(legend);
    return box;
}

// categoryChips: the opt-in activity categories (streaming/social/…) derived
// read-time from this client's DNS names. Absent unless profiling is enabled.
function categoryChips(cats) {
    const box = document.createElement("div");
    box.className = "fp-feats";
    box.innerHTML = cats.map((c) =>
        `<span class="chip" title="Activity category · opt-in, from DNS names, computed on the box, never exported">${escapeHTML(c)}</span>`).join(" ");
    return box;
}

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

let detailChart = null;
let rebuildSpark = null; // rebuilds the drawer sparkline with fresh theme tokens
addEventListener("themechange", () => { if (rebuildSpark && !drawer.hidden) rebuildSpark(); });

// nameEditor: a manual display-name override for this client. Hidden for a
// shared/NAT aggregate — a per-device name is meaningless for many devices
// behind one identity, and the server rejects it anyway (blueprint R3).
function nameEditor(d) {
    const wrap = document.createElement("div");
    wrap.className = "cl-name-edit";
    const cur = d.displayName || "";
    const lbl = document.createElement("span");
    lbl.className = "empty";
    lbl.textContent = cur ? `Custom name: ${cur}` : "No custom name";
    const btn = document.createElement("button");
    btn.className = "btn-sub";
    btn.textContent = cur ? "Rename" : "Set name";
    btn.addEventListener("click", async () => {
        const name = await promptDialog("Display name for this client", {
            title: "Rename client", value: cur, placeholder: "e.g. Alex's iPhone",
        });
        if (name === null) return; // cancelled
        try {
            await send("PUT", `/api/ui/clients/names/${encodeURIComponent(d.name)}`, { name });
            toast(name.trim() ? `Named → ${name.trim()}` : "Custom name cleared");
            openDetail(d.name); // reload drawer with the new name
            load();             // refresh the list underneath
        } catch (err) { toast("Could not save: " + err.message, { type: "error" }); }
    });
    wrap.append(lbl, btn);
    return wrap;
}

async function openDetail(name) {
    if (detailChart) { detailChart.destroy(); detailChart = null; }
    rebuildSpark = null;
    title.textContent = name;
    detail.innerHTML = '<p class="empty">Loading…</p>';
    drawer.hidden = false;
    let d;
    try { d = await getJSON(`/api/ui/clients/${encodeURIComponent(name)}`); }
    catch (err) { detail.innerHTML = '<p class="empty"></p>'; detail.firstChild.textContent = "Could not load: " + err.message; return; }

    title.textContent = d.displayName || name;
    detail.innerHTML = "";
    const stat = document.createElement("p");
    stat.className = "empty";
    stat.textContent = `${fmtNum(d.queries)} queries · ${fmtNum(d.blocked)} blocked (last 24h)`;
    detail.append(stat);
    if (!(d.shared || d.natAggregate)) detail.append(nameEditor(d));

    const idHTML = identityHTML(d);
    if (idHTML) {
        const idRow = document.createElement("div");
        idRow.className = "fp-feats cl-identity";
        idRow.innerHTML = idHTML;
        detail.append(idRow);
    }

    if (d.history && d.history.length > 1) {
        detail.append(section("Activity"));
        const chartEl = document.createElement("div");
        chartEl.className = "chart spark";
        detail.append(chartEl);
        const xs = d.history.map((h) => h.ts);
        const ys = d.history.map((h) => (h.counts && h.counts.queries) || 0);
        // defer so the element has a width. Canvas can't resolve CSS vars —
        // resolve the token first. height matches the 120px .spark box.
        // rebuildSpark: a theme toggle re-runs this with fresh tokens.
        rebuildSpark = () => {
            if (detailChart) detailChart.destroy();
            chartEl.innerHTML = "";
            detailChart = lineChart(chartEl, {
                labels: ["queries"], colors: [cssTok("--c-resolved")], data: [xs, ys], height: 120, fmtVal: fmtNum,
            });
        };
        requestAnimationFrame(() => { if (rebuildSpark) rebuildSpark(); });
    }

    if (d.presence) {
        detail.append(section("Presence · when this device is active"));
        detail.append(presenceHeatmap(d.presence));
    }

    if (d.categories && d.categories.length) {
        detail.append(section("Activity categories"));
        detail.append(categoryChips(d.categories));
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
let classRetried = false; // one-shot re-poll guard for the cache-served class table

function classChip(cls) {
    return `<span class="chip cc-${cls || "unknown"}" title="device class · behaviour classifier — inferred from how this client queries DNS">${escapeHTML(cls || "unknown")}</span>`;
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
    if (!rows.length) {
        ccEmpty.hidden = false; ccBody.innerHTML = "";
        // Classes are cache-served: a cold box recomputes them in a background
        // goroutine, so the first fetch can be empty. Re-poll once so the table
        // fills in without a manual reload (identity chips in load() are fresh).
        if (!classRetried) { classRetried = true; setTimeout(loadClasses, 6000); }
        return;
    }
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
            `<td>${escapeHTML(c.displayName || c.name || "—")}</td>` +
            `<td class="cl-identity">${identityHTML(c) || "—"}</td>` +
            `<td class="num">${fmtNum(c.queries)}</td>` +
            `<td class="num">${fmtNum(c.blocked)}</td>` +
            `<td>${c.lastSeen ? fmtDateTime(c.lastSeen) : "—"}</td>`;
        tr.addEventListener("click", () => openDetail(c.name));
        body.append(tr);
    }
}

load();
loadClasses();
