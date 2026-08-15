package tui

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
)

// Dashboard is the htop-style console monitor. It owns all mutable UI state
// behind mu; background goroutines (SSE stream, 1s refresh) mutate it and poke
// redraw, while the main Run loop owns the tcell screen.
type Dashboard struct {
	api     *Client
	refresh time.Duration
	maxRows int

	mu        sync.Mutex
	connected bool
	system    System
	overview  Overview
	decoy     DecoyOverview
	topDom    []TopItem
	clients   []ClientInfo
	vitals    Vitals
	rows      []QueryItem // newest last
	paused    bool

	streamCount int64 // total SSE events seen (for live QPS)
	lastCount   int64
	qps         float64

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

	go d.streamLoop()
	go d.refreshLoop()

	events := make(chan tcell.Event, 16)
	quit := make(chan struct{})

	go screen.ChannelEvents(events, quit)

	d.draw(screen)

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

			d.draw(screen)
		case <-d.redrawCh:
			d.draw(screen)
		}
	}
}

// refreshLoop polls the JSON endpoints + local vitals every d.refresh.
func (d *Dashboard) refreshLoop() {
	tick := time.NewTicker(d.refresh)
	defer tick.Stop()

	for {
		d.refreshOnce()
		d.poke()
		<-tick.C
	}
}

func (d *Dashboard) refreshOnce() {
	sys, err := d.api.System()

	v := ReadVitals()

	d.mu.Lock()
	defer d.mu.Unlock()

	d.vitals = v
	// live QPS from the SSE event delta over one refresh interval
	d.qps = float64(d.streamCount-d.lastCount) / d.refresh.Seconds()
	d.lastCount = d.streamCount

	if err != nil {
		d.connected = false

		return
	}

	d.connected = true
	d.system = sys

	if o, e := d.api.Overview(); e == nil {
		d.overview = o
	}

	if n, e := d.api.NoiseOverview(); e == nil {
		d.decoy = n
	}

	if t, e := d.api.Top("domain", 8); e == nil {
		d.topDom = t
	}

	if cl, e := d.api.Clients(); e == nil {
		d.clients = cl
	}
}

// streamLoop keeps the SSE feed connected, reconnecting on drop.
func (d *Dashboard) streamLoop() {
	for {
		_ = d.api.Stream(func(q QueryItem) {
			d.mu.Lock()
			d.streamCount++

			if !d.paused {
				d.rows = append(d.rows, q)
				if len(d.rows) > d.maxRows {
					d.rows = d.rows[len(d.rows)-d.maxRows:]
				}
			}
			d.mu.Unlock()
			d.poke()
		})

		time.Sleep(time.Second) // backoff before reconnect
	}
}

// ---- rendering ----

func drawText(s tcell.Screen, x, y int, style tcell.Style, text string) {
	for _, r := range text {
		s.SetContent(x, y, r, nil, style)
		x++
	}
}

func (d *Dashboard) draw(s tcell.Screen) {
	w, h := s.Size()
	s.Clear()

	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.connected {
		d.drawSplash(s, w, h)
		s.Show()

		return
	}

	base := tcell.StyleDefault
	d.drawHeader(s, w, base)
	d.drawMeters(s, w, base)

	// two-column body: query stream left, side panels right
	rightX := w - 34
	sidePanel := rightX > 40

	streamW := w
	if sidePanel {
		streamW = rightX - 1
		d.drawSidePanels(s, rightX, w, h, base)
	}

	d.drawStream(s, streamW, h, base)
	d.drawFooter(s, w, h, base)

	s.Show()
}

func (d *Dashboard) drawSplash(s tcell.Screen, w, h int) {
	msg := "waiting for blocky at " + d.api.Base + " ..."
	drawText(s, (w-len(msg))/2, h/2, tcell.StyleDefault.Foreground(tcell.ColorYellow), msg)
}

func (d *Dashboard) drawHeader(s tcell.Screen, w int, base tcell.Style) {
	host, _ := os.Hostname()
	up := (time.Duration(d.system.UptimeSeconds) * time.Second).String()
	left := fmt.Sprintf(" blocky %s  up %s  %s", d.system.Version, up, host)
	right := time.Now().Format("2006-01-02 15:04:05 ")

	title := base.Foreground(tcell.ColorWhite).Background(tcell.ColorBlue).Bold(true)
	for x := range w {
		s.SetContent(x, 0, ' ', nil, title)
	}

	drawText(s, 0, 0, title, left)
	drawText(s, w-len(right), 0, title, right)
}

// drawMeter draws a labelled htop gauge: "label [|||||    ] text".
func drawMeter(s tcell.Screen, x, y, barW int, base tcell.Style, label string, frac float64, text string) {
	drawText(s, x, y, base, fmt.Sprintf("%-6s", label))
	bx := x + 6
	s.SetContent(bx, y, '[', nil, base)

	bar := meterBar(frac, 1, barW)
	col := levelFor(frac).Color()
	drawText(s, bx+1, y, base.Foreground(col), bar)

	s.SetContent(bx+1+barW, y, ']', nil, base)
	drawText(s, bx+2+barW, y, base, " "+text)
}

