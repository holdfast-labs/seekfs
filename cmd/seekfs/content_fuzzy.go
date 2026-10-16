package main

// Content typo tolerance. Filename search forgives typos; `content:` leaves
// were pure substring matches, so `content:recieve` found nothing despite a
// corpus full of `receive`. When a content query returns zero results, this
// file corrects each positive content word independently from the index's term
// dictionary (the CXTR term postings, which are folded words) and retries the
// query with the best corrections.
//
// It never degrades exact results: it only fires on a total miss, and a retry
// is kept only when it returns results. `case:true` queries and regex leaves
// are never corrected, and words shorter than 4 runes are never corrected.
//
// Candidate generation reuses fuzzy.go's machinery (pigeonhole pieces, letter
// bitmaps, bounded Damerau-Levenshtein) so a dictionary with millions of terms
// stays fast: length, piece-substring and bitmap prefilters reject almost every
// term without running the distance DP, and only true candidates allocate.

import (
	"bytes"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// contentFuzzyMinWordRunes is the smallest query word ever corrected. fsearch
// forgives typos in content words of 5+ letters; correcting 4-letter words too
// does better without risk, because a correction only lands when the exact
// query missed everything and the retry matches.
const contentFuzzyMinWordRunes = 4

// contentFuzzyMaxTrials caps the retried searches per zero-result content
// query: single-word trials first (minimal rewrites win), then the combined
// rewrite, then a single-word second-best.
const contentFuzzyMaxTrials = 4

// contentFuzzyCandidate is one dictionary term within edit distance of a query
// word, with its document frequency for ranking.
type contentFuzzyCandidate struct {
	term string
	dist int
	df   int
}

// contentFuzzyLetterMask is fuzzyLetterMask over bytes instead of a string, so
// the dictionary scan never allocates per term. The bit assignment must stay
// identical to fuzzyLetterMask: a-z, 0-9 and ._- get their own bit, everything
// else shares one. It also reports whether the key is pure ASCII, which lets
// the scan skip the rune count (runes == bytes) and run the distance DP on
// bytes directly.
func contentFuzzyLetterMask(b []byte) (uint64, bool) {
	var mask uint64
	ascii := true
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c >= 'a' && c <= 'z':
			mask |= 1 << (c - 'a')
		case c >= '0' && c <= '9':
			mask |= 1 << (26 + c - '0')
		case c == '.':
			mask |= 1 << 36
		case c == '_':
			mask |= 1 << 37
		case c == '-':
			mask |= 1 << 38
		case c < 0x80:
			mask |= 1 << 39
		default:
			mask |= 1 << 39
			ascii = false
		}
	}
	return mask, ascii
}

// damerauLevenshteinBoundedBytes is damerauLevenshteinBounded on bytes for
// pure-ASCII pairs, where bytes and runes coincide. It avoids the two
// allocating rune conversions per candidate on the dictionary scan's hot path.
func damerauLevenshteinBoundedBytes(a, b []byte, max int) int {
	la, lb := len(a), len(b)
	if la-lb > max || lb-la > max {
		return max + 1
	}
	if la == 0 {
		if lb <= max {
			return lb
		}
		return max + 1
	}
	if lb == 0 {
		if la <= max {
			return la
		}
		return max + 1
	}
	prev2 := make([]int, lb+1)
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d := prev[j] + 1
			if v := cur[j-1] + 1; v < d {
				d = v
			}
			if v := prev[j-1] + cost; v < d {
				d = v
			}
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				if t := prev2[j-2] + 1; t < d {
					d = t
				}
			}
			cur[j] = d
			if d < rowMin {
				rowMin = d
			}
		}
		if rowMin > max {
			return max + 1
		}
		prev2, prev, cur = prev, cur, prev2
	}
	if prev[lb] > max {
		return max + 1
	}
	return prev[lb]
}

