// chart.js — the ONE uPlot wrapper: theme tokens, crosshair, tooltip, resize.
/* global uPlot */

// Read tokens at CALL time, not module import: the theme toggle flips
// data-theme without a reload, so cached values would paint stale-theme charts.
export const cssTok = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

function axis(extra) {
    return Object.assign({
        stroke: cssTok("--ink2"),
        font: `11px ${cssTok("--font-mono") || "monospace"}`,
        grid: { stroke: cssTok("--grid"), width: 1 },
        ticks: { stroke: cssTok("--grid"), width: 1 },
    }, extra);
}

// Simple crosshair tooltip plugin: shows time + per-series values.
function tooltipPlugin(labels, colors, rawSeries, fmtVal) {
    let tip;
    return {
        hooks: {
            init(u) {
                tip = document.createElement("div");
                tip.className = "chart-tip";
                tip.style.display = "none";
                u.over.parentElement.appendChild(tip);
                u.over.addEventListener("mouseleave", () => { tip.style.display = "none"; });
            },
            setCursor(u) {
                const { left, top, idx } = u.cursor;
                if (idx == null || left < 0) { tip.style.display = "none"; return; }
                const ts = u.data[0][idx];
                let html = `<div class="tip-t">${new Date(ts * 1000).toLocaleString([], { hour12: false })}</div>`;
                for (let i = 0; i < labels.length; i++) {
                    const v = (rawSeries ? rawSeries[i] : u.data[i + 1])[idx];
                    html += `<div><span class="tip-dot" style="background:${colors[i]}"></span>${labels[i]} ${fmtVal(v ?? 0)}</div>`;
                }
                tip.innerHTML = html;
                tip.style.display = "block";
                const rect = u.over.getBoundingClientRect();
                const x = left + 12 + tip.offsetWidth > rect.width ? left - tip.offsetWidth - 12 : left + 12;
                tip.style.left = x + "px";
                tip.style.top = Math.max(0, top - 10) + "px";
            },
        },
    };
}

function observe(el, u, height) {
    const ro = new ResizeObserver(() => {
        u.setSize({ width: el.clientWidth, height });
    });
    ro.observe(el);
    // Disconnect when the chart is destroyed. Without this, every re-render
    // (destroy + recreate on a range switch) leaks a live observer still holding
    // the destroyed uPlot, and each fires setSize() on it on the next resize.
    u.hooks.destroy = (u.hooks.destroy || []).concat(() => ro.disconnect());
}

// Stacked area time series. data = [xs, s1, s2, ...] (raw, unstacked).
// xRange (optional [minSec, maxSec]) pins the time axis to the selected window so
// sparse data (a single hourly bucket on a fresh box) doesn't auto-range to a
// nonsense multi-year span.
export function stackedArea(el, { labels, colors, data, xRange, height = 260, fmtVal = (v) => String(v) }) {
    const xs = data[0];
    const raw = data.slice(1);
    // cumulative stack for drawing; tooltip shows raw values
    const acc = new Array(xs.length).fill(0);
    const stacked = raw.map((vals) => vals.map((v, i) => (acc[i] += v || 0)));

    const series = [{}].concat(labels.map((label, i) => ({
        label,
        stroke: colors[i],
        fill: colors[i] + "59", // ~35% alpha
        width: 2,
        points: { show: false },
    })));

    const bands = [];
    for (let i = raw.length; i > 1; i--) bands.push({ series: [i, i - 1] });

    const u = new uPlot({
        width: el.clientWidth || 600,
        height,
        series,
        bands,
        axes: [axis({ grid: { show: false } }), axis({ size: 56 })],
        cursor: { drag: { setScale: false }, focus: { prox: 1e6 } },
        scales: {
            x: xRange ? { time: true, range: () => xRange } : { time: true },
            y: { range: (u, min, max) => [0, Math.max(max, 1)] },
        },
        legend: { live: false },
        plugins: [tooltipPlugin(labels, colors, raw, fmtVal)],
    }, [xs, ...stacked], el);

    observe(el, u, height);
    return u;
}

// Plain multi-line time series (kept for later pages).
export function lineChart(el, { labels, colors, data, height = 260, fmtVal = (v) => String(v) }) {
    const series = [{}].concat(labels.map((label, i) => ({
        label,
        stroke: colors[i],
        width: 2,
        points: { show: false },
    })));

    const u = new uPlot({
        width: el.clientWidth || 600,
        height,
        series,
        axes: [axis({ grid: { show: false } }), axis({ size: 56 })],
        cursor: { drag: { setScale: false } },
        legend: { live: false, show: labels.length > 1 },
        plugins: [tooltipPlugin(labels, colors, null, fmtVal)],
    }, data, el);

    observe(el, u, height);
    return u;
}
