package main

// Posting-level fold (Option A): merge a shard's delta into its existing
// encoded sections instead of re-extracting every base doc from disk. The
// doc table, paths, filter rows, aliases and gram postings are carried for
// unchanged docs and updated only for changed/added docs. The term dictionary
// carries its old keys/df and adds the new docs' terms; it does not subtract
// removed docs. A bounded staleness counter makes the next fold fall back to
// the full re-extract path, which restores exact term df and un-prunes dense
// grams.

import (
	"context"
	"encoding/binary"
	"sort"
)

var (
	// contentPostingFoldMaxFolds bounds posting-level folds per shard before a
	// full re-extract resets the term dictionary's stale df and the pruned set.
	// Vars so tests can lower them.
	contentPostingFoldMaxFolds = 32
	// contentPostingFoldMaxChangedPercent bounds the share of a shard's docs
	// that may be changed/removed by posting-level folds before the next fold
	// is a full re-extract.
	contentPostingFoldMaxChangedPercent = 25

	// contentPostingFoldExtract is the disk re-read a posting fold uses for
	// changed/added docs. A var so tests can count reads.
	contentPostingFoldExtract = contentBuildDocSafe

	// contentPostingFoldChoiceHook reports each dirty shard's fold choice. It
	// is nil in production; tests use it to prove the fallback engaged.
	contentPostingFoldChoiceHook func(shard string, posting bool)
)

// contentFoldCounter is one shard's posting-fold staleness state, persisted in
// the manifest's CXFC section.
type contentFoldCounter struct {
	Folds   uint32
	Changed uint32
}