// contentFuzzyScanIndex adds every term of one posting dictionary within tau
// edits of word to keep (summing document frequencies). The prefilters are all
// sound: the rune-length gate, the pigeonhole piece-substring test and the
// letter-bitmap test can only reject terms farther than tau, so no correction
// within tau is ever missed by them.
// contentFuzzyScanWorkers splits a large dictionary scan across workers; two
// fill the GOMAXPROCS=2 budget gorun.sh grants without oversubscribing.
const contentFuzzyScanWorkers = 2

// contentFuzzyScanCacheEntries bounds the scan memo below: repeated typos
// (interactive refinements, retries, parallel agents) skip the O(dict) scan.
const contentFuzzyScanCacheEntries = 64

// contentFuzzyScanCacheMaxTerms bounds one memo entry: a hostile word matching
// thousands of terms is rescanned rather than hoarded.
const contentFuzzyScanCacheMaxTerms = 2048

// contentFuzzyScanKey identifies one memoized dictionary scan: the dict's
// content generation (unique per decode, so a hit is exact-equivalent to a
// rescan even across address reuse) plus the word and threshold.
type contentFuzzyScanKey struct {
	gen  uint64
	word string
	tau  int
}

// contentFuzzyScanCache memoizes per-dictionary within-tau term sets. A
// dictionary is immutable after decode, so entries never go stale; the
// resident delta vocabulary (mutable) is deliberately not cached and merges
// fresh on every call. Cached maps are never mutated after insert: callers
// merge-summate copies into their own map, and concurrent readers only read.
var contentFuzzyScanCache = struct {
	sync.Mutex
	order []contentFuzzyScanKey
	items map[contentFuzzyScanKey]map[string]int
	hits  atomic.Uint64
}{items: make(map[contentFuzzyScanKey]map[string]int)}

// contentFuzzyScanCacheEnabled is a var so differential tests can run the
// scan memoized and raw and require identical corrections.
var contentFuzzyScanCacheEnabled = true

func contentFuzzyScanCacheLookup(key contentFuzzyScanKey) (map[string]int, bool) {
	if !contentFuzzyScanCacheEnabled {
		return nil, false
	}
	contentFuzzyScanCache.Lock()
	defer contentFuzzyScanCache.Unlock()
	m, ok := contentFuzzyScanCache.items[key]
	if ok {
		contentFuzzyScanCache.hits.Add(1)
	}
	return m, ok
}

func contentFuzzyScanCacheStore(key contentFuzzyScanKey, m map[string]int) {
	if !contentFuzzyScanCacheEnabled || len(m) > contentFuzzyScanCacheMaxTerms {
		return
	}
	contentFuzzyScanCache.Lock()
	defer contentFuzzyScanCache.Unlock()
	if _, dup := contentFuzzyScanCache.items[key]; dup {
		return
	}
	if len(contentFuzzyScanCache.order) >= contentFuzzyScanCacheEntries {
		oldest := contentFuzzyScanCache.order[0]
		contentFuzzyScanCache.order = contentFuzzyScanCache.order[1:]
		delete(contentFuzzyScanCache.items, oldest)
	}
	contentFuzzyScanCache.order = append(contentFuzzyScanCache.order, key)
	contentFuzzyScanCache.items[key] = m
}

// contentFuzzyScanMinEntries is the dictionary size above which the scan
// parallelizes; small corpora (every unit test) stay single-threaded.
const contentFuzzyScanMinEntries = 10000

