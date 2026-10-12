package main

import "testing"

func TestHistoryBackForward(t *testing.T) {
	h := NewHistory(10)
	h.Push(HistoryEntry{Query: "a"})
	h.Push(HistoryEntry{Query: "b"})
	h.Push(HistoryEntry{Query: "c"})
	if !h.CanBack() || h.CanForward() {
		t.Fatal("at newest: back yes, forward no")
	}
	e, ok := h.Back()
	if !ok || e.Query != "b" {
		t.Fatalf("back = %v %v", e, ok)
	}
	e, ok = h.Back()
	if !ok || e.Query != "a" {
		t.Fatalf("back = %v %v", e, ok)
	}
	if _, ok := h.Back(); ok {
		t.Fatal("back past oldest must fail")
	}
	e, ok = h.Forward()
	if !ok || e.Query != "b" {
		t.Fatalf("forward = %v %v", e, ok)
	}
	// Pushing after going back drops the forward branch.
	h.Push(HistoryEntry{Query: "d"})
	if h.CanForward() {
		t.Fatal("push must drop forward branch")
	}
	e, _ = h.Current()
	if e.Query != "d" {
		t.Fatalf("current = %v", e)
	}
}

func TestHistoryDuplicatePush(t *testing.T) {
	h := NewHistory(10)
	h.Push(HistoryEntry{Query: "a"})
	h.Push(HistoryEntry{Query: "a"})
	if h.CanBack() {
		t.Fatal("duplicate push must not create an entry")
	}
}

func TestHistoryLimit(t *testing.T) {
	h := NewHistory(3)
	for _, q := range []string{"a", "b", "c", "d"} {
		h.Push(HistoryEntry{Query: q})
	}
	e, _ := h.Current()
	if e.Query != "d" {
		t.Fatalf("current = %v", e)
	}
	h.Back()
	h.Back()
	if _, ok := h.Back(); ok {
		t.Fatal("oldest (a) must have been evicted")
	}
}
