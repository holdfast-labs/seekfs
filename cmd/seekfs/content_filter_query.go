package main

// Tier-2 query paths: the offline single-term search and the service
// per-volume fill behind tier 1. Both share one contract: the full tier's
// results and order are unchanged (tier-2 hits append in filter key order up
// to the limit), verification reads the file from disk with the same
// extractor and limits, snippets render from the verified text, and a
// verification cap stops the scan with complete:false instead of reading the
// whole drive.

import (
	"os"
	"sort"
	"strings"
)

// filterRecordText verifies one filter record's file and returns its stored
// text: the delta wins when it covers the key (fresh, no disk read and no
// fingerprint check — the delta is newer than the filter by construction),
// otherwise the bloom prefilter gates a disk verification that honors the
// record fingerprint. ok=false means skip (tombstoned, prefilter-rejected,
// changed, missing, or unextractable). didDisk reports whether a disk
// verification was consumed. capped reports the per-query disk cap stopped
// the read, in which case the caller stops the scan (complete:false), never
// a silent skip. Callers share one verified counter per query so the cap
// bounds the whole query, not one volume or lane; a nil counter disables the
// cap for drive-bounded exact counts (the count lane).
func (vol *serviceVolumeIndex) filterRecordText(rec *contentFilterRecord, livePath string, pq parsedQuery, settings contentExtractSettings, verified *int) (text []byte, ok, didDisk, capped bool) {
	if vol == nil || vol.content == nil || rec == nil {
		return nil, false, false, false
	}
	if delta := vol.content.deltaView(); delta != nil {
		if t, found, deleted := delta.textFor(rec.Key); found {
			if deleted {
				return nil, false, false, false
			}
			return t, true, false, false
		}
	}
	if !contentFilterPrefilterOk(rec, pq) {
		return nil, false, false, false
	}
	if verified != nil {
		if *verified >= contentFilterMaxVerify {
			return nil, false, false, true
		}
		*verified++
	}
	// livePath is the record-index path (empty means the stored record path):
	// renames keep the key but move the file, so verification follows the
	// live path while the fingerprint still gates staleness.
	at := *rec
	if livePath != "" {
		at.Path = livePath
	}
	stored, ok := contentFilterVerifyStored(at.Path, at, settings)
	return stored, ok, true, false
}

// filterRecordCandidate reports whether a filter record may verify without
// touching disk: a live delta doc always may, a tombstoned one never does,
// and otherwise the bloom prefilter decides. Loops check it before paying
// record resolution, so resolution (and the path memo) is spent only on
// records that can still match.
func filterRecordCandidate(rec *contentFilterRecord, delta *contentDelta, pq parsedQuery) bool {
	if rec == nil {
		return false
	}
	if delta != nil {
		if _, found, deleted := delta.textFor(rec.Key); found {
			return !deleted
		}
	}
	return contentFilterPrefilterOk(rec, pq)
}

// searchFilter serves one offline term against the full tier plus the filter.
// Tier-1 hits come first in docID order (identical to search; the DocID
// ^uint32(0) sentinel marks filter hits, which address no tier-1 doc);
// filter hits append in filter key order until limit. complete=false means
// the disk verification cap stopped the filter scan with records unvisited.
func (r *contentReader) searchFilter(term string, limit int, flt *contentFilter, settings contentExtractSettings) ([]contentHit, bool) {
	hits := r.search(term, limit)
	if flt == nil || len(flt.Records) == 0 {
		return hits, true
	}
	if limit > 0 && len(hits) >= limit {
		return hits, true
	}
	folded := contentFoldText([]byte(term))
	if len(folded) == 0 {
		return hits, true
	}
	grams := contentFilterGramsOf(folded)
	verified := 0
	for i := range flt.Records {
		if limit > 0 && len(hits) >= limit {
			return hits, true
		}
		rec := &flt.Records[i]
		if _, inTier1 := contentLookupFRN(r.idx.Docs, rec.Key); inTier1 {
			continue
		}
		if len(grams) > 0 && !rec.mayMatch(grams) {
			continue
		}
		if verified >= contentFilterMaxVerify {
			return hits, false
		}
		verified++
		stored, ok := contentFilterVerifyStored(rec.Path, *rec, settings)
		if !ok {
			continue
		}
		off := contentLeafFirstOffset(contentFoldText(stored), folded)
		if off < 0 {
			continue
		}
		rawOff, rawEnd := contentFoldSpanToRaw(stored, contentFoldText(stored), off, off+len(folded))
		hits = append(hits, contentHit{Path: flt.displayPath(rec), DocID: ^uint32(0), Offset: rawOff, Snippet: contentSnippetWindow(stored, rawOff, rawEnd-rawOff)})
	}
	return hits, true
}

// contentLaneTier2Hit is one tier-2 hit collected during a compound-lane
// drive: the owning volume, the filter key (drive order is NOT result
// order), the record id for ranking, and the verified entry with its snippet
// already attached.
type contentLaneTier2Hit struct {
	volume int
	key    uint64
	local  int
	entry  Entry
}

