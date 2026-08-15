package querylog

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// QueryItem is the shared JSON shape of one query for the UI API: both the
// /api/ui/queries search results and the /api/ui/stream SSE events use it, so
// the field names can't drift apart.
type QueryItem struct {
	TS          string   `json:"ts"`
	Client      string   `json:"client"`
	ClientNames []string `json:"clientNames"`
	Question    string   `json:"question"`
	Qtype       string   `json:"qtype"`
	Rtype       string   `json:"rtype"`
	Rcode       string   `json:"rcode"`
	Answer      string   `json:"answer"`
	DurationMs  int64    `json:"durationMs"`
	Transport   string   `json:"transport"`
	FpHash      string   `json:"fpHash"`
	Reason      string   `json:"reason"`
	Decoy       bool     `json:"decoy"`
	DecoySource string   `json:"decoySource"`
}

// ItemFromLogEntry maps a live entry to the contract shape (stream path).
func ItemFromLogEntry(e *LogEntry) QueryItem {
	return QueryItem{
		TS:          e.Start.Format(time.RFC3339),
		Client:      e.ClientIP,
		ClientNames: e.ClientNames,
		Question:    strings.TrimSuffix(e.QuestionName, "."),
		Qtype:       e.QuestionType,
		Rtype:       e.ResponseType,
		Rcode:       e.ResponseCode,
		Answer:      e.Answer,
		DurationMs:  e.DurationMs,
		Transport:   e.Fingerprint.Transport.String(),
		FpHash:      e.Fingerprint.Hash(),
		Reason:      e.ResponseReason,
		Decoy:       e.Decoy,
		DecoySource: e.DecoySource,
	}
}

// hubSubBuffer is the per-subscriber channel capacity; a subscriber that falls
// this far behind loses events instead of slowing anything down.
const hubSubBuffer = 256

// Hub fans live query log entries out to SSE subscribers. A nil *Hub is a
// valid no-op publisher, so callers never need a nil check.
type Hub struct {
	mu   sync.RWMutex
	subs map[chan []byte]struct{}
	qps  qpsCounter // rolling per-second query counts for the UI QPS readout
}

func NewHub() *Hub {
	return &Hub{subs: make(map[chan []byte]struct{})}
}

// qpsCounter is a lock-guarded ring of per-second query counts over the last
// hour, so the UI can show live QPS over several windows. Cheap: 3600 int
// buckets, incremented per query and summed on read.
type qpsCounter struct {
	mu    sync.Mutex
	count [3600]int64
	sec   [3600]int64 // the unix second each bucket currently holds (stale-guard)
}

func (q *qpsCounter) incr(now int64) {
	i := now % 3600

	q.mu.Lock()
	if q.sec[i] != now { // reused bucket from an hour ago: reset before counting
		q.sec[i], q.count[i] = now, 0
	}
	q.count[i]++
	q.mu.Unlock()
}

// rate returns queries/sec averaged over the last window seconds (1..3600).
func (q *qpsCounter) rate(now, window int64) float64 {
	if window <= 0 {
		return 0
	}

	if window > 3600 {
		window = 3600
	}

	var sum int64

	q.mu.Lock()
	for s := now - window + 1; s <= now; s++ {
		i := ((s % 3600) + 3600) % 3600
		if q.sec[i] == s {
			sum += q.count[i]
		}
	}
	q.mu.Unlock()

	return float64(sum) / float64(window)
}

// QPS returns the query rate over the last window (real + decoy queries).
// A nil Hub reports 0.
func (h *Hub) QPS(window time.Duration) float64 {
	if h == nil {
		return 0
	}

	return h.qps.rate(time.Now().Unix(), int64(window.Seconds()))
}

// Subscribe returns a channel of marshalled QueryItem JSON and an unsubscribe
// function (idempotent). The channel is never closed by the hub.
func (h *Hub) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, hubSubBuffer)

	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once

	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
		})
	}
}

// Publish marshals the entry once and offers it to every subscriber.
// A full subscriber buffer drops the event: a slow browser never blocks DNS.
func (h *Hub) Publish(entry *LogEntry) {
	if h == nil {
		return
	}

	h.qps.incr(time.Now().Unix()) // count every query for the QPS readout, even with no subscribers

	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(h.subs) == 0 {
		return
	}

	data, err := json.Marshal(ItemFromLogEntry(entry))
	if err != nil {
		return // programming error only; nothing useful to do per query
	}

	for ch := range h.subs {
		select {
		case ch <- data:
		default: // subscriber too slow, drop
		}
	}
}
