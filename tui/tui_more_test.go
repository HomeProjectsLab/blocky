package tui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
)

// ---- rendering harness ----

// mtScreen returns an initialised SimulationScreen of the given size.
func mtScreen(t *testing.T, w, h int) tcell.SimulationScreen {
	t.Helper()

	sc := tcell.NewSimulationScreen("UTF-8")
	if err := sc.Init(); err != nil {
		t.Fatalf("screen init: %v", err)
	}

	sc.SetSize(w, h)

	return sc
}

// mtRow reads back the visible text of screen row y (trailing blanks trimmed).
func mtRow(sc tcell.SimulationScreen, y int) string {
	cells, w, h := sc.GetContents()
	if y < 0 || y >= h {
		return ""
	}

	var b strings.Builder

	for x := 0; x < w; x++ {
		c := cells[y*w+x]
		if len(c.Runes) > 0 {
			b.WriteRune(c.Runes[0])
		} else {
			b.WriteByte(' ')
		}
	}

	return strings.TrimRight(b.String(), " ")
}

// mtScreenText joins every row so a substring assertion can scan the whole frame.
func mtScreenText(sc tcell.SimulationScreen) string {
	_, _, h := sc.GetContents()

	rows := make([]string, 0, h)
	for y := 0; y < h; y++ {
		rows = append(rows, mtRow(sc, y))
	}

	return strings.Join(rows, "\n")
}

// ---- drawClients ----

func TestDrawClientsRendersHeadAndSubLine(t *testing.T) {
	sc := mtScreen(t, 60, 10)
	clients := []ClientInfo{
		{Name: "phone", IPs: []string{"192.168.1.5"}, Queries: 42, Blocked: 7, DeviceGuess: "iPhone"},
	}
	drawClients(sc, 0, 0, 9, tcell.StyleDefault, clients, 40)
	sc.Show()

	head := mtRow(sc, 0)
	if head != "phone  42/7" {
		t.Errorf("head line = %q, want %q", head, "phone  42/7")
	}

	sub := mtRow(sc, 1)
	if sub != "  192.168.1.5 iPhone" {
		t.Errorf("sub line = %q, want %q", sub, "  192.168.1.5 iPhone")
	}
}

func TestDrawClientsNATMarker(t *testing.T) {
	sc := mtScreen(t, 60, 10)
	clients := []ClientInfo{
		{Name: "router", Queries: 100, Blocked: 3, NatAggregate: true, FpCount: 4},
	}
	drawClients(sc, 0, 0, 9, tcell.StyleDefault, clients, 40)
	sc.Show()

	head := mtRow(sc, 0)
	if !strings.Contains(head, "[NAT x4]") {
		t.Errorf("head = %q, want NAT marker", head)
	}
}

func TestDrawClientsEmptyListDrawsNothing(t *testing.T) {
	sc := mtScreen(t, 60, 10)
	drawClients(sc, 0, 0, 9, tcell.StyleDefault, nil, 40)
	sc.Show()

	if txt := strings.TrimSpace(mtScreenText(sc)); txt != "" {
		t.Errorf("empty client list rendered %q, want blank", txt)
	}
}

func TestDrawClientsNoDeviceGuessShowsIPOnly(t *testing.T) {
	sc := mtScreen(t, 60, 10)
	clients := []ClientInfo{
		{Name: "nas", IPs: []string{"10.0.0.9"}, Queries: 5, Blocked: 0},
	}
	drawClients(sc, 0, 0, 9, tcell.StyleDefault, clients, 40)
	sc.Show()

	if sub := mtRow(sc, 1); sub != "  10.0.0.9" {
		t.Errorf("sub line = %q, want IP only", sub)
	}
}

func TestDrawClientsNoIPNoGuessSkipsSubLine(t *testing.T) {
	sc := mtScreen(t, 60, 10)
	clients := []ClientInfo{
		{Name: "host-a", Queries: 1, Blocked: 0},
		{Name: "host-b", Queries: 2, Blocked: 0},
	}
	drawClients(sc, 0, 0, 9, tcell.StyleDefault, clients, 40)
	sc.Show()

	// With no sub-line, the two heads land on consecutive rows.
	if r0, r1 := mtRow(sc, 0), mtRow(sc, 1); r0 != "host-a  1/0" || r1 != "host-b  2/0" {
		t.Errorf("rows = %q / %q, want back-to-back heads", r0, r1)
	}
}

// IP equal to the resolved name must not be repeated on the sub-line.
func TestDrawClientsIPEqualsNameSuppressed(t *testing.T) {
	sc := mtScreen(t, 60, 10)
	clients := []ClientInfo{
		{Name: "10.0.0.1", IPs: []string{"10.0.0.1"}, Queries: 3, Blocked: 1},
	}
	drawClients(sc, 0, 0, 9, tcell.StyleDefault, clients, 40)
	sc.Show()

	if sub := mtRow(sc, 1); sub != "" {
		t.Errorf("sub line = %q, want empty (IP==name suppressed)", sub)
	}
}