func contentFuzzyScanIndex(dict *contentTermDict, word []rune, wordBytes []byte, wordASCII bool, wordMask uint64, pieces [][]byte, tau int, keep map[string]int) {
	if dict == nil || dict.keyCount() == 0 {
		return
	}
	// Memoize the per-dictionary scan: it is a pure function of immutable
	// dict content, and the merge into keep is additive, so a hit merges
	// exactly what a rescan would. The memo key carries the dict's content
	// generation, never its address.
	key := contentFuzzyScanKey{gen: dict.gen, word: string(wordBytes), tau: tau}
	if cached, ok := contentFuzzyScanCacheLookup(key); ok {
		for k, v := range cached {
			keep[k] += v
		}
		return
	}
	found := make(map[string]int)
	if dict.keyCount() < contentFuzzyScanMinEntries {
		contentFuzzyScanRange(dict, word, wordBytes, wordASCII, wordMask, pieces, tau, 0, dict.keyCount(), found)
	} else {
		// Each worker fills its own map over a disjoint entry range; the merge
		// sums frequencies, so the result is identical to a serial scan.
		type shardResult struct {
			m map[string]int
		}
		results := make([]shardResult, contentFuzzyScanWorkers)
		var wg sync.WaitGroup
		for w := 0; w < contentFuzzyScanWorkers; w++ {
			lo := w * dict.keyCount() / contentFuzzyScanWorkers
			hi := (w + 1) * dict.keyCount() / contentFuzzyScanWorkers
			results[w].m = make(map[string]int)
			wg.Add(1)
			go func(w, lo, hi int) {
				defer wg.Done()
				contentFuzzyScanRange(dict, word, wordBytes, wordASCII, wordMask, pieces, tau, lo, hi, results[w].m)
			}(w, lo, hi)
		}
		wg.Wait()
		for _, r := range results {
			for k, v := range r.m {
				found[k] += v
			}
		}
	}
	contentFuzzyScanCacheStore(key, found)
	for k, v := range found {
		keep[k] += v
	}
}

func contentFuzzyScanRange(dict *contentTermDict, word []rune, wordBytes []byte, wordASCII bool, wordMask uint64, pieces [][]byte, tau int, lo, hi int, keep map[string]int) {
	if dict == nil {
		return
	}
	loLen, hiLen := len(word)-tau, len(word)+tau
	dict.scanRange(lo, hi, func(key []byte, df int) bool {
		// Byte length is a sound lower-bound prefilter (runes <= bytes), so
		// short keys skip everything below entirely.
		if len(key) < loLen {
			return true
		}
		// The letter bitmap is a single cheap pass that also reports ASCII.
		// It runs before the piece-substring tests (the scan's hottest cost):
		// most keys miss on bits alone, and ASCII keys additionally get
		// their exact rune length for free, so out-of-range lengths reject
		// before any substring search. Every gate below is sound (it only
		// rejects terms provably farther than tau), so the order changes
		// cost, never the kept set.
		keyMask, keyASCII := contentFuzzyLetterMask(key)
		if fuzzyLetterMaskMisses(wordMask, keyMask) > tau {
			return true
		}
		if keyASCII {
			if len(key) > hiLen {
				return true
			}
		} else if n := utf8.RuneCount(key); n < loLen || n > hiLen {
			return true
		}
		matched := len(pieces) == 0
		for _, p := range pieces {
			if bytes.Contains(key, p) {
				matched = true
				break
			}
		}
		if !matched {
			return true
		}
		// Only true candidates reach the DP. A pure-ASCII pair runs it on
		// bytes directly, avoiding both rune conversions.
		var d int
		if keyASCII && wordASCII {
			d = damerauLevenshteinBoundedBytes(key, wordBytes, tau)
		} else {
			d = damerauLevenshteinBounded([]rune(string(key)), word, tau)
		}
		if d < 1 || d > tau {
			return true
		}
		keep[string(key)] += df
		return true
	})
}

// contentFuzzyDictCount binary-searches the sorted dictionary for term,
// returning its document frequency. The restart index positions the search,
// so membership is logarithmic and costs microseconds.
func contentFuzzyDictCount(dict *contentTermDict, term string) (int, bool) {
	if dict == nil || term == "" {
		return 0, false
	}
	return dict.count(term)
}

