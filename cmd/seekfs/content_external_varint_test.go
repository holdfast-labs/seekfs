package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"testing"
)

// buildExternalToPostings feeds the builder in ascending-docID-per-key order
// and returns its Finish output.
func buildExternalToPostings(t *testing.T, arena int, postings map[string][]contentDocFreq) ([]byte, []string) {
	t.Helper()
	keys := make([]string, 0, len(postings))
	for k := range postings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b, err := newContentExternalSectionBuilder(t.TempDir(), arena)
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}
	defer b.Close()
	for _, k := range keys {
		for _, p := range postings[k] {
			b.Add(k, p.docID)
		}
	}
	out, err := b.Finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	return out, b.PrunedKeys()
}

// TestContentExternalVarintRoundTrip drives the grouped delta-varint runs
// through spill and the k-way merge with many keys, single-doc keys, and
// large docID gaps (0 and 1<<32-1), and requires byte-identical output to the
// in-memory encoder.
func TestContentExternalVarintRoundTrip(t *testing.T) {
	const maxU32 = uint32(0xffffffff)
	postings := map[string][]contentDocFreq{}
	postings["aaa"] = []contentDocFreq{
		{docID: 0}, {docID: 5}, {docID: 1 << 31}, {docID: maxU32},
	}
	postings["aab"] = []contentDocFreq{{docID: 0}}                  // singleton at 0
	postings["aac"] = []contentDocFreq{{docID: maxU32}}             // singleton at max
	postings["aad"] = []contentDocFreq{{docID: 0}, {docID: maxU32}} // huge delta

	rng := rand.New(rand.NewSource(42))
	// Keys run from "aba" up; 300 keys with 1..50 ascending, sometimes gapped
	// postings. Include zero-docID singletons.
	for i := 0; i < 300; i++ {
		k := fmt.Sprintf("%c%c%c", 'a'+byte(i/(26*26)), 'a'+byte((i/26)%26), 'a'+byte(i%26))
		if _, seen := postings[k]; seen {
			continue
		}
		n := rng.Intn(50) + 1
		list := make([]contentDocFreq, 0, n)
		doc := uint32(0)
		if i%5 == 0 {
			list = append(list, contentDocFreq{docID: 0})
		}
		for len(list) < n {
			doc += uint32(1 + rng.Intn(1<<20))
			list = append(list, contentDocFreq{docID: doc})
		}
		postings[k] = list
	}

	want := encodeContentPostingSection(postings)
	// Tiny arena forces many spills so groups are split across runs and the
	// merge has to interleave them.
	got, _ := buildExternalToPostings(t, 128, postings)
	if !bytes.Equal(got, want) {
		t.Fatalf("grouped-run build differs from encodeContentPostingSection: got %d bytes, want %d", len(got), len(want))
	}
}

// TestContentExternalVarintTornRun proves a run truncated mid-group or
// mid-varint makes the merge return an error instead of stopping silently.
func TestContentExternalVarintTornRun(t *testing.T) {
	const maxU32 = uint32(0xffffffff)
	build := func(t *testing.T) (*contentExternalSectionBuilder, int64) {
		t.Helper()
		b, err := newContentExternalSectionBuilder(t.TempDir(), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		b.Add("aaa", 0)
		b.Add("aaa", 3000000000)
		b.Add("zzz", maxU32) // single-doc group with a 5-byte first-docID varint
		if err := b.spill(); err != nil {
			t.Fatal(err)
		}
		if len(b.runs) != 1 {
			t.Fatalf("runs = %d, want 1", len(b.runs))
		}
		fi, err := os.Stat(b.runs[0])
		if err != nil {
			t.Fatal(err)
		}
		return b, fi.Size()
	}

	var tmp [binary.MaxVarintLen64]byte
	nLen := binary.PutUvarint(tmp[:], 1)
	firstLen := binary.PutUvarint(tmp[:], uint64(maxU32))

	// Cut after the last group's 3-byte key: EOF inside the group header.
	b, size := build(t)
	defer b.Close()
	if err := os.Truncate(b.runs[0], size-int64(nLen+firstLen)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.merge(); err == nil {
		t.Fatal("merge on a mid-group truncated run returned nil error")
	}

	// Cut the last byte of the trailing multi-byte varint: EOF mid-varint.
	b2, size2 := build(t)
	defer b2.Close()
	if err := os.Truncate(b2.runs[0], size2-1); err != nil {
		t.Fatal(err)
	}
	if _, err := b2.merge(); err == nil {
		t.Fatal("merge on a mid-varint truncated run returned nil error")
	}
}

// TestContentExternalVarintRunSize measures the on-disk run files for a
// realistic synthetic corpus and requires them below 0.35x of the old fixed
// 8-bytes-per-posting form.
func TestContentExternalVarintRunSize(t *testing.T) {
	const totalPostings = 200000
	const oldRecordSize = 8
	dir := t.TempDir()
	b, err := newContentExternalSectionBuilder(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	rng := rand.New(rand.NewSource(1234))
	written := 0
	for k := 0; written < totalPostings; k++ {
		key := fmt.Sprintf("%c%c%c", 'a'+byte(k/(26*26))%26, 'a'+byte((k/26)%26), 'a'+byte(k%26))
		n := 1 + rng.Intn(20)
		doc := uint32(rng.Intn(1 << 17))
		for i := 0; i < n && written < totalPostings; i++ {
			doc += uint32(1 + rng.Intn(8))
			b.Add(key, doc)
			written++
		}
	}
	if err := b.spill(); err != nil {
		t.Fatal(err)
	}
	var runBytes int64
	for _, path := range b.runs {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		runBytes += fi.Size()
	}
	old := int64(totalPostings) * oldRecordSize
	ratio := float64(runBytes) / float64(old)
	t.Logf("runBytes=%d oldBytes=%d runs=%d ratio=%.3f", runBytes, old, len(b.runs), ratio)
	if ratio > 0.35 {
		t.Fatalf("grouped run size ratio %.3f exceeds 0.35 (runBytes=%d oldBytes=%d)", ratio, runBytes, old)
	}
}
