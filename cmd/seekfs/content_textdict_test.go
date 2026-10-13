package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestContentTrainTextDictPicksRepeatedLines(t *testing.T) {
	sample := []byte("#include <stdio.h>\nint main() {\nreturn 0;\n}\n" +
		"#include <stdio.h>\nint helper() {\nreturn 1;\n}\n" +
		"#include <stdio.h>\nint main() {\nreturn 0;\n}\n" +
		"totally unique line here xyz\nanother singleton\n")
	dict := contentTrainTextDict(sample, 96<<10)
	if len(dict) == 0 {
		t.Fatal("no dict trained from repeated lines")
	}
	text := string(dict)
	for _, want := range []string{"#include <stdio.h>", "int main() {", "return 0;"} {
		if !strings.Contains(text, want+"\n") {
			t.Fatalf("dict missing repeated line %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "totally unique") || strings.Contains(text, "singleton") {
		t.Fatalf("dict contains singleton lines:\n%s", text)
	}
	// Deterministic: same input, same bytes.
	if again := contentTrainTextDict(sample, 96<<10); string(again) != text {
		t.Fatal("trainer is nondeterministic")
	}
	// Budget honored.
	if small := contentTrainTextDict(sample, 20); len(small) > 20 {
		t.Fatalf("dict %d bytes over the 20-byte budget", len(small))
	}
	// Nothing repeated: no dict (caller encodes plain).
	if d := contentTrainTextDict([]byte("a\nb\nc\n"), 96<<10); d != nil {
		t.Fatalf("singleton-only input trained %d bytes", len(d))
	}
	if d := contentTrainTextDict(nil, 96<<10); d != nil {
		t.Fatal("empty input trained a dict")
	}
	// Short lines (< 4 bytes) never qualify.
	if d := contentTrainTextDict([]byte("a\na\na\nbb\nbb\nbb\n"), 96<<10); d != nil {
		t.Fatalf("short lines trained %q", d)
	}
}

// v8 end to end on a corpus spanning the sample cap: the first blocks
// encode plain, the rest against the trained dict, and everything decodes.
// Forcing the sample cap tiny exercises the mid-stream train path that
// production hits at 8 MiB.
func TestContentTextDictBuildEndToEnd(t *testing.T) {
	oldSample, oldMin := contentDictSampleBytes, contentDictMinLogical
	contentDictSampleBytes = 32 << 10
	contentDictMinLogical = 0
	defer func() { contentDictSampleBytes, contentDictMinLogical = oldSample, oldMin }()

	docs := make([]contentBuildDoc, 0, 200)
	for i := 0; i < 200; i++ {
		body := "#include <stdio.h>\n#pragma once\n// boilerplate line one\n// boilerplate line two\nint func" +
			fmt.Sprint(i) + "() { return " + fmt.Sprint(i) + "; }\n" + strings.Repeat("padding text here ", 60)
		docs = append(docs, contentBuildDoc{path: fmt.Sprintf("f%03d.c", i), frn: uint64(3000 + i), text: []byte(body)})
	}
	idx, err := assembleContentIndex(docs, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dictSec, ok := idx.Sections[contentSectionDictText]
	if !ok || len(dictSec) == 0 {
		t.Fatal("forced dict build produced no CXTD section")
	}
	if _, _, err := decodeContentTextDict(dictSec); err != nil {
		t.Fatalf("CXTD does not decode: %v", err)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	// Every doc verifies identically through dict and plain frames.
	for i := range docs {
		got := r.docText(uint32(i))
		if string(got) != string(docs[i].text) {
			t.Fatalf("doc %d mismatch (%d vs %d bytes)", i, len(got), len(docs[i].text))
		}
	}
	if hits := r.search("boilerplate", 0); len(hits) != 200 {
		t.Fatalf("boilerplate hits = %d; want 200", len(hits))
	}
	if hits := r.search("func199", 0); len(hits) != 1 {
		t.Fatalf("func199 hits = %d; want 1", len(hits))
	}
}

// Same input twice builds byte-identical sidecars (deterministic sampling,
// training, blocking and encoding), dict included.
func TestContentTextDictBuildDeterministic(t *testing.T) {
	oldSample, oldMin := contentDictSampleBytes, contentDictMinLogical
	contentDictSampleBytes = 16 << 10
	contentDictMinLogical = 0
	defer func() { contentDictSampleBytes, contentDictMinLogical = oldSample, oldMin }()

	docs := make([]contentBuildDoc, 0, 100)
	for i := 0; i < 100; i++ {
		docs = append(docs, contentBuildDoc{
			path: fmt.Sprintf("g%03d.txt", i),
			frn:  uint64(4000 + i),
			text: []byte(fmt.Sprintf("common header line\nshared boilerplate %d\nbody %d %s\n", i%7, i, strings.Repeat("x", i%97))),
		})
	}
	build := func() []byte {
		idx, err := assembleContentIndex(docs, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		// BuiltAt stamps wall-clock time; pin it so byte-identity compares
		// the encoding, not the clock (like the stream tests do).
		idx.BuiltAt = time.Unix(0, 5)
		return contentIndexEncode(idx)
	}
	if a, b := build(), build(); !bytes.Equal(a, b) {
		t.Fatalf("same input built different bytes (%d vs %d)", len(a), len(b))
	}
}

// Below the logical threshold no dictionary is trained or stored: tiny
// corpora stay plain.
func TestContentTextDictSkippedWhenTiny(t *testing.T) {
	docs := []contentBuildDoc{{path: "a.txt", frn: 1, text: []byte("hello needle world")}}
	idx, err := assembleContentIndex(docs, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.Sections[contentSectionDictText]; ok {
		t.Fatal("tiny build must not carry a CXTD section")
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	if hits := r.search("needle", 0); len(hits) != 1 {
		t.Fatalf("tiny search hits = %d; want 1", len(hits))
	}
}

// Dict frames without their CXTD fail closed (no text, no hits, no panic):
// a torn sidecar can never serve wrong bytes.
func TestContentTextDictMissingFailsClosed(t *testing.T) {
	oldSample, oldMin := contentDictSampleBytes, contentDictMinLogical
	contentDictSampleBytes = 4 << 10
	contentDictMinLogical = 0
	defer func() { contentDictSampleBytes, contentDictMinLogical = oldSample, oldMin }()

	docs := make([]contentBuildDoc, 0, 60)
	for i := 0; i < 60; i++ {
		docs = append(docs, contentBuildDoc{
			path: fmt.Sprintf("h%03d.txt", i),
			frn:  uint64(5000 + i),
			text: []byte(fmt.Sprintf("shared line alpha\nshared line beta\ndoc %d %s\n", i, strings.Repeat("y", 200))),
		})
	}
	idx, err := assembleContentIndex(docs, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.Sections[contentSectionDictText]; !ok {
		t.Fatal("forced dict build produced no CXTD section")
	}
	delete(idx.Sections, contentSectionDictText)
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	// Plain-head docs (cut before training) still serve; dict frames do not.
	// Either way nothing fabricated appears.
	for _, h := range r.search("shared", 0) {
		text := r.docText(h.DocID)
		if len(text) == 0 {
			continue
		}
		if !strings.Contains(string(text), "shared") {
			t.Fatalf("doc %d served text without the term", h.DocID)
		}
	}
}

func TestContentTextDictSectionRoundTrip(t *testing.T) {
	content := []byte("#include <x>\n#pragma once\n")
	enc := encodeContentTextDict(contentTextDictID, content)
	if enc == nil {
		t.Fatal("no section for valid dict")
	}
	id, back, err := decodeContentTextDict(enc)
	if err != nil {
		t.Fatal(err)
	}
	if id != contentTextDictID || string(back) != string(content) {
		t.Fatalf("round trip = (%d, %q)", id, back)
	}
	if encodeContentTextDict(0, content) != nil {
		t.Fatal("zero id must not encode")
	}
	if encodeContentTextDict(1, nil) != nil {
		t.Fatal("empty content must not encode")
	}
	for _, bad := range [][]byte{nil, {}, {1, 2, 3}, make([]byte, 8)} {
		if _, _, err := decodeContentTextDict(bad); err == nil {
			t.Fatalf("%d-byte section decoded without error", len(bad))
		}
	}
	// Zero id / empty content rejected.
	zeroID := encodeContentTextDict(7, content)
	zeroID[0], zeroID[1], zeroID[2], zeroID[3] = 0, 0, 0, 0
	if _, _, err := decodeContentTextDict(zeroID); err == nil {
		t.Fatal("zero-id section decoded without error")
	}
}
