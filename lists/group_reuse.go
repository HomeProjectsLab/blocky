package lists

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"sync"

	"github.com/0xERR0R/blocky/config"
)

// Per-group cache reuse across resolver rebuilds.
//
// A config apply tears down the server and rebuilds every resolver, which
// re-runs NewListCache. Re-streaming and re-parsing every blocklist category
// from sqlite takes minutes on a Pi and leaves DNS down the whole time, even
// though the usual edit (one manual deny domain, a category toggle) changes a
// single group and leaves every other category byte-identical.
//
// This registry keeps each group's built (immutable) cache alive across the
// rebuild, keyed by a fingerprint of what the group is built from. On the next
// build, a group whose fingerprint is unchanged reuses the prior cache by
// reference instead of re-streaming; only changed/new groups are rebuilt. The
// registry is process-scoped, mirroring SetBlocklistProvider.

// reusableGroup pairs a group's content fingerprint with a by-reference handle
// to its built cache (see stringcache.GroupedStringCache.SnapshotGroup).
type reusableGroup struct {
	fingerprint string
	snapshot    any
}

//nolint:gochecknoglobals // process-wide registry, like blProvider above
var (
	groupReuseMu sync.Mutex
	groupReuse   = map[ListCacheType]map[string]reusableGroup{}
)

// lookupReusableGroup returns the prior build's snapshot for a group when its
// fingerprint still matches. A miss (new group, changed group, or no prior
// build) returns ok=false and the caller rebuilds — the fail-safe direction.
func lookupReusableGroup(t ListCacheType, group, fingerprint string) (any, bool) {
	groupReuseMu.Lock()
	defer groupReuseMu.Unlock()

	g, ok := groupReuse[t][group]
	if ok && g.fingerprint == fingerprint {
		return g.snapshot, true
	}

	return nil, false
}

// registerReusableGroups replaces the whole registry entry for a list type with
// the groups from the just-finished build. Groups that vanished (e.g. a category
// toggled off) are dropped, so their caches become unreferenced and can be GC'd
// — the registry never pins more than the current build's groups.
func registerReusableGroups(t ListCacheType, groups map[string]reusableGroup) {
	groupReuseMu.Lock()
	defer groupReuseMu.Unlock()

	groupReuse[t] = groups
}

// groupReuseEligible reports whether a group's content is fully captured by its
// fingerprint, so reusing it can never serve stale content. Only two source
// kinds qualify: inline text (the bytes ARE the content, and they are hashed)
// and "blocklist:<cat>" refs (content is version-gated via BlocklistVersion).
// HTTP and plain-file sources can change out-of-band between refreshes — the
// very reason periodic refresh re-downloads them — so a group containing one is
// never reused and always rebuilt, exactly as before this change.
func groupReuseEligible(sources []config.BytesSource) bool {
	for _, s := range sources {
		switch {
		case s.Type == config.BytesSourceTypeText:
		case s.Type == config.BytesSourceTypeFile && strings.HasPrefix(s.From, BlocklistSourcePrefix):
		default:
			return false
		}
	}

	return true
}

// groupFingerprint hashes everything a group's built cache depends on: each
// source's type and bytes in order (which already inlines manual deny/allow
// entries, so a manual edit changes it), plus the stored content-version of
// every "blocklist:<cat>" source (so an updater refresh busts just that group).
// Fields are length-prefixed so no source boundary can collide with another.
func groupFingerprint(sources []config.BytesSource) string {
	h := sha256.New()

	var num [8]byte

	write := func(b []byte) {
		binary.BigEndian.PutUint64(num[:], uint64(len(b)))
		_, _ = h.Write(num[:])
		_, _ = h.Write(b)
	}

	for _, s := range sources {
		binary.BigEndian.PutUint16(num[:2], uint16(s.Type))
		_, _ = h.Write(num[:2])
		write([]byte(s.From))

		if s.Type == config.BytesSourceTypeFile && strings.HasPrefix(s.From, BlocklistSourcePrefix) {
			cat := strings.TrimPrefix(s.From, BlocklistSourcePrefix)
			write([]byte(blocklistVersion(cat)))
		}
	}

	return hex.EncodeToString(h.Sum(nil))
}