func encodeContentFoldCounters(counters map[string]contentFoldCounter) []byte {
	if len(counters) == 0 {
		return nil
	}
	names := make([]string, 0, len(counters))
	for name := range counters {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []byte
	out = binary.LittleEndian.AppendUint32(out, uint32(len(names)))
	for _, name := range names {
		if len(name) > 1<<20 {
			continue
		}
		out = binary.LittleEndian.AppendUint32(out, uint32(len(name)))
		out = append(out, name...)
		out = binary.LittleEndian.AppendUint32(out, counters[name].Folds)
		out = binary.LittleEndian.AppendUint32(out, counters[name].Changed)
	}
	return out
}

func decodeContentFoldCounters(data []byte) map[string]contentFoldCounter {
	if len(data) < 4 {
		return nil
	}
	off := 0
	get32 := func() (uint32, bool) {
		if off+4 > len(data) {
			return 0, false
		}
		v := binary.LittleEndian.Uint32(data[off:])
		off += 4
		return v, true
	}
	n, ok := get32()
	if !ok || n > 1<<20 {
		return nil
	}
	out := make(map[string]contentFoldCounter, n)
	for i := uint32(0); i < n; i++ {
		l, ok := get32()
		if !ok || int(l) > len(data)-off {
			return nil
		}
		name := string(data[off : off+int(l)])
		off += int(l)
		folds, ok := get32()
		if !ok {
			return nil
		}
		changed, ok := get32()
		if !ok {
			return nil
		}
		out[name] = contentFoldCounter{Folds: folds, Changed: changed}
	}
	if off != len(data) {
		return nil
	}
	return out
}

// contentPostingFoldStale reports whether a shard must use the full re-extract
// path now. pending is the number of base docs this fold would change or
// remove; it joins the accumulated counter so a large single fold falls back
// before the staleness is persisted.
func contentPostingFoldStale(c contentFoldCounter, docs, pending int) bool {
	if docs <= 0 {
		return true
	}
	if int(c.Folds) > contentPostingFoldMaxFolds {
		return true
	}
	return (int(c.Changed)+pending)*100 > docs*contentPostingFoldMaxChangedPercent
}

// contentShardDroppedDocs counts the base docs the delta replaces or deletes.
// Added docs do not stale the term dictionary (their terms are added), so they
// do not count.
func contentShardDroppedDocs(sh *contentShard, drop map[uint64]struct{}) int {
	if sh == nil || sh.idx == nil {
		return 0
	}
	n := 0
	for i := range sh.idx.Docs {
		if _, ok := drop[sh.idx.Docs[i].FRN]; ok {
			n++
		}
	}
	return n
}

// contentPostingFoldResult is one rewritten shard.
type contentPostingFoldResult struct {
	idx         *contentIndex
	rewritten   int
	changed     int
	dedupeFiles int64
	dedupeBytes int64
}

// contentShardPostingFold rewrites one dirty shard from its encoded sections
// plus the incoming delta docs. ok=false means the caller must use the full
// re-extract path (missing sections, an unexpected alias shape, or a base that
// cannot be merged safely).
func contentShardPostingFold(
	vol *serviceVolumeIndex,
	sh *contentShard,
	incoming []contentDeltaDoc,
	drop map[uint64]struct{},
	header *contentIndex,
	settings contentExtractSettings,
	ctx context.Context,
	livePath func(frn uint64) string,
) (*contentPostingFoldResult, bool) {
	if sh == nil || sh.idx == nil || sh.reader == nil || sh.reader.filter == nil || sh.reader.paths == nil {
		return nil, false
	}
	baseDocs := sh.idx.Docs
	if len(baseDocs) == 0 {
		return nil, false
	}
	if sh.reader.filter.rowCount() != len(baseDocs) {
		return nil, false
	}

	// Old dedupe shape: aliases have no postings; each alias names one primary.
	oldAlias := make([]bool, len(baseDocs))
	oldPrimary := make([]int, len(baseDocs))
	for i := range oldPrimary {
		oldPrimary[i] = i
	}
	for _, pair := range sh.idx.Aliases {
		a, p := int(pair.Alias), int(pair.Primary)
		if a < 0 || a >= len(baseDocs) || p < 0 || p >= len(baseDocs) || a == p {
			return nil, false
		}
		oldAlias[a] = true
		oldPrimary[a] = p
	}

	type foldDoc struct {
		oldLocal int
		doc      contentDoc
		path     string
		frn      uint64
		tok      contentTokenizedDoc
		fresh    bool
	}
	merged := make([]foldDoc, 0, len(baseDocs)+len(incoming))
	remap := make([]int, len(baseDocs))
	for i := range remap {
		remap[i] = -1
	}
	changed := 0
	for i := range baseDocs {
		d := baseDocs[i]
		if _, gone := drop[d.FRN]; gone {
			changed++
			continue
		}
		p := sh.reader.docPath(uint32(i))
		if p == "" {
			// No stored path: the full fold drops the doc too (it cannot
			// re-read it), so drop it here and leave its postings out.
			continue
		}
		merged = append(merged, foldDoc{oldLocal: i, doc: d, path: p, frn: d.FRN})
	}

	cctx := contentWithExtractSettings(ctx, settings)
	for _, d := range incoming {
		if d.Deleted {
			continue
		}
		bd, ok := contentPostingFoldRead(cctx, d, livePath)
		if !ok {
			// Match the full fold: a delta-covered key serves the delta text
			// even when the file cannot be re-read right now.
			bd = contentBuildDoc{
				path:        d.Path,
				frn:         d.FRN,
				text:        d.Text,
				class:       d.ContentType,
				version:     d.ExtractorVersion,
				fileSize:    d.FileSize,
				fileModNano: d.FileModNano,
				seq:         d.FRNSeq,
			}
		}
		tok := contentTokenizeDocForBuild(bd.text)
		cd := contentDoc{
			FRN:              bd.frn,
			ContentType:      bd.class,
			ExtractorVersion: bd.version,
			DocLen:           tok.docLen,
			RawSize:          int64(tok.textLen),
			ModUnix:          bd.modUnix,
			ContentHash:      tok.hash,
			TextLen:          tok.textLen,
			FileSize:         bd.fileSize,
			FileModNano:      bd.fileModNano,
			FRNSeq:           bd.seq,
		}
		merged = append(merged, foldDoc{oldLocal: -1, doc: cd, path: bd.path, frn: bd.frn, tok: tok, fresh: true})
	}

	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].frn != merged[j].frn {
			return merged[i].frn < merged[j].frn
		}
		return merged[i].path < merged[j].path
	})
	outDocs := make([]contentDoc, len(merged))
	paths := make([]string, len(merged))
	for i := range merged {
		merged[i].doc.DocID = uint32(i)
		outDocs[i] = merged[i].doc
		paths[i] = merged[i].path
		if o := merged[i].oldLocal; o >= 0 {
			remap[o] = i
		}
	}

	// Recompute the dedupe map from the doc table's (length, hash) key. This
	// matches the assembler: first DocID wins, empty text never aliases.
	finalAlias := make([]bool, len(outDocs))
	aliasPairs := make([]contentAliasPair, 0)
	dedupeSeen := make(map[contentDedupeKey]uint32, len(outDocs))
	var dedupeFiles, dedupeBytes int64
	for i := range outDocs {
		d := &outDocs[i]
		if d.TextLen == 0 {
			continue
		}
		key := contentDedupeKey{length: int(d.TextLen), hash: d.ContentHash}
		if primary, dup := dedupeSeen[key]; dup {
			finalAlias[i] = true
			aliasPairs = append(aliasPairs, contentAliasPair{Alias: uint32(i), Primary: primary})
			dedupeFiles++
			dedupeBytes += int64(d.TextLen)
		} else {
			dedupeSeen[key] = uint32(i)
		}
	}

	// Which old postings survive, and which old primary's postings a promoted
	// alias inherits. Aliases share their primary's bytes, so copying the old
	// posting row is exact when the old primary is gone.
	dropOld := make([]bool, len(baseDocs))
	copyPrimary := make(map[int]int)
	for i := range baseDocs {
		newID := remap[i]
		if newID < 0 || finalAlias[newID] {
			dropOld[i] = true
			continue
		}
		if !oldAlias[i] {
			continue
		}
		p := oldPrimary[i]
		if p < 0 || p >= len(baseDocs) {
			return nil, false
		}
		if remap[p] >= 0 && !finalAlias[remap[p]] {
			// An alias cannot outrank its surviving primary. Refuse rather
			// than guess.
			return nil, false
		}
		if _, dup := copyPrimary[p]; dup {
			return nil, false
		}
		copyPrimary[p] = newID
	}

	addPostings := make(map[string][]uint32)
	for i := range merged {
		if merged[i].oldLocal >= 0 || finalAlias[i] {
			continue
		}
		for gram := range merged[i].tok.grams {
			key := contentGramU32String(gram)
			addPostings[key] = append(addPostings[key], uint32(i))
		}
	}

	postings := make(map[string][]contentDocFreq)
	pruned := make(map[string]struct{})
	if sh.reader.pruned != nil {
		for g := range sh.reader.pruned {
			pruned[contentGramU32String(g)] = struct{}{}
		}
	}
	if sh.reader.grams != nil {
		for i := 0; i < sh.reader.grams.entryCount(); i++ {
			e := sh.reader.grams.entryAt(i)
			key := string(e.key[:])
			if _, isPruned := pruned[key]; isPruned {
				continue
			}
			next, found := sh.reader.grams.postings(key)
			if !found {
				continue
			}
			ids := make([]uint32, 0, int(e.count)+len(addPostings[key]))
			for {
				id, _, ok := next()
				if !ok {
					break
				}
				if to, cp := copyPrimary[int(id)]; cp {
					ids = append(ids, uint32(to))
					continue
				}
				if int(id) < len(remap) {
					if to := remap[id]; to >= 0 && !dropOld[id] {
						ids = append(ids, uint32(to))
					}
				}
			}
			ids = append(ids, addPostings[key]...)
			ids = contentSortDocIDs(ids)
			if len(ids) == 0 {
				continue
			}
			if len(ids) > contentGramPruneDF {
				pruned[key] = struct{}{}
				continue
			}
			freqs := make([]contentDocFreq, len(ids))
			for j, id := range ids {
				freqs[j] = contentDocFreq{docID: id}
			}
			postings[key] = freqs
		}
	}
	for key, add := range addPostings {
		if _, seen := postings[key]; seen {
			continue
		}
		if _, isPruned := pruned[key]; isPruned {
			continue
		}
		ids := contentSortDocIDs(append([]uint32(nil), add...))
		if len(ids) == 0 {
			continue
		}
		if len(ids) > contentGramPruneDF {
			pruned[key] = struct{}{}
			continue
		}
		freqs := make([]contentDocFreq, len(ids))
		for j, id := range ids {
			freqs[j] = contentDocFreq{docID: id}
		}
		postings[key] = freqs
	}

	// Term dictionary: carry old keys/df, then add the new primaries' terms.
	// Removed docs are not subtracted; the counter bounds the staleness.
	terms := make(map[string]uint32)
	if sh.reader.terms != nil {
		terms = make(map[string]uint32, sh.reader.terms.keyCount())
		for i := 0; i < sh.reader.terms.keyCount(); i++ {
			k, ok := sh.reader.terms.keyAt(i)
			if !ok {
				return nil, false
			}
			terms[k] = uint32(sh.reader.terms.dfAt(i))
		}
	}
	for i := range merged {
		if merged[i].oldLocal >= 0 || finalAlias[i] {
			continue
		}
		for term := range merged[i].tok.terms {
			terms[term]++
		}
	}

	// Filter rows: unchanged docs keep their exact row (including pruned-gram
	// bits); changed/added docs get a fresh row from their own gram set.
	rows := make([]contentFilterRow, len(outDocs))
	for i := range merged {
		if o := merged[i].oldLocal; o >= 0 {
			row, ok := sh.reader.filter.row(o)
			if !ok {
				return nil, false
			}
			rows[i] = row
			continue
		}
		class := contentFilterClassForGrams(len(merged[i].tok.grams))
		bits := make([]byte, contentFilterClassBits[class]/8)
		contentFilterBitsOfU32(bits, merged[i].tok.grams)
		rows[i] = contentFilterRow{Class: class, Bits: bits}
	}

	// Encode the rebuilt sections.
	termKeys := make([]string, 0, len(terms))
	for k := range terms {
		termKeys = append(termKeys, k)
	}
	sort.Strings(termKeys)
	termsSec := encodeContentTermDict(termKeys, terms)
	gramsSec := encodeContentPostingSection(postings)
	pruneKeys := make([]string, 0, len(pruned))
	for k := range pruned {
		pruneKeys = append(pruneKeys, k)
	}
	prunedSec := encodeContentPrunedGrams(pruneKeys)
	filterSec := encodeContentFilterSection(
		sh.reader.filter.MaxRaw, sh.reader.filter.MaxText,
		sh.reader.filter.CapsExplicit, sh.reader.filter.Root,
		sh.reader.filter.Encoding, rows)

	built := &contentStreamedBuild{
		docs:        outDocs,
		paths:       paths,
		aliasPairs:  aliasPairs,
		dedupeFiles: dedupeFiles,
		dedupeBytes: dedupeBytes,
	}
	idx := contentShardHeader(header, built)
	idx.Sections = map[uint32][]byte{contentSectionPaths: encodeContentPaths(paths)}
	if len(termsSec) > 0 {
		idx.Sections[contentSectionTerms] = termsSec
	}
	if len(gramsSec) > 0 {
		idx.Sections[contentSectionGrams] = gramsSec
	}
	if len(prunedSec) > 0 {
		idx.Sections[contentSectionPruned] = prunedSec
	}
	if len(filterSec) > 0 {
		idx.Sections[contentSectionFilter] = filterSec
	}
	_ = vol
	return &contentPostingFoldResult{
		idx:         idx,
		rewritten:   len(outDocs),
		changed:     changed,
		dedupeFiles: dedupeFiles,
		dedupeBytes: dedupeBytes,
	}, true
}

func contentPostingFoldRead(ctx context.Context, d contentDeltaDoc, livePath func(uint64) string) (contentBuildDoc, bool) {
	if doc, _, ok := contentPostingFoldExtract(ctx, contentBuildItem{frn: d.FRN, path: d.Path, inRepo: true, seq: d.FRNSeq}); ok {
		return doc, true
	}
	if livePath != nil {
		if lp := livePath(d.FRN); lp != "" && lp != d.Path {
			if doc, _, ok := contentPostingFoldExtract(ctx, contentBuildItem{frn: d.FRN, path: lp, inRepo: true, seq: d.FRNSeq}); ok {
				return doc, true
			}
		}
	}
	return contentBuildDoc{}, false
}

func contentSortDocIDs(ids []uint32) []uint32 {
	if len(ids) < 2 {
		return ids
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := ids[:1]
	for _, id := range ids[1:] {
		if id != out[len(out)-1] {
			out = append(out, id)
		}
	}
	return out
}
