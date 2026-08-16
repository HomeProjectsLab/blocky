package log

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/sirupsen/logrus"
)

// logRingSize bounds the on-load snapshot buffer. Fixed for now.
// ponytail: constant, not config — promote to a knob when someone needs to tune it.
const logRingSize = 500

// logSubBuffer is the per-subscriber channel capacity; a subscriber that falls
// this far behind loses lines instead of slowing down the caller's log call.
const logSubBuffer = 256

// LogLine is the JSON shape of one captured log entry, shared by the /api/ui/logs
// SSE stream and the /api/ui/logs/recent snapshot, so field names can't drift.
type LogLine struct {
	TS     string            `json:"ts"`               // entry.Time.Format("15:04:05.000")
	Level  string            `json:"level"`            // entry.Level.String()
	Prefix string            `json:"prefix"`           // entry.Data["prefix"], "" if none
	Msg    string            `json:"msg"`              //
	Fields map[string]string `json:"fields,omitempty"` // Data minus "prefix", stringified
}

// LogHub is a logrus.Hook that captures every log entry into a bounded ring
// buffer (for the on-load snapshot) and fans it out to live SSE subscribers.
// The query log has sqlite for history; the log hub has no DB, so it owns the ring.
type LogHub struct {
	mu     sync.Mutex
	subs   map[chan []byte]struct{}
	ring   [][]byte       // marshalled LogLine JSON, newest last, capped at logRingSize
	levels []logrus.Level // parallel to ring: the level of each entry, for ?level= filtering
}

// Console is the package singleton, installed as a hook in init() (logger.go).
//
//nolint:gochecknoglobals
var Console = &LogHub{subs: map[chan []byte]struct{}{}}

// Levels captures every level; SetLevel(cfg.Level) already gates what reaches Fire,
// so raising the log level to cut Pi3 load costs the hook nothing.
func (h *LogHub) Levels() []logrus.Level { return logrus.AllLevels }

// Fire records the entry. Three hard guarantees, all in this function:
//  1. re-entrancy: it NEVER calls logrus/log.* (no self-recursion) and a marshal
//     panic is swallowed by recover so it can't kill the caller's log call.
//  2. non-blocking: a full subscriber buffer drops the line; a slow browser never blocks DNS.
//  3. bounded: the ring is trimmed on every append.
func (h *LogHub) Fire(e *logrus.Entry) error {
	defer func() { _ = recover() }() // (1) marshal panic must not kill the caller

	data, err := json.Marshal(lineFrom(e)) // (1) never call logrus/log.* in here
	if err != nil {
		return nil // (1) drop silently; logging the error would risk recursion
	}

	// ponytail: single Lock (not RLock+Lock) — Fire already holds the write lock
	// for the ring; a browsers-count of subscribers makes contention irrelevant.
	h.mu.Lock()

	h.ring = append(h.ring, data)
	h.levels = append(h.levels, e.Level)

	if len(h.ring) > logRingSize {
		h.ring = h.ring[len(h.ring)-logRingSize:]
		h.levels = h.levels[len(h.levels)-logRingSize:]
	}

	for ch := range h.subs {
		select {
		case ch <- data: // (2) non-blocking
		default: // subscriber too slow, drop
		}
	}

	h.mu.Unlock()

	return nil
}

// lineFrom reads e.Data directly into the contract shape. It does NOT call the
// logger's Formatter (cheaper, and avoids any formatter that might touch the
// logger). prefix is pulled out; every other field is stringified with fmt.Sprint.
func lineFrom(e *logrus.Entry) LogLine {
	line := LogLine{
		TS:    e.Time.Format("15:04:05.000"),
		Level: e.Level.String(),
		Msg:   e.Message,
	}

	if p, ok := e.Data[prefixField].(string); ok {
		line.Prefix = p
	}

	for k, v := range e.Data {
		if k == prefixField {
			continue
		}

		if line.Fields == nil {
			line.Fields = make(map[string]string, len(e.Data))
		}

		line.Fields[k] = fmt.Sprint(v)
	}

	return line
}

// Subscribe returns a channel of marshalled LogLine JSON and an idempotent
// unsubscribe function. The channel is never closed by the hub.
func (h *LogHub) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, logSubBuffer)

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

// Recent returns a snapshot of the ring, oldest first, keeping only lines at or
// above min. A nil min (pass nil) returns everything.
func (h *LogHub) Recent(min *logrus.Level) [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([][]byte, 0, len(h.ring))

	for i, data := range h.ring {
		// logrus levels are DESCENDING by severity (Panic=0 … Trace=6), so
		// "at or above min" means level number <= min.
		if min != nil && h.levels[i] > *min {
			continue
		}

		out = append(out, data)
	}

	return out
}
