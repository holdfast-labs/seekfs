package feature

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestBoundedProtocolFrames(t *testing.T) {
	var buf bytes.Buffer
	want := Request{Version: Version, ID: 1, Op: "query", Query: "two words"}
	if err := Write(&buf, want); err != nil {
		t.Fatal(err)
	}
	scanner := NewScanner(&buf)
	if !scanner.Scan() {
		t.Fatal(scanner.Err())
	}
	var got Request
	if err := json.Unmarshal(scanner.Bytes(), &got); err != nil || got.Query != want.Query || got.ID != want.ID {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	if err := Write(&buf, Request{Query: strings.Repeat("x", MaxFrameBytes)}); err == nil {
		t.Fatal("oversized outgoing frame accepted")
	}
	scanner = NewScanner(strings.NewReader(strings.Repeat("x", MaxFrameBytes) + "\n"))
	if scanner.Scan() || scanner.Err() == nil {
		t.Fatal("oversized incoming frame accepted")
	}
}
