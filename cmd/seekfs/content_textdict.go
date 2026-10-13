package main

// Per-shard text dictionary (format v8, CXTD section).
//
// Small (64–128 KiB) text blocks lose the cross-file redundancy big blocks
// get for free: each block only sees its ~1–15 documents. A zstd dictionary
// trained per shard from the shard's own head restores most of it — the
// encoder matches block content against the dictionary history, so includes,
// guards, license headers and boilerplate compress across blocks again.
//
// There is no in-process zstd dict trainer in the module set, and the full
// cover algorithm is out of scope. Instead this file trains the dictionary
// CONTENT only (frequent source lines, which carry the bulk of cross-file
// repetition) and stores it raw: klauspost supports raw-content dicts on
// both sides (WithEncoderDictRaw/WithDecoderDictRaw), which need no entropy
// tables at all — the content is used as initial history. The format,
// upgrade and integrity story therefore stays trivial: CXTD is one more
// CRC-covered section, each shard file carries its own, and a missing CXTD
// means plain decoding.
//
// Training is deterministic (doc order in, total order out), so identical
// inputs produce byte-identical sidecars.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sort"
)

// contentSectionDictText is the CXTD section tag: one shard's text dict.
const contentSectionDictText uint32 = 'C'<<24 | 'X'<<16 | 'T'<<8 | 'D'

// contentTextDictID is the zstd dictionary ID every CXTD carries and every
// v8 text frame references. Raw dicts need an ID to rendezvous encoder and
// decoder; a single fixed ID keeps both sides trivial (shards never share
// decoders, so per-shard contents never collide).
const contentTextDictID = 1

// contentDictSampleBytes caps the head of a shard's logical stream the
// trainer reads: the first documents in docID order, whole documents only,
// newline-joined. 8 MiB covers ~1000 average files — plenty diverse for
// line statistics — for a bounded transient. A var so tests exercise the
// mid-stream train path on small corpora.
var contentDictSampleBytes = 8 << 20

// contentDictBudget is the trained content size cap (zstd dictionaries much
// bigger than this pay more in window/encoder tables than they return on
// small blocks).
const contentDictBudget = 96 << 10

// contentDictMinLogical skips the dictionary below this logical size: on a
// tiny shard a ~100 KiB stored dict costs more than it can return. A var so
// tests force the dict path on small corpora.
var contentDictMinLogical = uint64(2 << 20)

// contentDictMinLine / contentDictMaxLine bound the lines admitted to
// statistics: shorter than 4 bytes never pays back its match overhead,
// longer than 512 is usually minified noise.
const contentDictMinLine = 4
const contentDictMaxLine = 512

// contentTrainTextDict selects dictionary content from the shard head:
// whole repeated lines by (-count, line) total order, joined with '\n' to
// the budget. Only lines seen at least twice qualify — a singleton line
// can never match across blocks. Returns nil when nothing qualifies (the
// caller then encodes plain and stores no CXTD).
func contentTrainTextDict(sample []byte, budget int) []byte {
	if budget <= 0 {
		return nil
	}
	counts := make(map[string]int)
	start := 0
	for i := 0; i <= len(sample); i++ {
		if i == len(sample) || sample[i] == '\n' {
			if l := i - start; l >= contentDictMinLine && l <= contentDictMaxLine {
				counts[string(sample[start:i])]++
			}
			start = i + 1
		}
	}
	type lineCount struct {
		line string
		n    int
	}
	sorted := make([]lineCount, 0, len(counts))
	for line, n := range counts {
		if n >= 2 {
			sorted = append(sorted, lineCount{line, n})
		}
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].n != sorted[j].n {
			return sorted[i].n > sorted[j].n
		}
		return sorted[i].line < sorted[j].line
	})
	var out []byte
	for _, e := range sorted {
		if len(out)+len(e.line)+1 > budget {
			break
		}
		out = append(out, e.line...)
		out = append(out, '\n')
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// encodeContentTextDict serializes a CXTD payload: the dict ID the frames
// reference plus the raw content. An empty content encodes to nil (no
// section), matching the posting builders' convention.
func encodeContentTextDict(id uint32, content []byte) []byte {
	if id == 0 || len(content) == 0 {
		return nil
	}
	out := make([]byte, 0, 8+len(content))
	out = binary.LittleEndian.AppendUint32(out, id)
	out = binary.LittleEndian.AppendUint32(out, 0) // flags, reserved
	out = append(out, content...)
	return out
}

// decodeContentTextDict parses a CXTD payload into its ID and content.
// Content aliases the section bytes (like frames alias CXST); decoder pools
// copy it at construction so there is no lifetime coupling to the mapping.
func decodeContentTextDict(data []byte) (uint32, []byte, error) {
	if len(data) < 8 {
		return 0, nil, errors.New("content text dict truncated")
	}
	id := binary.LittleEndian.Uint32(data[0:])
	if id == 0 {
		return 0, nil, errors.New("content text dict has no id")
	}
	content := data[8:]
	if len(content) == 0 {
		return 0, nil, errors.New("content text dict is empty")
	}
	return id, content, nil
}

// contentDictEqual parses two CXTD payloads for test comparison.
func contentDictEqual(a, b []byte) bool {
	return bytes.Equal(a, b)
}
