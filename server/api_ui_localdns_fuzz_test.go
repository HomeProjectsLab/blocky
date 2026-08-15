package server

// Standard-library fuzz + property coverage for the local-DNS zone assembler.
// These are plain `go test` funcs (they coexist with the Ginkgo suite). Run a
// crash-shakeout with:
//
//	go test ./server/... -run=^$ -fuzz=FuzzAssembleZone -fuzztime=15s
//
// then leave them as seed-corpus regressions (drop the -fuzz flag).

import (
	"strings"
	"testing"

	"github.com/miekg/dns"
)

// FuzzAssembleZone drives random {name,type,ttl,value} rows through assembleZone
// and then the same parser the loader uses. Invariants:
//   - assembleZone and the parser never panic on any input;
//   - the output either round-trips cleanly or is cleanly rejected (an error) —
//     it must never silently produce a different number of records than rows in
//     (one row here), which would be record injection / corruption;
//   - a cleanly-parsed row preserves the TTL: nil -> 3600, else the given value.
func FuzzAssembleZone(f *testing.F) {
	seeds := []struct {
		name, typ, value string
		ttl              uint32
		ttlNil           bool
	}{
		{"web.lan", "A", "10.0.0.5", 3600, false},
		{"www.lan", "CNAME", "web.lan", 0, true},
		{"nocache.lan", "A", "10.0.0.5", 0, false}, // explicit 0 must survive
		{"greet.lan", "TXT", "hello world", 120, false},
		{"lan", "MX", "10 mail.lan", 300, false},
		{"_sip._tcp.lan", "SRV", "10 5 5060 sip.lan", 60, false},
		{"x.lan", "PTR", "10.0.0.1", 3600, false},
		{"v6.lan", "AAAA", "2001:db8::1", 42, false},
		{"", "", "", 0, true},                        // empty row
		{"  a.lan  ", " a ", " 10.0.0.9 ", 1, false}, // whitespace + case
		{"bad.lan", "A", "not-an-ip", 3600, false},   // should be cleanly rejected
	}
	for _, s := range seeds {
		f.Add(s.name, s.typ, s.value, s.ttl, s.ttlNil)
	}

	f.Fuzz(func(t *testing.T, name, typ, value string, ttl uint32, ttlNil bool) {
		row := localDNSRow{Name: name, Type: typ, Value: value}
		if !ttlNil {
			row.TTL = &ttl
		}

		text := assembleZone([]localDNSRow{row}) // must not panic

		zp := dns.NewZoneParser(strings.NewReader(text), "", "")

		var rrs []dns.RR
		for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
			rrs = append(rrs, rr) // must not panic
		}

		if err := zp.Err(); err != nil {
			return // cleanly rejected — acceptable
		}

		// One row in must never yield MORE than one record: >1 means the name/value
		// smuggled extra records past assembly (injection / silent corruption). Zero
		// records is the parser safely skipping a degenerate line (e.g. a ';' comment)
		// — the putLocalDNS endpoint rejects that via its round-trip count guard.
		if len(rrs) > 1 {
			t.Fatalf("one row assembled to %d parsed records (record injection): text=%q", len(rrs), text)
		}

		if len(rrs) == 0 {
			return // degenerate-but-parseable line; endpoint guards this, not assembleZone
		}

		want := ttl
		if ttlNil {
			want = 3600
		}

		if got := rrs[0].Header().Ttl; got != want {
			t.Fatalf("TTL not preserved: want %d got %d (nil=%v) text=%q", want, got, ttlNil, text)
		}
	})
}
