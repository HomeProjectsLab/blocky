package tui

import (
	"fmt"
	"slices"

	"github.com/gdamore/tcell/v2"
)

// snapshot is an immutable copy of the dashboard state taken under the mutex so
// every panel renders lock-free from a consistent frame.
type snapshot struct {
	caps      Caps
	connected bool
	base      string

	system   System
	overview Overview
	latency  Latency
	decoy    DecoyOverview
	blocking Blocking
	vitals   Vitals

	topDom     []TopItem
	topBlocked []TopItem
	clients    []ClientInfo
	rows       []QueryItem

	qps         float64
	qpsHist     []float64
	configDirty bool
	paused      bool
}

func (s *snapshot) blockFrac() float64 {
	if s.overview.Queries > 0 {
		return float64(s.overview.Blocked) / float64(s.overview.Queries)
	}

	return 0
}

func (s *snapshot) cacheFrac() float64 {
	if s.overview.Queries > 0 {
		return float64(s.overview.Cached) / float64(s.overview.Queries)
	}

	return 0
}

// ---- low-level cell helpers ----

func setCell(s tcell.Screen, x, y int, r rune, style tcell.Style) {
	s.SetContent(x, y, r, nil, style)
}

// panelBox draws a box-drawing border with an inline title and returns the inner
// content rect. Safe on tiny rects (returns an empty inner rect).
func panelBox(s tcell.Screen, r Rect, g GlyphSet, style tcell.Style, title string) Rect {
	if r.W < 2 || r.H < 2 {
		return Rect{r.X, r.Y, 0, 0}
	}

	setCell(s, r.X, r.Y, g.TL, style)
	setCell(s, r.X+r.W-1, r.Y, g.TR, style)
	setCell(s, r.X, r.Y+r.H-1, g.BL, style)
	setCell(s, r.X+r.W-1, r.Y+r.H-1, g.BR, style)

	for x := r.X + 1; x < r.X+r.W-1; x++ {
		setCell(s, x, r.Y, g.H, style)
		setCell(s, x, r.Y+r.H-1, g.H, style)
	}

	for y := r.Y + 1; y < r.Y+r.H-1; y++ {
		setCell(s, r.X, y, g.V, style)
		setCell(s, r.X+r.W-1, y, g.V, style)
	}

	if title != "" && r.W > 5 {
		drawText(s, r.X+2, r.Y, style.Bold(true), " "+trunc(title, r.W-4)+" ")
	}

	return Rect{r.X + 1, r.Y + 1, r.W - 2, r.H - 2}
}

// drawGauge draws "LABEL ▕████▁▁▏ suffix" starting at x,y and returns the x just
// past it. The fill colour tracks the level (gradient on truecolor).
func drawGauge(s tcell.Screen, x, y int, caps Caps, base tcell.Style, label string, frac float64, barW int, suffix string) int {
	g := caps.Glyphs

	if label != "" {
		drawText(s, x, y, base, label+" ")
		x += len(label) + 1
	}

	setCell(s, x, y, g.BarL, base)
	x++

	bar := meterBarWith(frac, 1, barW, g.Bar, g.BarEmpty)
	drawText(s, x, y, base.Foreground(caps.Accent(frac)), bar)
	x += barW

	setCell(s, x, y, g.BarR, base)
	x++

	if suffix != "" {
		drawText(s, x, y, base, " "+suffix)
		x += len(suffix) + 1
	}

	return x
}

// ---- panels ----

var (
	styBase = tcell.StyleDefault
	styHdr  = tcell.StyleDefault.Foreground(tcell.ColorAqua)
	styBar  = tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorBlue).Bold(true)
	styDim  = tcell.StyleDefault.Dim(true)
)

// panelTitle draws the top status bar (version / uptime / host … clock).
func panelTitle(s tcell.Screen, r Rect, snap *snapshot) {
	if r.Empty() {
		return
	}

	for x := r.X; x < r.X+r.W; x++ {
		setCell(s, x, r.Y, ' ', styBar)
	}

	brand := fmt.Sprintf(" JungleBlock %s", snap.system.Version)
	tag := "  a wilder DNS"
	info := fmt.Sprintf("  up %s  %s", fmtUptime(snap.system.UptimeSeconds), snap.base)
	right := timeNow().Format("2006-01-02 15:04:05 ")

	// dim tagline keeps the bar background so it blends into the header band
	styBarSub := tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorBlue).Dim(true)
	drawText(s, r.X, r.Y, styBar, trunc(brand, r.W))
	drawText(s, r.X+len(brand), r.Y, styBarSub, trunc(tag, r.W-len(brand)))
	drawText(s, r.X+len(brand)+len(tag), r.Y, styBar, trunc(info, r.W-len(brand)-len(tag)))
	if r.W > len(right) {
		drawText(s, r.X+r.W-len(right), r.Y, styBar, right)
	}
}