// A head line longer than width is truncated to exactly width cells.
func TestDrawClientsNarrowWidthTruncates(t *testing.T) {
	sc := mtScreen(t, 60, 10)
	clients := []ClientInfo{
		{Name: "a-very-long-hostname-that-overflows", Queries: 123456, Blocked: 9},
	}
	width := 12
	drawClients(sc, 0, 0, 9, tcell.StyleDefault, clients, width)
	sc.Show()

	if head := mtRow(sc, 0); len(head) > width {
		t.Errorf("head %q longer than width %d", head, width)
	}
}

// When maxY is hit after a head line, its sub-line must be dropped rather than
// spilling onto the footer row.
func TestDrawClientsSubLineDroppedAtMaxY(t *testing.T) {
	sc := mtScreen(t, 60, 10)
	clients := []ClientInfo{
		{Name: "dev", IPs: []string{"1.2.3.4"}, Queries: 1, Blocked: 0, DeviceGuess: "TV"},
	}
	// head lands on y=0 which equals maxY, so no room for the sub-line.
	drawClients(sc, 0, 0, 0, tcell.StyleDefault, clients, 40)
	sc.Show()

	if sub := mtRow(sc, 1); sub != "" {
		t.Errorf("sub line = %q, want dropped at maxY", sub)
	}
}

// ---- helpers ----

func TestTruncBoundaries(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"hello", 5, "hello"}, // exact fit
		{"hello", 4, "hell"},  // overflow
		{"hello", 6, "hello"}, // room to spare
		{"hello", 0, ""},      // zero width
		{"hello", -3, ""},     // negative width
		{"", 5, ""},           // empty input
		// wide runes cost 2 columns each: 12-col budget fits 6, not 12.
		{strings.Repeat("客", 10), 12, strings.Repeat("客", 6)},
		// odd budget can't fit a trailing 2-col rune: stops one short.
		{strings.Repeat("客", 10), 5, strings.Repeat("客", 2)},
	}
	for _, c := range cases {
		if got := trunc(c.s, c.n); got != c.want {
			t.Errorf("trunc(%q,%d)=%q want %q", c.s, c.n, got, c.want)
		}
	}
}

func TestHHMMSS(t *testing.T) {
	// UTC RFC3339 renders in local tz, so only assert shape, not digits.
	if got := hhmmss("2026-08-15T13:45:07Z"); len(got) != 8 || got[2] != ':' || got[5] != ':' {
		t.Errorf("hhmmss RFC3339 = %q, want HH:MM:SS shape", got)
	}
	if got := hhmmss("2026-08-15T13:45:07+02:00"); len(got) != 8 || got[2] != ':' || got[5] != ':' {
		t.Errorf("hhmmss offset = %q, want HH:MM:SS shape", got)
	}
	// non-parseable, >=8 chars -> last 8 chars.
	if got := hhmmss("abcdefghXYZ"); got != "defghXYZ" {
		t.Errorf("hhmmss fallback tail = %q, want %q", got, "defghXYZ")
	}
	// exactly 8 non-parseable chars -> returned whole.
	if got := hhmmss("12345678"); got != "12345678" {
		t.Errorf("hhmmss 8-char = %q", got)
	}
	// <8 chars, not parseable -> raw.
	if got := hhmmss("short"); got != "short" {
		t.Errorf("hhmmss short = %q, want raw", got)
	}
	if got := hhmmss(""); got != "" {
		t.Errorf("hhmmss empty = %q", got)
	}
	// non-parseable ts with a multibyte tail must slice on rune boundaries,
	// never mid-sequence -> output stays valid UTF-8 (no U+FFFD).
	if got := hhmmss("bad-time-€€€"); !utf8.ValidString(got) {
		t.Errorf("hhmmss multibyte tail = %q, not valid UTF-8", got)
	}
}

func TestClampAndPct(t *testing.T) {
	if clamp(5, 1) != 1 {
		t.Error("clamp above max")
	}
	if clamp(0.3, 1) != 0.3 {
		t.Error("clamp below max unchanged")
	}
	if clamp(-2, 1) != -2 {
		t.Error("clamp only bounds the top, not the bottom")
	}
	if pct(0) != "0%" || pct(1) != "100%" || pct(0.5) != "50%" {
		t.Errorf("pct wrong: %q %q %q", pct(0), pct(1), pct(0.5))
	}
}

// ---- readSSE edge cases (complements TestReadSSE) ----

func TestReadSSEIgnoresMalformedAndOtherEvents(t *testing.T) {
	stream := strings.Join([]string{
		"event: heartbeat", // non-query event, ignored
		`data: {"ts":"x"}`,
		"",
		"event: query", // malformed JSON, skipped silently
		"data: {not json",
		"",
		"event: query", // multi-line data concatenated
		`data: {"ts":"t3","client":"c",`,
		`data: "question":"multi.example"}`,
		"",
	}, "\n") + "\n"

	var got []QueryItem
	if err := readSSE(strings.NewReader(stream), func(q QueryItem) { got = append(got, q) }); err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Fatalf("want 1 valid query, got %d (%+v)", len(got), got)
	}
	if got[0].Question != "multi.example" {
		t.Errorf("multi-line data not concatenated: %+v", got[0])
	}
}

