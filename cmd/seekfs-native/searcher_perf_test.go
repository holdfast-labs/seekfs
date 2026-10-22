package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSearcherLeadingEdge(t *testing.T) {
	sent := make(chan string, 4)
	c := &Client{}
	c.exchange = func(req serviceRequest) (serviceResponse, error) {
		sent <- req.Query
		return serviceResponse{OK: true}, nil
	}
	s := NewSearcher(c, 50, 120*time.Millisecond)
	s.OnDeliver(func(SearchResult) {})
	start := time.Now()
	s.Query("alpha", false, "")
	select {
	case q := <-sent:
		if q != "alpha" {
			t.Fatalf("sent query = %q, want alpha", q)
		}
		if d := time.Since(start); d >= 100*time.Millisecond {
			t.Fatalf("first query waited %v; leading edge must not wait for the 120 ms debounce", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first query was not sent")
	}
}

func TestSearcherCoalescesInFlight(t *testing.T) {
	started := make(chan string, 32)
	release := make(chan struct{})
	var mu sync.Mutex
	var sends []string
	c := &Client{}
	c.exchange = func(req serviceRequest) (serviceResponse, error) {
		mu.Lock()
		sends = append(sends, req.Query)
		mu.Unlock()
		started <- req.Query
		<-release
		return serviceResponse{OK: true}, nil
	}
	s := NewSearcher(c, 50, 120*time.Millisecond)
	delivered := make(chan SearchResult, 32)
	s.OnDeliver(func(r SearchResult) { delivered <- r })

	s.Query("q0", false, "")
	select {
	case q := <-started:
		if q != "q0" {
			t.Fatalf("first send = %q, want q0", q)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first query was not sent")
	}
	for i := 1; i <= 10; i++ {
		s.Query(fmt.Sprintf("q%d", i), false, "")
	}
	close(release)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case r := <-delivered:
			if r.Query != "q10" {
				continue
			}
			mu.Lock()
			n := len(sends)
			last := ""
			if n > 0 {
				last = sends[n-1]
			}
			mu.Unlock()
			if n > 3 {
				t.Fatalf("rapid queries caused %d sends (%v), want at most 3", n, sends)
			}
			if last != "q10" {
				t.Fatalf("last send = %q, want q10", last)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for the final result")
		}
	}
}

func TestSearcherDropsStaleResponse(t *testing.T) {
	started := make(chan string, 8)
	gate := make(chan struct{})
	c := &Client{}
	c.exchange = func(req serviceRequest) (serviceResponse, error) {
		started <- req.Query
		if req.Query == "old" {
			<-gate
		}
		return serviceResponse{OK: true, Rows: []jsonResult{{Path: `C:\` + req.Query, Name: req.Query}}}, nil
	}
	s := NewSearcher(c, 50, 120*time.Millisecond)
	delivered := make(chan SearchResult, 8)
	s.OnDeliver(func(r SearchResult) { delivered <- r })

	s.Query("old", false, "")
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("old query was not sent")
	}
	s.SearchNow("new", false, "")
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("new query was not sent")
	}
	select {
	case r := <-delivered:
		if r.Query != "new" {
			t.Fatalf("first delivered result = %q, want new", r.Query)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("new result not delivered")
	}
	close(gate)
	select {
	case r := <-delivered:
		t.Fatalf("stale result delivered: %q", r.Query)
	case <-time.After(300 * time.Millisecond):
	}
}
