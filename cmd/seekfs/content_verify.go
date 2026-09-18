package main

// P3 content query verification. Candidate generation draws a bounded superset
// of the records whose content could match (base postings mapped to record IDs
// through the resolver, or an ordered fallback scan), and content semantics are
// enforced INLINE during per-record verification: a record that fails content is
// dropped before it is appended to results or counted toward the limit. Peak
// memory therefore tracks the result limit, not the candidate set.
//
// Content is keyed on FRN, so base and overlay entries verify identically. When
// a complete candidate superset cannot be materialized within the memory budget
// the query is marked incomplete (degraded); a count in that state is refused,
// never silently reported as a partial number.

import (
	"bytes"
	"errors"
	"regexp"
	"sort"
	"strings"
)

// contentCandidateBudget caps any materialized content candidate slice. Beyond
// it the candidate set is not a complete superset, so the query is marked
// incomplete rather than silently truncated. A var so tests can lower it.
var contentCandidateBudget = 4_000_000

// contentScanPathCacheCap bounds the path-reconstruction memo used by content
// fallback scans. Past the cap the memo is reset, so peak memory is O(cap)
// instead of O(records scanned); resetting only forces recomputation, never a
// wrong path.
const contentScanPathCacheCap = 4096

// contentScanVisitBudget caps the number of records a content fallback scan
// visits before it declares its candidate superset incomplete. It is separate
// from contentCandidateBudget (which caps materialized matches) because a
// sparse-match scan can walk the whole volume while producing few matches. A
// var so tests can lower it.
var contentScanVisitBudget = contentCandidateBudget

// boundContentPathCache resets a content scan's path memo once it reaches
// contentScanPathCacheCap, keeping the scan's memory O(contentScanPathCacheCap)
// rather than O(records visited).
func boundContentPathCache(cache map[int]string) map[int]string {
	if len(cache) >= contentScanPathCacheCap {
		return make(map[int]string, contentScanPathCacheCap)
	}
	return cache
}

// errContentIncomplete refuses a count whose candidate superset was capped: a
// partial number would look exact.
var errContentIncomplete = errors.New("content count is incomplete: candidate budget exceeded; add a more selective term or filter")

// contentLeafMatcher precompiles the query's content leaves once so evaluating
// every candidate is a lookup plus a substring/regex test.
type contentLeafMatcher struct {
	leaves []contentLeaf
	size   int
	regex  map[int]*regexp.Regexp
}

func newContentLeafMatcher(pq parsedQuery) *contentLeafMatcher {
	leaves := contentAllLeaves(pq)
	m := &contentLeafMatcher{leaves: leaves}
	for i := range leaves {
		if leaves[i].LeafID+1 > m.size {
			m.size = leaves[i].LeafID + 1
		}
	}
	for _, leaf := range leaves {
		if leaf.Kind != contentLeafRegex {
			continue
		}
		re, err := regexp.Compile("(?i)" + leaf.Text)
		if err != nil {
			continue
		}
		if m.regex == nil {
			m.regex = make(map[int]*regexp.Regexp)
		}
		m.regex[leaf.LeafID] = re
	}
	return m
}

func (m *contentLeafMatcher) match(text []byte, leaf contentLeaf) bool {
	if leaf.Kind == contentLeafRegex {
		re := m.regex[leaf.LeafID]
		return re != nil && re.Match(text)
	}
	// Stored text is contentNormalizeText output (lowercased + repaired).
	return bytes.Contains(text, []byte(strings.ToLower(leaf.Text)))
}

// entryMatchesWithContent mirrors entryMatches but also enforces content leaves
// at every recursion level. Hot loops use entryMatchesWithContentMatcher with a
// matcher hoisted out of the loop.
func entryMatchesWithContent(vol *serviceVolumeIndex, entry Entry, pq parsedQuery, matchPath bool) bool {
	return entryMatchesWithContentMatcher(vol, entry, pq, matchPath, nil)
}

