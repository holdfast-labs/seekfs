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
	"unicode/utf8"
)

// Default per-query content budgets. They cap materialized content candidates
// (candidate budget) and records visited by a fallback scan (visit budget).
// They are defaults only: a caller threads explicit per-query values through
// queryOptions so no mutable package state is shared between concurrent
// queries. Past either cap the candidate superset is incomplete and the query
// is marked as such rather than silently truncated.
const (
	contentDefaultCandidateBudget = 4_000_000
	contentDefaultScanVisitBudget = 4_000_000
)

// contentScanPathCacheCap bounds the path-reconstruction memo used by content
// fallback scans. Past the cap the memo is reset, so peak memory is O(cap)
// instead of O(records scanned); resetting only forces recomputation, never a
// wrong path.
const contentScanPathCacheCap = 4096

// contentCandidateBudgetOf returns the query's candidate budget, or the default
// when unset (<= 0).
func contentCandidateBudgetOf(pq parsedQuery) int {
	if pq.ContentCandidateBudget > 0 {
		return pq.ContentCandidateBudget
	}
	return contentDefaultCandidateBudget
}

// contentScanVisitBudgetOf returns the query's fallback-scan visit budget, or
// the default when unset (<= 0).
func contentScanVisitBudgetOf(pq parsedQuery) int {
	if pq.ContentScanVisitBudget > 0 {
		return pq.ContentScanVisitBudget
	}
	return contentDefaultScanVisitBudget
}

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
// every candidate is a lookup plus a substring/regex test. The needle is folded
// (or kept raw for a case-sensitive query) once here, not per candidate. Case is
// a query-level property (pq.CaseSensitive from `case:`), so every leaf shares
// it.
type contentLeafMatcher struct {
	leaves        []contentLeaf
	size          int
	caseSensitive bool
	regex         map[int]*regexp.Regexp
	needle        map[int][]byte
}

func newContentLeafMatcher(pq parsedQuery) *contentLeafMatcher {
	leaves := contentAllLeaves(pq)
	m := &contentLeafMatcher{leaves: leaves, caseSensitive: pq.CaseSensitive}
	for i := range leaves {
		if leaves[i].LeafID+1 > m.size {
			m.size = leaves[i].LeafID + 1
		}
	}
	for _, leaf := range leaves {
		if leaf.Kind == contentLeafRegex {
			pat := leaf.Text
			if !pq.CaseSensitive {
				pat = "(?i)" + pat
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				continue
			}
			if m.regex == nil {
				m.regex = make(map[int]*regexp.Regexp)
			}
			m.regex[leaf.LeafID] = re
			continue
		}
		if m.needle == nil {
			m.needle = make(map[int][]byte)
		}
		b := []byte(leaf.Text)
		if !pq.CaseSensitive {
			b = contentFoldText(b)
		}
		m.needle[leaf.LeafID] = b
	}
	return m
}

// needleOf returns the precomputed bytes a term/phrase leaf matches: the raw
// leaf text for a case-sensitive query, the folded text otherwise. It is also
// the needle a snippet searches with, so the snippet uses the same case policy
// as verification.
func (m *contentLeafMatcher) needleOf(leaf contentLeaf) []byte {
	if b, ok := m.needle[leaf.LeafID]; ok {
		return b
	}
	b := []byte(leaf.Text)
	if !m.caseSensitive {
		b = contentFoldText(b)
	}
	return b
}

// haystackFor returns the text a leaf is matched against: the case-preserving
// raw text for a case-sensitive query, the folded text otherwise. The caller
// folds the document once per entry and reuses it for every leaf, so a
// case-insensitive query costs one fold per record, not one per leaf.
func (m *contentLeafMatcher) haystackFor(raw, folded []byte) []byte {
	if m.caseSensitive {
		return raw
	}
	return folded
}

