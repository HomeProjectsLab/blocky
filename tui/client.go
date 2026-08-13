package tui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// QueryItem mirrors querylog.QueryItem (the /api/ui/queries + /api/ui/stream
// contract). Only the fields the dashboard renders are kept.
type QueryItem struct {
	TS          string `json:"ts"`
	Client      string `json:"client"`
	Question    string `json:"question"`
	Qtype       string `json:"qtype"`
	Rtype       string `json:"rtype"`
	Rcode       string `json:"rcode"`
	Decoy       bool   `json:"decoy"`
	DecoySource string `json:"decoySource"`
}

// System mirrors /api/ui/system.
type System struct {
	Version       string `json:"version"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
	Goroutines    int    `json:"goroutines"`
	HeapAllocRaw  uint64 `json:"heapAllocBytes"`
	QueriesTotal  int64  `json:"queriesTotal"`
}

// Overview mirrors /api/ui/stats/overview (and /noise/overview partially).
type Overview struct {
	Queries int64   `json:"queries"`
	Blocked int64   `json:"blocked"`
	Cached  int64   `json:"cached"`
	Clients int64   `json:"clients"`
	AvgMs   float64 `json:"avgMs"`
	P95Ms   float64 `json:"p95Ms"`
}

// DecoyOverview mirrors /api/ui/noise/overview.
type DecoyOverview struct {
	Decoys   int64            `json:"decoys"`
	BySource map[string]int64 `json:"bySource"`
}

// TopItem mirrors one /api/ui/stats/top item.
type TopItem struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// Client is a thin JSON client for the blocky /api/ui/* endpoints.
type Client struct {
	Base string // e.g. http://localhost:80
	HTTP *http.Client
}

func NewClient(base string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{}}
}

func (c *Client) getJSON(path string, out any) error {
	resp, err := c.HTTP.Get(c.Base + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) System() (System, error) {
	var s System
	err := c.getJSON("/api/ui/system", &s)

	return s, err
}

func (c *Client) Overview() (Overview, error) {
	var o Overview
	err := c.getJSON("/api/ui/stats/overview", &o)

	return o, err
}

func (c *Client) NoiseOverview() (DecoyOverview, error) {
	var d DecoyOverview
	err := c.getJSON("/api/ui/noise/overview", &d)

	return d, err
}

// Top fetches the n most frequent values of col ("domain", "client", ...).
func (c *Client) Top(col string, n int) ([]TopItem, error) {
	var out struct {
		Items []TopItem `json:"items"`
	}
	err := c.getJSON(fmt.Sprintf("/api/ui/stats/top?col=%s&n=%d", col, n), &out)

	return out.Items, err
}

// Stream opens the SSE feed and calls onQuery for every "query" event until the
// connection drops or ctx-cancelled body close. Blocks; run in a goroutine.
func (c *Client) Stream(onQuery func(QueryItem)) error {
	resp, err := c.HTTP.Get(c.Base + "/api/ui/stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream: %s", resp.Status)
	}

	return readSSE(resp.Body, onQuery)
}

// readSSE parses a text/event-stream, emitting one QueryItem per
// "event: query\ndata: {json}\n\n" block. Comment lines (": ping") and other
// event types are ignored. Pure/testable: feed it any io.Reader.
func readSSE(r io.Reader, onQuery func(QueryItem)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var event, data string

	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "": // end of event block
			if event == "query" && data != "" {
				var q QueryItem
				if json.Unmarshal([]byte(data), &q) == nil {
					onQuery(q)
				}
			}

			event, data = "", ""
		case strings.HasPrefix(line, ":"): // comment / ping
			continue
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data += strings.TrimSpace(line[len("data:"):])
		}
	}

	return sc.Err()
}