// panelSystem paints the per-core CPU / RAM / disk / IO header band edge-to-edge.
func panelSystem(s tcell.Screen, r Rect, caps Caps, snap *snapshot) {
	if r.Empty() {
		return
	}

	sys := snap.system
	x, y := r.X+1, r.Y

	cores := sys.CPUPerCore
	for i, c := range cores {
		if x > r.X+r.W-16 {
			break
		}

		x = drawGauge(s, x, y, caps, styBase, fmt.Sprintf("CPU%d", i), c/100, 6, fmt.Sprintf("%.0f%%", c))
		x += 2
	}

	// RAM: prefer the sampler bytes, fall back to /proc vitals.
	memFrac, memTxt := memGauge(sys, snap.vitals)
	if x < r.X+r.W-18 {
		drawGauge(s, x, y, caps, styBase, "MEM", memFrac, 8, memTxt)
	}

	if r.H < 2 {
		return
	}

	y++
	x = r.X + 1

	if snap.vitals.HasTemp {
		x = drawGauge(s, x, y, caps, styBase, "TEMP", clamp(snap.vitals.TempC/85, 1), 6, fmt.Sprintf("%.0fC", snap.vitals.TempC))
		x += 2
	}

	if sys.DiskTotal > 0 {
		df := float64(sys.DiskUsed) / float64(sys.DiskTotal)
		x = drawGauge(s, x, y, caps, styBase, "DISK", df, 6, fmt.Sprintf("%.0f%% %s/%s", df*100, fmtBytes(sys.DiskUsed), fmtBytes(sys.DiskTotal)))
		x += 2
	}

	tail := fmt.Sprintf("R %s/s  W %s/s  goroutines %d  heap %s",
		fmtBytes(uint64(sys.DiskReadBps)), fmtBytes(uint64(sys.DiskWriteBps)), sys.Goroutines, fmtBytes(sys.HeapAllocRaw))
	drawText(s, x, y, styDim, trunc(tail, r.X+r.W-x-1))
}

func memGauge(sys System, v Vitals) (float64, string) {
	if sys.MemTotal > 0 {
		f := float64(sys.MemUsed) / float64(sys.MemTotal)
		return f, fmt.Sprintf("%.0f%% %s/%s", f*100, fmtBytes(sys.MemUsed), fmtBytes(sys.MemTotal))
	}

	if v.HasMem {
		return v.MemUsedFrac, fmt.Sprintf("%.0f%% of %dMB", v.MemUsedFrac*100, v.MemTotalKB/1024)
	}

	return 0, "N/A"
}

// panelQPS draws the throughput sparkline + a big banner number for the current
// queries-per-second.
func panelQPS(s tcell.Screen, r Rect, caps Caps, snap *snapshot) {
	inner := panelBox(s, r, caps.Glyphs, styHdr, fmt.Sprintf("THROUGHPUT  %.0f q/s", snap.qps))
	if inner.Empty() {
		return
	}

	spark := sparkline(snap.qpsHist, inner.W, caps.Glyphs)
	drawText(s, inner.X, inner.Y, styBase.Foreground(tcell.ColorAqua), spark)

	if inner.H >= 4 {
		lines := bannerLines(fmt.Sprintf("%.0f", snap.qps))
		by := inner.Y + inner.H - 3
		for i, l := range lines {
			drawText(s, inner.X, by+i, styBase.Foreground(tcell.ColorAqua).Bold(true), trunc(l, inner.W))
		}
	}
}

