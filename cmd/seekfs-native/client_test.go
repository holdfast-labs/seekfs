package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientSearchBuildsRequest(t *testing.T) {
	var got serviceRequest
	c := &Client{Timeout: time.Second, seqBase: 1000}
	c.exchange = func(req serviceRequest) (serviceResponse, error) {
		got = req
		return serviceResponse{OK: true}, nil
	}
	_, err := c.Search(SearchParams{Query: "path:Downloads ext:png", Seq: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != "search" || got.Query != "Downloads ext:png" || !got.MatchPath {
		t.Fatalf("req = %+v", got)
	}
	if got.RequestSeq != 1005 {
		t.Fatalf("seq = %d, want 1005", got.RequestSeq)
	}
	if got.DeadlineUnix <= time.Now().UnixNano() {
		t.Fatal("deadline must be in the future")
	}
}

func TestClientSearchContentsMode(t *testing.T) {
	var got serviceRequest
	c := &Client{seqBase: 1000}
	c.exchange = func(req serviceRequest) (serviceResponse, error) {
		got = req
		return serviceResponse{OK: true}, nil
	}
	_, _ = c.Search(SearchParams{Query: "needle ext:go", ContentsMode: true, Limit: 50})
	if got.Query != "content:needle ext:go" {
		t.Fatalf("query = %q", got.Query)
	}
	if got.Limit != 50 {
		t.Fatalf("limit = %d", got.Limit)
	}
}

func TestClientSearchDefaultLimit(t *testing.T) {
	var got serviceRequest
	c := &Client{seqBase: 1}
	c.exchange = func(req serviceRequest) (serviceResponse, error) {
		got = req
		return serviceResponse{OK: true}, nil
	}
	_, _ = c.Search(SearchParams{Query: "x"})
	if got.Limit != defaultResultLimit {
		t.Fatalf("limit = %d, want %d", got.Limit, defaultResultLimit)
	}
}

func TestSearcherDebounceKeepsLatest(t *testing.T) {
	c := &Client{seqBase: 0}
	var calls atomic.Int64
	var lastQuery atomic.Value
	c.exchange = func(req serviceRequest) (serviceResponse, error) {
		calls.Add(1)
		lastQuery.Store(req.Query)
		return serviceResponse{OK: true, Rows: []jsonResult{{Path: `C:\a`, Name: "a"}}}, nil
	}
	s := NewSearcher(c, 50, 30*time.Millisecond)
	done := make(chan SearchResult, 8)
	s.OnDeliver(func(r SearchResult) { done <- r })
	s.Query("aaa", false, "")
	s.Query("aab", false, "")
	// Wait for a result for the latest seq.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case res := <-done:
			if res.Query == "aab" {
				if calls.Load() != 1 {
					t.Fatalf("calls = %d, want exactly 1 debounced call", calls.Load())
				}
				if lastQuery.Load().(string) != "aab" {
					t.Fatalf("query = %v", lastQuery.Load())
				}
				return
			}
			// stale/empty results for "aaa" are fine; keep waiting
		case <-deadline:
			t.Fatal("timed out waiting for debounced result")
		}
	}
}

func TestSearcherEmptyQueryIsImmediate(t *testing.T) {
	c := &Client{}
	c.exchange = func(req serviceRequest) (serviceResponse, error) {
		return serviceResponse{}, errors.New("must not be called")
	}
	s := NewSearcher(c, 50, time.Hour)
	done := make(chan SearchResult, 1)
	s.OnDeliver(func(r SearchResult) { done <- r })
	s.Query("   ", false, "")
	select {
	case res := <-done:
		if !res.OK || len(res.Rows) != 0 {
			t.Fatalf("res = %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("empty query must resolve immediately")
	}
}

func TestSearcherIncompleteQueryIsImmediate(t *testing.T) {
	c := &Client{}
	c.exchange = func(req serviceRequest) (serviceResponse, error) {
		return serviceResponse{}, errors.New("must not be called")
	}
	s := NewSearcher(c, 50, time.Hour)
	done := make(chan SearchResult, 1)
	s.OnDeliver(func(r SearchResult) { done <- r })
	s.Query("ext:", false, "")
	select {
	case res := <-done:
		if !res.OK {
			t.Fatalf("res = %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("incomplete query must resolve immediately")
	}
}

func TestSearcherMarksContentUnavailable(t *testing.T) {
	c := &Client{}
	c.exchange = func(req serviceRequest) (serviceResponse, error) {
		return serviceResponse{}, errors.New("content indexing unavailable: the content index is not built or still building")
	}
	s := NewSearcher(c, 50, time.Millisecond)
	done := make(chan SearchResult, 1)
	s.OnDeliver(func(r SearchResult) { done <- r })
	s.SearchNow("content:needle", true, "")
	select {
	case res := <-done:
		if res.OK || res.Err == nil {
			t.Fatalf("res = %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out")
	}
}

func TestIsTimeoutError(t *testing.T) {
	if !isTimeoutError(errors.New(`seekfs service "search" request timed out after 20s`)) {
		t.Fatal("timeout not detected")
	}
	if isTimeoutError(errors.New("connection refused")) {
		t.Fatal("non-timeout misclassified")
	}
}