// contentFuzzyVariantMembership adds dictionary terms reachable from the query
// word by pure transpositions (depth tau) or a single deletion, found by exact
// binary search instead of the piece scan. It closes the piece scan's one
// recall gap: a transposition straddling the piece boundary breaks every piece
// ("widhtcss" recovers "widthcss" only this way), while costing O(len*log n)
// instead of a second full scan. Every added term is within tau by
// construction.
func contentFuzzyVariantMembership(dicts []*contentTermDict, deltaWords map[string]int, word string, tau int, keep map[string]int) {
	wr := []rune(word)
	seen := map[string]struct{}{word: {}}
	add := func(v string) {
		if _, dup := seen[v]; dup {
			return
		}
		seen[v] = struct{}{}
		for _, d := range dicts {
			if c, ok := contentFuzzyDictCount(d, v); ok {
				keep[v] += c
			}
		}
		if c, ok := deltaWords[v]; ok {
			keep[v] += c
		}
	}
	layer := [][]rune{wr}
	for depth := 0; depth < tau; depth++ {
		var next [][]rune
		for _, cur := range layer {
			for i := 0; i+1 < len(cur); i++ {
				swapped := append(append([]rune(nil), cur[:i]...), cur[i+1], cur[i])
				swapped = append(swapped, cur[i+2:]...)
				add(string(swapped))
				next = append(next, swapped)
			}
		}
		layer = next
	}
	for i := range wr {
		add(string(wr[:i]) + string(wr[i+1:]))
	}
}

// contentFuzzyCorrections returns the best corrections for word across every
// reader's term dictionary plus the resident delta vocabulary, ordered by
// edit distance, then document frequency, then term for determinism. At most
// maxN are returned. Readers cover sharded indexes (each shard's dictionary is
// scanned) and multiple volumes (frequencies merge by summation).
func contentFuzzyCorrections(readers []*contentReader, deltaWords map[string]int, word string, maxN int) []contentFuzzyCandidate {
	if maxN <= 0 || utf8.RuneCountInString(word) < contentFuzzyMinWordRunes {
		return nil
	}
	wr := []rune(word)
	wb := []byte(word)
	wordASCII := true
	for _, c := range wb {
		if c >= 0x80 {
			wordASCII = false
			break
		}
	}
	tau := fuzzyThresholdForTerm(word)
	wordMask := fuzzyLetterMask(word)
	strPieces := fuzzyPigeonholePieces(word, tau)
	pieces := make([][]byte, 0, len(strPieces))
	for _, p := range strPieces {
		pieces = append(pieces, []byte(p))
	}
	merged := make(map[string]int)
	var dicts []*contentTermDict
	for _, r := range readers {
		if r == nil || r.idx == nil {
			continue
		}
		if len(r.idx.shards) > 0 {
			for _, sh := range r.idx.shards {
				if sh == nil || sh.reader == nil {
					continue
				}
				contentFuzzyScanIndex(sh.reader.terms, wr, wb, wordASCII, wordMask, pieces, tau, merged)
				dicts = append(dicts, sh.reader.terms)
			}
			continue
		}
		contentFuzzyScanIndex(r.terms, wr, wb, wordASCII, wordMask, pieces, tau, merged)
		dicts = append(dicts, r.terms)
	}
	contentFuzzyVariantMembership(dicts, deltaWords, word, tau, merged)
	// The resident delta is newer than the base dictionaries; its vocabulary
	// corrects typos of words that only exist in unflushed changes.
	for dw, df := range deltaWords {
		if dw == word {
			continue
		}
		dr := []rune(dw)
		if len(dr) < len(wr)-tau || len(dr) > len(wr)+tau {
			continue
		}
		if d := damerauLevenshteinBounded(dr, wr, tau); d >= 1 && d <= tau {
			merged[dw] += df
		}
	}
	delete(merged, word)
	out := make([]contentFuzzyCandidate, 0, len(merged))
	for term := range merged {
		d := damerauLevenshteinBounded([]rune(term), wr, tau)
		if d >= 1 && d <= tau {
			out = append(out, contentFuzzyCandidate{term: term, dist: d, df: merged[term]})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].dist != out[j].dist {
			return out[i].dist < out[j].dist
		}
		if out[i].df != out[j].df {
			return out[i].df > out[j].df
		}
		return out[i].term < out[j].term
	})
	if len(out) > maxN {
		out = out[:maxN]
	}
	return out
}

// contentFuzzyReaders collects the content readers of every volume for
// dictionary scans. A volume with only a resident delta contributes no reader;
// its vocabulary arrives through contentFuzzyDeltaWords.
func contentFuzzyReaders(volumes []*serviceVolumeIndex) []*contentReader {
	var out []*contentReader
	for _, vol := range volumes {
		if vol == nil || vol.content == nil {
			continue
		}
		if r, _ := vol.content.readerResolverView(); r != nil {
			out = append(out, r)
		}
	}
	return out
}

