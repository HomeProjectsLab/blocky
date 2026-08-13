// format.js — number/duration/time formatters.

export function fmtNum(n) {
    if (n == null) return "—";
    if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
    if (n >= 1e4) return (n / 1e3).toFixed(1) + "k";
    return String(n);
}

export function fmtMs(ms) {
    if (ms == null) return "—";
    if (ms >= 1000) return (ms / 1000).toFixed(2) + " s";
    return (ms >= 100 ? Math.round(ms) : ms.toFixed(1)) + " ms";
}

export function fmtPct(part, total) {
    if (!total) return "0%";
    return ((part / total) * 100).toFixed(1) + "%";
}

export function fmtClock(ts) {
    return new Date(ts).toLocaleTimeString([], { hour12: false });
}

export function fmtDateTime(ts) {
    const d = new Date(ts);
    return d.toLocaleDateString() + " " + d.toLocaleTimeString([], { hour12: false });
}

export function fmtUptime(seconds) {
    if (seconds == null) return "";
    const d = Math.floor(seconds / 86400);
    const h = Math.floor((seconds % 86400) / 3600);
    const m = Math.floor((seconds % 3600) / 60);
    if (d > 0) return `up ${d}d ${h}h`;
    if (h > 0) return `up ${h}h ${m}m`;
    return `up ${m}m`;
}

// Status band for a query item: structure encodes meaning, fixed everywhere.
export function band(item) {
    if (item.rcode && item.rcode !== "NOERROR" && item.rcode !== "NXDOMAIN") return "error";
    switch (item.rtype) {
    case "RESOLVED": return "resolved";
    case "CACHED": return "cached";
    case "BLOCKED":
    case "FILTERED": return "blocked";
    default: return "other";
    }
}
