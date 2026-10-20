package main

import "strings"

// globalEntryDedupeKey identifies one logical file in a global result set.
// FRN is the stable identity on a USN volume. A walk volume may not carry a
// usable FRN, so the lowercased path is the fallback identity there.
type globalEntryDedupeKey struct {
	volume int
	frn    uint64
	path   string
}

func globalRankedEntryKey(item globalRankedEntry) (globalEntryDedupeKey, bool) {
	if item.entry.FRN != 0 {
		return globalEntryDedupeKey{volume: item.volume, frn: item.entry.FRN}, true
	}
	if path := strings.ToLower(item.entry.Path); path != "" {
		return globalEntryDedupeKey{volume: item.volume, path: path}, true
	}
	return globalEntryDedupeKey{}, false
}

// preferGlobalRankedEntry reports whether a should survive over b for one
// identity. A live overlay row is fresher than any base row. Between two base
// rows the newer modification time wins, and a later position breaks a tie
// (the fold appends the replacement after the row it shadows).
func preferGlobalRankedEntry(a, b globalRankedEntry) bool {
	if a.overlay != b.overlay {
		return a.overlay
	}
	if a.entry.ModUnix != b.entry.ModUnix {
		return a.entry.ModUnix > b.entry.ModUnix
	}
	return false
}

// dedupeGlobalRankedEntries keeps one row per logical file, at the position of
// the preferred row. The base index can carry a stale and a replacement record
// with the same FRN after a fold that did not shadow the stale row; every global
// lane must not return both. Order is otherwise unchanged.
func dedupeGlobalRankedEntries(items []globalRankedEntry) []globalRankedEntry {
	if len(items) < 2 {
		return items
	}
	preferred := make(map[globalEntryDedupeKey]int, len(items))
	for i, item := range items {
		key, ok := globalRankedEntryKey(item)
		if !ok {
			continue
		}
		if prev, found := preferred[key]; !found || preferGlobalRankedEntry(item, items[prev]) {
			preferred[key] = i
		}
	}
	if len(preferred) == len(items) {
		return items
	}
	out := make([]globalRankedEntry, 0, len(preferred))
	for i, item := range items {
		key, ok := globalRankedEntryKey(item)
		if !ok {
			out = append(out, item)
			continue
		}
		if preferred[key] == i {
			out = append(out, item)
		}
	}
	return out
}
