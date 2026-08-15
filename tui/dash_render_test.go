package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// cannedDash builds a Dashboard with representative state, ready to draw without
// any background goroutines running.
func cannedDash(caps Caps) *Dashboard {
	d := New(NewClient("http://x"), 0)
	d.caps = caps
	d.connected = true
	d.system = System{
		Version: "0.24", UptimeSeconds: 273120, Goroutines: 84, HeapAllocRaw: 22 * 1024 * 1024,
		CPUPerCore: []float64{61, 28, 44, 19}, CPUTotal: 38,
		MemUsed: 2500 * 1024 * 1024, MemTotal: 3800 * 1024 * 1024,
		DiskUsed: 9 * 1024 * 1024 * 1024, DiskTotal: 32 * 1024 * 1024 * 1024,
		DiskReadBps: 1.2e6, DiskWriteBps: 3.4e5,
	}
	d.overview = Overview{Queries: 1_240_000, Blocked: 384_000, Cached: 843_000, Clients: 18, AvgMs: 4, P95Ms: 22}
	d.latency = Latency{P50: 4, P95: 22, P99: 61}
	d.decoy = DecoyOverview{BySource: map[string]int64{"replay": 30000, "persona": 14000}}
	d.blocking = Blocking{Categories: []BlockCategory{{Name: "ads", Domains: 82000}, {Name: "tracking", Domains: 41000}}}
	d.configDirty = true
	d.topDom = []TopItem{{Name: "doubleclick.net", Count: 8402}, {Name: "analytics.google.com", Count: 5533}}
	d.clients = []ClientInfo{
		{Name: "laptop", IPs: []string{"192.168.1.24"}, Queries: 12480, Blocked: 3820, DeviceGuess: "MacBook", NatAggregate: true, FpCount: 3},
	}
	d.qpsHist = []float64{2, 5, 3, 8, 6, 9, 12, 7, 4, 10, 42}
	d.qps = 42
	d.rows = []QueryItem{
		{TS: "2026-08-15T14:22:07Z", Client: "laptop", Question: "analytics.google.com", Qtype: "A", Rtype: "BLOCKED"},
		{TS: "2026-08-15T14:22:07Z", Client: "phone", Question: "api.weather.com", Qtype: "A", Rtype: "RESOLVED"},
		{TS: "2026-08-15T14:22:06Z", Client: "tv", Question: "telemetry.samsung.com", Qtype: "A", Rtype: "RESOLVED", Decoy: true, DecoySource: "replay"},
		{TS: "2026-08-15T14:22:05Z", Client: "laptop", Question: "cdn.jsdelivr.net", Qtype: "A", Rtype: "CACHED"},
	}

	return d
}

func TestRenderSmoke(t *testing.T) {
	tiers := []struct {
		name string
		w, h int
		caps Caps
	}{
		{"tiny-fbcon", 78, 20, Detect(8, "linux", false)},
		{"large-truecolor", 160, 48, Detect(1<<24, "xterm-256color", false)},
	}

	for _, tc := range tiers {
		t.Run(tc.name, func(t *testing.T) {
			sc := mtScreen(t, tc.w, tc.h)
			d := cannedDash(tc.caps)

			d.draw(sc) // must not panic
			sc.Show()

			txt := mtScreenText(sc)
			for _, want := range []string{"JungleBlock 0.24", "analytics.google.com", "queries 1.2M"} {
				if !strings.Contains(txt, want) {
					t.Errorf("%s frame missing %q", tc.name, want)
				}
			}
		})
	}
}

// fbcon frames must never contain braille or fine 1/8-block glyphs.
func TestFbconNeverRendersBraille(t *testing.T) {
	sc := mtScreen(t, 160, 48)
	d := cannedDash(Detect(8, "linux", false))
	d.draw(sc)
	sc.Show()

	for _, r := range mtScreenText(sc) {
		if r >= 0x2800 && r <= 0x28FF {
			t.Fatalf("fbcon frame rendered braille rune %q", r)
		}
		// eighth-blocks are tofu on fbcon; the half (▄ U+2584) and full (█ U+2588)
		// blocks are CP437-safe and used deliberately.
		if (r >= 0x2581 && r <= 0x2583) || (r >= 0x2585 && r <= 0x2587) {
			t.Fatalf("fbcon frame rendered fine-block rune %q", r)
		}
	}
}

func TestRenderSplashWhenDisconnected(t *testing.T) {
	sc := mtScreen(t, 100, 30)
	d := New(NewClient("http://host:80"), 0)
	d.caps = Detect(256, "xterm", false)
	// connected stays false
	d.draw(sc)
	sc.Show()

	if !strings.Contains(mtScreenText(sc), "waiting for JungleBlock") {
		t.Error("disconnected dashboard must show the splash")
	}
}

// sanity: a truecolor result glyph on the ticker is coloured per result type.
func TestTickerResultColors(t *testing.T) {
	sc := mtScreen(t, 160, 48)
	d := cannedDash(Detect(1<<24, "xterm", false))
	d.draw(sc)
	sc.Show()

	cells, w, _ := sc.GetContents()
	seen := map[tcell.Color]bool{}
	for _, c := range cells[:len(cells)] {
		_ = w
		fg, _, _ := c.Style.Decompose()
		seen[fg] = true
	}
	if !seen[tcell.ColorRed] || !seen[tcell.ColorGreen] {
		t.Error("ticker should colour BLOCKED red and ok green")
	}
}

// No tier may emit U+FFFD (a byte-sliced multi-byte glyph) or stray control
// runes — the trunc() byte-vs-rune regression.
func TestRenderNoInvalidRunes(t *testing.T) {
	for _, tc := range []struct {
		name string
		w, h int
		caps Caps
	}{
		{"tiny", 78, 20, Detect(8, "linux", false)},
		{"medium", 110, 32, Detect(256, "xterm", false)},
		{"large", 160, 48, Detect(1<<24, "xterm-256color", false)},
	} {
		sc := mtScreen(t, tc.w, tc.h)
		d := cannedDash(tc.caps)
		d.draw(sc)
		sc.Show()

		cells, w, _ := sc.GetContents()
		for i, c := range cells {
			for _, r := range c.Runes {
				if r == '�' || (r < 0x20 && r != 0) {
					t.Errorf("%s: invalid rune %U at (%d,%d)", tc.name, r, i%w, i/w)
				}
			}
		}
	}
}
