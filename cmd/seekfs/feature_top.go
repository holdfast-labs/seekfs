package main

import (
	"container/heap"
	"fmt"
	"sort"
	"strings"
)

const featureResultByteBudget = 16 << 20

// Keep only the requested page while verifying every candidate for ordering
// and completeness. Selected strings must outlive the filename mmap lock.
type featureTopHeap struct {
	entries      []Entry
	pq           parsedQuery
	limit, bytes int
}

func (h featureTopHeap) Len() int { return len(h.entries) }
func (h featureTopHeap) Less(i, j int) bool {
	return compareFeatureEntries(h.entries[i], h.entries[j], h.pq) > 0
}
func (h featureTopHeap) Swap(i, j int) { h.entries[i], h.entries[j] = h.entries[j], h.entries[i] }
func (h *featureTopHeap) Push(value any) {
	e := value.(Entry)
	h.entries = append(h.entries, e)
	h.bytes += featureEntryBytes(e)
}
func (h *featureTopHeap) Pop() any {
	n := len(h.entries) - 1
	e := h.entries[n]
	h.entries[n] = Entry{}
	h.entries = h.entries[:n]
	h.bytes -= featureEntryBytes(e)
	return e
}

func featureEntryBytes(e Entry) int {
	return 256 + len(e.Path) + len(e.Name) + len(e.LowerPath) + len(e.LowerName)
}

func compareFeatureEntries(a, b Entry, pq parsedQuery) int {
	if root := firstNonEmpty(pq.CWDBias, pq.RootBias); root != "" {
		aBias, bBias := pathUnder(a.Path, root), pathUnder(b.Path, root)
		if aBias != bBias {
			if aBias {
				return -1
			}
			return 1
		}
	}
	return compareSearchAllEntries(a, b, pq)
}

func (h *featureTopHeap) offer(e Entry) error {
	if h.limit == 0 {
		return nil
	}
	oldBytes := 0
	if len(h.entries) >= h.limit {
		if compareFeatureEntries(e, h.entries[0], h.pq) >= 0 {
			return nil
		}
		oldBytes = featureEntryBytes(h.entries[0])
	}
	if h.bytes+featureEntryBytes(e)-oldBytes > featureResultByteBudget {
		return fmt.Errorf("feature result byte budget exceeded; reduce -n")
	}
	e.Path, e.Name = strings.Clone(e.Path), strings.Clone(e.Name)
	e.LowerPath, e.LowerName = strings.Clone(e.LowerPath), strings.Clone(e.LowerName)
	if len(h.entries) < h.limit {
		heap.Push(h, e)
	} else {
		h.bytes += featureEntryBytes(e) - oldBytes
		h.entries[0] = e
		heap.Fix(h, 0)
	}
	return nil
}

func (h *featureTopHeap) sorted() []Entry {
	sort.Slice(h.entries, func(i, j int) bool { return compareFeatureEntries(h.entries[i], h.entries[j], h.pq) < 0 })
	return h.entries
}
