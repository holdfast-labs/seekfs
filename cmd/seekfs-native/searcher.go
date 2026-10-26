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

// Searcher runs as-you-type searches with a leading edge and latest-wins
// coalescing, and drops stale responses by sequence number, mirroring
// UIApp.SearchAsync in cmd/seekfs/ui.go. The first query after an idle period
// is sent without a trailing debounce. While one request is in flight only the
// latest query is kept and sent when that request returns. All service I/O
// happens on a background goroutine; results arrive on the deliver channel for
// the UI thread to pick up.
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
	inFlight  bool
}

// searcherBurst is the upper bound on the pre-send coalescing tick. It merges
// keystrokes that arrive in the same input burst before the first request
// leaves, but it is much shorter than the old 120 ms trailing debounce.
const searcherBurst = 2 * time.Millisecond

// burstWindow clamps the constructor's debounce argument to a short pre-send
// coalescing tick. The argument no longer delays every search by its full
// value; leading-edge sends start after at most searcherBurst.
func burstWindow(debounce time.Duration) time.Duration {
	if debounce <= 0 || debounce > searcherBurst {
		return searcherBurst
	}
	return debounce
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

// Query queues a search with a leading edge. If no request is in flight it is
// sent after a short burst-coalescing tick; otherwise only the latest query is
// kept and sent when the in-flight request returns. Empty or incomplete input
// yields an immediate empty result.
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
		s.timer = nil
	}
	if !s.inFlight {
		want := seq
		s.timer = time.AfterFunc(burstWindow(s.debounce), func() { s.fire(want) })
	}
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
	if !s.hasPend || s.pending.Seq != want || s.stopped || s.inFlight {
		s.mu.Unlock()
		return
	}
	p := s.pending
	s.hasPend = false
	s.inFlight = true
	s.mu.Unlock()
	go s.runLoop(p)
}

// runLoop sends p, then sends the latest pending query (if any) as soon as the
// previous request returns. Only one request is in flight at a time.
func (s *Searcher) runLoop(p SearchParams) {
	for {
		s.run(p)
		s.mu.Lock()
		if s.stopped || !s.hasPend {
			s.inFlight = false
			s.mu.Unlock()
			return
		}
		p = s.pending
		s.hasPend = false
		s.mu.Unlock()
	}
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