// contentFuzzyDeltaWords tokenizes every live resident delta document into a
// word -> frequency map, so typos of words that only exist in unflushed
// changes still correct. Tombstoned documents are excluded by live().
func contentFuzzyDeltaWords(volumes []*serviceVolumeIndex) map[string]int {
	var out map[string]int
	for _, vol := range volumes {
		if vol == nil || vol.content == nil {
			continue
		}
		d := vol.content.deltaView()
		if d == nil {
			continue
		}
		for _, doc := range d.live() {
			for w, c := range contentTermsOf(doc.Text) {
				if utf8.RuneCountInString(w) < contentFuzzyMinWordRunes {
					continue
				}
				if out == nil {
					out = make(map[string]int)
				}
				out[w] += int(c)
			}
		}
	}
	return out
}

// contentFuzzyIsWordByte mirrors contentTermsOf's word rule: ASCII
// alphanumerics, underscore, and any high byte (so a UTF-8 word stays whole).
func contentFuzzyIsWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b >= 0x80
}

// contentFuzzyCorrectableWords returns the unique correctable words of folded
// leaf text in first-seen order: word runs of at least contentFuzzyMinWordRunes
// runes.
func contentFuzzyCorrectableWords(folded string) []string {
	var out []string
	seen := make(map[string]struct{})
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		w := folded[start:end]
		start = -1
		if utf8.RuneCountInString(w) < contentFuzzyMinWordRunes {
			return
		}
		if _, dup := seen[w]; !dup {
			seen[w] = struct{}{}
			out = append(out, w)
		}
	}
	for i := 0; i <= len(folded); i++ {
		if i < len(folded) && contentFuzzyIsWordByte(folded[i]) {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
	}
	return out
}

// contentFuzzyApplyCorrection splices corrections back into folded leaf text,
// replacing exactly the word spans present in corr and preserving every
// separator, so a phrase keeps its spaces and a term keeps its punctuation.
func contentFuzzyApplyCorrection(folded string, corr map[string]string) string {
	if len(corr) == 0 {
		return folded
	}
	var b strings.Builder
	b.Grow(len(folded))
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		w := folded[start:end]
		start = -1
		if c, ok := corr[w]; ok {
			b.WriteString(c)
		} else {
			b.WriteString(w)
		}
	}
	for i := 0; i <= len(folded); i++ {
		if i < len(folded) && contentFuzzyIsWordByte(folded[i]) {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
		if i < len(folded) {
			b.WriteByte(folded[i])
		}
	}
	return b.String()
}

// contentFuzzyLeafBase folds a term/phrase leaf into the retry base: a
// trailing "~" (the filename fuzzy marker) is dropped, then the shared content
// fold applies. Verification folds the query side identically, so retrying with
// the folded text is exact-equivalent to retrying with the raw text.
func contentFuzzyLeafBase(text string) string {
	text = strings.TrimSuffix(text, "~")
	return string(contentFoldText([]byte(text)))
}

// contentFuzzyRewriteLeaves applies fn to every positive content leaf: the
// top-level conjuncts and the leaves nested in OR groups. NOT groups are
// exclusion and are never rewritten.
func contentFuzzyRewriteLeaves(pq *parsedQuery, fn func(*contentLeaf)) {
	for i := range pq.Content {
		fn(&pq.Content[i])
	}
	for g := range pq.OrGroups {
		for a := range pq.OrGroups[g] {
			contentFuzzyRewriteLeaves(&pq.OrGroups[g][a], fn)
		}
	}
}

// contentFuzzyExpandQuery replaces each explicit fuzzy leaf with an OR over
// indexed vocabulary terms. Variants are ranked by document frequency.
// contentFuzzyGrepDistance is a content~ leaf's edit distance: explicit
// content~N, else by word length (0 below 4 runes, 1 for 4-7, 2 for 8+).
func contentFuzzyGrepDistance(leaf contentLeaf) int {
	if leaf.FuzzyDistance >= 0 {
		return leaf.FuzzyDistance
	}
	switch n := utf8.RuneCountInString(leaf.Text); {
	case n < 4:
		return 0
	case n < 8:
		return 1
	default:
		return 2
	}
}