// entryMatchesWithContentMatcher evaluates the entry's document against every
// content leaf once, then applies the joint name+content predicate. A nil vol
// (or a query with no content leaves) degrades to entryMatches.
func entryMatchesWithContentMatcher(vol *serviceVolumeIndex, entry Entry, pq parsedQuery, matchPath bool, m *contentLeafMatcher) bool {
	if vol == nil || !queryHasAnyContentLeaf(pq) {
		return entryMatches(entry, pq, matchPath)
	}
	if m == nil {
		m = newContentLeafMatcher(pq)
	}
	matched := make([]bool, m.size)
	if text, ok := vol.contentTextForEntry(&entry); ok {
		for _, leaf := range m.leaves {
			matched[leaf.LeafID] = m.match(text, leaf)
		}
	}
	return entryMatchesContentAt(entry, pq, matchPath, matched)
}

// entryMatchesContentAt is entryMatches over a document whose content-leaf
// results are already computed. OR groups require the SAME alternative to
// satisfy both its name and content constraints (a content alternative and a
// name alternative cannot satisfy the group separately); NOT groups drop the
// entry when any negative alternative fully matches.
func entryMatchesContentAt(entry Entry, pq parsedQuery, matchPath bool, matched []bool) bool {
	if !entryMatchesScalar(entry, pq, matchPath) {
		return false
	}
	for _, leaf := range pq.Content {
		if leaf.LeafID < 0 || leaf.LeafID >= len(matched) || !matched[leaf.LeafID] {
			return false
		}
	}
	for _, group := range pq.OrGroups {
		ok := false
		for i := range group {
			if entryMatchesContentAt(entry, group[i], matchPath || group[i].MatchPath, matched) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, neg := range pq.NotGroups {
		if entryMatchesContentAt(entry, neg, matchPath || neg.MatchPath, matched) {
			return false
		}
	}
	return true
}

// contentTextForEntry resolves an entry's normalized content text. The delta
// wins over the base for a changed FRN; a tombstoned delta doc means absent.
func (vol *serviceVolumeIndex) contentTextForEntry(entry *Entry) ([]byte, bool) {
	if vol == nil || vol.content == nil || entry.FRN == 0 {
		return nil, false
	}
	reader, resolver := vol.content.readerResolverView()
	if delta := vol.content.deltaView(); delta != nil {
		if text, found, deleted := delta.textFor(entry.FRN); found {
			if deleted {
				return nil, false
			}
			return text, true
		}
	}
	if reader == nil || resolver == nil {
		return nil, false
	}
	docIndex, ok := resolver.docForFRN(entry.FRN)
	if !ok {
		return nil, false
	}
	text := reader.docText(uint32(docIndex))
	if len(text) == 0 {
		return nil, false
	}
	return text, true
}

// contentUsableForQuery reports whether a volume can apply content semantics.
func (vol *serviceVolumeIndex) contentUsableForQuery() bool {
	if vol == nil || vol.content == nil {
		return false
	}
	return vol.content.usableForQuery()
}

// contentVolumeName names a volume for the degraded set, including nil/empty
// volumes that would otherwise be silently omitted.
func contentVolumeName(vol *serviceVolumeIndex) string {
	if vol == nil {
		return "<nil>"
	}
	if vol.volume != "" {
		return vol.volume
	}
	if vol.index != nil && vol.index.Volume != "" {
		return vol.index.Volume
	}
	return "<unknown>"
}

// contentUsableVolumes partitions the eligible volumes into those that can
// apply content semantics and those that cannot. A content query errors only
// when NO volume is usable; otherwise the usable volumes answer and the skipped
// names (including nil and empty volumes) are surfaced as a degraded (partial)
// result. It is never correct to report "no matches" for a content query on the
// strength of an unusable volume.
func contentUsableVolumes(volumes []*serviceVolumeIndex, pq parsedQuery) (usable []*serviceVolumeIndex, skipped []string) {
	if !queryHasAnyContentLeaf(pq) {
		return volumes, nil
	}
	for _, vol := range volumes {
		if vol != nil && vol.contentUsableForQuery() {
			usable = append(usable, vol)
			continue
		}
		skipped = append(skipped, contentVolumeName(vol))
	}
	return usable, skipped
}

// markContentQueryDegraded records that a content query answered from only a
// subset of the eligible volumes, so the response can say the result is partial
// instead of implying completeness.
func markContentQueryDegraded(trace *searchTrace, skipped []string) {
	if trace == nil || len(skipped) == 0 {
		return
	}
	trace.ContentPartial = true
	trace.ContentSkippedVolumes = append([]string(nil), skipped...)
}

// contentCandidates returns compact record indices that are a superset of the
// records matching the query's positive content constraints, capped at
// contentCandidateBudget. ok=false means the caller should fall back to a
// normal scan (still correct, post-filtered inline); ok=true with an empty
// slice means there are genuinely no content matches and the caller must NOT
// fall back. A capped set marks the trace incomplete.
func (vol *serviceVolumeIndex) contentCandidates(pq parsedQuery) ([]int, bool) {
	if !contentSearchEnabled() || vol == nil || vol.content == nil {
		return nil, false
	}
	reader, resolver := vol.content.readerResolverView()
	if reader == nil || resolver == nil {
		return nil, false
	}
	sets := make([][]uint32, 0, len(pq.Content)+len(pq.OrGroups))
	if len(pq.Content) > 0 {
		// A top-level Content leaf is a conjunct, so its postings alone are a
		// superset of the whole query; OR groups only narrow it further.
		for _, leaf := range pq.Content {
			if leaf.Kind == contentLeafRegex {
				continue
			}
			term := strings.ToLower(leaf.Text)
			if !contentTermTrigramSafe(term) {
				// A gram the builder skipped (control byte) would look up
				// empty and falsely report zero; fall back to a scan.
				return nil, false
			}
			sets = append(sets, reader.candidates(term))
		}
	} else {
		// No top-level Content: coverage must come from content-driven OR
		// groups. A non-content alternative means the query can match without
		// content, so no postings superset exists; fall back.
		driven := false
		for _, group := range pq.OrGroups {
			docs, groupDriven, ok := contentOrGroupCandidateDocs(reader, group)
			if !ok {
				return nil, false
			}
			if !groupDriven {
				continue
			}
			driven = true
			if docs != nil {
				sets = append(sets, docs)
			}
		}
		if !driven {
			return nil, false
		}
	}
	if len(sets) == 0 {
		// Every positive content leaf is a regex or otherwise unconstrained:
		// no postings superset exists. Decline so the ordered content scan
		// handles it with inline verification.
		return nil, false
	}
	candidates := sets[0]
	for _, set := range sets[1:] {
		candidates = intersectSortedDocIDs(candidates, set)
		if len(candidates) == 0 {
			break
		}
	}
	capped := false
	if len(candidates) > contentCandidateBudget {
		candidates = candidates[:contentCandidateBudget]
		capped = true
	}
	out := make([]int, 0, len(candidates))
	for _, docID := range candidates {
		if id, ok := resolver.recordID(int(docID)); ok {
			out = append(out, int(id))
		}
	}
	// Delta docs may not be in the base postings (the base is stale), so add
	// every live delta FRN that maps to a base record. Low-memory mode does not
	// build vol.frns, so recordIDForFRN falls back to the persisted FRN column.
	if delta := vol.content.deltaView(); delta != nil {
		for _, doc := range delta.live() {
			if id, ok := vol.recordIDForFRN(doc.FRN); ok {
				out = append(out, id)
			}
		}
	}
	sort.Ints(out)
	out = uniqueSortedInts(out)
	if len(out) > contentCandidateBudget {
		out = out[:contentCandidateBudget]
		capped = true
	}
	if capped {
		pq.Trace.setContentIncomplete()
	}
	return out, true
}

// contentOrGroupCandidateDocs unions the content candidate doc sets of an OR
// group's alternatives. ok=false means a non-content alternative makes the
// group unbounded; docs==nil with driven=true means the union is unconstrained
// (a regex alternative).
func contentOrGroupCandidateDocs(reader *contentReader, group []parsedQuery) (docs []uint32, driven, ok bool) {
	var union []uint32
	have := false
	for i := range group {
		altDocs, altDriven, altOK := contentAltCandidateDocs(reader, group[i])
		if !altOK {
			return nil, false, false
		}
		if !altDriven {
			return nil, false, false
		}
		driven = true
		if altDocs == nil {
			return nil, true, true
		}
		if !have {
			union = altDocs
			have = true
		} else {
			union = unionSortedDocIDs(union, altDocs)
		}
	}
	if !have {
		return nil, true, true
	}
	return union, true, true
}

// contentAltCandidateDocs intersects an alternative's own positive Content leaf
// postings with its nested content-driven OR groups (AND semantics). driven
// reports whether the alternative has any content constraint at all.
func contentAltCandidateDocs(reader *contentReader, alt parsedQuery) (docs []uint32, driven, ok bool) {
	var sets [][]uint32
	for _, leaf := range alt.Content {
		driven = true
		if leaf.Kind == contentLeafRegex {
			continue
		}
		term := strings.ToLower(leaf.Text)
		if !contentTermTrigramSafe(term) {
			return nil, false, false
		}
		sets = append(sets, reader.candidates(term))
	}
	for _, group := range alt.OrGroups {
		groupDocs, groupDriven, groupOK := contentOrGroupCandidateDocs(reader, group)
		if !groupOK {
			return nil, false, false
		}
		if !groupDriven || groupDocs == nil {
			continue // non-content or unconstrained: no postings constraint here
		}
		driven = true
		sets = append(sets, groupDocs)
	}
	if !driven {
		return nil, false, true
	}
	if len(sets) == 0 {
		return nil, true, true
	}
	docs = sets[0]
	for _, set := range sets[1:] {
		docs = intersectSortedDocIDs(docs, set)
	}
	return docs, true, true
}

// contentTermTrigramSafe reports whether every trigram of term is indexable.
// contentGramsOf skips grams containing a control byte, so a lookup miss for
// such a gram is not evidence of absence; the caller must fall back to a scan
// rather than report an empty set.
func contentTermTrigramSafe(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] < 32 {
			return false
		}
	}
	return true
}

