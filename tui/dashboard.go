package tui

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
)

// timeNow is indirected so the clock can be pinned in tests.
var timeNow = time.Now

// Dashboard is the htop-style console monitor. It owns all mutable UI state
// behind mu; background goroutines (SSE stream, refresh) mutate it and poke
// redraw, while the main Run loop owns the tcell screen. State is snapshotted
// under mu and rendered lock-free.
type Dashboard struct {
	api     *Client
	refresh time.Duration
	maxRows int
	caps    Caps

	mu          sync.Mutex
	connected   bool
	system      System
	overview    Overview
	latency     Latency
	decoy       DecoyOverview
	blocking    Blocking
	configDirty bool
	topDom      []TopItem
	topBlocked  []TopItem
	clients     []ClientInfo
	vitals      Vitals
	rows        []QueryItem // newest last
	paused      bool

	streamCount int64 // total SSE events seen (for live QPS)
	lastCount   int64
	lastSample  time.Time // when lastCount was taken; QPS divides by real elapsed
	qps         float64
	qpsHist     []float64 // rolling QPS samples, oldest first

	redrawCh chan struct{}
}

func New(api *Client, refresh time.Duration) *Dashboard {
	if refresh <= 0 {
		refresh = time.Second
	}

	return &Dashboard{
		api:      api,
		refresh:  refresh,
		maxRows:  500,
		redrawCh: make(chan struct{}, 1),
	}
}

func (d *Dashboard) poke() {
	select {
	case d.redrawCh <- struct{}{}:
	default:
	}
}

// Run initialises a real tty screen and drives the loop until q/Ctrl-C.
func (d *Dashboard) Run() error {
	screen, err := tcell.NewScreen()
	if err != nil {
		return err
	}

	if err := screen.Init(); err != nil {
		return err
	}
	defer screen.Fini()

	return d.RunOn(screen)
}

// RunOn drives an already-initialised screen (real or SimulationScreen).
func (d *Dashboard) RunOn(screen tcell.Screen) error {
	screen.Clear()
	d.caps = DetectScreen(screen)

	go d.streamLoop()
	go d.refreshLoop()

	events := make(chan tcell.Event, 16)
	quit := make(chan struct{})

	go screen.ChannelEvents(events, quit)

	d.draw(screen)

	// Coalesce poke-driven redraws to ~10 fps: sustained QPS otherwise turns
	// every SSE event into a full-screen Clear()+repaint, pinning a Pi3 core.
	const minRedraw = 100 * time.Millisecond

	frame := time.NewTicker(minRedraw)
	defer frame.Stop()

	dirty := false

	for {
		select {
		case ev := <-events:
			switch e := ev.(type) {
			case *tcell.EventKey:
				if e.Key() == tcell.KeyCtrlC || e.Rune() == 'q' {
					close(quit)

					return nil
				}

				if e.Rune() == 'p' {
					d.mu.Lock()
					d.paused = !d.paused
					d.mu.Unlock()
				}
			case *tcell.EventResize:
				screen.Sync()
			}

			// user input: draw immediately for responsiveness
			d.draw(screen)

			dirty = false
		case <-d.redrawCh:
			dirty = true
		case <-frame.C:
			if dirty {
				d.draw(screen)

				dirty = false
			}
		}
	}
}

// refreshLoop polls the JSON endpoints on two tiers: the fast tier (system,
// overview, latency, config-status) every interval, the slow tier (clients,
// batched top-N, noise, blocklist — heavier scans) every 8th tick. SSE holds one
// origin connection slot; the slow tier is batched to stay under the 6-conn cap.
func (d *Dashboard) refreshLoop() {
	tick := time.NewTicker(d.refresh)
	defer tick.Stop()

	for i := 0; ; i++ {
		d.refreshFast()

		if i%8 == 0 {
			d.refreshSlow()
		}

		d.poke()
		<-tick.C
	}
}