// contentLaneFilterFallback verifies one lane-driven record against tier 2
// after the tier-1 inline verify missed: the record is in the filename side
// but has no tier-1 doc. It rebuilds the entry (path-memoized, so the repeat
// reconstruction is a memo hit), re-applies the scalar side (a scalar miss
// must not cost a disk read), then verifies from the delta or disk. ok=false
// is a miss or skip; capped stops the fallback with the trace marked
// incomplete by the caller. A nil verified counter disables the disk cap:
// the count lane is drive-bounded (exact by construction), while the search
// lane shares the per-query cap with the per-volume path.
func contentLaneFilterFallback(vol *serviceVolumeIndex, idx *Index, recIndex int, pathCache map[int]string, pq parsedQuery, m *contentLeafMatcher, positive []contentLeaf, settings contentExtractSettings, verified *int) (key uint64, entry Entry, ok, didDisk, capped bool) {
	if vol == nil || vol.content == nil || idx == nil || m == nil {
		return 0, Entry{}, false, false, false
	}
	flt := vol.content.filterView()
	if flt == nil || len(flt.Records) == 0 {
		return 0, Entry{}, false, false, false
	}
	rec := idx.compactRecord(recIndex)
	if rec.Deleted {
		return 0, Entry{}, false, false, false
	}
	entry = compactEntryFromRecord(idx, recIndex, rec, pathCache, true)
	if vol.contentWalkOriginAttached() {
		var kok bool
		key, kok = contentWalkKeyOfEntry(&entry)
		if !kok {
			return 0, Entry{}, false, false, false
		}
	} else {
		key = entry.FRN
		if key == 0 {
			return 0, Entry{}, false, false, false
		}
	}
	frec, found := flt.lookup(key)
	if !found {
		return 0, Entry{}, false, false, false
	}
	// Tier 1 owns keys it covers; its miss stands.
	if _, resolver := vol.content.readerResolverView(); resolver != nil {
		if _, ok := resolver.docForFRN(key); ok {
			return 0, Entry{}, false, false, false
		}
	}
	if !entryMatchesScalar(entry, pq, pq.MatchPath) {
		return 0, Entry{}, false, false, false
	}
	text, ok, didDisk, capped := vol.filterRecordText(&frec, entry.Path, pq, settings, verified)
	if capped {
		return 0, Entry{}, false, false, true
	}
	if !ok {
		return 0, Entry{}, false, didDisk, false
	}
	if !entryMatchesContentAt(entry, pq, pq.MatchPath, contentFilterMatchText(text, m)) {
		return 0, Entry{}, false, didDisk, false
	}
	var folded []byte
	if !m.caseSensitive {
		folded = contentFoldText(text)
	}
	entry.Snippet = contentSnippet(text, folded, positive, m)
	return key, entry, true, didDisk, false
}

// appendLaneTier2Hits sorts lane-collected tier-2 hits by (volume, key) and
// appends them after the rank-ordered tier-1 buckets: the same tier-1 order +
// key-ordered tier-2 the per-volume path produces, so the lane answers
// identically with the lane off.
func appendLaneTier2Hits(ranked []globalRankedEntry, tier2 []contentLaneTier2Hit, rankers []func(int) int) []globalRankedEntry {
	if len(tier2) == 0 {
		return ranked
	}
	sort.Slice(tier2, func(i, j int) bool {
		if tier2[i].volume != tier2[j].volume {
			return tier2[i].volume < tier2[j].volume
		}
		return tier2[i].key < tier2[j].key
	})
	for _, h := range tier2 {
		rank := int(^uint(0) >> 1)
		if h.volume >= 0 && h.volume < len(rankers) && rankers[h.volume] != nil {
			rank = rankers[h.volume](h.local)
		}
		ranked = append(ranked, globalRankedEntry{entry: h.entry, rank: rank, volume: h.volume, tie: h.entry.Path})
	}
	return ranked
}
// into the caller's count (count). Search appends in filter key order up to
// the caller's effective limit and marks the trace incomplete when the disk
// verification cap stops the scan with records unvisited. Count tallies every
// tier-2 match and refuses with errContentIncomplete when capped, never a
// partial number — the same contract as the tier-1 candidate-budget overrun.
func (vol *serviceVolumeIndex) appendFilterTier(matches []Entry, opts queryOptions, pq parsedQuery, pathCache map[int]string, countOnly bool) ([]Entry, error) {
	if vol == nil || vol.content == nil {
		return matches, nil
	}
	flt := vol.content.filterView()
	if flt == nil || len(flt.Records) == 0 {
		return matches, nil
	}
	m := newContentLeafMatcher(pq)
	settings := contentServiceExtractSettings()
	if countOnly {
		n, err := vol.countFilterTier(flt, pq, m, pathCache, settings)
		if err != nil {
			return matches, err
		}
		if opts.contentCount != nil {
			*opts.contentCount += n
		}
		return matches, nil
	}
	need := normalizedLimit(opts.Limit, false)
	if len(matches) >= need {
		return matches, nil
	}
	positive := contentPositiveLeaves(pq)
	out, capped := vol.searchFilterTier(flt, pq, m, positive, pathCache, matches, need, settings)
	if capped && opts.Trace != nil {
		opts.Trace.setContentIncomplete()
	}
	return out, nil
}

