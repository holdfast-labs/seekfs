package main

// history.go: back/forward navigation over past searches (query + scope +
// contents mode), like a browser history.

// HistoryEntry is one visited search.
type HistoryEntry struct {
	Query        string
	Under        string
	ContentsMode bool
}

func (e HistoryEntry) isEmpty() bool {
	return e.Query == "" && e.Under == ""
}

// History is a bounded back/forward stack.
type History struct {
	entries []HistoryEntry
	pos     int // index of current; -1 when empty
	limit   int
}

func NewHistory(limit int) *History {
	if limit <= 0 {
		limit = 100
	}
	return &History{pos: -1, limit: limit}
}

func (h *History) Push(e HistoryEntry) {
	if h.pos >= 0 && h.entries[h.pos] == e {
		return // same as current: no new entry
	}
	// Drop any forward entries, then append.
	h.entries = append(h.entries[:h.pos+1], e)
	if len(h.entries) > h.limit {
		h.entries = h.entries[len(h.entries)-h.limit:]
	}
	h.pos = len(h.entries) - 1
}

func (h *History) Back() (HistoryEntry, bool) {
	if h.pos <= 0 {
		return HistoryEntry{}, false
	}
	h.pos--
	return h.entries[h.pos], true
}

func (h *History) Forward() (HistoryEntry, bool) {
	if h.pos+1 >= len(h.entries) {
		return HistoryEntry{}, false
	}
	h.pos++
	return h.entries[h.pos], true
}

func (h *History) Current() (HistoryEntry, bool) {
	if h.pos < 0 || h.pos >= len(h.entries) {
		return HistoryEntry{}, false
	}
	return h.entries[h.pos], true
}

func (h *History) CanBack() bool    { return h.pos > 0 }
func (h *History) CanForward() bool { return h.pos+1 < len(h.entries) }