func TestReadSSEEmptyStream(t *testing.T) {
	var n int
	if err := readSSE(strings.NewReader(""), func(QueryItem) { n++ }); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("empty stream produced %d events", n)
	}
}

// A query event whose block never terminates with a blank line is not emitted
// (SSE requires the trailing blank line to flush the event).
func TestReadSSEUnterminatedBlockNotEmitted(t *testing.T) {
	stream := "event: query\n" + `data: {"ts":"t","question":"q"}` + "\n" // no blank line
	var n int
	if err := readSSE(strings.NewReader(stream), func(QueryItem) { n++ }); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("unterminated block emitted %d events, want 0", n)
	}
}

// ---- HTTP client ----

func mtServer(t *testing.T, path, body string, status int) *Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path != "" && r.URL.Path != path {
			// let query-string paths (e.g. /stats/top) match on prefix
			if !strings.HasPrefix(r.URL.Path, path) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return NewClient(srv.URL)
}

func TestClientsDecodesNewFields(t *testing.T) {
	body := `{"clients":[
		{"name":"phone","ips":["192.168.1.5","192.168.1.6"],"queries":42,"blocked":7,
		 "lastSeen":"2026-08-15T00:00:00Z","natAggregate":true,"fpCount":3,"deviceGuess":"iPhone"},
		{"name":"nas","queries":5,"blocked":0}
	]}`
	c := mtServer(t, "/api/ui/clients", body, http.StatusOK)

	cs, err := c.Clients()
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("want 2 clients, got %d", len(cs))
	}

	p := cs[0]
	if p.Name != "phone" || len(p.IPs) != 2 || !p.NatAggregate || p.FpCount != 3 || p.DeviceGuess != "iPhone" {
		t.Errorf("phone decoded wrong: %+v", p)
	}

	// omitempty fields must default cleanly on the second client.
	n := cs[1]
	if n.NatAggregate || n.FpCount != 0 || n.DeviceGuess != "" || len(n.IPs) != 0 {
		t.Errorf("nas defaults wrong: %+v", n)
	}
}

func TestClientsEmptyList(t *testing.T) {
	c := mtServer(t, "/api/ui/clients", `{"clients":[]}`, http.StatusOK)
	cs, err := c.Clients()
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 0 {
		t.Errorf("want empty, got %d", len(cs))
	}
}

func TestGetJSONNon200IsError(t *testing.T) {
	c := mtServer(t, "/api/ui/clients", "nope", http.StatusServiceUnavailable)
	if _, err := c.Clients(); err == nil {
		t.Error("non-200 status must surface an error")
	}
}

func TestGetJSONMalformedBodyIsError(t *testing.T) {
	c := mtServer(t, "/api/ui/clients", "{ this is not json", http.StatusOK)
	if _, err := c.Clients(); err == nil {
		t.Error("malformed JSON must surface a decode error")
	}
}

func TestClientConnectionRefusedIsError(t *testing.T) {
	// point at a closed port; the Get must fail before any decode.
	c := NewClient("http://127.0.0.1:1")
	if _, err := c.Clients(); err == nil {
		t.Error("unreachable server must surface a transport error")
	}
}

func TestSystemOverviewNoiseTopDecode(t *testing.T) {
	// One server multiplexing every endpoint the dashboard polls.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ui/system", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"v1.2","uptimeSeconds":90,"goroutines":12,"heapAllocBytes":1024,"queriesTotal":500}`))
	})
	mux.HandleFunc("/api/ui/stats/overview", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"queries":500,"blocked":40,"cached":100,"clients":9,"avgMs":1.5,"p95Ms":8.0}`))
	})
	mux.HandleFunc("/api/ui/noise/overview", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"decoys":33,"bySource":{"cohort":20,"walk":13}}`))
	})
	mux.HandleFunc("/api/ui/stats/top", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("col") != "domain" || r.URL.Query().Get("n") != "8" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"name":"a.com","count":9},{"name":"b.com","count":4}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL)

	sys, err := c.System()
	if err != nil || sys.Version != "v1.2" || sys.UptimeSeconds != 90 || sys.QueriesTotal != 500 {
		t.Errorf("System() = %+v, err %v", sys, err)
	}

	ov, err := c.Overview()
	if err != nil || ov.Queries != 500 || ov.Blocked != 40 || ov.P95Ms != 8.0 {
		t.Errorf("Overview() = %+v, err %v", ov, err)
	}

	no, err := c.NoiseOverview()
	if err != nil || no.Decoys != 33 || no.BySource["cohort"] != 20 {
		t.Errorf("NoiseOverview() = %+v, err %v", no, err)
	}

	top, err := c.Top("domain", 8)
	if err != nil || len(top) != 2 || top[0].Name != "a.com" || top[0].Count != 9 {
		t.Errorf("Top() = %+v, err %v", top, err)
	}
}

// NewClient must strip a trailing slash so paths don't double up.
func TestNewClientTrimsTrailingSlash(t *testing.T) {
	if NewClient("http://host:80/").Base != "http://host:80" {
		t.Error("trailing slash not trimmed")
	}
}