func (d *Dashboard) refreshFast() {
	// Fetch everything before taking the lock: holding d.mu across HTTP calls
	// freezes draw/SSE/input whenever the API stalls.
	sys, err := d.api.System()
	v := ReadVitals()

	var (
		o          Overview
		l          Latency
		dirty      bool
		oErr, lErr error
		dirtyErr   error
	)

	if err == nil {
		o, oErr = d.api.Overview()
		l, lErr = d.api.LatencyPct()
		dirty, dirtyErr = d.api.ConfigDirty()
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.vitals = v

	// divide by measured elapsed, not d.refresh: stalled API calls above can
	// stretch a tick to ~20s and a fixed 1s divisor would report ~20x real QPS
	now := timeNow()
	elapsed := now.Sub(d.lastSample)

	if d.lastSample.IsZero() || elapsed <= 0 {
		elapsed = d.refresh
	}

	d.qps = float64(d.streamCount-d.lastCount) / elapsed.Seconds()
	d.lastCount = d.streamCount
	d.lastSample = now

	d.qpsHist = append(d.qpsHist, d.qps)
	if len(d.qpsHist) > 120 {
		d.qpsHist = d.qpsHist[len(d.qpsHist)-120:]
	}

	if err != nil {
		d.connected = false

		return
	}

	d.connected = true
	d.system = sys

	if oErr == nil {
		d.overview = o
	}

	if lErr == nil {
		d.latency = l
	}

	if dirtyErr == nil {
		d.configDirty = dirty
	}
}

func (d *Dashboard) refreshSlow() {
	cols, _ := d.api.TopMulti([]string{"domain", "blocked"}, 12)
	clients, _ := d.api.Clients()
	noise, noiseErr := d.api.NoiseOverview()
	blocking, blockErr := d.api.Blocking()

	d.mu.Lock()
	defer d.mu.Unlock()

	if cols != nil {
		d.topDom = cols["domain"]
		d.topBlocked = cols["blocked"]
	}

	if clients != nil {
		d.clients = clients
	}

	if noiseErr == nil {
		d.decoy = noise
	}

	if blockErr == nil {
		d.blocking = blocking
	}
}

// streamLoop keeps the SSE feed connected, reconnecting on drop.
func (d *Dashboard) streamLoop() {
	for {
		_ = d.api.Stream(func(q QueryItem) {
			d.mu.Lock()
			d.streamCount++
			paused := d.paused

			if !paused {
				d.rows = append(d.rows, q)
				if len(d.rows) > d.maxRows {
					d.rows = d.rows[len(d.rows)-d.maxRows:]
				}
			}
			d.mu.Unlock()

			// while paused the visible rows don't change; skip the redraw poke
			if !paused {
				d.poke()
			}
		})

		time.Sleep(time.Second) // backoff before reconnect
	}
}

// buildSnapshot copies the live state under the mutex so the draw path renders
// lock-free from a consistent frame.
func (d *Dashboard) buildSnapshot() *snapshot {
	d.mu.Lock()
	defer d.mu.Unlock()

	host, _ := os.Hostname()

	return &snapshot{
		caps:        d.caps,
		connected:   d.connected,
		base:        host,
		system:      d.system,
		overview:    d.overview,
		latency:     d.latency,
		decoy:       d.decoy,
		blocking:    d.blocking,
		vitals:      d.vitals,
		topDom:      d.topDom,
		topBlocked:  d.topBlocked,
		clients:     d.clients,
		rows:        append([]QueryItem(nil), d.rows...),
		qps:         d.qps,
		qpsHist:     append([]float64(nil), d.qpsHist...),
		configDirty: d.configDirty,
		paused:      d.paused,
	}
}

// ---- rendering ----

func drawText(s tcell.Screen, x, y int, style tcell.Style, text string) {
	for _, r := range text {
		w := runewidth.RuneWidth(r)
		if w == 0 {
			// combining mark: attach to the previous cell, don't advance.
			continue
		}

		s.SetContent(x, y, r, nil, style)
		x += w
	}
}

func (d *Dashboard) draw(s tcell.Screen) {
	w, h := s.Size()
	s.Clear()

	// Clamp to a sane floor: some terminals report 0×0 or stale dims.
	if w < 20 {
		w = 20
	}

	if h < 6 {
		h = 6
	}

	snap := d.buildSnapshot()

	if !snap.connected {
		d.drawSplash(s, w, h)
		s.Show()

		return
	}

	root := Rect{0, 0, w, h}

	switch pickTier(w, h) {
	case tierTiny:
		d.drawTiny(s, root, snap)
	case tierMedium:
		d.drawMedium(s, root, snap)
	default:
		d.drawLarge(s, root, snap)
	}

	s.Show()
}

func (d *Dashboard) drawSplash(s tcell.Screen, w, h int) {
	msg := "waiting for JungleBlock at " + d.api.Base + " ..."
	// clamp: a negative start x makes tcell drop the leading cells, rendering only the tail
	drawText(s, max(0, (w-len(msg))/2), h/2, tcell.StyleDefault.Foreground(tcell.ColorYellow), msg)
}

// drawTiny: title · compact KPI strip (2 rows) · live ticker · footer.
func (d *Dashboard) drawTiny(s tcell.Screen, root Rect, snap *snapshot) {
	caps := snap.caps

	title, rest := root.Rows(1)
	body, footer := rest.RowsBottom(1)
	kpi, ticker := body.Rows(2)

	panelTitle(s, title, snap)

	row1 := Rect{kpi.X + 1, kpi.Y, kpi.W - 1, 1}
	x := drawGauge(s, row1.X, row1.Y, caps, styBase, "QPS", clamp(snap.qps/50, 1), 8, fmt.Sprintf("%.0f", snap.qps))
	x = drawGauge(s, x+2, row1.Y, caps, styBase, "BLOCK", snap.blockFrac(), 6, pct(snap.blockFrac()))
	drawGauge(s, x+2, row1.Y, caps, styBase, "CACHE", snap.cacheFrac(), 6, pct(snap.cacheFrac()))

	if kpi.H >= 2 {
		memFrac, _ := memGauge(snap.system, snap.vitals)
		cpu := snap.system.CPUTotal / 100
		x = drawGauge(s, row1.X, kpi.Y+1, caps, styBase, "CPU", cpu, 8, fmt.Sprintf("%.0f%%", snap.system.CPUTotal))
		x = drawGauge(s, x+2, kpi.Y+1, caps, styBase, "MEM", memFrac, 6, pct(memFrac))

		if snap.vitals.HasTemp {
			drawGauge(s, x+2, kpi.Y+1, caps, styBase, "TEMP", clamp(snap.vitals.TempC/85, 1), 5, fmt.Sprintf("%.0fC", snap.vitals.TempC))
		}
	}

	panelTicker(s, ticker, caps, snap)
	panelFooter(s, footer, snap)
}

// drawMedium: title · system band · heroes · mid (ticker + lists) · bottom · footer.
func (d *Dashboard) drawMedium(s tcell.Screen, root Rect, snap *snapshot) {
	caps := snap.caps

	title, rest := root.Rows(1)
	sys, rest := rest.Rows(2)
	rest, footer := rest.RowsBottom(1)

	b := rest.SplitV(10, 12, 6) // heroes/mid/bottom
	heroes, mid, bottom := b[0], b[1], b[2]

	panelTitle(s, title, snap)
	panelSystem(s, sys, caps, snap)

	hp := heroes.SplitH(4, 3, 3)
	panelQPS(s, hp[0], caps, snap)
	panelBlocked(s, hp[1], caps, snap)
	panelCache(s, hp[2], caps, snap)

	mp := mid.SplitH(3, 2)
	panelTicker(s, mp[0], caps, snap)
	rc := mp[1].SplitV(1, 1)
	panelTopList(s, rc[0], caps, "TOP DOMAINS", snap.topDom)
	panelClients(s, rc[1], caps, snap)

	bp := bottom.SplitH(2, 3)
	panelDecoy(s, bp[0], caps, snap)
	panelBlocklist(s, bp[1], caps, snap)

	panelFooter(s, footer, snap)
}

// drawLarge: full HDMI grid in four bands — heroes · lists · ticker · bottom.
func (d *Dashboard) drawLarge(s tcell.Screen, root Rect, snap *snapshot) {
	caps := snap.caps

	title, rest := root.Rows(1)
	sys, rest := rest.Rows(2) // panelSystem draws exactly 2 rows; no dead 3rd row
	rest, footer := rest.RowsBottom(1)

	b := rest.SplitV(13, 14, 11, 6) // heroes/lists/ticker/bottom — reclaimed row → taller heroes
	heroes, lists, ticker, bottom := b[0], b[1], b[2], b[3]

	panelTitle(s, title, snap)
	panelSystem(s, sys, caps, snap)

	hp := heroes.SplitH(4, 3, 3)
	panelQPS(s, hp[0], caps, snap)
	panelBlocked(s, hp[1], caps, snap)
	panelCache(s, hp[2], caps, snap)

	lp := lists.SplitH(3, 3, 4)
	panelTopList(s, lp[0], caps, "TOP DOMAINS", snap.topDom)
	panelTopList(s, lp[1], caps, "TOP BLOCKED", snap.topBlocked)
	panelClients(s, lp[2], caps, snap)

	panelTicker(s, ticker, caps, snap)

	bp := bottom.SplitH(1, 2)
	panelDecoy(s, bp[0], caps, snap)
	panelBlocklist(s, bp[1], caps, snap)

	panelFooter(s, footer, snap)
}

func drawClients(s tcell.Screen, x, y, maxY int, base tcell.Style, clients []ClientInfo, width int) {
	dim := base.Dim(true)

	for _, c := range clients {
		if y > maxY {
			break
		}

		nat := ""
		if c.NatAggregate {
			nat = fmt.Sprintf(" [NAT x%d]", c.FpCount)
		}

		name := c.DisplayName
		if name == "" {
			name = c.Name
		}

		head := fmt.Sprintf("%s  %d/%d%s", name, c.Queries, c.Blocked, nat)
		drawText(s, x, y, base, trunc(head, width))
		y++

		ip := ""
		if len(c.IPs) > 0 && c.IPs[0] != c.Name {
			ip = c.IPs[0]
		}

		// R3 NAT-gate: a shared address shows "shared / N devices", never the
		// (meaningless) union of every device's facets behind it. Otherwise show
		// the compact OS / vendor / app identity — a traffic heuristic.
		var parts []string
		if ip != "" {
			parts = append(parts, ip)
		}
		switch {
		case c.Shared || c.NatAggregate:
			// R3: shared address never leaks the per-device facet union — only
			// its shared label, and nothing (not a dangling separator) when empty.
			if c.SharedLabel != "" {
				parts = append(parts, c.SharedLabel)
			}
		case c.OS != "" || len(c.Vendor) > 0 || len(c.Apps) > 0:
			if c.OS != "" {
				parts = append(parts, c.OS)
			}
			parts = append(parts, c.Vendor...)
			parts = append(parts, c.Model...)
			parts = append(parts, c.Apps...)
		case c.DeviceGuess != "":
			parts = append(parts, c.DeviceGuess)
		}
		sub := strings.Join(parts, " · ")

		if sub != "" && y <= maxY {
			drawText(s, x, y, dim, "  "+trunc(sub, width-2))
			y++
		}
	}
}

func drawTop(s tcell.Screen, x, y, rows int, base tcell.Style, items []TopItem, width int) {
	for i, it := range items {
		if i >= rows {
			break
		}

		line := fmt.Sprintf("%6d %s", it.Count, it.Name)
		drawText(s, x, y+i, base, trunc(line, width))
	}
}

// ---- small helpers ----

func clamp(f, max float64) float64 {
	if f > max {
		return max
	}

	return f
}

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

// trunc clips s to at most n display columns. Width is measured with
// runewidth so East-Asian-wide/emoji runes (2 cols) and combining marks
// (0 cols) are counted correctly and the slice lands on a rune boundary —
// a byte-slice would cut a multi-byte glyph mid-sequence and emit U+FFFD,
// and a rune-count would let a wide name overrun the field/panel border.
func trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}

	cols := 0
	for i, r := range s {
		w := runewidth.RuneWidth(r)
		if cols+w > n {
			return s[:i]
		}

		cols += w
	}

	return s
}

// hhmmss extracts HH:MM:SS from an RFC3339 timestamp, falling back to the raw
// string's tail.
func hhmmss(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.Format("15:04:05")
	}

	if r := []rune(ts); len(r) >= 8 {
		return string(r[len(r)-8:])
	}

	return ts
}