func contentFuzzyExpandQuery(volumes []*serviceVolumeIndex, opts queryOptions) (queryOptions, bool) {
	pq, err := parseQuery(opts)
	if err != nil {
		return opts, false
	}
	readers, delta := contentFuzzyReaders(volumes), contentFuzzyDeltaWords(volumes)
	var impossible bool
	hasFuzzy := false
	var expand func(*parsedQuery)
	expand = func(q *parsedQuery) {
		var keepContent []contentLeaf
		for _, leaf := range q.Content {
			if !leaf.Fuzzy {
				keepContent = append(keepContent, leaf)
				continue
			}
			hasFuzzy = true
			tau := contentFuzzyGrepDistance(leaf)
			word := string(contentFoldText([]byte(leaf.Text)))
			variants := make(map[string]int)
			dicts := make([]*contentTermDict, 0)
			for _, r := range readers {
				if r == nil || r.idx == nil {
					continue
				}
				if len(r.idx.shards) > 0 {
					for _, sh := range r.idx.shards {
						if sh != nil && sh.reader != nil {
							dicts = append(dicts, sh.reader.terms)
						}
					}
				} else {
					dicts = append(dicts, r.terms)
				}
			}
			wr, wb := []rune(word), []byte(word)
			if tau > 0 {
				mask, ascii := fuzzyLetterMask(word), true
				for _, b := range wb {
					if b >= 0x80 {
						ascii = false
						break
					}
				}
				sp := fuzzyPigeonholePieces(word, tau)
				pieces := make([][]byte, 0, len(sp))
				for _, p := range sp {
					pieces = append(pieces, []byte(p))
				}
				for _, d := range dicts {
					contentFuzzyScanIndex(d, wr, wb, ascii, mask, pieces, tau, variants)
				}
				contentFuzzyVariantMembership(dicts, nil, word, tau, variants)
			}
			for dw, df := range delta {
				if dw == word {
					continue
				}
				dr := []rune(dw)
				if len(dr) >= len(wr)-tau && len(dr) <= len(wr)+tau && damerauLevenshteinBounded(dr, wr, tau) <= tau {
					variants[dw] += df
				}
			}
			if c := delta[word]; c > 0 {
				variants[word] += c
			}
			// Include exact dictionary terms even though the variant helper intentionally excludes its seed.
			for _, d := range dicts {
				if c, ok := contentFuzzyDictCount(d, word); ok {
					variants[word] += c
				}
			}
			for term := range variants {
				if damerauLevenshteinBounded([]rune(term), wr, tau) > tau {
					delete(variants, term)
				}
			}
			terms := make([]contentFuzzyCandidate, 0, len(variants))
			for term, df := range variants {
				terms = append(terms, contentFuzzyCandidate{term: term, df: df})
			}
			sort.Slice(terms, func(i, j int) bool {
				if terms[i].df != terms[j].df {
					return terms[i].df > terms[j].df
				}
				return terms[i].term < terms[j].term
			})
			if len(terms) > 64 {
				terms = terms[:64]
			}
			if len(terms) == 0 {
				impossible = true
				continue
			}
			group := make([]parsedQuery, 0, len(terms))
			for _, term := range terms {
				group = append(group, parsedQuery{Content: []contentLeaf{{Kind: contentLeafTerm, Text: term.term, WholeWord: true}}})
			}
			q.OrGroups = append(q.OrGroups, group)
		}
		q.Content = keepContent
		for i := range q.OrGroups {
			for j := range q.OrGroups[i] {
				expand(&q.OrGroups[i][j])
			}
		}
		for i := range q.NotGroups {
			expand(&q.NotGroups[i])
		}
	}
	expand(&pq)
	if !hasFuzzy {
		return opts, true
	}
	if impossible {
		pq.Impossible = true
	}
	// The variant leaves are new: give every leaf a distinct id, since the
	// matcher keys needles and match flags by LeafID.
	contentAssignLeafIDs(&pq)
	opts.parsedOverride = &pq
	return opts, true
}

