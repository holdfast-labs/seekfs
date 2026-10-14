package main

// Content-hash dedupe (§6z, WORK_QUEUE item 1): identical files (stored-text
// length + sha256 of the stored text, both computed during extraction) are
// extracted and stored ONCE. Every path still appears in results.
//
// Mechanism: the streaming assembler (assembleContentIndexStreamed) keeps
// the first document of each content group as the primary — full text in
// the CXST store plus term/gram postings — and records every later
// identical document as an alias: a full doc-table row (own DocID, FRN,
// path, fingerprint and hash, so resolution, reconcile and counts keep
// working per path) whose TextOff/TextLen point at the primary's bytes and
// which contributes no postings of its own. The alias pairs ride a
// self-contained CXAL section (v10; nothing else changes encoding), and
// readers expand posting-derived candidate sets over the alias map before
// verification, so every duplicate path verifies (against the shared
// bytes), snippets and counts list each path, and scan-shaped queries
// (which enumerate documents, not postings) need no expansion at all.
//
// Shards dedupe within themselves: each shard assembles independently, so
// a duplicate split across two shards is stored twice. The linked-worktree
// filter removes the corpus-scale duplicate case (a dozen checkouts of one
// repo); what remains is same-folder copies, which share a shard by
// construction. Folds recompute dedupe through the same assembler, so a
// file edited away from its group transparently becomes a primary again.
//
// Another agent (content/filter-tier, §6y) may also bump the format: this
// change is one version bump plus two self-contained sections, and a
// merge-time renumber keeps both.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// contentSectionAlias and contentSectionInclusion are the v10 sections.
// CXAL maps alias DocIDs to their primary; CXIS carries the inclusion
// stats. Both verify eagerly (small sections).
const (
	contentSectionAlias     uint32 = 'C'<<24 | 'X'<<16 | 'A'<<8 | 'L'
	contentSectionInclusion uint32 = 'C'<<24 | 'X'<<16 | 'I'<<8 | 'S'
)

// contentAliasPair maps one alias DocID to the primary whose text and
// postings it shares.
type contentAliasPair struct {
	Alias   uint32
	Primary uint32
}

// encodeContentAliases encodes sorted alias pairs: uvarint count, then
// (alias, primary) uvarint pairs. Empty input encodes to empty (the
// section is then omitted, keeping dup-free builds byte-identical to v9
// apart from the version).
func encodeContentAliases(pairs []contentAliasPair) []byte {
	if len(pairs) == 0 {
		return nil
	}
	cp := append([]contentAliasPair(nil), pairs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Alias < cp[j].Alias })
	var b []byte
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], uint64(len(cp)))
	b = append(b, tmp[:n]...)
	for _, p := range cp {
		n := binary.PutUvarint(tmp[:], uint64(p.Alias))
		b = append(b, tmp[:n]...)
		n = binary.PutUvarint(tmp[:], uint64(p.Primary))
		b = append(b, tmp[:n]...)
	}
	return b
}

// decodeContentAliases validates and decodes a CXAL section for a table of
// docCount documents: aliases strictly ascending, every ID in range, no
// alias naming itself or another alias as primary (one level only, so
// expansion terminates), and no duplicate alias.
func decodeContentAliases(data []byte, docCount int) ([]contentAliasPair, error) {
	if len(data) == 0 {
		return nil, nil
	}
	n, m := binary.Uvarint(data)
	if m <= 0 {
		return nil, errors.New("content alias section truncated")
	}
	data = data[m:]
	if n > uint64(docCount) {
		return nil, fmt.Errorf("content alias section claims %d aliases for %d docs", n, docCount)
	}
	pairs := make([]contentAliasPair, 0, n)
	var last uint32
	for i := uint64(0); i < n; i++ {
		a, m := binary.Uvarint(data)
		if m <= 0 {
			return nil, errors.New("content alias section truncated")
		}
		data = data[m:]
		p, m := binary.Uvarint(data)
		if m <= 0 {
			return nil, errors.New("content alias section truncated")
		}
		data = data[m:]
		if a >= uint64(docCount) || p >= uint64(docCount) {
			return nil, fmt.Errorf("content alias %d->%d out of range for %d docs", a, p, docCount)
		}
		if a == p {
			return nil, fmt.Errorf("content alias %d names itself", a)
		}
		if i > 0 && uint32(a) <= last {
			return nil, errors.New("content alias section not sorted")
		}
		last = uint32(a)
		pairs = append(pairs, contentAliasPair{Alias: uint32(a), Primary: uint32(p)})
	}
	if len(data) != 0 {
		return nil, errors.New("content alias section has trailing bytes")
	}
	// No chaining: a primary must never itself be an alias (the builder
	// records first-seen documents as primaries, so chains cannot arise).
	aliasSet := make(map[uint32]struct{}, len(pairs))
	for _, pr := range pairs {
		aliasSet[pr.Alias] = struct{}{}
	}
	for _, pr := range pairs {
		if _, chained := aliasSet[pr.Primary]; chained {
			return nil, fmt.Errorf("content alias %d chains through %d", pr.Alias, pr.Primary)
		}
	}
	return pairs, nil
}