func unionSortedDocIDs(a, b []uint32) []uint32 {
	out := make([]uint32, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		default:
			out = append(out, b[j])
			j++
		}
	}
	out = append(out, a[i:]...)
	out = append(out, b[j:]...)
	return out
}

// searchContentVolume runs the content candidate + inline verification path for
// one volume and returns the verified, ordered entries WITHOUT applying the
// user limit; the caller merges and limits. It is the single implementation
// shared by the search and count paths so they agree exactly (count ==
// len(search)) for every query shape whose semantics do not require a stat.
func (vol *serviceVolumeIndex) searchContentVolume(opts queryOptions, countOnly bool) ([]Entry, error) {
	locked, ok := lockVolumeSearch(vol, opts)
	if !ok {
		return nil, errQueryCanceled
	}
	pathCache := make(map[int]string)
	hidden := vol.snapshotHiddenBaseIDs()
	matches, err := searchCompactWithCacheHidden(vol.index, opts, countOnly, pathCache, vol.nameTermCandidates, hidden, vol)
	if err != nil {
		return nil, err
	}
	matches = vol.mergeOverlayMatches(matches, opts, countOnly, pathCache)
	vol.trimSearchCachesLocked()
	if locked {
		vol.searchMu.Unlock()
	}
	// Search applies the Entry.Exists/implicit-:under filesystem re-check; a
	// count deliberately does not stat, so for those two shapes count and search
	// can diverge. Every other content shape agrees exactly.
	return filterImplicitUnderExisting(matches, opts, countOnly), nil
}