// contentFuzzyRewrite is one retried query tree plus the corrected words to
// display when it wins.
type contentFuzzyRewrite struct {
	tree *parsedQuery
	show string
}

// contentFuzzyRewriteTree clones pq and splices sub into every positive
// term/phrase leaf.
func contentFuzzyRewriteTree(pq parsedQuery, sub map[string]string) *parsedQuery {
	next := cloneParsedQuery(pq)
	contentFuzzyRewriteLeaves(&next, func(leaf *contentLeaf) {
		if leaf.Kind == contentLeafRegex {
			return
		}
		leaf.Text = contentFuzzyApplyCorrection(contentFuzzyLeafBase(leaf.Text), sub)
	})
	return &next
}

// contentFuzzyRewriteTrials builds the retried query trees for a zero-result
// content query, cheapest-first: single-word rewrites (minimal changes win),
// then the combined rewrite, then a single-word second-best. It returns nil
// when the query is ineligible (a case-sensitive query, no positive content
// word, no dictionary vocabulary, or no correction found).
func contentFuzzyRewriteTrials(volumes []*serviceVolumeIndex, opts queryOptions) ([]contentFuzzyRewrite, bool) {
	if !contentSearchEnabled() || opts.parsedOverride != nil {
		return nil, false
	}
	pq, err := parseQuery(opts)
	if err != nil || pq.contentCaseSensitive() || !queryHasPositiveContentLeaf(pq) {
		return nil, false
	}
	var words []string
	seen := make(map[string]struct{})
	for _, leaf := range contentPositiveLeaves(pq) {
		if leaf.Kind == contentLeafRegex {
			continue
		}
		for _, w := range contentFuzzyCorrectableWords(contentFuzzyLeafBase(leaf.Text)) {
			if _, dup := seen[w]; !dup {
				seen[w] = struct{}{}
				words = append(words, w)
			}
		}
	}
	if len(words) == 0 {
		return nil, false
	}
	readers := contentFuzzyReaders(volumes)
	deltaWords := contentFuzzyDeltaWords(volumes)
	if len(readers) == 0 && len(deltaWords) == 0 {
		return nil, false
	}
	best := make(map[string][]contentFuzzyCandidate, len(words))
	for _, w := range words {
		if c := contentFuzzyCorrections(readers, deltaWords, w, 2); len(c) > 0 {
			best[w] = c
		}
	}
	if len(best) == 0 {
		return nil, false
	}
	var rewrites []contentFuzzyRewrite
	singleCap := len(words)
	if singleCap > 3 {
		singleCap = 3
	}
	for _, w := range words[:singleCap] {
		c, ok := best[w]
		if !ok {
			continue
		}
		rewrites = append(rewrites, contentFuzzyRewrite{
			tree: contentFuzzyRewriteTree(pq, map[string]string{w: c[0].term}),
			show: c[0].term,
		})
		if len(rewrites) >= contentFuzzyMaxTrials {
			break
		}
	}
	if len(best) > 1 && len(rewrites) < contentFuzzyMaxTrials {
		sub := make(map[string]string, len(best))
		var show []string
		for _, w := range words {
			if c, ok := best[w]; ok {
				sub[w] = c[0].term
				show = append(show, c[0].term)
			}
		}
		rewrites = append(rewrites, contentFuzzyRewrite{
			tree: contentFuzzyRewriteTree(pq, sub),
			show: strings.Join(show, " "),
		})
	}
	if len(best) == 1 && len(rewrites) < contentFuzzyMaxTrials {
		for w, c := range best {
			if len(c) < 2 {
				break
			}
			rewrites = append(rewrites, contentFuzzyRewrite{
				tree: contentFuzzyRewriteTree(pq, map[string]string{w: c[1].term}),
				show: c[1].term,
			})
			break
		}
	}
	return rewrites, true
}