// panelBlocked draws the hero blocked-percentage banner.
func panelBlocked(s tcell.Screen, r Rect, caps Caps, snap *snapshot) {
	inner := panelBox(s, r, caps.Glyphs, styHdr, "BLOCKED")
	if inner.Empty() {
		return
	}

	frac := snap.blockFrac()
	red := styBase.Foreground(tcell.ColorRed).Bold(true)

	if inner.H >= 3 {
		lines := bannerLines(fmt.Sprintf("%.0f%%", frac*100))
		off := (inner.H - 3) / 2
		for i, l := range lines {
			cx := inner.X + (inner.W-len([]rune(l)))/2
			if cx < inner.X {
				cx = inner.X
			}
			drawText(s, cx, inner.Y+off+i, red, trunc(l, inner.W))
		}
	} else {
		drawText(s, inner.X, inner.Y, red, pct(frac))
	}

	sub := fmt.Sprintf("of %d", snap.overview.Queries)
	drawText(s, inner.X, inner.Y+inner.H-1, styDim, trunc(sub, inner.W))
}

// panelCache draws hit/miss gauges plus latency percentiles.
func panelCache(s tcell.Screen, r Rect, caps Caps, snap *snapshot) {
	inner := panelBox(s, r, caps.Glyphs, styHdr, fmt.Sprintf("CACHE  %.0f%%", snap.cacheFrac()*100))
	if inner.Empty() {
		return
	}

	hit := snap.cacheFrac()
	barW := inner.W - 16
	if barW < 4 {
		barW = 4
	}

	drawGauge(s, inner.X, inner.Y, caps, styBase, "HIT ", hit, barW, pct(hit))
	if inner.H >= 2 {
		drawGauge(s, inner.X, inner.Y+1, caps, styBase, "MISS", 1-hit, barW, pct(1-hit))
	}

	if inner.H >= 3 {
		l := snap.latency
		if l.P50 == 0 && l.P95 == 0 {
			l.P50, l.P95 = snap.overview.AvgMs, snap.overview.P95Ms
		}
		lat := fmt.Sprintf("p50 %.0fms  p95 %.0fms  p99 %.0fms", l.P50, l.P95, l.P99)
		drawText(s, inner.X, inner.Y+inner.H-1, styDim, trunc(lat, inner.W))
	}
}

// panelTopList lists a top-N column (count-first), filling the rect.
func panelTopList(s tcell.Screen, r Rect, caps Caps, title string, items []TopItem) {
	inner := panelBox(s, r, caps.Glyphs, styHdr, title)
	if inner.Empty() {
		return
	}

	drawTop(s, inner.X, inner.Y, inner.H, styBase, items, inner.W)
}

// panelClients lists identified clients with NAT/device enrichment.
func panelClients(s tcell.Screen, r Rect, caps Caps, snap *snapshot) {
	inner := panelBox(s, r, caps.Glyphs, styHdr, "TOP CLIENTS")
	if inner.Empty() {
		return
	}

	drawClients(s, inner.X, inner.Y, inner.Y+inner.H-1, styBase, snap.clients, inner.W)
}

// panelTicker draws the live SSE query stream, newest at the bottom with a
// one-row highlight, coloured by result, using caps result glyphs.
func panelTicker(s tcell.Screen, r Rect, caps Caps, snap *snapshot) {
	title := fmt.Sprintf("LIVE QUERIES  %d buffered  p pause", len(snap.rows))
	if snap.paused {
		title += "  [PAUSED]"
	}

	inner := panelBox(s, r, caps.Glyphs, styHdr, title)
	if inner.Empty() {
		return
	}

	rows := snap.rows
	if len(rows) > inner.H {
		rows = rows[len(rows)-inner.H:]
	}

	newest := len(rows) - 1
	for i, q := range rows {
		style, mark := tickerStyle(caps, q)
		line := fmt.Sprintf("%s %c %-14s %-30s %-5s %s",
			hhmmss(q.TS), mark, trunc(q.Client, 14), trunc(q.Question, 30), trunc(q.Qtype, 5), resultText(q))

		if i == newest {
			style = style.Bold(true).Reverse(true) // cheap sweep on the freshest row
		}

		drawText(s, inner.X, inner.Y+i, style, trunc(line, inner.W))
	}
}

func tickerStyle(caps Caps, q QueryItem) (tcell.Style, rune) {
	g := caps.Glyphs

	switch {
	case q.Decoy:
		return styDim.Foreground(tcell.ColorGray), g.Decoy
	case q.Rtype == "BLOCKED":
		return styBase.Foreground(tcell.ColorRed), g.Blocked
	case q.Rtype == "CACHED":
		return styBase.Foreground(tcell.ColorTeal), g.Cached
	default:
		return styBase.Foreground(tcell.ColorGreen), g.OK
	}
}

