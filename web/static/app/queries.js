// queries.js — query explorer: filter form, paginated table, CSV export.
import { getJSON, send } from "./api.js";
import { confirmDialog, toast } from "./modal.js";
import { fmtDateTime, band, csvField } from "./format.js";

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
        if (item.decoy) tr.dataset.decoy = "1";
        const cells = [
            fmtDateTime(item.ts), item.client, item.question, item.qtype,
            item.rtype, item.decoySource || "", item.rcode, item.answer, String(item.durationMs),
        ];
        tr.innerHTML = `<td class="rail"></td>` + cells.map((c, i) =>
            `<td class="${i === 2 || i === 7 ? "q" : i === 4 ? "rt" : i === 5 ? "src" : i === 8 ? "num" : ""}"></td>`).join("");
        const tds = tr.querySelectorAll("td");
        cells.forEach((c, i) => {
            if (i === 5 && c) { tds[i + 1].innerHTML = `<span class="src-tag"></span>`; tds[i + 1].firstChild.textContent = c; }
            else { tds[i + 1].textContent = c ?? ""; }
        });
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
        "rcode", "answer", "durationMs", "transport", "fpHash", "reason", "decoy", "decoySource"];
    const lines = [cols.join(",")].concat(
        rows.map((r) => cols.map((c) => csvField(r[c])).join(",")));
    const blob = new Blob([lines.join("\n")], { type: "text/csv" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = "jungleblock-queries.csv";
    a.click();
    URL.revokeObjectURL(a.href);
});

// Download the whole query log DB (server VACUUM-INTO snapshot streamed as a
// .db attachment). A plain navigation — the browser handles the download.
document.getElementById("q-download").addEventListener("click", () => {
    window.location = "/api/ui/queries/export";
});

// Clear the entire query log (raw rows + hourly aggregates). Config, blocklists
// and client identity are kept.
const purgeBtn = document.getElementById("q-purge");
purgeBtn.addEventListener("click", async () => {
    const ok = await confirmDialog(
        "Delete ALL logged queries and their stats? Config, blocklists and client identity are kept. This can't be undone.",
        { title: "Clear query log", okText: "Delete all", danger: true });
    if (!ok) return;

    purgeBtn.disabled = true;
    try {
        await send("DELETE", "/api/ui/queries");
        toast("Query log cleared.", { type: "success" });
        offset = 0;
        load();
    } catch (err) {
        toast("Could not clear the log: " + err.message, { type: "error" });
    } finally {
        purgeBtn.disabled = false;
    }
});

load();