// tryContentFuzzyRewrite corrects typos in positive content words after the
// exact query returned nothing. It returns the retried matches, the corrected
// words for display ("receive"), and true when a retry matched; the first
// trial that returns results wins.
func tryContentFuzzyRewrite(volumes []*serviceVolumeIndex, opts queryOptions, matches []Entry) ([]Entry, string, bool) {
	if len(matches) > 0 {
		return matches, "", false
	}
	rewrites, ok := contentFuzzyRewriteTrials(volumes, opts)
	if !ok {
		return matches, "", false
	}
	for _, rw := range rewrites {
		ropts := opts
		ropts.parsedOverride = rw.tree
		m, err := searchServiceVolumes(volumes, ropts, false)
		if err == nil && len(m) > 0 {
			serviceLog("contentfuzzy rewrote q=%q correction=%q results=%d", opts.Query, rw.show, len(m))
			return m, rw.show, true
		}
	}
	return matches, "", false
}

// countContentFuzzyRewrite mirrors tryContentFuzzyRewrite for a content count:
// when the exact count is zero it retries the fuzzy rewrites through the same
// per-volume tally path, returning the first nonzero total plus the correction
// shown. ok=false means no retry applied (exact total stands); an error refuses
// the count rather than reporting the exact zero as truth.
func countContentFuzzyRewrite(volumes []*serviceVolumeIndex, opts queryOptions, usable []*serviceVolumeIndex) (int, string, bool, error) {
	rewrites, ok := contentFuzzyRewriteTrials(usable, opts)
	if !ok {
		return 0, "", false, nil
	}
	// The gate is a total miss: count the exact query first through the same
	// path a normal content count uses.
	total := 0
	for _, vol := range volumes {
		if queryCanceled(parsedQuery{DeadlineUnix: opts.DeadlineUnix, Cancel: opts.Cancel}) {
			return 0, "", false, errQueryCanceled
		}
		var n int
		var err error
		if vol != nil && vol.contentUsableForQuery() {
			n, err = vol.countContentVolume(opts)
		} else if pqr, perr := parseQuery(opts); perr == nil && filenameAnswerable(pqr) && vol != nil {
			n, err = vol.countFilenameOnlyVolume(opts, pqr)
		} else {
			continue
		}
		if err != nil {
			return 0, "", false, err
		}
		total += n
	}
	if total > 0 {
		return 0, "", false, nil
	}
	for _, rw := range rewrites {
		ropts := opts
		ropts.parsedOverride = rw.tree
		n := 0
		failed := false
		for _, vol := range volumes {
			if queryCanceled(parsedQuery{DeadlineUnix: opts.DeadlineUnix, Cancel: opts.Cancel}) {
				return 0, "", false, errQueryCanceled
			}
			var m int
			var err error
			if vol != nil && vol.contentUsableForQuery() {
				m, err = vol.countContentVolume(ropts)
			} else {
				continue
			}
			if err != nil {
				failed = true
				break
			}
			n += m
		}
		if !failed && n > 0 {
			serviceLog("contentfuzzy count rewrote q=%q correction=%q results=%d", opts.Query, rw.show, n)
			return n, rw.show, true, nil
		}
	}
	return 0, "", false, nil
}

// contentFuzzyCorrectOfflineText corrects every correctable word of an offline
// query string to its best dictionary term, for the `content -db` command. It
// returns the corrected text and the corrected words for display.
func contentFuzzyCorrectOfflineText(r *contentReader, text string) (string, string, bool) {
	folded := string(contentFoldText([]byte(text)))
	words := contentFuzzyCorrectableWords(folded)
	if len(words) == 0 {
		return "", "", false
	}
	sub := make(map[string]string, len(words))
	var show []string
	for _, w := range words {
		if c := contentFuzzyCorrections([]*contentReader{r}, nil, w, 1); len(c) > 0 {
			sub[w] = c[0].term
			show = append(show, c[0].term)
		}
	}
	if len(sub) == 0 {
		return "", "", false
	}
	return contentFuzzyApplyCorrection(folded, sub), strings.Join(show, " "), true
}