func resultText(q QueryItem) string {
	if q.Decoy {
		src := q.DecoySource
		if src == "" {
			src = "decoy"
		}

		return "decoy(" + src + ")"
	}

	return q.Rtype
}

// panelDecoy shows the noise/decoy source mix.
func panelDecoy(s tcell.Screen, r Rect, caps Caps, snap *snapshot) {
	total := int64(0)
	for _, c := range snap.decoy.BySource {
		total += c
	}

	inner := panelBox(s, r, caps.Glyphs, styHdr, fmt.Sprintf("DECOY / NOISE  %d total", total))
	if inner.Empty() {
		return
	}

	// Fixed render order: map iteration order would shuffle rows every frame.
	srcs := make([]string, 0, len(snap.decoy.BySource))
	for src := range snap.decoy.BySource {
		srcs = append(srcs, src)
	}

	slices.Sort(srcs)

	y := inner.Y
	for _, src := range srcs {
		cnt := snap.decoy.BySource[src]
		if y >= inner.Y+inner.H {
			break
		}

		frac := 0.0
		if total > 0 {
			frac = float64(cnt) / float64(total)
		}

		drawGauge(s, inner.X, y, caps, styBase, fmt.Sprintf("%-8s", trunc(src, 8)), frac, inner.W-24, fmt.Sprintf("%d", cnt))
		y++
	}
}

// panelBlocklist shows blocklist category counts and a config-dirty warning.
func panelBlocklist(s tcell.Screen, r Rect, caps Caps, snap *snapshot) {
	inner := panelBox(s, r, caps.Glyphs, styHdr, "BLOCKLIST")
	if inner.Empty() {
		return
	}

	line := ""
	for i, c := range snap.blocking.Categories {
		if i > 0 {
			line += " · "
		}

		line += fmt.Sprintf("%s %s", c.Name, fmtCount(c.Domains))
	}

	line += fmt.Sprintf("   allow %d  deny %d", len(snap.blocking.Allow), len(snap.blocking.Deny))
	drawText(s, inner.X, inner.Y, styBase, trunc(line, inner.W))

	if snap.configDirty && inner.H >= 2 {
		warn := styBase.Foreground(tcell.ColorYellow).Bold(true)
		drawText(s, inner.X, inner.Y+1, warn, trunc("! config dirty — changes pending, needs apply", inner.W))
	}
}

// panelFooter draws the bottom summary bar.
func panelFooter(s tcell.Screen, r Rect, snap *snapshot) {
	if r.Empty() {
		return
	}

	decoyTotal := int64(0)
	for _, c := range snap.decoy.BySource {
		decoyTotal += c
	}

	for x := r.X; x < r.X+r.W; x++ {
		setCell(s, x, r.Y, ' ', styBar)
	}

	pauseLbl := ""
	if snap.paused {
		pauseLbl = "  [PAUSED]"
	}

	o := snap.overview
	left := " q quit  p pause" + pauseLbl
	right := fmt.Sprintf("queries %s  blocked %s (%.0f%%)  cached %s  decoys %s ",
		fmtCount(o.Queries), fmtCount(o.Blocked), snap.blockFrac()*100, fmtCount(o.Cached), fmtCount(decoyTotal))

	drawText(s, r.X, r.Y, styBar, trunc(left, r.W))
	if r.W > len(right) {
		drawText(s, r.X+r.W-len(right), r.Y, styBar, right)
	}
}

// ---- formatting helpers ----

func fmtUptime(sec int64) string {
	if sec < 0 {
		sec = 0
	}

	d := sec / 86400
	h := (sec % 86400) / 3600
	m := (sec % 3600) / 60

	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh %dm", d, h, m)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

func fmtBytes(b uint64) string {
	const u = 1024.0

	f := float64(b)

	switch {
	case f >= u*u*u:
		return fmt.Sprintf("%.1fG", f/(u*u*u))
	case f >= u*u:
		return fmt.Sprintf("%.0fM", f/(u*u))
	case f >= u:
		return fmt.Sprintf("%.0fK", f/u)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func fmtCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fK", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}