func (d *Dashboard) drawMeters(s tcell.Screen, w int, base tcell.Style) {
	barW := 20
	if w < 60 {
		barW = 10
	}

	o := d.overview
	blockFrac, cacheFrac := 0.0, 0.0
	if o.Queries > 0 {
		blockFrac = float64(o.Blocked) / float64(o.Queries)
		cacheFrac = float64(o.Cached) / float64(o.Queries)
	}

	// column 1: service meters
	drawMeter(s, 1, 2, barW, base, "QPS", clamp(d.qps/50, 1), fmt.Sprintf("%.1f", d.qps))
	drawMeter(s, 1, 3, barW, base, "Block", blockFrac, pct(blockFrac))
	drawMeter(s, 1, 4, barW, base, "Cache", cacheFrac, pct(cacheFrac))

	// column 2: Pi vitals
	x2 := barW + 34
	v := d.vitals

	loadFrac, loadTxt := 0.0, "N/A"
	if v.HasLoad {
		loadFrac, loadTxt = clamp(v.Load/4, 1), fmt.Sprintf("%.2f", v.Load)
	}

	memTxt := "N/A"
	if v.HasMem {
		memTxt = fmt.Sprintf("%s%% of %dMB", strings.TrimSuffix(pct(v.MemUsedFrac), "%"), v.MemTotalKB/1024)
	}

	tempFrac, tempTxt := 0.0, "N/A"
	if v.HasTemp {
		tempFrac, tempTxt = clamp(v.TempC/85, 1), fmt.Sprintf("%.1fC", v.TempC)
	}

	drawMeter(s, x2, 2, barW, base, "Load", loadFrac, loadTxt)
	drawMeter(s, x2, 3, barW, base, "Mem", d.vitals.MemUsedFrac, memTxt)
	drawMeter(s, x2, 4, barW, base, "Temp", tempFrac, tempTxt)
}

func (d *Dashboard) drawStream(s tcell.Screen, w, h int, base tcell.Style) {
	top := 6
	hdr := base.Foreground(tcell.ColorAqua).Bold(true)
	drawText(s, 1, top, hdr, "LIVE QUERIES  (p pause)")
	drawText(s, 1, top+1, base.Dim(true),
		fmt.Sprintf("%-8s %-15s %-28s %-5s %s", "TIME", "CLIENT", "DOMAIN", "TYPE", "RESULT"))

	avail := h - (top + 2) - 1 // leave footer line
	if avail < 1 {
		return
	}

	start := 0
	if len(d.rows) > avail {
		start = len(d.rows) - avail
	}

	y := top + 2
	for _, q := range d.rows[start:] {
		style := base
		result := q.Rtype

		switch {
		case q.Decoy:
			style = base.Dim(true).Foreground(tcell.ColorGray)
			src := q.DecoySource
			if src == "" {
				src = "decoy"
			}

			result = "~" + src
		case q.Rtype == "BLOCKED":
			style = base.Foreground(tcell.ColorRed)
		case q.Rtype == "CACHED":
			style = base.Foreground(tcell.ColorTeal)
		default:
			style = base.Foreground(tcell.ColorGreen)
		}

		line := fmt.Sprintf("%-8s %-15s %-28s %-5s %s",
			hhmmss(q.TS), trunc(q.Client, 15), trunc(q.Question, 28), trunc(q.Qtype, 5), trunc(result, 12))
		drawText(s, 1, y, style, trunc(line, w-2))
		y++
	}
}

func (d *Dashboard) drawSidePanels(s tcell.Screen, x, w, h int, base tcell.Style) {
	hdr := base.Foreground(tcell.ColorAqua).Bold(true)
	half := (h - 8) / 2

	drawText(s, x, 6, hdr, "TOP DOMAINS")
	drawTop(s, x, 7, half-1, base, d.topDom, w-x-1)

	cy := 7 + half
	drawText(s, x, cy, hdr, "CLIENTS")
	drawClients(s, x, cy+1, h-2, base, d.clients, w-x-1)
}

// drawClients renders the identified-client list: one head line per client
// (hostname  queries/blocked  [NAT xN]) plus a dim sub-line (ip · guess) when
// there is room and info to show. Stops at maxY, leaving the footer clear.
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

		head := fmt.Sprintf("%s  %d/%d%s", c.Name, c.Queries, c.Blocked, nat)
		drawText(s, x, y, base, trunc(head, width))
		y++

		ip := ""
		if len(c.IPs) > 0 && c.IPs[0] != c.Name {
			ip = c.IPs[0]
		}

		var sub string
		switch {
		case ip != "" && c.DeviceGuess != "":
			sub = ip + " " + c.DeviceGuess
		case ip != "":
			sub = ip
		default:
			sub = c.DeviceGuess
		}

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

func (d *Dashboard) drawFooter(s tcell.Screen, w, h int, base tcell.Style) {
	decoyTotal := int64(0)
	for _, c := range d.decoy.BySource {
		decoyTotal += c
	}

	foot := base.Foreground(tcell.ColorWhite).Background(tcell.ColorBlue)
	for x := range w {
		s.SetContent(x, h-1, ' ', nil, foot)
	}

	pauseLbl := ""
	if d.paused {
		pauseLbl = " [PAUSED]"
	}

	left := " q quit  p pause" + pauseLbl
	right := fmt.Sprintf("queries %d  blocked %d  decoys %d ",
		d.overview.Queries, d.overview.Blocked, decoyTotal)
	drawText(s, 0, h-1, foot, left)
	drawText(s, w-len(right), h-1, foot, right)
}

// ---- small helpers ----

func clamp(f, max float64) float64 {
	if f > max {
		return max
	}

	return f
}

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

func trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}

	if len(s) <= n {
		return s
	}

	return s[:n]
}

// hhmmss extracts HH:MM:SS from an RFC3339 timestamp, falling back to the raw
// string's tail.
func hhmmss(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.Format("15:04:05")
	}

	if len(ts) >= 8 {
		return ts[len(ts)-8:]
	}

	return ts
}