// searchFilterTier verifies the volume's filter behind its tier-1 results.
// matches holds the tier-1 (+overlay) entries; hits append in filter key
// order until need (the caller's remaining limit; <=0 means unbounded, used
// only by tests). It returns the extended slice and whether the disk
// verification cap stopped the scan with records unvisited (capped), which
// the caller surfaces as ContentIncomplete. pathCache is the caller's
// reconstruction memo; settings is the service extraction policy.
func (vol *serviceVolumeIndex) searchFilterTier(flt *contentFilter, pq parsedQuery, m *contentLeafMatcher, positive []contentLeaf, pathCache map[int]string, matches []Entry, need int, settings contentExtractSettings) ([]Entry, bool) {
	if flt == nil || len(flt.Records) == 0 || m == nil || vol == nil || vol.content == nil || vol.index == nil {
		return matches, false
	}
	_, resolver := vol.content.readerResolverView()
	delta := vol.content.deltaView()
	verified := 0
	for i := range flt.Records {
		if need > 0 && len(matches) >= need {
			return matches, false
		}
		rec := &flt.Records[i]
		// Tier 1 wins: a key the base covers (a promotion since the filter
		// was written) is answered from the index, never twice.
		if resolver != nil {
			if _, ok := resolver.docForFRN(rec.Key); ok {
				continue
			}
		}
		// Bloom gate before record resolution: most records die here with
		// no path work and no disk read.
		if !filterRecordCandidate(rec, delta, pq) {
			continue
		}
		id, ok := vol.recordIDForFRN(rec.Key)
		if !ok {
			continue
		}
		rec2 := vol.index.compactRecord(id)
		if rec2.Deleted || rec2.Mode&uint32(os.ModeDir) != 0 {
			continue
		}
		path := vol.index.reconstructCompactPathCached(id, pathCache)
		if path == "" {
			continue
		}
		entry := Entry{Path: path, Name: rec2.Name, LowerName: strings.ToLower(rec2.Name), LowerPath: strings.ToLower(path), Mode: rec2.Mode, Size: rec2.Size, ModUnix: rec2.ModUnix, IndexSource: vol.index.Source, FRN: rec2.FRN}
		if !entryMatchesScalar(entry, pq, pq.MatchPath) {
			continue
		}
		text, ok, _, capped := vol.filterRecordText(rec, path, pq, settings, &verified)
		if capped {
			return matches, true
		}
		if !ok {
			continue
		}
		matched := contentFilterMatchText(text, m)
		if !entryMatchesContentAt(entry, pq, pq.MatchPath, matched) {
			continue
		}
		var folded []byte
		if !m.caseSensitive {
			folded = contentFoldText(text)
		}
		entry.Snippet = contentSnippet(text, folded, positive, m)
		matches = append(matches, entry)
	}
	return matches, false
}

// countFilterTier tallies tier-2 matches without materializing entries. A
// capped scan is refused with errContentIncomplete, never reported partial —
// the same contract as the tier-1 candidate-budget overrun.
func (vol *serviceVolumeIndex) countFilterTier(flt *contentFilter, pq parsedQuery, m *contentLeafMatcher, pathCache map[int]string, settings contentExtractSettings) (int, error) {
	if flt == nil || len(flt.Records) == 0 || m == nil || vol == nil || vol.content == nil || vol.index == nil {
		return 0, nil
	}
	_, resolver := vol.content.readerResolverView()
	delta := vol.content.deltaView()
	n := 0
	verified := 0
	for i := range flt.Records {
		rec := &flt.Records[i]
		if resolver != nil {
			if _, ok := resolver.docForFRN(rec.Key); ok {
				continue
			}
		}
		if !filterRecordCandidate(rec, delta, pq) {
			continue
		}
		id, ok := vol.recordIDForFRN(rec.Key)
		if !ok {
			continue
		}
		rec2 := vol.index.compactRecord(id)
		if rec2.Deleted || rec2.Mode&uint32(os.ModeDir) != 0 {
			continue
		}
		path := vol.index.reconstructCompactPathCached(id, pathCache)
		if path == "" {
			continue
		}
		entry := Entry{Path: path, Name: rec2.Name, LowerName: strings.ToLower(rec2.Name), LowerPath: strings.ToLower(path), Mode: rec2.Mode, Size: rec2.Size, ModUnix: rec2.ModUnix, IndexSource: vol.index.Source, FRN: rec2.FRN}
		if !entryMatchesScalar(entry, pq, pq.MatchPath) {
			continue
		}
		text, ok, _, capped := vol.filterRecordText(rec, path, pq, settings, &verified)
		if capped {
			return 0, errContentIncomplete
		}
		if !ok {
			continue
		}
		if !entryMatchesContentAt(entry, pq, pq.MatchPath, contentFilterMatchText(text, m)) {
			continue
		}
		n++
	}
	return n, nil
}
