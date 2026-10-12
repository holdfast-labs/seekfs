package main

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SearchResult is one completed search delivered to the UI thread.
type SearchResult struct {
	Seq          int64
	Query        string // raw typed query
	ServiceQuery string // normalized query actually sent
	OK           bool
	Message      string
	Rows         []ResultRow
	Count        int
	ElapsedMS    int64
	SearchMS     float64
	Source       string
	Content      *contentState
	ContentMode  bool
	Err          error
}

// Searcher debounces as-you-type searches and drops stale responses by
// sequence number, mirroring UIApp.SearchAsync in cmd/seekfs/ui.go. All
// service I/O happens on a single background goroutine; results arrive on
// the deliver channel for the UI thread to pick up.
type Searcher struct {
	client *Client
	limit  int

	mu        sync.Mutex
	seq       atomic.Int64
	debounce  time.Duration
	timer     *time.Timer
	pending   SearchParams
	hasPend   bool
	deliver   chan SearchResult
	stopped   bool
	readyFn   func() bool
	onDeliver func(SearchResult)
}

func NewSearcher(client *Client, limit int, debounce time.Duration) *Searcher {
	if limit <= 0 {
		limit = defaultResultLimit
	}
	if debounce <= 0 {
		debounce = 120 * time.Millisecond
	}
	return &Searcher{client: client, limit: limit, debounce: debounce}
}

// OnDeliver sets the callback invoked (on the searcher goroutine) with each
// fresh result. The UI must marshal to its own thread.
func (s *Searcher) OnDeliver(fn func(SearchResult)) {
	s.onDeliver = fn
}

// Query queues a debounced search; only the latest input within the window
// is sent. Empty or incomplete input yields an immediate empty result.
func (s *Searcher) Query(raw string, contentsMode bool, under string) int64 {
	seq := s.seq.Add(1)
	trimmed := strings.TrimSpace(raw)
	live := normalizeLiveQuery(trimmed)
	if strings.TrimSpace(live) == "" {
		s.emit(SearchResult{Seq: seq, Query: raw, OK: true, ContentMode: contentsMode})
		return seq
	}
	s.mu.Lock()
	s.pending = SearchParams{Query: live, ContentsMode: contentsMode, Under: under, Limit: s.limit, Seq: seq}
	s.hasPend = true
	if s.timer != nil {
		s.timer.Stop()
	}
	want := seq
	s.timer = time.AfterFunc(s.debounce, func() { s.fire(want) })
	s.mu.Unlock()
	return seq
}

// SearchNow sends immediately, bypassing the debounce (used for Enter).
func (s *Searcher) SearchNow(raw string, contentsMode bool, under string) int64 {
	seq := s.seq.Add(1)
	trimmed := strings.TrimSpace(raw)
	if strings.TrimSpace(normalizeLiveQuery(trimmed)) == "" {
		s.emit(SearchResult{Seq: seq, Query: raw, OK: true, ContentMode: contentsMode})
		return seq
	}
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.hasPend = false
	s.mu.Unlock()
	go s.run(SearchParams{Query: normalizeLiveQuery(trimmed), ContentsMode: contentsMode, Under: under, Limit: s.limit, Seq: seq})
	return seq
}

func (s *Searcher) fire(want int64) {
	s.mu.Lock()
	if !s.hasPend || s.pending.Seq != want || s.stopped {
		s.mu.Unlock()
		return
	}
	p := s.pending
	s.hasPend = false
	s.mu.Unlock()
	s.run(p)
}

func (s *Searcher) run(p SearchParams) {
	start := time.Now()
	resp, err := s.client.Search(p)
	elapsed := time.Since(start).Milliseconds()
	if p.Seq != s.seq.Load() {
		return // superseded by newer typing
	}
	res := SearchResult{
		Seq:         p.Seq,
		Query:       p.Query,
		OK:          err == nil && resp.OK,
		Rows:        rowsFromServiceResponse(resp),
		Count:       len(rowsFromServiceResponse(resp)),
		ElapsedMS:   elapsed,
		SearchMS:    resp.SearchMS,
		Source:      resp.Source,
		Content:     resp.Content,
		ContentMode: p.ContentsMode,
		Err:         err,
	}
	if err != nil {
		res.Message = err.Error()
		if contentUnavailable(err) {
			res.Message = "Content index unavailable: " + err.Error()
		}
	} else if !resp.OK {
		res.Message = firstNonEmpty(resp.Message, "search failed")
		if contentUnavailableString(resp.Message) {
			res.Message = "Content index unavailable: " + resp.Message
		}
	} else if resp.Fuzzy {
		res.Message = "showing close matches"
	}
	if q, _ := buildServiceQuery(p.Query, p.ContentsMode, false); q != "" {
		res.ServiceQuery = q
	}
	s.emit(res)
}

func (s *Searcher) emit(res SearchResult) {
	if s.onDeliver != nil {
		s.onDeliver(res)
		return
	}
	if s.deliver != nil {
		select {
		case s.deliver <- res:
		default:
		}
	}
}

func contentUnavailableString(msg string) bool {
	return contentUnavailable(errString(msg))
}

type errString string

func (e errString) Error() string { return string(e) }
