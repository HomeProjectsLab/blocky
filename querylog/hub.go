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
}

func NewHub() *Hub {
	return &Hub{subs: make(map[chan []byte]struct{})}
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