// contentAliasExpandMap builds the primary -> aliases expansion from
// decoded pairs.
func contentAliasExpandMap(pairs []contentAliasPair) map[uint32][]uint32 {
	if len(pairs) == 0 {
		return nil
	}
	out := make(map[uint32][]uint32, len(pairs))
	for _, p := range pairs {
		out[p.Primary] = append(out[p.Primary], p.Alias)
	}
	for k := range out {
		sort.Slice(out[k], func(i, j int) bool { return out[k][i] < out[k][j] })
	}
	return out
}

// encodeContentInclusionStats encodes the CXIS section: uvarint reason
// count, then (name, files, bytes) triples in name order, then the five
// scalar tallies. Deterministic for identical input.
func encodeContentInclusionStats(s contentInclusionStats) []byte {
	names := make([]string, 0, len(s.Skipped))
	for r := range s.Skipped {
		names = append(names, r)
	}
	sort.Strings(names)
	var b []byte
	var tmp [binary.MaxVarintLen64]byte
	put := func(v uint64) {
		n := binary.PutUvarint(tmp[:], v)
		b = append(b, tmp[:n]...)
	}
	put(uint64(len(names)))
	for _, r := range names {
		put(uint64(len(r)))
		b = append(b, r...)
		put(uint64(s.Skipped[r]))
		put(uint64(s.SkippedBytes[r]))
	}
	put(uint64(s.Truncated))
	put(uint64(s.TruncatedBytes))
	put(uint64(s.DedupeFiles))
	put(uint64(s.DedupeBytes))
	put(uint64(s.WorktreesSkipped))
	return b
}

// decodeContentInclusionStats decodes a CXIS section. A missing section is
// zero stats (hand-built v10 fixtures); trailing bytes fail the load.
func decodeContentInclusionStats(data []byte) (contentInclusionStats, error) {
	var s contentInclusionStats
	if len(data) == 0 {
		return s, nil
	}
	n, m := binary.Uvarint(data)
	if m <= 0 {
		return s, errors.New("content inclusion section truncated")
	}
	data = data[m:]
	s.Skipped = make(map[string]int64, n)
	s.SkippedBytes = make(map[string]int64, n)
	for i := uint64(0); i < n; i++ {
		l, m := binary.Uvarint(data)
		if m <= 0 || uint64(len(data[m:])) < l {
			return s, errors.New("content inclusion section truncated")
		}
		data = data[m:]
		name := string(data[:l])
		data = data[l:]
		files, m := binary.Uvarint(data)
		if m <= 0 {
			return s, errors.New("content inclusion section truncated")
		}
		data = data[m:]
		bs, m := binary.Uvarint(data)
		if m <= 0 {
			return s, errors.New("content inclusion section truncated")
		}
		data = data[m:]
		s.Skipped[name] = int64(files)
		s.SkippedBytes[name] = int64(bs)
	}
	more := func() (int64, error) {
		v, m := binary.Uvarint(data)
		if m <= 0 {
			return 0, errors.New("content inclusion section truncated")
		}
		data = data[m:]
		return int64(v), nil
	}
	var err error
	if s.Truncated, err = more(); err != nil {
		return s, err
	}
	if s.TruncatedBytes, err = more(); err != nil {
		return s, err
	}
	if s.DedupeFiles, err = more(); err != nil {
		return s, err
	}
	if s.DedupeBytes, err = more(); err != nil {
		return s, err
	}
	if s.WorktreesSkipped, err = more(); err != nil {
		return s, err
	}
	if len(data) != 0 {
		return s, errors.New("content inclusion section has trailing bytes")
	}
	return s, nil
}

// contentDedupeTotalsForIndex recomputes the dedupe savings for a loaded
// index from its alias map: aliases count files, their shared text length
// counts saved extracted-text bytes. Folds use it for retained (clean)
// shards whose assembler counts are gone with their build.
func contentDedupeTotalsForIndex(idx *contentIndex) (files, bytes int64) {
	if idx == nil || len(idx.Aliases) == 0 {
		return 0, 0
	}
	aliasSet := make(map[uint32]struct{}, len(idx.Aliases))
	for _, p := range idx.Aliases {
		aliasSet[p.Alias] = struct{}{}
	}
	for i := range idx.Docs {
		if _, ok := aliasSet[idx.Docs[i].DocID]; ok {
			files++
			bytes += int64(idx.Docs[i].TextLen)
		}
	}
	return files, bytes
}

// contentDedupeKey identifies identical content: stored-text length plus
// the sha256 of the stored text (the doc table's ContentHash).
type contentDedupeKey struct {
	length int
	hash   [contentHashLen]byte
}

// contentDedupeSeen maps content keys to the primary doc's text location
// within one assembly.
type contentDedupeSeen struct {
	docID   uint32
	textOff uint64
	textLen uint32
}
