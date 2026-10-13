package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

var testContentFixedTime = time.Unix(0, 1_700_000_000_000_000_000)

// contentPerfCorpus exercises ASCII, UTF-8, invalid bytes, BOMs and case
// edges through the build fast paths.
func contentPerfCorpus() [][]byte {
	return [][]byte{
		[]byte(""),
		[]byte("hello world foo foo bar"),
		[]byte("Mixed CASE Identifiers and UNDER_SCORES 123"),
		[]byte("tabs\tand\nnewlines\n\n\nand   spaces"),
		[]byte("café naïve résumé \u4e2d\u6587 \U0001f600"),
		[]byte("invalid \xff\xfe bytes \x80here"),
		[]byte("\xef\xbb\xbfBOM marked file"),
		[]byte("\xff\xfeh\x00i\x00"),
		[]byte("dotless \u0131 and dotted \u0130 kelvin K \u212a sharp \u00df sigma \u03a3\u03c2\u03c3"),
		[]byte("a\x00b"),
		[]byte(strings.Repeat("ab ", 1<<16)),
	}
}

func TestContentFoldMatchesToLower(t *testing.T) {
	for i, b := range contentPerfCorpus() {
		if got, want := contentFoldText(b), []byte(strings.ToLower(string(b))); string(got) != string(want) {
			t.Fatalf("corpus %d: fold = %q, want ToLower %q", i, got, want)
		}
	}
}

func TestContentTermsOfFoldedMatches(t *testing.T) {
	for i, b := range contentPerfCorpus() {
		folded := contentFoldText(b)
		if got, want := contentTermsOfFolded(folded), contentTermsOf(folded); !reflect.DeepEqual(got, want) {
			t.Fatalf("corpus %d: terms-folded differs", i)
		}
	}
}

func TestContentGramsU32Matches(t *testing.T) {
	for i, b := range contentPerfCorpus() {
		folded := contentFoldText(b)
		want := contentGramsOf(folded)
		got := contentGramsOfU32(folded)
		if len(got) != len(want) {
			t.Fatalf("corpus %d: grams %d, want %d", i, len(got), len(want))
		}
		for g := range got {
			if _, ok := want[contentGramU32String(g)]; !ok {
				t.Fatalf("corpus %d: gram %q not in string set", i, contentGramU32String(g))
			}
		}
	}
}

func TestContentStoredTextIdentity(t *testing.T) {
	corpus := contentPerfCorpus()
	// Valid UTF-8 with an alternating-NUL pattern: repair reinterprets it as
	// BOM-less UTF-16, so the helper must take the exact path, not alias.
	nuls := make([]byte, 0, 200)
	for i := 0; i < 100; i++ {
		nuls = append(nuls, 'A', 0x00)
	}
	corpus = append(corpus, nuls)
	for i, b := range corpus {
		if got, want := contentStoredText(b), contentRepairText(b); string(got) != string(want) {
			t.Fatalf("corpus %d: stored text differs from repair", i)
		}
	}
	// Valid UTF-8 aliases without copying.
	b := []byte("plain ascii text")
	if s := contentStoredText(b); len(s) != len(b) || (len(s) > 0 && &s[0] != &b[0]) {
		t.Fatal("valid UTF-8 must alias the input")
	}
}

func TestContentExtractSafelyMatchesDirectForText(t *testing.T) {
	ctx := contentWithExtractSettings(context.Background(), contentDefaultExtractSettings())
	e := contentTextExtractor{}
	for i, b := range contentPerfCorpus() {
		r := &contentSliceReader{b: b}
		want, werr := e.Extract(ctx, r, int64(len(b)))
		got, gerr := contentExtractSafely(ctx, e, &contentSliceReader{b: b}, int64(len(b)))
		if (werr != nil) != (gerr != nil) || string(got.Text) != string(want.Text) ||
			got.Skipped != want.Skipped || got.Binary != want.Binary || got.Truncated != want.Truncated {
			t.Fatalf("corpus %d: safely=%+v,%v direct=%+v,%v", i, got, gerr, want, werr)
		}
	}
}

func TestContentSlurpMatchesNaiveReads(t *testing.T) {
	dir := t.TempDir()
	small := strings.Repeat("needle haystack ", 500)
	big := strings.Repeat("x", contentExtractMaxRawBytes+4096)
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte(small), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"small.txt", "big.txt"} {
		p := filepath.Join(dir, name)
		s, err := contentSlurpNoRecall(p, contentExtractMaxRawBytes)
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		info, _ := f.Stat()
		wantHead, _ := contentReadBounded(f, info.Size(), 512)
		wantBody, _ := contentReadBounded(f, info.Size(), int(contentExtractMaxRawBytes))
		f.Close()
		if string(s.headBytes()) != string(wantHead) {
			t.Fatalf("%s: head differs", name)
		}
		gotBody, err := contentReadBounded(s.reader(), s.size, int(contentExtractMaxRawBytes))
		if err != nil || string(gotBody) != string(wantBody) {
			t.Fatalf("%s: body differs", name)
		}
		if s.size != info.Size() {
			t.Fatalf("%s: size %d, want %d", name, s.size, info.Size())
		}
		s.close()
	}
}

