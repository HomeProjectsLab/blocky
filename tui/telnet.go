package tui

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
)

// telnetFrameInterval is how often the headless engine re-renders and (if the
// frame changed) broadcasts. 5fps is plenty for a monitor and keeps the Pi3 and
// the LAN quiet; the live ticker still feels live.
const telnetFrameInterval = 200 * time.Millisecond

// telnetInit hides the client's cursor and clears its screen once on connect.
// Sent as plain ANSI — no telnet IAC negotiation, so `nc host port` works too:
// this is a one-way display, we never read the client, so char/line mode and
// remote echo are irrelevant.
const telnetInit = "\x1b[?25l\x1b[2J\x1b[H"

// ServeTelnet runs the dashboard state engine headless and broadcasts the
// rendered frame to every TCP client on ln at a fixed virtual size. It is
// strictly one-way: all client input is discarded, no client can send commands.
// Blocks until ln fails/closes.
//
// ponytail: the virtual terminal is a fixed w×h for every viewer (telnet NAWS is
// ignored). One render feeds all clients; per-client sizing would mean a render
// per client. Add NAWS-driven per-client render only if that's ever wanted.
func (d *Dashboard) ServeTelnet(ln net.Listener, w, h int) error {
	// modern remote terminals: truecolor + unicode + braille (never the fbcon set).
	d.caps = Detect(1<<24, "xterm-256color", false)

	go d.streamLoop()
	go d.refreshLoop()

	hub := &telnetHub{clients: map[net.Conn]struct{}{}}

	go d.renderLoop(hub, w, h)

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}

		hub.add(conn)

		// Drain and discard everything the client sends (telnet negotiation,
		// keystrokes, NAWS) — reading also detects the close so we can drop it.
		go func() {
			_, _ = io.Copy(io.Discard, conn)
			hub.remove(conn)
			_ = conn.Close()
		}()
	}
}

// renderLoop redraws shared state to one off-screen SimulationScreen and
// broadcasts the encoded frame whenever it changes.
func (d *Dashboard) renderLoop(hub *telnetHub, w, h int) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		return
	}

	sim.SetSize(w, h)

	tick := time.NewTicker(telnetFrameInterval)
	defer tick.Stop()

	for range tick.C {
		d.draw(sim)

		frame := encodeFrame(sim)
		hub.broadcast(frame)
	}
}

// telnetHub is the set of connected viewers plus the most recent frame, sent to a
// client the instant it connects so it isn't staring at a blank screen until the
// next change.
type telnetHub struct {
	mu      sync.Mutex
	clients map[net.Conn]struct{}
	last    []byte
}

func (h *telnetHub) add(conn net.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.clients[conn] = struct{}{}

	_, _ = conn.Write([]byte(telnetInit))
	if h.last != nil {
		_ = writeFrame(conn, h.last)
	}
}

func (h *telnetHub) remove(conn net.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.clients, conn)
}

// broadcast sends frame to every client, but only when it differs from the last
// one sent (an idle dashboard produces identical frames — no need to spam them).
func (h *telnetHub) broadcast(frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if bytes.Equal(frame, h.last) {
		return
	}

	h.last = frame

	for conn := range h.clients {
		if err := writeFrame(conn, frame); err != nil {
			// a wedged/slow client must not stall the others: drop it.
			delete(h.clients, conn)

			_ = conn.Close()
		}
	}
}

func writeFrame(conn net.Conn, frame []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := conn.Write(frame)

	return err
}

// encodeFrame turns the SimulationScreen's cells into a full-screen ANSI frame:
// cursor home, then every row written at an absolute position with per-cell SGR
// colour only when the style changes. Full frames (no diff) so a client that
// connects mid-stream is correct immediately.
//
// ponytail: assumes width-1 cells (the dashboard uses only width-1 glyphs — box
// rules, braille, blocks, ASCII). A width-2 rune would shift its row; revisit if
// a wide glyph is ever added.
func encodeFrame(sim tcell.SimulationScreen) []byte {
	cells, w, h := sim.GetContents()

	var b bytes.Buffer

	b.WriteString("\x1b[H")

	prev := "" // last SGR emitted; persists across rows (cursor moves don't reset it)

	for y := 0; y < h; y++ {
		fmt.Fprintf(&b, "\x1b[%d;1H", y+1)

		for x := 0; x < w; x++ {
			c := cells[y*w+x]

			if sgr := styleSGR(c.Style); sgr != prev {
				b.WriteString(sgr)
				prev = sgr
			}

			if len(c.Runes) > 0 && c.Runes[0] != 0 {
				b.WriteRune(c.Runes[0])
			} else {
				b.WriteByte(' ')
			}
		}
	}

	b.WriteString("\x1b[0m")

	return b.Bytes()
}

// styleSGR builds the full SGR (reset + attrs + fg + bg) for a cell style. Always
// self-contained (leads with 0) so emitting it after any prior state is correct.
func styleSGR(st tcell.Style) string {
	fg, bg, attr := st.Decompose()

	parts := []string{"0"}

	if attr&tcell.AttrBold != 0 {
		parts = append(parts, "1")
	}

	if attr&tcell.AttrDim != 0 {
		parts = append(parts, "2")
	}

	if attr&tcell.AttrUnderline != 0 {
		parts = append(parts, "4")
	}

	if attr&tcell.AttrReverse != 0 {
		parts = append(parts, "7")
	}

	parts = append(parts, colorSGR(fg, true), colorSGR(bg, false))

	return "\x1b[" + strings.Join(parts, ";") + "m"
}

// colorSGR renders one truecolor SGR fragment, or the default-colour code when
// the cell has no explicit colour.
func colorSGR(c tcell.Color, fg bool) string {
	def, base := "49", 48
	if fg {
		def, base = "39", 38
	}

	if c == tcell.ColorDefault {
		return def
	}

	r, g, b := c.TrueColor().RGB()
	if r < 0 {
		return def
	}

	return fmt.Sprintf("%d;2;%d;%d;%d", base, r, g, b)
}
