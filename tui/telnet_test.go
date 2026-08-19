package tui

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// A connecting client is cleared+homed, then receives each broadcast frame.
func TestTelnetHubBroadcast(t *testing.T) {
	hub := &telnetHub{clients: map[net.Conn]struct{}{}}
	srv, cli := net.Pipe()

	read := func() []byte {
		buf := make([]byte, 4096)
		n, _ := cli.Read(buf)

		return buf[:n]
	}

	ch := make(chan []byte, 2)
	go func() { ch <- read() }() // the connect init sequence

	hub.add(srv)

	if init := <-ch; !bytes.Contains(init, []byte("\x1b[2J")) {
		t.Fatalf("connect must clear the client screen; got %q", init)
	}

	go func() { ch <- read() }() // the broadcast frame

	hub.broadcast([]byte("\x1b[HFRAME"))

	if frame := <-ch; !bytes.Contains(frame, []byte("FRAME")) {
		t.Fatalf("client must receive the broadcast frame; got %q", frame)
	}
}

// encodeFrame must produce a full-screen ANSI frame: cursor-home, absolute row
// positioning, the drawn runes, and a truecolor SGR for a coloured cell.
func TestEncodeFrame(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatal(err)
	}

	sim.SetSize(10, 2)
	drawText(sim, 0, 0, tcell.StyleDefault.Foreground(tcell.ColorRed).Bold(true), "HI")
	sim.Show()

	out := string(encodeFrame(sim))

	if !strings.HasPrefix(out, "\x1b[H") {
		t.Errorf("frame must start by homing the cursor; got %q", out[:min(6, len(out))])
	}

	if !strings.Contains(out, "\x1b[2;1H") {
		t.Error("frame must position each row absolutely (missing row-2 move)")
	}

	// ColorRed truecolor is 255;0;0, bold is attr 1.
	if !strings.Contains(out, "38;2;255;0;0") {
		t.Errorf("coloured cell must emit a truecolor fg SGR; got %q", out)
	}

	if !strings.Contains(out, "HI") {
		t.Error("frame must contain the drawn text")
	}
}

// colorSGR falls back to the terminal default code when a cell has no colour.
func TestColorSGRDefault(t *testing.T) {
	if got := colorSGR(tcell.ColorDefault, true); got != "39" {
		t.Errorf("default fg = %q, want 39", got)
	}

	if got := colorSGR(tcell.ColorDefault, false); got != "49" {
		t.Errorf("default bg = %q, want 49", got)
	}
}
