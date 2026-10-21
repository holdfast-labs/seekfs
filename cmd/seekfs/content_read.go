package main

// Read side of the offline content index: dictionary lookup, trigram candidate
// generation, per-file filter narrowing, and substring verification by reading
// matched files from disk (v11: no stored text).

import (
	"bytes"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Snippet window bounds. The window is measured in runes (not bytes) and gets
// an ASCII ellipsis on each truncated side.
const (
	contentSnippetMaxRunes     = 200
	contentSnippetContextRunes = 60
)

var contentSnippetSpace = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ")

// contentHit is one matching document.
type contentHit struct {
	Path    string
	DocID   uint32
	Offset  int
	Snippet string
}

// contentSnippetWindow returns a bounded window of text around the match at byte
// offset off. Newlines and tabs are collapsed to spaces so the snippet stays one
// line. Only the bytes around the window are decoded, so the work is O(window)
// rather than O(len(text)); the start and end snap to rune boundaries so a
// multibyte rune is never split. text must be valid UTF-8 (the stored form),
// which preserves case since PF-6a, so no case-carrying source is needed.
func contentSnippetWindow(text []byte, off, matchLen int) string {
	// off == len(text) is not a match start: there is no byte to window around,
	// and indexing text[off] below would panic.
	if off < 0 || off >= len(text) {
		return ""
	}
	if matchLen < 0 {
		matchLen = 0
	}
	end := off + matchLen
	if end > len(text) {
		end = len(text)
	}
	// Snap to rune boundaries: a caller offset can land inside a multibyte rune
	// (and a boundary-cut window must not split one). off < len(text) is
	// rechecked so the loop can never index past the end.
	for off > 0 && off < len(text) && !utf8.RuneStart(text[off]) {
		off--
	}
	for end > off && end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	// Up to contentSnippetContextRunes runes of context before the match.
	start := off
	for n := 0; n < contentSnippetContextRunes && start > 0; n++ {
		_, size := utf8.DecodeLastRune(text[:start])
		start -= size
	}
	// The window is contentSnippetMaxRunes runes starting at start.
	stop := start
	for n := 0; n < contentSnippetMaxRunes && stop < len(text); n++ {
		_, size := utf8.DecodeRune(text[stop:])
		stop += size
	}
	if stop < end {
		stop = end
	}
	window := []rune(contentSnippetSpace.Replace(string(text[start:stop])))
	if start > 0 {
		window = append([]rune("..."), window...)
	}
	if stop < len(text) {
		window = append(window, []rune("...")...)
	}
	return string(window)
}

// contentReader is a decoded `.gsx` ready to query. In v11 there is no
// stored text: candidates come from the trigram postings (plus the pruned
// set), the per-file filter (CXFL) narrows them before any disk read, and
// verification reads the file from disk with the build's extraction policy
// (extract, below).
type contentReader struct {
	idx     *contentIndex
	terms   *contentTermDict
	grams   *contentPostingIndex
	paths   *contentPathTable
	filter  *contentFilterSection
	extract contentExtractSettings
	// pruned is the CXGP pruned-gram set: dense grams the builder dropped
	// from the postings (see contentGramPruneDF). Nil when the index
	// prunes nothing. candidateStreamIn skips pruned grams as
	// constraints; a term left with no usable gram falls back to the
	// full scan like a sub-3-byte term. The filter still constrains pruned
	// grams (it is never pruned), so dense shapes narrow before any read.
	pruned map[uint32]struct{}
	// aliasExpand maps a primary docID to its alias docIDs (§6z dedupe):
	// aliases share the primary's bytes but own no postings, so every
	// posting-derived candidate set expands over this map before
	// verification. Nil when the index dedupes nothing. On a composite
	// the IDs are global (rebased); shard readers carry local maps.
	aliasExpand map[uint32][]uint32
}

// expandAliasDocIDs unions docIDs with every alias of an included primary
// and sorts ascending. Idempotent: inputs already containing aliases (the
// all-documents stream, scan shapes) come back unchanged apart from order.
func (r *contentReader) expandAliasDocIDs(ids []uint32) []uint32 {
	if len(r.aliasExpand) == 0 {
		// Posting streams and the all-documents stream are ascending by
		// construction; with no aliases there is nothing to add.
		return ids
	}
	seen := make(map[uint32]struct{}, len(ids))
	out := make([]uint32, 0, len(ids))
	add := func(id uint32) {
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, id := range ids {
		add(id)
		for _, a := range r.aliasExpand[id] {
			add(a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// openContentReader decodes the queryable sections of idx.
func openContentReader(idx *contentIndex) (*contentReader, error) {
	if idx == nil {
		return nil, errors.New("nil content index")
	}
	if idx.shards != nil {
		// The manifest carries no rows itself; shard readers hold their own
		// filters. Adopt the first shard's root and extraction identity so
		// relative document paths resolve and verify with the build policy.
		r := &contentReader{idx: idx, extract: contentDefaultExtractSettings()}
		for _, sh := range idx.shards {
			if sh != nil && sh.reader != nil && sh.reader.filter != nil {
				r.filter = &contentFilterSection{Root: sh.reader.filter.Root}
				r.extract = sh.reader.extract
				break
			}
		}
		// Global alias map: rebase each shard's local pairs (shard
		// readers decoded their own CXAL at open; a shard without one
		// contributes nothing). The manifest itself holds no docs.
		for _, sh := range idx.shards {
			if sh == nil || sh.reader == nil || len(sh.reader.aliasExpand) == 0 {
				continue
			}
			if r.aliasExpand == nil {
				r.aliasExpand = make(map[uint32][]uint32)
			}
			for prim, aliases := range sh.reader.aliasExpand {
				gp := sh.base + prim
				ga := make([]uint32, 0, len(aliases))
				for _, a := range aliases {
					ga = append(ga, sh.base+a)
				}
				r.aliasExpand[gp] = ga
			}
		}
		return r, nil
	}
	r := &contentReader{
		idx:     idx,
		extract: contentDefaultExtractSettings(),
	}
	if sec := idx.Sections[contentSectionPaths]; len(sec) > 0 {
		paths, err := openContentPaths(sec, len(idx.Docs))
		if err != nil {
			return nil, err
		}
		r.paths = paths
	}
	if sec := idx.Sections[contentSectionTerms]; len(sec) > 0 {
		t, err := decodeContentTermDict(sec)
		if err != nil {
			return nil, err
		}
		r.terms = t
	}
	if sec := idx.Sections[contentSectionGrams]; len(sec) > 0 {
		g, err := decodeContentPostingIndex(sec)
		if err != nil {
			return nil, err
		}
		r.grams = g
	}
	// A v9 shard pruned of dense grams carries the set as CXGP; a corrupt
	// set fails the load (like corrupt postings) so a query can never read
	// a pruned gram as absent. Absent CXGP means unpruned postings.
	if sec := idx.Sections[contentSectionPruned]; len(sec) > 0 {
		p, err := decodeContentPrunedGrams(sec)
		if err != nil {
			return nil, err
		}
		r.pruned = p
	}
	// The v11 per-file filter: bit rows in DocID order plus the build's
	// extraction identity (caps, root, encoding) for disk verification. A
	// corrupt section fails the load; an ABSENT section only disables the
	// prefilter (postings + disk verification still answer, only slower).
	if sec := idx.Sections[contentSectionFilter]; len(sec) > 0 {
		f, err := decodeContentFilterSection(sec, len(idx.Docs))
		if err != nil {
			return nil, err
		}
		r.filter = f
		if f.MaxRaw > 0 {
			r.extract.maxRaw = f.MaxRaw
		}
		if f.MaxText > 0 {
			r.extract.maxText = f.MaxText
		}
		// The caps identity rides with the caps: explicit caps verify
		// uniformly, policy defaults verify repo-aware fail-open (see
		// contentDiskExtractOnce).
		r.extract.capsExplicit = f.CapsExplicit
		if f.Encoding != "" {
			if m, err := parseContentEncoding(f.Encoding); err == nil {
				r.extract.encoding = m
			}
		}
	} else if idx.Policy.MaxRaw > 0 {
		r.extract.maxRaw = idx.Policy.MaxRaw
		if idx.Policy.MaxText > 0 {
			r.extract.maxText = idx.Policy.MaxText
		}
	}
	// Alias expansion prefers the decoded field (in-memory builds that
	// never round-tripped through a file); the CXAL section is the same
	// data for loads.
	if len(idx.Aliases) > 0 {
		r.aliasExpand = contentAliasExpandMap(idx.Aliases)
	} else if sec := idx.Sections[contentSectionAlias]; len(sec) > 0 {
		if pairs, err := decodeContentAliases(sec, len(idx.Docs)); err != nil {
			return nil, err
		} else {
			r.aliasExpand = contentAliasExpandMap(pairs)
		}
	}
	return r, nil
}

// filterRow returns the filter bits for a document (shard-aware).
func (r *contentReader) filterRow(docID uint32) (contentFilterRow, bool) {
	if r.idx.shards != nil {
		if sh, local, ok := r.shardOf(docID); ok && sh.reader != nil {
			return sh.reader.filterRow(local)
		}
		return contentFilterRow{}, false
	}
	if r.filter == nil {
		return contentFilterRow{}, false
	}
	return r.filter.row(int(docID))
}

// filterRoot returns the build root for resolving relative document paths.
func (r *contentReader) filterRoot() string {
	if r.filter != nil {
		return r.filter.Root
	}
	return ""
}

// verifyPath resolves a document's stored path for disk verification: stored
// absolute paths verify directly; relative ones resolve against the build
// root carried by CXFL.
func (r *contentReader) verifyPath(docID uint32) string {
	p := r.docPath(docID)
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	if root := r.filterRoot(); root != "" {
		return filepath.Join(root, filepath.FromSlash(p))
	}
	return p
}

// docPath returns the path for a docID.
func (r *contentReader) docPath(docID uint32) string {
	if r.idx.shards != nil {
		if sh, local, ok := r.shardOf(docID); ok {
			return sh.reader.docPath(local)
		}
		return ""
	}
	if r.paths == nil {
		return ""
	}
	return r.paths.path(docID)
}

// search returns documents whose text contains term (case-insensitive
// substring), ordered by docID, up to limit (limit <= 0 means unlimited).
// v11 verifies from disk: candidates come from the trigram postings, the
// per-file filter narrows them before any read, and each surviving candidate
// is opened, extracted with the build policy, and substring-matched. With
// §6z aliases the candidate set materializes first so duplicates expand
// into docID order before the limit applies (every duplicate path is
// verified from its own bytes and listed, not just the primary); without
// aliases the stream stays lazy, so a small limit never allocates an
// all-document slice. The
// second return reports whether the scan ran to completion: false means the
// disk verification cap stopped it with records unvisited (a visible subset,
// never silent).
func (r *contentReader) search(term string, limit int) ([]contentHit, bool) {
	return r.searchWithBudget(term, limit, 0)
}

// searchWithBudget is the offline reader's opt-in time-bounded counterpart.
// The ordinary search method remains exhaustive for internal callers/tests;
// the standalone content CLI supplies a deadline only for a positive explicit
// --budget. A timed-out scan returns its verified prefix and complete=false;
// each already-started file read remains non-interruptible.
func (r *contentReader) searchWithBudget(term string, limit, budgetMS int) ([]contentHit, bool) {
	var deadline time.Time
	if budgetMS > 0 {
		deadline = time.Now().Add(time.Duration(budgetMS) * time.Millisecond)
	}
	t := string(contentFoldAccent([]byte(term)))
	if t == "" {
		return nil, true
	}
	accentInsensitive := string(contentFoldAccent([]byte(term))) == string(contentFoldText([]byte(term)))
	needle := []byte(t)
	if !accentInsensitive {
		needle = contentFoldCaseExact([]byte(term))
	}
	// Filter bits are built from the accent-folded text (v12), so the gate
	// tests the accent-folded query; an accent-exact needle is only the
	// verify-time haystack form.
	grams := contentFilterGramsOf([]byte(t))
	next := r.candidateStream(t)
	if len(r.aliasExpand) > 0 {
		ids := r.expandAliasDocIDs(collectDocIDStream(next, 0))
		nextIdx := 0
		next = func() (uint32, bool) {
			if nextIdx >= len(ids) {
				return 0, false
			}
			id := ids[nextIdx]
			nextIdx++
			return id, true
		}
	}
	var hits []contentHit
	verified := 0
	for {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return hits, false
		}
		id, ok := next()
		if !ok {
			return hits, true
		}
		if hit, ok, capped := r.verifyDiskContent(id, needle, grams, accentInsensitive, &verified); capped {
			return hits, false
		} else if ok {
			hits = append(hits, *hit)
			if limit > 0 && len(hits) >= limit {
				return hits, true
			}
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return hits, false
		}
	}
}

// verifyDiskContent tests one candidate document and builds its hit (nil when
// the document does not match). The filter gate runs first (bit tests only),
// so most non-matches never touch disk; survivors are read fresh from disk
// with the build's extraction policy and matched exactly like the service
// verify path (ASCII fast path, then fold with offset mapping). needle is the
// pre-folded query bytes and grams its precomputed filter keys, both hoisted
// by the caller. capped reports the per-query disk verification budget
// (contentDiskVerifyBudget) stopped the read.
func (r *contentReader) verifyDiskContent(id uint32, needle []byte, grams []uint32, accentInsensitive bool, verified *int) (*contentHit, bool, bool) {
	if len(grams) > 0 {
		row, ok := r.filterRow(id)
		if !ok {
			// No filter row (a section the builder never wrote): the file
			// may still match, so verify from disk without narrowing.
		} else if !contentFilterMayMatch(row, grams) {
			return nil, false, false
		}
	}
	path := r.verifyPath(id)
	if path == "" {
		return nil, false, false
	}
	if *verified >= contentDiskVerifyBudget {
		return nil, false, true
	}
	*verified++
	text, ok := contentDiskReadForVerify(path, r.extract)
	if !ok {
		return nil, false, false
	}
	if accentInsensitive && isASCIIBytes(needle) {
		if off, ascii := asciiCaseIndex(text, needle); ascii {
			if off < 0 {
				return nil, false, false
			}
			return &contentHit{
				Path:    r.docPath(id),
				DocID:   id,
				Offset:  off,
				Snippet: contentSnippetWindow(text, off, len(needle)),
			}, true, false
		}
	}
	var lower []byte
	var offsets []int
	if accentInsensitive {
		lower, offsets = contentFoldAccentWithOffsets(text)
	} else {
		lower, offsets = contentFoldCaseExactWithOffsets(text)
	}
	off := bytes.Index(lower, needle)
	if off < 0 {
		return nil, false, false
	}
	rawOff, rawEnd := contentAccentSpanToRaw(text, offsets, off, off+len(needle))
	return &contentHit{
		Path:    r.docPath(id),
		DocID:   id,
		Offset:  rawOff,
		Snippet: contentSnippetWindow(text, rawOff, rawEnd-rawOff),
	}, true, false
}

// contentDiskVerifyBudget is the per-query disk-verification safety valve used
// when the caller sets no explicit --content-verify-budget. The historical
// default remains 512 reads; a positive --budget is an additional opt-in time
// bound. Past this valve the query stops and reports complete:false rather than
// reading the whole drive.
// Delta hits never count (memory, not disk). A var so tests pin it.
var contentDiskVerifyBudget = 512

// contentDiskVerifyPageUnit and contentDiskVerifyPageScaleMax size the default
// valve to the requested page: one contentDiskVerifyBudget per started unit of
// page slots, up to the scale max. A low-precision superset (regex with common
// literals verifies ~1 hit per 10 reads on Chromium) otherwise fills about
// 50 hits at any limit. Pages up to one unit keep the plain valve, and an
// explicit --content-verify-budget is never scaled. Vars so tests pin them.
var (
	contentDiskVerifyPageUnit     = 32
	contentDiskVerifyPageScaleMax = 16
)

// candidates returns the docIDs that could contain term: trigram intersection
// when the term has at least three bytes, otherwise every document. The result
// is always a superset; verification decides. At most budget docIDs are
// materialized (budget <= 0 means all), so a broad list is not decoded in full.
// §6z aliases expand into the set before the budget trims it.
func (r *contentReader) candidates(term string, budget int) []uint32 {
	ids := collectDocIDStream(r.candidateStream(term), budget)
	ids = r.expandAliasDocIDs(ids)
	if budget > 0 && len(ids) > budget {
		ids = ids[:budget]
	}
	return ids
}

// candidateStream returns a lazy, ascending stream of the docIDs that could
// contain term: the merge-intersection of its trigram lists, or every document
// when the term has no trigram constraint. Each trigram list is pulled block by
// block, so the lists are never held in memory at once.
func (r *contentReader) candidateStream(term string) func() (uint32, bool) {
	return r.candidateStreamIn(term, nil)
}

// candidateStreamIn is candidateStream limited to the shards mask selects
// (see shardMaskUnder); a nil mask, or an unsharded reader, means all docs.
func (r *contentReader) candidateStreamIn(term string, mask []bool) func() (uint32, bool) {
	if r.idx.shards != nil {
		return r.shardedCandidateStream(term, mask)
	}
	if len(term) < 3 || r.grams == nil {
		return r.allDocIDStream()
	}
	streams := make([]func() (uint32, bool), 0, len(term)-2)
	seen := make(map[string]struct{})
	for i := 0; i+3 <= len(term); i++ {
		gram := term[i : i+3]
		if _, ok := seen[gram]; ok {
			continue
		}
		seen[gram] = struct{}{}
		// A pruned gram is no evidence of anything: the builder dropped
		// its postings for size (see contentGramPruneDF), so it constrains
		// nothing and verification decides. Only an indexed-but-absent
		// gram proves the term is in no document.
		if r.prunedGram(gram) {
			continue
		}
		postings, found := r.grams.postings(gram)
		if !found {
			return contentEmptyDocIDStream
		}
		streams = append(streams, func() (uint32, bool) {
			docID, _, ok := postings()
			return docID, ok
		})
	}
	if len(streams) == 0 {
		// Every gram was pruned: no posting constraint remains. Fall back
		// to the full scan like a sub-3-byte term; verification decides.
		// The stream stays ascending, so ordered callers are unaffected.
		return r.allDocIDStream()
	}
	return intersectDocIDStreams(streams)
}

// prunedGram reports whether gram is in the index's pruned set (dropped for
// size at build time). Grams are always 3 bytes at this call site.
func (r *contentReader) prunedGram(gram string) bool {
	if len(r.pruned) == 0 || len(gram) != 3 {
		return false
	}
	_, ok := r.pruned[contentGramU32Of(gram)]
	return ok
}

// allDocIDStream yields every document index in ascending order without
// materializing the slice.
func (r *contentReader) allDocIDStream() func() (uint32, bool) {
	next := uint32(0)
	total := uint32(len(r.idx.Docs))
	return func() (uint32, bool) {
		if next >= total {
			return 0, false
		}
		id := next
		next++
		return id, true
	}
}

// contentEmptyDocIDStream yields nothing.
func contentEmptyDocIDStream() (uint32, bool) { return 0, false }

// unionDocIDStreams lazily k-way unions ascending docID streams into one
// ascending stream of DISTINCT docIDs, holding one cursor per stream rather than
// any stream's full list. Every stream sitting on an emitted docID is advanced
// before the next value, so duplicates collapse. A consumer that stops at a
// budget (the candidate cap) stops pulling every source, so no alternative's
// postings are decoded in full.
func unionDocIDStreams(streams []func() (uint32, bool)) func() (uint32, bool) {
	if len(streams) == 0 {
		return contentEmptyDocIDStream
	}
	cur := make([]uint32, len(streams))
	live := make([]bool, len(streams))
	for i, s := range streams {
		cur[i], live[i] = s()
	}
	return func() (uint32, bool) {
		var minID uint32
		any := false
		for i := range streams {
			if live[i] && (!any || cur[i] < minID) {
				minID = cur[i]
				any = true
			}
		}
		if !any {
			return 0, false
		}
		for i := range streams {
			if live[i] && cur[i] == minID {
				cur[i], live[i] = streams[i]()
			}
		}
		return minID, true
	}
}

// intersectDocIDStreams lazily merge-intersects ascending docID streams, holding
// one cursor per stream rather than the posting lists themselves. A stream that
// has no common doc left stops the intersection.
func intersectDocIDStreams(streams []func() (uint32, bool)) func() (uint32, bool) {
	if len(streams) == 0 {
		return contentEmptyDocIDStream
	}
	cur := make([]uint32, len(streams))
	live := make([]bool, len(streams))
	for i, s := range streams {
		cur[i], live[i] = s()
	}
	return func() (uint32, bool) {
		for {
			var maxID uint32
			for i := range streams {
				if !live[i] {
					return 0, false
				}
				if cur[i] > maxID {
					maxID = cur[i]
				}
			}
			equal := true
			for i := range streams {
				if cur[i] != maxID {
					equal = false
					break
				}
			}
			if equal {
				for i := range streams {
					cur[i], live[i] = streams[i]()
				}
				return maxID, true
			}
			for i := range streams {
				for live[i] && cur[i] < maxID {
					cur[i], live[i] = streams[i]()
				}
			}
		}
	}
}

// collectDocIDStream materializes up to budget docIDs from an ascending stream
// (budget <= 0 means all). Callers that pass a budget never pull the rest of the
// stream, so its blocks are never decoded.
func collectDocIDStream(next func() (uint32, bool), budget int) []uint32 {
	var out []uint32
	for {
		id, ok := next()
		if !ok {
			return out
		}
		out = append(out, id)
		if budget > 0 && len(out) >= budget {
			return out
		}
	}
}

// contentDocCount is a convenience for telemetry and tests.
func (r *contentReader) contentDocCount() int { return len(r.idx.Docs) }

// sortedDocIDs is used by tests and the CLI to enumerate documents.
func (r *contentReader) sortedDocIDs() []uint32 {
	ids := make([]uint32, len(r.idx.Docs))
	for i := range ids {
		ids[i] = uint32(i)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
