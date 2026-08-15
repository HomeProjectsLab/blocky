package configstore

// Plain-testing fuzz + property tests (kept out of the Ginkgo suite so the
// fuzz corpus works with `go test -fuzz`). Each seeds a fresh store per run so
// iterations stay isolated. Normal `go test` replays only the seed corpus.

import (
	"strings"
	"testing"

	"github.com/0xERR0R/blocky/config"
	"gopkg.in/yaml.v2"
)

// fuzzStore opens a throwaway store in a per-run temp dir.
func fuzzStore(t *testing.T) *Store {
	t.Helper()

	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = s.Close() })

	return s
}

// upstreamsIntact asserts the seeded upstreams section still resolves — the
// canary for "a section write corrupted an unrelated section".
func upstreamsIntact(t *testing.T, s *Store) {
	t.Helper()

	cfg, err := s.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig after write: %v", err)
	}

	if got := cfg.Upstreams.EffectiveStrategy("default"); got != config.UpstreamStrategyRecursive {
		t.Fatalf("unrelated upstreams section corrupted: strategy=%v", got)
	}
}

// FuzzSetLocalDNSZone fuzzes the zone round-trip: an accepted zone must read
// back verbatim, a rejected one must leave the blob untouched, and the
// unrelated upstreams section must always survive. Never panics.
func FuzzSetLocalDNSZone(f *testing.F) {
	for _, seed := range []string{
		"",
		"host.lan. 3600 IN A 10.0.0.5\n",
		"a.lan. 3600 IN A 10.0.0.1\nb.lan. 3600 IN AAAA ::1\n",
		"not a valid zone !!!\n",
		"zone: [broken",
		"\n\n\n",
		"héllo.lan. 3600 IN A 10.0.0.9\n",
		strings.Repeat("x.lan. 3600 IN A 10.0.0.1\n", 50),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, zone string) {
		s := fuzzStore(t)

		before, err := s.RawYAML()
		if err != nil {
			t.Fatalf("RawYAML: %v", err)
		}

		if err := s.SetLocalDNSZone(zone); err != nil {
			// Rejected: blob must be byte-for-byte untouched.
			after, rErr := s.RawYAML()
			if rErr != nil {
				t.Fatalf("RawYAML after rejected set: %v", rErr)
			}

			if after != before {
				t.Fatalf("rejected SetLocalDNSZone mutated the blob")
			}

			return
		}

		// Accepted: zone reads back verbatim...
		got, err := s.GetLocalDNSZone()
		if err != nil {
			t.Fatalf("GetLocalDNSZone: %v", err)
		}

		if got != zone {
			t.Fatalf("zone round-trip mismatch:\n set = %q\n got = %q", zone, got)
		}

		// ...and the unrelated section is intact.
		upstreamsIntact(t, s)
	})
}