// match tests one leaf against a document. Regex leaves always run on the raw
// (case-preserving) text; the compiled pattern carries the (?i) flag when the
// query is case-insensitive.
func (m *contentLeafMatcher) match(raw, folded []byte, leaf contentLeaf) bool {
	if leaf.Kind == contentLeafRegex {
		re := m.regex[leaf.LeafID]
		return re != nil && re.Match(raw)
	}
	return bytes.Contains(m.haystackFor(raw, folded), m.needleOf(leaf))
}

// matchRawFirst is match with a lazy fold: a case-insensitive leaf whose
// lowercased needle already appears verbatim in the raw text is a match without
// folding, and only the first leaf that misses the raw fast path pays for the
// once-per-entry fold (cached through folded/foldedReady). A corpus of
// already-lowercase documents (the common case) then never folds at all. The
// match decision is identical to match: folding then testing a superset cannot
// introduce or drop a match.
func (m *contentLeafMatcher) matchRawFirst(raw []byte, folded *[]byte, foldedReady *bool, leaf contentLeaf) bool {
	if leaf.Kind == contentLeafRegex {
		re := m.regex[leaf.LeafID]
		return re != nil && re.Match(raw)
	}
	needle := m.needleOf(leaf)
	if bytes.Contains(raw, needle) {
		return true
	}
	if m.caseSensitive {
		return false
	}
	if !*foldedReady {
		*folded = contentFoldText(raw)
		*foldedReady = true
	}
	return bytes.Contains(*folded, needle)
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
		// Fold the document at most once for the whole entry, and only if a
		// case-insensitive leaf actually needs it; case-sensitive leaves read
		// the raw text. No per-leaf allocation.
		var folded []byte
		foldedReady := false
		for _, leaf := range m.leaves {
			matched[leaf.LeafID] = m.matchRawFirst(text, &folded, &foldedReady, leaf)
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

// contentLeafFirstOffset returns the byte offset of needle in text, or -1 when
// needle is nil. Callers pass the matcher's precomputed lowercased needle, so
// the leaf text is not re-lowercased per call; regex leaves are skipped by the
// caller because a regex match offset is not reliably available.
func contentLeafFirstOffset(text, needle []byte) int {
	if needle == nil {
		return -1
	}
	return bytes.Index(text, needle)
}

// contentPositiveLeaves returns the query's content leaves that contribute a
// positive match, in the canonical LeafID order. Leaves under NOT are exclusion
// and carry no relevance or snippet.
func contentPositiveLeaves(pq parsedQuery) []contentLeaf {
	var out []contentLeaf
	contentCollectPositiveLeaves(pq, &out)
	return out
}

func contentCollectPositiveLeaves(pq parsedQuery, out *[]contentLeaf) {
	*out = append(*out, pq.Content...)
	for g := range pq.OrGroups {
		for a := range pq.OrGroups[g] {
			contentCollectPositiveLeaves(pq.OrGroups[g][a], out)
		}
	}
}

// contentFoldOffsetToRaw maps a byte offset in a folded (lowercased) copy back
// to the case-preserving text it was folded from. strings.ToLower maps one rune
// to one rune, so the rune index is preserved; invalid-UTF-8 edge cases can only
// cost snippet precision, never a match.
func contentFoldOffsetToRaw(raw, folded []byte, off int) int {
	if off <= 0 {
		return 0
	}
	if off > len(folded) {
		off = len(folded)
	}
	n := utf8.RuneCount(folded[:off])
	i := 0
	for ; n > 0 && i < len(raw); n-- {
		_, size := utf8.DecodeRune(raw[i:])
		i += size
	}
	return i
}

// contentSnippet returns a bounded window around the first matching term/phrase
// content leaf, rendered from the case-preserving text. A query whose only
// content matches are regexes yields "". The positive-leaf list and matcher are
// hoisted by the caller so this is O(leaves) per result, not O(leaves) plus
// matcher construction. The caller passes the raw text and, for a
// case-insensitive query, the once-per-entry folded copy (nil otherwise).
func contentSnippet(raw, folded []byte, positive []contentLeaf, m *contentLeafMatcher) string {
	for _, leaf := range positive {
		if leaf.Kind == contentLeafRegex || !m.match(raw, folded, leaf) {
			continue
		}
		needle := m.needleOf(leaf)
		hay := m.haystackFor(raw, folded)
		off := contentLeafFirstOffset(hay, needle)
		if off < 0 {
			continue
		}
		if m.caseSensitive {
			return contentSnippetWindow(raw, off, len(needle))
		}
		// The folded offset maps to the raw offset by rune index; the match
		// length is the raw span between the two mapped boundaries, so a
		// case-changing fold (é -> É) still windows the real bytes.
		rawOff := contentFoldOffsetToRaw(raw, folded, off)
		rawEnd := contentFoldOffsetToRaw(raw, folded, off+len(needle))
		return contentSnippetWindow(raw, rawOff, rawEnd-rawOff)
	}
	return ""
}

// attachContentSnippets fills Entry.Snippet for content-query results. It is
// called after the result set is bounded by the limit, so the work is O(results).
// The matcher and positive-leaf list are built once per query by the caller.
func attachContentSnippets(entries []Entry, volByPath map[string]*serviceVolumeIndex, positive []contentLeaf, m *contentLeafMatcher) {
	if len(entries) == 0 || len(volByPath) == 0 || m == nil || len(positive) == 0 {
		return
	}
	for i := range entries {
		vol := volByPath[entries[i].Path]
		if vol == nil {
			continue
		}
		if text, ok := vol.contentTextForEntry(&entries[i]); ok {
			var folded []byte
			if !m.caseSensitive {
				folded = contentFoldText(text)
			}
			entries[i].Snippet = contentSnippet(text, folded, positive, m)
		}
	}
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

// filenameAnswerable reports whether a volume whose content index is unusable
// can still answer pq filename-only, treating every content leaf as false.
// Soundness (PF-7b):
//   - a top-level content leaf is a conjunct, so the query cannot match without
//     content;
//   - an OR group is satisfiable only if some alternative is itself answerable
//     without content;
//   - a NOT group that mentions content cannot be decided (treating its content
//     leaf as false would wrongly satisfy the exclusion), so the whole query is
//     refused;
//   - anything else is a pure filename/scalar predicate.
func filenameAnswerable(pq parsedQuery) bool {
	if len(pq.Content) > 0 {
		return false
	}
	for _, group := range pq.OrGroups {
		ok := false
		for i := range group {
			if filenameAnswerable(group[i]) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for i := range pq.NotGroups {
		if queryHasAnyContentLeaf(pq.NotGroups[i]) {
			return false
		}
	}
	return true
}

// stripContentLeaves returns a copy of pq with every content leaf removed:
// top-level Content is dropped, each OR group keeps only its filename-answerable
// alternatives (a kept content-only alternative would otherwise become
// match-all under entryMatches, which ignores Content), and NOT groups are
// copied as-is (filenameAnswerable guarantees none carries content). The result
// evaluates to exactly the original predicate under "all content leaves false",
// so it can never introduce a false positive.
func stripContentLeaves(pq parsedQuery) parsedQuery {
	out := pq
	out.Content = nil
	out.Terms = append([]string(nil), pq.Terms...)
	if len(pq.OrGroups) > 0 {
		out.OrGroups = make([][]parsedQuery, 0, len(pq.OrGroups))
		for _, group := range pq.OrGroups {
			stripped := make([]parsedQuery, 0, len(group))
			for i := range group {
				if filenameAnswerable(group[i]) {
					stripped = append(stripped, stripContentLeaves(group[i]))
				}
			}
			out.OrGroups = append(out.OrGroups, stripped)
		}
	}
	return out
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

// markContentQueryIncomplete surfaces a volume whose restart catch-up was
// truncated (health.Incomplete): the base is missing records, so the answer is
// not complete even though it was evaluated. Without this the query reported
// complete while `loaded --json` reported incomplete.
func markContentQueryIncomplete(trace *searchTrace, volumes []*serviceVolumeIndex) {
	if trace == nil {
		return
	}
	for _, vol := range volumes {
		if vol != nil && vol.content != nil && vol.content.healthIncomplete() {
			trace.setContentIncomplete()
			return
		}
	}
}

// contentCandidates returns compact record indices that are a superset of the
// records matching the query's positive content constraints, capped at the
// query's candidate budget. ok=false means the caller should fall back to a
// normal scan (still correct, post-filtered inline); ok=true with an empty
// slice means there are genuinely no content matches and the caller must NOT
// fall back. A capped set marks the trace incomplete.
func (vol *serviceVolumeIndex) contentCandidates(pq parsedQuery) ([]int, bool) {
	out, ok, _ := vol.contentCandidatesBounded(pq, 0)
	return out, ok
}

// contentCandidatesBounded is contentCandidates with an explicit materialization
// cap. cap > 0 stops the posting intersection (and the final set) at cap
// candidates; the third return reports whether the superset was truncated
// (capped) rather than fully materialized. A caller that only needs its top
// window can pass the window as cap and, on truncation, switch to the
// rank-ordered bounded scan: a truncated posting set is ordered by docID, not by
// rank, so it must not be used as the result ordering.
func (vol *serviceVolumeIndex) contentCandidatesBounded(pq parsedQuery, cap int) ([]int, bool, bool) {
	if !contentSearchEnabled() || vol == nil || vol.content == nil {
		return nil, false, false
	}
	reader, resolver := vol.content.readerResolverView()
	if reader == nil || resolver == nil {
		return nil, false, false
	}
	// Each positive content leaf contributes a lazy docID stream (the
	// merge-intersection of its trigrams); intersecting the streams and
	// materializing only up to the budget keeps peak memory O(budget + streams)
	// instead of decoding every posting list in full.
	var streams []func() (uint32, bool)
	if len(pq.Content) > 0 {
		// A top-level Content leaf is a conjunct, so its postings alone are a
		// superset of the whole query; OR groups only narrow it further.
		for _, leaf := range pq.Content {
			if leaf.Kind == contentLeafRegex {
				continue
			}
			term := string(contentFoldText([]byte(leaf.Text)))
			if !contentTermTrigramSafe(term) {
				// A gram the builder skipped (control byte) would look up
				// empty and falsely report zero; fall back to a scan.
				return nil, false, false
			}
			streams = append(streams, reader.candidateStream(term))
		}
	} else {
		// No top-level Content: coverage must come from content-driven OR
		// groups. A non-content alternative means the query can match without
		// content, so no postings superset exists; fall back.
		driven := false
		for _, group := range pq.OrGroups {
			groupStream, groupDriven, ok := contentOrGroupCandidateStream(reader, group)
			if !ok {
				return nil, false, false
			}
			if !groupDriven {
				continue
			}
			driven = true
			if groupStream != nil {
				streams = append(streams, groupStream)
			}
		}
		if !driven {
			return nil, false, false
		}
	}
	if len(streams) == 0 {
		// Every positive content leaf is a regex or otherwise unconstrained:
		// no postings superset exists. Decline so the ordered content scan
		// handles it with inline verification.
		return nil, false, false
	}
	budget := contentCandidateBudgetOf(pq)
	// materialize bounds how much of the posting intersection is decoded: the
	// explicit cap when the caller only needs its top window, else the full
	// query budget. Pull one past it so an over-cap set is still detected
	// without decoding the whole list.
	materialize := budget
	if cap > 0 && cap < materialize {
		materialize = cap
	}
	limit := materialize + 1
	if limit <= 0 {
		limit = materialize
	}
	candidates := collectDocIDStream(intersectDocIDStreams(streams), limit)
	// capped reports that the returned superset was truncated by the
	// materialization cap and must not be used as a complete ordered set. It is
	// deliberately separate from incompleteness: the page/window cap is an
	// internal optimization the in-order bounded scan compensates for (it finds
	// the true top page), so exceeding it is NOT by itself a completeness
	// failure. Only exceeding the true query budget sets ContentIncomplete, the
	// pre-PB8 contract; the caller's window probe covers the rest.
	capped := false
	budgetExceeded := false
	if materialize > 0 && len(candidates) > materialize {
		candidates = candidates[:materialize]
		capped = true
		// Materializing to the full budget and still overflowing is a real
		// budget overrun; a smaller explicit cap only truncates a superset the
		// caller still verifies.
		if materialize >= budget {
			budgetExceeded = true
		}
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
	if budget > 0 && len(out) > budget {
		out = out[:budget]
		capped = true
		budgetExceeded = true
	}
	if budgetExceeded {
		pq.Trace.setContentIncomplete()
	}
	return out, true, capped
}

// contentOrGroupCandidateStream is the streaming form of the OR group union: a
// lazy ascending stream over the distinct docIDs of the group's alternatives,
// capped only by the caller's collector budget. ok=false means a non-content
// alternative makes the group unbounded; a nil stream with driven=true means
// the union is unconstrained (a regex alternative). It is the same k-way union
// as the historical pairwise unionSortedDocIDs merge, but it materializes no
// alternative: the caller's budget+1 cap stops it before any full list decodes.
func contentOrGroupCandidateStream(reader *contentReader, group []parsedQuery) (stream func() (uint32, bool), driven, ok bool) {
	var streams []func() (uint32, bool)
	for i := range group {
		altStream, altDriven, altOK := contentAltCandidateStream(reader, group[i])
		if !altOK {
			return nil, false, false
		}
		if !altDriven {
			return nil, false, false
		}
		driven = true
		if altStream == nil {
			return nil, true, true
		}
		streams = append(streams, altStream)
	}
	if len(streams) == 0 {
		return nil, true, true
	}
	return unionDocIDStreams(streams), true, true
}

// contentAltCandidateStream intersects an alternative's own positive Content
// leaf postings with its nested content-driven OR groups (AND semantics) into a
// lazy ascending stream. driven reports whether the alternative has any content
// constraint at all; a nil stream with driven=true means the constraint is
// unconstrained (a regex leaf). Every source is a posting-backed stream, so an
// alternative is never materialized in full.
func contentAltCandidateStream(reader *contentReader, alt parsedQuery) (stream func() (uint32, bool), driven, ok bool) {
	var streams []func() (uint32, bool)
	for _, leaf := range alt.Content {
		driven = true
		if leaf.Kind == contentLeafRegex {
			continue
		}
		term := string(contentFoldText([]byte(leaf.Text)))
		if !contentTermTrigramSafe(term) {
			return nil, false, false
		}
		streams = append(streams, reader.candidateStream(term))
	}
	for _, group := range alt.OrGroups {
		groupStream, groupDriven, groupOK := contentOrGroupCandidateStream(reader, group)
		if !groupOK {
			return nil, false, false
		}
		if !groupDriven || groupStream == nil {
			continue // non-content or unconstrained: no postings constraint here
		}
		driven = true
		streams = append(streams, groupStream)
	}
	if !driven {
		return nil, false, true
	}
	if len(streams) == 0 {
		return nil, true, true
	}
	return intersectDocIDStreams(streams), true, true
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

// unionSortedDocIDs is the historical pairwise union of two ascending docID
// slices. The OR candidate path now streams (unionDocIDStreams); this remains
// the reference the streamed union is differentially tested against.
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
	// can diverge (count may exceed search). Every other content shape agrees
	// exactly. This function is only reached through the content path, so the
	// query always carries a content leaf: flag the divergence on a count so it
	// is visible, but still return the count (ContentIncomplete is what refuses
	// an inexact count; this is not that).
	if countOnly && opts.Trace != nil && (opts.Under != "" || opts.Exists) {
		opts.Trace.ContentCountDivergent = true
	}
	return filterImplicitUnderExisting(matches, opts, countOnly), nil
}

// searchFilenameOnlyVolume evaluates pq's filename-answerable subset on a volume
// whose content index is unusable. It strips every content leaf and reuses the
// content per-volume scan, so candidate generation, overlay merge, the bounded
// window and the Exists/under re-check match the normal filename path exactly.
// The caller has already checked filenameAnswerable(pq).
func (vol *serviceVolumeIndex) searchFilenameOnlyVolume(opts queryOptions, countOnly bool, pq parsedQuery) ([]Entry, error) {
	stripped := stripContentLeaves(pq)
	opts.parsedOverride = &stripped
	return vol.searchContentVolume(opts, countOnly)
}

// countFilenameOnlyVolume is countContentVolume for the filename-only fallback:
// the stripped query is threaded through the same tally path so count == len of
// the filename-only search on the same volume.
func (vol *serviceVolumeIndex) countFilenameOnlyVolume(opts queryOptions, pq parsedQuery) (int, error) {
	stripped := stripContentLeaves(pq)
	opts.parsedOverride = &stripped
	return vol.countContentVolume(opts)
}

// queryNeedsEntryPath reports whether any level of the query reads Entry.Path
// (a path match, :under, Exists, parent/dir filters or a regexp). When false, a
// content count can evaluate the query from the record's name/metadata alone
// and never reconstruct a path.
func queryNeedsEntryPath(pq parsedQuery) bool {
	if pq.MatchPath || pq.Under != "" || pq.Exists ||
		len(pq.Parents) > 0 || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 {
		return true
	}
	for _, group := range pq.OrGroups {
		for i := range group {
			if queryNeedsEntryPath(group[i]) {
				return true
			}
		}
	}
	for i := range pq.NotGroups {
		if queryNeedsEntryPath(pq.NotGroups[i]) {
			return true
		}
	}
	return false
}

// countContentVolume returns the number of records matching a content query
// without materializing an Entry slice: the base scan counts in place (and
// skips path reconstruction unless the query needs it), and the overlay delta
// is counted through the same predicate. It keeps count == len(search) for the
// same query.
func (vol *serviceVolumeIndex) countContentVolume(opts queryOptions) (int, error) {
	count := 0
	opts.contentCount = &count
	if _, err := vol.searchContentVolume(opts, true); err != nil {
		return 0, err
	}
	// The base scan counted base matches; add the overlay count here so a
	// cancellation inside the overlay walk can be surfaced.
	if snap := vol.snap.Load(); snap != nil && len(snap.records) > 0 {
		pq, err := parseQuery(opts)
		if err != nil {
			return 0, err
		}
		n, err := vol.overlayLiveMatchCountCancellable(snap, pq)
		if err != nil {
			return 0, err
		}
		count += n
	}
	return count, nil
}

// contentRelevanceWindow is the enlarged per-volume result window fetched
// before a post-verify content ordering (sort:relevance, or the default
// name/path order). It is min(budget, max(limit*100, 4096)), computed without
// overflow: once limit exceeds budget/100 the product cannot fall below the
// budget, so the budget is the min.
func contentRelevanceWindow(limit, budget int) int {
	if limit <= 0 {
		return 0
	}
	if budget <= 0 {
		budget = contentDefaultCandidateBudget
	}
	if limit > budget/100 {
		return budget
	}
	window := limit * 100
	if window < 4096 {
		window = 4096
	}
	if window > budget {
		window = budget
	}
	return window
}

// nameLowerNameShared reports whether more than one record in the volume has
// the given lowercased name. A page-limited per-volume search can split a shared
// name group, and the cross-volume merge breaks name ties by path while a volume
// breaks them by record id, so such a volume must widen to the window to stay
// byte-identical with the unbounded search. An unknown or unsorted order reports
// shared (conservative): widening is always the correct fallback.
func (vol *serviceVolumeIndex) nameLowerNameShared(lowerName string) bool {
	if vol == nil || vol.index == nil || lowerName == "" {
		return true
	}
	order := vol.mappedOrCompactNameOrder()
	if len(order) == 0 {
		return true
	}
	lo := sort.Search(len(order), func(i int) bool {
		return vol.index.compactLowerNameAt(int(order[i])) >= lowerName
	})
	if lo >= len(order) || vol.index.compactLowerNameAt(int(order[lo])) != lowerName {
		return false
	}
	return lo+1 < len(order) && vol.index.compactLowerNameAt(int(order[lo+1])) == lowerName
}

// searchContentServiceVolumes merges the per-volume content results in the same
// order/limit semantics as the other multi-volume paths: per-volume results are
// concatenated, globally sorted when they span volumes, and the user limit is
// applied only after inline content verification. It checks cancellation
// between volumes so a superseded query does not start the next full scan.
//
// A volume whose content index is unusable contributes its filename-answerable
// matches (PF-7b): the content matches are still missing and the volume is
// surfaced as degraded by the caller, but the filename matches it can answer are
// no longer dropped. When pq is not filename-answerable (a required top-level
// content leaf, a content-only OR, or a content negation) the volume is skipped
// exactly as before.
func searchContentServiceVolumes(volumes []*serviceVolumeIndex, opts queryOptions, countOnly bool, pq parsedQuery) ([]Entry, error) {
	postVerify := !countOnly && queryHasAnyContentLeaf(pq)
	relevance := postVerify && pq.SortColumn == "relevance"
	// A content query with no explicit sort must return the same relative order
	// as the equivalent filename query. Single-volume order comes from the
	// volume's rankForQuery(pq) candidate order (the same source the filename
	// single-volume path uses; see nameTermCandidates). Multi-volume order is
	// applied after the merge with the shared comparator below. The default
	// order still fetches an enlarged bounded window so a candidate budget cap
	// is surfaced as incomplete rather than silently truncating the page.
	defaultOrder := postVerify && pq.SortColumn == ""
	userLimit := 0
	if !countOnly {
		userLimit = normalizedLimit(opts.Limit, false)
	}
	// Relevance and default order must rank the best matches, not an arbitrary
	// candidate-order page: fetch an enlarged per-volume window, order it, then
	// trim to the user limit. Reaching the window means more matches may exist,
	// which is surfaced as an incomplete (degraded) result rather than a silent
	// truncation.
	window := 0
	if relevance || defaultOrder {
		window = contentRelevanceWindow(userLimit, contentCandidateBudgetOf(pq))
	}
	filenameOnly := filenameAnswerable(pq)
	var volByPath map[string]*serviceVolumeIndex
	if postVerify {
		volByPath = make(map[string]*serviceVolumeIndex)
	}
	results := make([]Entry, 0, 64)
	for _, vol := range volumes {
		if queryCanceled(parsedQuery{DeadlineUnix: opts.DeadlineUnix, Cancel: opts.Cancel}) {
			return nil, errQueryCanceled
		}
		volOpts := opts
		if window > 0 {
			volOpts.Limit = window
		}
		contentVolume := vol != nil && vol.contentUsableForQuery()
		// A non-relevance query has candidate order == result order, so the
		// first userLimit verified candidates are exactly the page, single- or
		// multi-volume: verify only the page and probe the window separately for
		// the completeness signal. The multi-volume merge reorders by (name,
		// path) while a volume orders by (name, record id); if the page boundary
		// splits a shared name that difference could drop a globally-ranked
		// entry, so a volume whose boundary name is shared anywhere in the index
		// falls back to the window below. A pending overlay disables the bounded
		// candidate fast path, so it must also disable the page reduction:
		// otherwise the window probe below (len(matches) >= window) can never
		// fire and a window-full query reports complete.
		reduced := !opts.contentFullCandidates && defaultOrder && contentVolume &&
			userLimit > 0 && userLimit < window && vol.snapshotHiddenBaseIDs().empty()
		if reduced {
			volOpts.Limit = userLimit
			volOpts.contentWindow = window
		}
		probe := &contentCompletenessProbe{}
		volOpts.contentProbe = probe
		var matches []Entry
		var err error
		if contentVolume {
			matches, err = vol.searchContentVolume(volOpts, countOnly)
			// The page-limit reduction is safe only when the completeness signal
			// survives it: a capped superset (window-capped, page full) carries
			// it here, and a provably-under-window superset is already complete.
			// Anything else (an unbounded source such as a regex, or a superset
			// exactly at the window) must widen back to the window so the
			// len(matches) >= window probe can decide, exactly as pre-PB8. A
			// shared boundary name forces the widen for merge-tie parity.
			//
			// Gate on the page being filled (len >= userLimit), not on an exact
			// equality: a pending overlay addition is merged in after the base
			// page and makes the merged count exceed userLimit even though the
			// base page filled, so equality would skip the widen for an
			// unknown-probe query and report complete=true for a window that was
			// never evaluated. The boundary is the last entry the global limit
			// keeps (index userLimit-1 of the name/path-sorted merged set), not
			// the merged tail, which an overlay addition can occupy.
			widen := err == nil && reduced && len(matches) >= userLimit &&
				((!probe.windowCapped && !probe.supersetBelowWindow) ||
					vol.nameLowerNameShared(entryLowerName(matches[userLimit-1])))
			if widen {
				volOpts.Limit = window
				volOpts.contentWindow = 0
				probe = &contentCompletenessProbe{}
				volOpts.contentProbe = probe
				matches, err = vol.searchContentVolume(volOpts, countOnly)
			}
		} else if filenameOnly && vol != nil {
			matches, err = vol.searchFilenameOnlyVolume(volOpts, countOnly, pq)
		} else {
			continue
		}
		if err != nil {
			return nil, err
		}
		// The window probe is the pre-PB8 completeness signal: a match set that
		// reaches the window may have more beyond it. The reduced path cannot run
		// it (matches is page-bounded), so when it truncated the superset at the
		// window and the page filled, the window-capped flag carries the same
		// signal. A page that did not fill proves all matches are below the
		// window, so it stays complete even though the superset was capped.
		if window > 0 && len(matches) >= window {
			opts.Trace.setContentIncomplete()
		} else if reduced && probe.windowCapped && len(matches) >= userLimit {
			opts.Trace.setContentIncomplete()
		}
		if postVerify && contentVolume {
			// A filename-only volume is deliberately left out of volByPath: it
			// has no content matches, so it must not gain a content snippet or
			// content relevance from a stale/delta document.
			for i := range matches {
				if _, ok := volByPath[matches[i].Path]; !ok {
					volByPath[matches[i].Path] = vol
				}
			}
		}
		results = append(results, matches...)
	}
	var contentMatcher *contentLeafMatcher
	var positive []contentLeaf
	if postVerify {
		contentMatcher = newContentLeafMatcher(pq)
		positive = contentPositiveLeaves(pq)
	}
	if !countOnly && relevance {
		sortContentEntriesByRelevance(results, volByPath, pq, contentMatcher)
	} else if !countOnly && entriesSpanMultipleVolumes(results) {
		// Multi-volume filename parity: the merged set uses the shared
		// comparator (compareSearchAllEntries via sortSearchAllEntries), exactly
		// as the filename multi-volume path does. A single volume is already in
		// the volume's rankForQuery order from candidate generation and must not
		// be re-sorted, since compareSearchAllEntryNamePath breaks basename ties
		// by path while the filename single-volume rank breaks them by record
		// id.
		sortSearchAllEntries(results, pq)
	}
	if !countOnly && userLimit > 0 && len(results) > userLimit {
		results = results[:userLimit]
	}
	// Snippets are attached last so the work is bounded by the returned set.
	if postVerify {
		attachContentSnippets(results, volByPath, positive, contentMatcher)
	}
	opts.Trace.setPlannerMode("service-content")
	opts.Trace.setComplete(opts.Trace == nil || (!opts.Trace.ContentPartial && !opts.Trace.ContentIncomplete))
	return results, nil
}
