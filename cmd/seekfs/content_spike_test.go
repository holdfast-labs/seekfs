package main

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestContentSpike measures the content pipeline over a real directory and
// checks the candidate model end to end in memory: extraction, trigram postings,
// candidate selection, verification, brute-force parity, and a real encoded
// `.gsx`.
//
// It is skipped unless SEEKFS_CONTENT_SPIKE_DIR is set:
//
//	$env:SEEKFS_CONTENT_SPIKE_DIR="F:\git\seekfs"
//	go test ./cmd/seekfs -run TestContentSpike -v -count=1 -timeout 900s
func TestContentSpike(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("SEEKFS_CONTENT_SPIKE_DIR"))
	if root == "" {
		t.Skip("set SEEKFS_CONTENT_SPIKE_DIR to run the content spike")
	}

	var peakHeap atomic.Uint64
	stopSampler := make(chan struct{})
	go func() {
		var m runtime.MemStats
		for {
			select {
			case <-stopSampler:
				return
			default:
			}
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peakHeap.Load() {
				peakHeap.Store(m.HeapAlloc)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	var (
		files, extracted, skippedBinary, skippedCap, unclaimed int
		rawBytes, textBytes                                    int64
		start                                                  = time.Now()
	)
	texts := make([]string, 0, 1024)
	textParts := make([][]byte, 0, 1024)
	docs := make([]contentDoc, 0, 1024)
	postings := make(map[string][]int32)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := strings.ToLower(d.Name())
			if name == ".git" || name == "node_modules" || name == ".opencode" || name == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		files++
		switch contentPathExtension(path) {
		case ".exe", ".dll", ".gsi", ".zip", ".ico", ".syso", ".so", ".o", ".a", ".out":
			return nil
		}
		f, ferr := os.Open(path)
		if ferr != nil {
			return nil
		}
		defer f.Close()
		info, serr := f.Stat()
		if serr != nil || info.IsDir() {
			return nil
		}
		size := info.Size()
		head, _ := contentReadBounded(f, size, 512)
		e := contentExtractorForPath(path, head)
		if e == nil {
			unclaimed++
			return nil
		}
		res, eerr := e.Extract(context.Background(), f, size)
		if eerr != nil {
			return nil
		}
		if res.Skipped {
			if res.Reason == "raw size over cap" {
				skippedCap++
			} else {
				skippedBinary++
			}
			return nil
		}
		extracted++
		rawBytes += size
		textBytes += int64(len(res.Text))
		lower := strings.ToLower(string(res.Text))
		docID := int32(len(texts))
		texts = append(texts, lower)
		textParts = append(textParts, []byte(lower))
		docs = append(docs, contentDoc{
			DocID:            uint32(docID),
			FRN:              uint64(docID) + 1,
			ContentType:      res.Class,
			ExtractorVersion: e.Version(),
			DocLen:           uint32(len(lower)),
		})
		seen := make(map[string]struct{}, len(lower)/8)
		for i := 0; i+3 <= len(lower); i++ {
			g := lower[i : i+3]
			if g[0] < 32 || g[1] < 32 || g[2] < 32 {
				continue
			}
			if _, ok := seen[g]; ok {
				continue
			}
			seen[g] = struct{}{}
			postings[g] = append(postings[g], docID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	close(stopSampler)
	elapsed := time.Since(start)

	var postingCount int64
	for _, ids := range postings {
		postingCount += int64(len(ids))
	}

	// Real encoded `.gsx` floor: CXDT + CXGR (delta-varint postings) + CXST
	// (flate-compressed text). CXTR/CXRN are P1 and add to this.
	idx := newContentIndex()
	idx.Docs = docs
	idx.Sections[contentSectionGrams] = contentSpikeGramBlob(postings)
	idx.Sections[contentSectionText] = contentSpikeFlate(textParts)
	realBytes := int64(len(contentIndexEncode(idx)))

	t.Logf("root=%s", root)
	t.Logf("files=%d extracted=%d skipped_binary=%d skipped_cap=%d unclaimed=%d", files, extracted, skippedBinary, skippedCap, unclaimed)
	t.Logf("raw_bytes=%d text_bytes=%d text/raw=%.3f", rawBytes, textBytes, ratio(textBytes, rawBytes))
	t.Logf("distinct_trigrams=%d postings=%d", len(postings), postingCount)
	t.Logf("encoded_gsx_bytes=%d gsx/raw=%.3f (CXDT+CXGR+CXST; CXTR/CXRN excluded)", realBytes, ratio(realBytes, rawBytes))
	t.Logf("elapsed=%s throughput_mb_s=%.1f peak_heap=%dMB final_heap=%dMB",
		elapsed.Round(time.Millisecond), float64(rawBytes)/elapsed.Seconds()/1e6,
		int64(peakHeap.Load())>>20, heapAllocMB())
	if extracted > 0 {
		t.Logf("per_1000_files: raw=%.0fKB text=%.0fKB gsx=%.0fKB",
			float64(rawBytes)/float64(extracted)*1000/1024,
			float64(textBytes)/float64(extracted)*1000/1024,
			float64(realBytes)/float64(extracted)*1000/1024)
	}

	for _, term := range []string{"needle", "content", "packed", "trigram", "zzzxnope"} {
		candidates, ok := contentSpikeCandidates(postings, term)
		if !ok {
			t.Logf("query %q: no trigram plan (term shorter than 3), brute force only", term)
			continue
		}
		verified := map[int32]bool{}
		for _, id := range candidates {
			if strings.Contains(texts[id], term) {
				verified[id] = true
			}
		}
		brute := bruteSet(texts, term)
		for id := range brute {
			if !verified[id] {
				t.Fatalf("query %q: candidate model missed doc %d (false negative)", term, id)
			}
		}
		t.Logf("query %q: candidates=%d verified=%d brute_force=%d reduction=%.1fx",
			term, len(candidates), len(verified), len(brute), ratio(int64(len(candidates)), int64(len(brute))))
	}
}

func heapAllocMB() int64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc) >> 20
}

// contentSpikeGramBlob serializes postings as (gram key u32, count uvarint,
// delta-varint docIDs), the shape CXGR will use.
func contentSpikeGramBlob(postings map[string][]int32) []byte {
	var buf bytes.Buffer
	var scratch [binary.MaxVarintLen64]byte
	for g, ids := range postings {
		var key [4]byte
		binary.LittleEndian.PutUint32(key[:], uint32(g[0])<<16|uint32(g[1])<<8|uint32(g[2]))
		buf.Write(key[:])
		buf.Write(scratch[:binary.PutUvarint(scratch[:], uint64(len(ids)))])
		prev := int32(-1)
		for _, id := range ids {
			n := binary.PutUvarint(scratch[:], uint64(id-prev-1))
			buf.Write(scratch[:n])
			prev = id
		}
	}
	return buf.Bytes()
}

func contentSpikeFlate(parts [][]byte) []byte {
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestSpeed)
	for _, p := range parts {
		_, _ = w.Write(p)
	}
	_ = w.Close()
	return buf.Bytes()
}

func contentSpikeCandidates(postings map[string][]int32, term string) ([]int32, bool) {
	if len(term) < 3 {
		return nil, false
	}
	var current map[int32]int
	for i := 0; i+3 <= len(term); i++ {
		g := term[i : i+3]
		ids := postings[g]
		if len(ids) == 0 {
			return nil, true
		}
		next := make(map[int32]int, len(ids))
		for _, id := range ids {
			if current == nil {
				next[id] = 1
			} else if _, ok := current[id]; ok {
				next[id] = 1
			}
		}
		current = next
		if len(current) == 0 {
			return nil, true
		}
	}
	out := make([]int32, 0, len(current))
	for id := range current {
		out = append(out, id)
	}
	return out, true
}

func bruteSet(texts []string, term string) map[int32]bool {
	out := map[int32]bool{}
	for i, t := range texts {
		if strings.Contains(t, term) {
			out[int32(i)] = true
		}
	}
	return out
}

func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