// FuzzSetPrivacy fuzzes privacy field values over a valid baseline: on accept,
// every fuzzed field reads back unchanged and upstreams survive; on reject the
// stored privacy stays at the (decoy-disabled) baseline. Never panics.
func FuzzSetPrivacy(f *testing.F) {
	// seed: (decoyEnable, startH, endH, ttlEnable, ttlPct, edns, caseRand, shadow, companionPct)
	f.Add(false, 0, 24, false, uint(10), false, false, true, uint(40))
	f.Add(true, 9, 21, true, uint(20), true, true, true, uint(0))
	f.Add(true, 0, 1, false, uint(0), false, false, false, uint(100))
	f.Add(true, 23, 24, true, uint(90), true, false, true, uint(50))
	f.Add(true, 10, 10, false, uint(200), false, false, false, uint(999)) // invalid: start>=end, pct too big

	f.Fuzz(func(t *testing.T, decoyEnable bool, startH, endH int, ttlEnable bool, ttlPct uint,
		edns, caseRand, shadow bool, companionPct uint,
	) {
		s := fuzzStore(t)

		p, err := s.GetPrivacy()
		if err != nil {
			t.Fatalf("baseline GetPrivacy: %v", err)
		}

		p.Decoy.Enable = decoyEnable
		p.Decoy.ActiveHoursStart = startH
		p.Decoy.ActiveHoursEnd = endH
		p.Decoy.CompanionPct = companionPct
		p.TTLJitter.Enable = ttlEnable
		p.TTLJitter.PercentPct = ttlPct
		p.EDNSPadding.Enable = edns
		p.QueryCaseRandomization = caseRand
		p.ShadowBlockedQueries = shadow

		if err := s.SetPrivacy(p); err != nil {
			// Rejected: privacy stays at baseline (decoy disabled) and the blob
			// still loads cleanly.
			got, gErr := s.GetPrivacy()
			if gErr != nil {
				t.Fatalf("GetPrivacy after rejected set: %v", gErr)
			}

			if got.Decoy.Enable {
				t.Fatalf("rejected SetPrivacy leaked decoy.enable into the blob")
			}

			return
		}

		got, err := s.GetPrivacy()
		if err != nil {
			t.Fatalf("GetPrivacy after set: %v", err)
		}

		// Every fuzzed field round-trips.
		switch {
		case got.Decoy.Enable != decoyEnable:
			t.Fatalf("decoy.enable: set %v got %v", decoyEnable, got.Decoy.Enable)
		case got.Decoy.ActiveHoursStart != startH:
			t.Fatalf("activeHoursStart: set %d got %d", startH, got.Decoy.ActiveHoursStart)
		case got.Decoy.ActiveHoursEnd != endH:
			t.Fatalf("activeHoursEnd: set %d got %d", endH, got.Decoy.ActiveHoursEnd)
		case got.Decoy.CompanionPct != companionPct:
			t.Fatalf("companionPct: set %d got %d", companionPct, got.Decoy.CompanionPct)
		case got.TTLJitter.Enable != ttlEnable:
			t.Fatalf("ttlJitter.enable: set %v got %v", ttlEnable, got.TTLJitter.Enable)
		case got.TTLJitter.PercentPct != ttlPct:
			t.Fatalf("ttlJitter.percent: set %d got %d", ttlPct, got.TTLJitter.PercentPct)
		case got.EDNSPadding.Enable != edns:
			t.Fatalf("ednsPadding.enable: set %v got %v", edns, got.EDNSPadding.Enable)
		case got.QueryCaseRandomization != caseRand:
			t.Fatalf("queryCaseRandomization: set %v got %v", caseRand, got.QueryCaseRandomization)
		case got.ShadowBlockedQueries != shadow:
			t.Fatalf("shadowBlockedQueries: set %v got %v", shadow, got.ShadowBlockedQueries)
		}

		upstreamsIntact(t, s)
	})
}

// FuzzNestedMap feeds arbitrary YAML shapes through the same decode path the
// section readers use, then hands every level to nestedMap. Pure no-panic
// property test.
func FuzzNestedMap(f *testing.F) {
	for _, seed := range []string{
		"customDNS:\n  zone: z\n",
		"customDNS:\n  mapping:\n    a: b\n",
		"a: [1, 2, 3]\n",
		"a:\n  b:\n    c: d\n",
		"42: value\n",
		"- 1\n- 2\n",
		"just a scalar\n",
		"",
		"null\n",
		"a: {b: {c: {d: e}}}\n",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		// map[string]any decode path (what SetLocalDNSZone/GetLocalDNSZone use)
		root := map[string]any{}
		if err := yaml.Unmarshal([]byte(raw), &root); err == nil {
			for _, v := range root {
				walkNestedMap(v)
			}
		}

		// arbitrary-shape decode path
		var any1 any
		if err := yaml.Unmarshal([]byte(raw), &any1); err == nil {
			walkNestedMap(any1)
		}
	})
}

// walkNestedMap recursively runs nestedMap over every node of a decoded YAML
// tree — nestedMap must never panic on any shape.
func walkNestedMap(v any) {
	m := nestedMap(v) // must not panic
	for _, child := range m {
		walkNestedMap(child)
	}

	// also descend the raw yaml.v2 shapes nestedMap doesn't normalize
	switch t := v.(type) {
	case map[any]any:
		for _, child := range t {
			walkNestedMap(child)
		}
	case []any:
		for _, child := range t {
			walkNestedMap(child)
		}
	}
}