// searchContentServiceVolumes merges the per-volume content results in the same
// order/limit semantics as the other multi-volume paths: per-volume results are
// concatenated, globally sorted when they span volumes, and the user limit is
// applied only after inline content verification. It checks cancellation
// between volumes so a superseded query does not start the next full scan.
func searchContentServiceVolumes(volumes []*serviceVolumeIndex, opts queryOptions, countOnly bool, pq parsedQuery) ([]Entry, error) {
	results := make([]Entry, 0, 64)
	for _, vol := range volumes {
		if queryCanceled(parsedQuery{DeadlineUnix: opts.DeadlineUnix, Cancel: opts.Cancel}) {
			return nil, errQueryCanceled
		}
		matches, err := vol.searchContentVolume(opts, countOnly)
		if err != nil {
			return nil, err
		}
		results = append(results, matches...)
	}
	if !countOnly && entriesSpanMultipleVolumes(results) {
		sortSearchAllEntries(results, pq)
	}
	if !countOnly {
		if limit := normalizedLimit(opts.Limit, false); limit > 0 && len(results) > limit {
			results = results[:limit]
		}
	}
	opts.Trace.setPlannerMode("service-content")
	opts.Trace.setComplete(opts.Trace == nil || (!opts.Trace.ContentPartial && !opts.Trace.ContentIncomplete))
	return results, nil
}
