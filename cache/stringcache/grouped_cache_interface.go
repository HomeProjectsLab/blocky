package stringcache

type GroupedStringCache interface {
	// Contains checks if one or more groups in the cache contains the search string.
	// Returns a map of matching group -> the rule that matched, or nil/empty when
	// the string was not found.
	//
	// The returned map must be freshly allocated and owned by the caller: callers
	// (e.g. ChainedGroupedCache) may retain and mutate it. Implementations must not
	// return a retained, shared, or pooled map.
	Contains(searchString string, groups []string) map[string]string

	// Refresh creates new factory for the group to be refreshed.
	// Calling Finish on the factory will perform the group refresh.
	Refresh(group string) GroupFactory

	// ElementCount returns the amount of elements in the group
	ElementCount(group string) int

	// SnapshotGroup captures the group's currently-built cache as an opaque,
	// by-reference handle. The built caches are immutable after construction, so
	// the handle can be installed into another cache of the same concrete type
	// (via RestoreGroup) without copying or re-parsing the group's entries. Used
	// to reuse an unchanged blocklist group across a resolver rebuild.
	SnapshotGroup(group string) any

	// RestoreGroup installs a snapshot previously returned by SnapshotGroup on a
	// cache of the same concrete type, by reference (copy-on-write). The two
	// caches then share one immutable copy of the group's entries.
	RestoreGroup(group string, snapshot any)
}

type GroupFactory interface {
	// AddEntry adds a new string to the factory to be added later to the cache groups.
	AddEntry(entry string) bool

	// Count returns amount of processed string in the factory
	Count() int

	// Finish replaces the group in cache with factory's content
	Finish()
}
