// queries.js — query explorer: filter form, paginated table, CSV export.
import { getJSON } from "./api.js";
import { fmtDateTime, band } from "./format.js";

const LIMIT = 50;

const form = document.getElementById("qform");
const body = document.getElementById("qtable-body");
const emptyMsg = document.getElementById("q-empty");
const prevBtn = document.getElementById("prev-btn");
const nextBtn = document.getElementById("next-btn");
const pagerInfo = document.getElementById("pager-info");

let offset = 0;
let total = 0;
let rows = [];

function params() {
    const f = new FormData(form);
    const p = {
        client: f.get("client")?.trim(),
        domain: f.get("domain")?.trim(),
        qtype: f.get("qtype"),
        rtype: f.get("rtype"),
        limit: LIMIT,
        offset,
    };
    if (f.get("from")) p.from = new Date(f.get("from")).toISOString();
    if (f.get("to")) p.to = new Date(f.get("to")).toISOString();
    if (f.get("decoys")) p.decoys = "true";
    return p;
}

async function load() {
    let res;
    try {
        res = await getJSON("/api/ui/queries", params());
    } catch (err) {
        emptyMsg.textContent = `Search failed (${err.message}) — check the filters and try again.`;
        emptyMsg.hidden = false;
        return;
    }
    total = res.total || 0;
    rows = res.items || [];
    render();
}

function render() {
    body.innerHTML = "";
    emptyMsg.hidden = rows.length > 0;
    emptyMsg.textContent = "No queries match — clear a filter or widen the time range.";
    for (const item of rows) {
        const tr = document.createElement("tr");
        tr.dataset.band = band(item);
        const cells = [
            fmtDateTime(item.ts), item.client, item.question, item.qtype,
            item.rtype, item.rcode, item.answer, String(item.durationMs),
        ];
        tr.innerHTML = `<td class="rail"></td>` + cells.map((c, i) =>
            `<td class="${i === 2 || i === 6 ? "q" : i === 4 ? "rt" : i === 7 ? "num" : ""}"></td>`).join("");
        const tds = tr.querySelectorAll("td");
        cells.forEach((c, i) => { tds[i + 1].textContent = c ?? ""; });
        body.append(tr);
    }
    const fromN = total === 0 ? 0 : offset + 1;
    const toN = offset + rows.length;
    pagerInfo.textContent = `${fromN}–${toN} of ${total}`;
    prevBtn.disabled = offset === 0;
    nextBtn.disabled = offset + LIMIT >= total;
}

form.addEventListener("submit", (ev) => {
    ev.preventDefault();
    offset = 0;
    load();
});

prevBtn.addEventListener("click", () => { offset = Math.max(0, offset - LIMIT); load(); });
nextBtn.addEventListener("click", () => { offset += LIMIT; load(); });

// CSV export of the current page's rows (client-side blob).
document.getElementById("csv-btn").addEventListener("click", () => {
    const cols = ["ts", "client", "clientNames", "question", "qtype", "rtype",
        "rcode", "answer", "durationMs", "transport", "fpHash", "reason", "decoy"];
    const escCSV = (v) => {
        if (Array.isArray(v)) v = v.join(";");
        const s = v == null ? "" : String(v);
        return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
    };
    const lines = [cols.join(",")].concat(
        rows.map((r) => cols.map((c) => escCSV(r[c])).join(",")));
    const blob = new Blob([lines.join("\n")], { type: "text/csv" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = "blocky-queries.csv";
    a.click();
    URL.revokeObjectURL(a.href);
});

load();