func TestContentParallelAssembleIsByteIdentical(t *testing.T) {
	build := []contentBuildDoc{}
	terms := []string{"needle", "HayStack", "café", "under_score", "x", "FFI", "ffi", strings.Repeat("w", 200)}
	for i := 0; i < 300; i++ {
		text := terms[i%len(terms)] + " filler " + strings.Repeat("ab ", i%37) + "\nline two here"
		if i%7 == 0 {
			text += " invalid \xff\xfe tail"
		}
		build = append(build, contentBuildDoc{
			frn:  uint64(1000 + i),
			path: filepath.ToSlash(filepath.Join("src", "file"+strings.Repeat("0", i%3)+".txt")),
			text: []byte(text),
		})
	}
	// The assembler sorts by (frn, path); keep input sorted like production.
	sort.Slice(build, func(i, j int) bool {
		if build[i].frn != build[j].frn {
			return build[i].frn < build[j].frn
		}
		return build[i].path < build[j].path
	})
	encode := func(workers, batch int) []byte {
		restoreW := contentBuildWorkers
		restoreD := contentAssembleBatchDocs
		defer func() { contentBuildWorkers, contentAssembleBatchDocs = restoreW, restoreD }()
		contentBuildWorkers = workers
		contentAssembleBatchDocs = batch
		i := 0
		idx, err := assembleContentIndexStream(func() (contentBuildDoc, bool, error) {
			if i >= len(build) {
				return contentBuildDoc{}, false, nil
			}
			d := build[i]
			i++
			return d, true, nil
		}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		idx.BuiltAt = testContentFixedTime
		return contentIndexEncode(idx)
	}
	serial := encode(1, 1)
	parallel := encode(8, 64)
	if string(serial) != string(parallel) {
		t.Fatalf("parallel assembly differs: %d vs %d bytes", len(parallel), len(serial))
	}
	wide := encode(16, 7)
	if string(serial) != string(wide) {
		t.Fatal("batch-7 assembly differs from serial")
	}
}

func TestContentLowerIOPriorityPolicy(t *testing.T) {
	restoreActive := contentUserActiveNow
	restoreBattery := contentOnBatteryNow
	defer func() { contentUserActiveNow, contentOnBatteryNow = restoreActive, restoreBattery }()
	t.Setenv("SEEKFS_CONTENT_IOPRIORITY", "")
	contentUserActiveNow = func() bool { return false }
	contentOnBatteryNow = func() bool { return false }
	if contentLowerIOPriorityNow() {
		t.Fatal("idle build must keep normal IO priority")
	}
	contentUserActiveNow = func() bool { return true }
	if !contentLowerIOPriorityNow() {
		t.Fatal("active user must lower build IO priority")
	}
	contentUserActiveNow = func() bool { return false }
	contentOnBatteryNow = func() bool { return true }
	if !contentLowerIOPriorityNow() {
		t.Fatal("battery must lower build IO priority")
	}
	t.Setenv("SEEKFS_CONTENT_IOPRIORITY", "low")
	contentUserActiveNow = func() bool { return false }
	contentOnBatteryNow = func() bool { return false }
	if !contentLowerIOPriorityNow() {
		t.Fatal("explicit low must lower IO priority")
	}
	t.Setenv("SEEKFS_CONTENT_IOPRIORITY", "normal")
	if contentLowerIOPriorityNow() {
		t.Fatal("explicit non-low must keep normal IO priority")
	}
}

// The parallel spill-run sort must produce byte-identical sections to the
// serial sort at any worker count, including across many spilled runs.
func TestContentParallelSpillIsByteIdentical(t *testing.T) {
	build := func(workers int) []byte {
		restore := contentBuildWorkers
		contentBuildWorkers = workers
		defer func() { contentBuildWorkers = restore }()
		b, err := newContentExternalSectionBuilder(t.TempDir(), 1<<10)
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		// Deterministic pseudo-random 3-byte gram keys across shared
		// prefixes, plus duplicate keys with ascending docIDs (the
		// builder's contract since v7).
		for i := 0; i < 20000; i++ {
			key := string(rune('a'+(i*7919)%26)) + string(rune('a'+(i*104729)%26)) + string(rune('a'+(i/26)%26))
			b.Add(key, uint32(i/3))
		}
		out, err := b.Finish()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	serial := build(1)
	for _, w := range []int{2, 8, 16} {
		if got := build(w); string(got) != string(serial) {
			t.Fatalf("workers=%d section differs from serial (%d vs %d bytes)", w, len(got), len(serial))
		}
	}
	// Counting path: every key is 3 bytes (grams), with shared keys across
	// many docs in ascending docID order (the builder's contract).
	build3 := func(workers int) []byte {
		restore := contentBuildWorkers
		contentBuildWorkers = workers
		defer func() { contentBuildWorkers = restore }()
		b, err := newContentExternalSectionBuilder(t.TempDir(), 1<<10)
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		grams := []string{"the", "ion", "e_a", "   ", "(){", "xyz"}
		for i := 0; i < 20000; i++ {
			b.Add(grams[i%len(grams)], uint32(i/2))
		}
		out, err := b.Finish()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	serial3 := build3(1)
	for _, w := range []int{2, 8, 16} {
		if got := build3(w); string(got) != string(serial3) {
			t.Fatalf("3-byte workers=%d section differs from serial", w)
		}
	}
}
