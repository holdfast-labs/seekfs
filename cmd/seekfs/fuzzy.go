package main

import (
	"math/bits"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Fuzzy matching fills only the shortfall left by exact search: it runs after
// the normal planner underfills the result limit, and its results always sort
// below exact-tier results. See docs/FUZZY_RANKED_SEARCH_PLAN.md.

const (
	fuzzyMinTermRunes  = 3
	fuzzyMaxDistance   = 2
	fuzzyCandidateCap  = serviceNameTrigramCandidateMaxIDs
	fuzzyCollectFactor = 16
	// Terms this long split into pigeonhole pieces of 5+ runes, selective
	// enough to replace depth-2 deletion variants.
	fuzzyLongTermRunes = 15
)

// fuzzyThresholdForTerm picks the allowed Damerau-Levenshtein distance for a
// term. Thresholds stay conservative because sliding-window verification over
// long names multiplies candidate chances: distance 2 only opens up for long
// terms where typos are more likely to be multiple and distinct.
func fuzzyThresholdForTerm(term string) int {
	if utf8.RuneCountInString(term) < 8 {
		return 1
	}
	return 2
}

// damerauLevenshteinBounded returns the optimal-string-alignment distance
// between a and b, capped at max. It returns max+1 as soon as the distance is
// known to exceed max so callers can skip cheaply.
func damerauLevenshteinBounded(a, b []rune, max int) int {
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

// damerauLevenshteinBoundedReuse is damerauLevenshteinBounded over pre-split
// runes with reusable DP rows, so per-candidate whole-name checks allocate
// nothing after the scratch warms up.
func damerauLevenshteinBoundedReuse(a, b []rune, max int, sc *fuzzyDistanceScratch) int {
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
	sc.ensureDamerau(lb + 1)
	prev2, prev, cur := sc.dPrev2, sc.dPrev, sc.dCur
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

// fuzzyDeletionVariants returns the term plus every string reachable by up to
// depth edits restricted to single-character deletions and adjacent
// transpositions (deduplicated). Deletion chains cover substitutions,
// insertions, and deletions per the SymSpell argument; explicit transposition
// variants cover Damerau swaps, which pure deletion neighborhoods miss.
func fuzzyDeletionVariants(term string, depth int) []string {
	set := map[string]struct{}{term: {}}
	layer := []string{term}
	for d := 0; d < depth; d++ {
		next := make([]string, 0, len(layer)*6)
		for _, s := range layer {
			runes := []rune(s)
			for i := 0; i < len(runes); i++ {
				variant := string(runes[:i]) + string(runes[i+1:])
				if _, ok := set[variant]; !ok {
					set[variant] = struct{}{}
					next = append(next, variant)
				}
				if i > 0 {
					swappedRunes := []rune(s)
					swappedRunes[i-1], swappedRunes[i] = swappedRunes[i], swappedRunes[i-1]
					variant := string(swappedRunes)
					if _, ok := set[variant]; !ok {
						set[variant] = struct{}{}
						next = append(next, variant)
					}
				}
			}
		}
		layer = next
	}
	out := make([]string, 0, len(set))
	out = append(out, term)
	for variant := range set {
		if variant != term {
			out = append(out, variant)
		}
	}
	return out
}

// fuzzyPigeonholePieces splits term into tau+1 contiguous pieces: a name
// within tau edits of the term must contain at least one piece verbatim, so
// piece postings recover substitutions and omissions in the middle of the
// term, which query-side deletion variants cannot reach ("relese" never
// appears inside "release"). Pieces shorter than a trigram have no posting
// and are dropped; when tau+1 pieces would be too short, halves still cover
// every single-edit typo.
func fuzzyPigeonholePieces(term string, tau int) []string {
	runes := []rune(term)
	parts := tau + 1
	for parts > 2 && len(runes)/parts < 3 {
		parts--
	}
	out := make([]string, 0, parts)
	dropped := false
	for i := 0; i < parts; i++ {
		piece := runes[i*len(runes)/parts : (i+1)*len(runes)/parts]
		if len(piece) >= 3 {
			out = append(out, string(piece))
		} else {
			dropped = true
		}
	}
	if dropped {
		// A dropped short piece voids the pigeonhole guarantee ("reprt"
		// splits into "re" and "prt"; the omission breaks "prt"). Every
		// trigram of the term restores coverage for edits near either end.
		seen := make(map[string]struct{}, len(out)+len(runes))
		for _, piece := range out {
			seen[piece] = struct{}{}
		}
		for i := 0; i+3 <= len(runes); i++ {
			gram := string(runes[i : i+3])
			if _, dup := seen[gram]; !dup {
				seen[gram] = struct{}{}
				out = append(out, gram)
			}
		}
	}
	return out
}

// fuzzyLetterMask records which characters occur in s: a-z, 0-9 and the
// common filename punctuation get their own bit, everything else shares one.
func fuzzyLetterMask(s string) uint64 {
	var mask uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
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
		default:
			mask |= 1 << 39
		}
	}
	return mask
}

// fuzzyLetterMaskMisses counts term characters absent from the name. Each
// edit removes at most one distinct term character, so a name missing more
// than tau of them cannot be within tau edits of any window and is rejected
// without running the distance DP.
func fuzzyLetterMaskMisses(termMask, nameMask uint64) int {
	return bits.OnesCount64(termMask &^ nameMask)
}

// fuzzyLetterMaskASCII is fuzzyLetterMask fused with the ASCII test: the mask
// loop already visits every byte, so it reports whether any byte is non-ASCII
// for free. Hot callers use this to skip the separate asciiOnlyString scan.
func fuzzyLetterMaskASCII(s string) (mask uint64, ascii bool) {
	ascii = true
	for i := 0; i < len(s); i++ {
		c := s[i]
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

// foldFuzzyText normalizes text for fuzzy comparison: fullwidth forms fold to
// their ASCII equivalents and combining marks are stripped so "sodanco"
// matches "Só Danço". Comparison-side only; index data is untouched.
func foldFuzzyText(s string) string {
	if asciiOnlyString(s) {
		return s // nothing to fold; skips NFD on every candidate name
	}
	decomposed := norm.NFD.String(s)
	var b strings.Builder
	b.Grow(len(decomposed))
	for _, r := range decomposed {
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		} else if r == 0x3000 {
			r = ' '
		}
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// fuzzyDistanceScratch holds reusable buffers for fuzzy distance
// computation across the candidates of one query. The per-candidate DP rows
// and rune decode buffer are the dominant allocation source in fuzzy search
// (tens of thousands of candidates per typo query), so hot callers thread one
// scratch through every candidate instead of allocating per candidate.
type fuzzyDistanceScratch struct {
	wPrev2, wPrev, wCur    []int
	wPrev2S, wPrevS, wCurS []int
	dPrev2, dPrev, dCur    []int
	nameRunes              []rune
	nameRunes2             []rune
	asciiLower             []byte
}

func (sc *fuzzyDistanceScratch) ensureWindow(n int) {
	if cap(sc.wPrev) < n {
		size := max(n, 2*cap(sc.wPrev))
		sc.wPrev2 = make([]int, size)
		sc.wPrev = make([]int, size)
		sc.wCur = make([]int, size)
		sc.wPrev2S = make([]int, size)
		sc.wPrevS = make([]int, size)
		sc.wCurS = make([]int, size)
		return
	}
	sc.wPrev2 = sc.wPrev2[:n]
	sc.wPrev = sc.wPrev[:n]
	sc.wCur = sc.wCur[:n]
	sc.wPrev2S = sc.wPrev2S[:n]
	sc.wPrevS = sc.wPrevS[:n]
	sc.wCurS = sc.wCurS[:n]
}

func (sc *fuzzyDistanceScratch) ensureDamerau(n int) {
	if cap(sc.dPrev) < n {
		size := max(n, 2*cap(sc.dPrev))
		sc.dPrev2 = make([]int, size)
		sc.dPrev = make([]int, size)
		sc.dCur = make([]int, size)
		return
	}
	sc.dPrev2 = sc.dPrev2[:n]
	sc.dPrev = sc.dPrev[:n]
	sc.dCur = sc.dCur[:n]
}

// lowerNameASCIITo lowercases ASCII bytes in place into the scratch buffer.
// Callers must have verified every byte is ASCII.
func (sc *fuzzyDistanceScratch) lowerNameASCIITo(s string) []byte {
	if cap(sc.asciiLower) < len(s) {
		sc.asciiLower = make([]byte, len(s))
	} else {
		sc.asciiLower = sc.asciiLower[:len(s)]
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		sc.asciiLower[i] = c
	}
	return sc.asciiLower
}

// fuzzyNameDistance finds the best (lowest) bounded Damerau-Levenshtein
// distance between the term and any window of the lowercased name whose rune
// length is within tau of the term length. prefix reports whether the best
// alignment starts at the beginning of the name. ok is false when even the
// best window exceeds tau.
//
// It runs one semi-global optimal-string-alignment DP (free start and end in
// the name) instead of a bounded DP per window, so cost is O(name x term)
// rather than O(name x term^2) per candidate. Each cell carries the window
// start of its cheapest path, ties going to the earliest start, so
// windowPos is the first start among best windows.
func fuzzyNameDistance(nameLower, term string, tau int) (dist int, prefix bool, windowPos int, ok bool) {
	var sc fuzzyDistanceScratch
	return fuzzyNameDistanceScratch(nameLower, []rune(term), asciiOnlyString(term), tau, &sc)
}

// fuzzyNameDistanceScratch is fuzzyNameDistance with caller-provided term
// runes (hoisted per query) and reusable buffers, so per-candidate cost is
// compute only. termASCII must report whether term holds only ASCII bytes;
// the ASCII fast path then runs the DP over bytes with no rune decoding.
// A name shorter than len(termRunes)-tau cannot hold a qualifying window and
// is declined before any DP work.
func fuzzyNameDistanceScratch(nameLower string, termRunes []rune, termASCII bool, tau int, sc *fuzzyDistanceScratch) (dist int, prefix bool, windowPos int, ok bool) {
	_, nameASCII := fuzzyLetterMaskASCII(nameLower)
	return fuzzyNameDistanceScratchKnown(nameLower, termRunes, termASCII, nameASCII, tau, sc)
}

// fuzzyNameDistanceScratchKnown is fuzzyNameDistanceScratch with both ASCII
// flags caller-provided, so the per-candidate scan inside the mask computation
// doubles as the ASCII test and no second scan is needed.
func fuzzyNameDistanceScratchKnown(nameLower string, termRunes []rune, termASCII, nameASCII bool, tau int, sc *fuzzyDistanceScratch) (dist int, prefix bool, windowPos int, ok bool) {
	lt := len(termRunes)
	if nameASCII {
		if termASCII {
			if len(nameLower) == 0 {
				return tau + 1, false, 0, false
			}
			if len(nameLower) < lt-tau {
				return tau + 1, false, len(nameLower), false
			}
			// Keep the decoded runes for the caller's whole-name check.
			nameRunes := sc.nameRunes[:0]
			for i := 0; i < len(nameLower); i++ {
				nameRunes = append(nameRunes, rune(nameLower[i]))
			}
			sc.nameRunes = nameRunes
			return fuzzyNameDistanceBytes(nameLower, termRunes, tau, sc)
		}
		nameRunes := sc.nameRunes[:0]
		for i := 0; i < len(nameLower); i++ {
			nameRunes = append(nameRunes, rune(nameLower[i]))
		}
		sc.nameRunes = nameRunes
		if len(nameRunes) == 0 {
			return tau + 1, false, 0, false
		}
		if len(nameRunes) < lt-tau {
			return tau + 1, false, len(nameRunes), false
		}
		return fuzzyNameDistanceRunes(nameRunes, termRunes, tau, sc)
	}
	nameRunes := sc.nameRunes[:0]
	for _, r := range nameLower {
		nameRunes = append(nameRunes, r)
	}
	sc.nameRunes = nameRunes
	if len(nameRunes) == 0 {
		return tau + 1, false, 0, false
	}
	if len(nameRunes) < lt-tau {
		return tau + 1, false, len(nameRunes), false
	}
	return fuzzyNameDistanceRunes(nameRunes, termRunes, tau, sc)
}

// fuzzyNameDistanceBytes runs the semi-global DP over ASCII bytes. t carries
// the term as runes (equal to its bytes on the ASCII path).
func fuzzyNameDistanceBytes(name string, t []rune, tau int, sc *fuzzyDistanceScratch) (dist int, prefix bool, windowPos int, ok bool) {
	ln := len(name)
	sc.ensureWindow(ln + 1)
	prev2, prev, cur := sc.wPrev2, sc.wPrev, sc.wCur
	prev2S, prevS, curS := sc.wPrev2S, sc.wPrevS, sc.wCurS
	// Row 0 aligns the empty term at zero cost (free start); reused rows
	// must be reset because every later row reads row 0. prevS is fully
	// rewritten below; prev2 is unread until row 2, by which time it holds
	// row 0 via rotation.
	for j := 0; j <= ln; j++ {
		prev[j] = 0
		prevS[j] = j
	}
	lastJ0 := 1
	for i := 1; i <= len(t); i++ {
		cur[0], curS[0] = i, 0
		rowMin := cur[0]
		ti := byte(t[i-1])
		// Triangular band: aligning t[:i] to a window ending at j needs a
		// window of length within tau of i, so columns j < i-tau can never
		// reach tau and are skipped. Their only in-band reader is the left
		// neighbor of the band edge, which sees INF instead (its true cost
		// provably exceeds tau). Values above tau are exact-threshold only;
		// callers ignore them on decline. Columns below the band keep stale
		// rotated values, so the best scan below stays inside the band too.
		j0 := i - tau
		if j0 <= 1 {
			j0 = 1
		} else {
			// Out-of-band left neighbor: unread anywhere else (the next
			// row's band starts one column later), so the scribble is
			// safe. j0 == 1 keeps cur[0] == i, the exact empty-window
			// cost the band edge legitimately reads.
			cur[j0-1] = fuzzyBandINF
		}
		lastJ0 = j0
		for j := j0; j <= ln; j++ {
			cost := 1
			if ti == name[j-1] {
				cost = 0
			}
			d, s := prev[j-1]+cost, prevS[j-1]
			if v := prev[j] + 1; v < d || (v == d && prevS[j] < s) {
				d, s = v, prevS[j]
			}
			if v := cur[j-1] + 1; v < d || (v == d && curS[j-1] < s) {
				d, s = v, curS[j-1]
			}
			if i > 1 && j > 1 && ti == name[j-2] && t[i-2] == rune(name[j-1]) {
				if v := prev2[j-2] + 1; v < d || (v == d && prev2S[j-2] < s) {
					d, s = v, prev2S[j-2]
				}
			}
			cur[j], curS[j] = d, s
			if d < rowMin {
				rowMin = d
			}
		}
		if rowMin > tau {
			return tau + 1, false, ln, false
		}
		prev2, prev, cur = prev, cur, prev2
		prev2S, prevS, curS = prevS, curS, prev2S
	}
	// Columns below the last band hold stale rotated values; their true
	// cost exceeds tau, so the best window (when one reaches tau) always
	// ends inside the band.
	best, bestStart := tau+1, ln
	for j := lastJ0; j <= ln; j++ {
		if prev[j] < best || (prev[j] == best && prevS[j] < bestStart) {
			best, bestStart = prev[j], prevS[j]
		}
	}
	if best > tau {
		return best, false, bestStart, false
	}
	return best, bestStart == 0, bestStart, true
}

// fuzzyBandINF stands in for out-of-band cells, whose true cost provably
// exceeds any tau callers pass (small ints keep +1 arithmetic exact).
const fuzzyBandINF = 1 << 30

// fuzzyNameDistanceRunes runs the semi-global DP over rune slices with
// scratch rows.
func fuzzyNameDistanceRunes(name, t []rune, tau int, sc *fuzzyDistanceScratch) (dist int, prefix bool, windowPos int, ok bool) {
	ln := len(name)
	sc.ensureWindow(ln + 1)
	prev2, prev, cur := sc.wPrev2, sc.wPrev, sc.wCur
	prev2S, prevS, curS := sc.wPrev2S, sc.wPrevS, sc.wCurS
	// Row 0 aligns the empty term at zero cost (free start); see the byte
	// path for why reused rows need the reset.
	for j := 0; j <= ln; j++ {
		prev[j] = 0
		prevS[j] = j
	}
	lastJ0 := 1
	for i := 1; i <= len(t); i++ {
		cur[0], curS[0] = i, 0
		rowMin := cur[0]
		// Same triangular band as the byte path: columns j < i-tau can
		// never reach tau, so they are skipped with an INF left edge.
		j0 := i - tau
		if j0 <= 1 {
			j0 = 1
		} else {
			cur[j0-1] = fuzzyBandINF
		}
		lastJ0 = j0
		for j := j0; j <= ln; j++ {
			cost := 1
			if t[i-1] == name[j-1] {
				cost = 0
			}
			d, s := prev[j-1]+cost, prevS[j-1]
			if v := prev[j] + 1; v < d || (v == d && prevS[j] < s) {
				d, s = v, prevS[j]
			}
			if v := cur[j-1] + 1; v < d || (v == d && curS[j-1] < s) {
				d, s = v, curS[j-1]
			}
			if i > 1 && j > 1 && t[i-1] == name[j-2] && t[i-2] == name[j-1] {
				if v := prev2[j-2] + 1; v < d || (v == d && prev2S[j-2] < s) {
					d, s = v, prev2S[j-2]
				}
			}
			cur[j], curS[j] = d, s
			if d < rowMin {
				rowMin = d
			}
		}
		if rowMin > tau {
			return tau + 1, false, ln, false
		}
		prev2, prev, cur = prev, cur, prev2
		prev2S, prevS, curS = prevS, curS, prev2S
	}
	// Columns below the last band hold stale rotated values; their true
	// cost exceeds tau, so the best window (when one reaches tau) always
	// ends inside the band.
	best, bestStart := tau+1, ln
	for j := lastJ0; j <= ln; j++ {
		if prev[j] < best || (prev[j] == best && prevS[j] < bestStart) {
			best, bestStart = prev[j], prevS[j]
		}
	}
	if best > tau {
		return best, false, bestStart, false
	}
	return best, bestStart == 0, bestStart, true
}

// fuzzyScoredCandidate is a distance-verified fuzzy survivor without a
// materialized entry. Path reconstruction dominates per-candidate cost, so
// survivors sort by their path-free keys first and only the winners (plus a
// tied group cut off by the limit) ever build entries.
type fuzzyScoredCandidate struct {
	vol       int
	id        int
	dist      int
	wholeDist int
	prefix    bool
	windowPos int
	lenDiff   int
	rank      uint64
}

func fuzzyCandidateRankLess(a, b fuzzyScoredCandidate) bool {
	if a.dist != b.dist {
		return a.dist < b.dist
	}
	// A typo of a whole filename ("BUImLD.gn") should land on that
	// name, not on a longer name containing it ("BUILD.gni").
	if a.wholeDist != b.wholeDist {
		return a.wholeDist < b.wholeDist
	}
	if a.prefix != b.prefix {
		return a.prefix
	}
	if a.windowPos != b.windowPos {
		return a.windowPos < b.windowPos
	}
	if a.lenDiff != b.lenDiff {
		return a.lenDiff < b.lenDiff
	}
	if a.rank != b.rank {
		return a.rank < b.rank
	}
	return false
}

// fuzzyParallelScoreMinIDs is the candidate count above which scoring fans
// out across GOMAXPROCS workers. Below it the goroutine fan-out costs more
// than the DP it would parallelize.
const fuzzyParallelScoreMinIDs = 1024

// fuzzyRefScore is the per-candidate verdict of scoreFuzzyRefs, aligned with
// its input ids so parallel scoring merges deterministically.
type fuzzyRefScore struct {
	ok        bool
	dist      int
	wholeDist int
	prefix    bool
	windowPos int
	lenDiff   int
	rank      uint64
}

// scoreFuzzyRefs verifies candidate ids against the term: persisted-lower
// name, fused letter-mask prefilter, scratch distance DP, rank and whole-name
// distance for survivors. Results align with ids; chunks above
// fuzzyParallelScoreMinIDs score across workers (each with its own scratch)
// and merge in order, so parallel and serial runs produce identical output.
func scoreFuzzyRefs(vol *serviceVolumeIndex, rankVec []uint32, ids []int, termRunes []rune, termASCII bool, termMask uint64, tau int, serialSC *fuzzyDistanceScratch) []fuzzyRefScore {
	scores := make([]fuzzyRefScore, len(ids))
	workers := 1
	if len(ids) >= fuzzyParallelScoreMinIDs {
		workers = min(runtime.GOMAXPROCS(0), max(1, len(ids)/fuzzyParallelScoreMinIDs))
	}
	if workers <= 1 {
		scoreFuzzyRefRange(vol, rankVec, ids, scores, termRunes, termASCII, termMask, tau, serialSC)
		return scores
	}
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * len(ids) / workers
		end := (worker + 1) * len(ids) / workers
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			var sc fuzzyDistanceScratch
			scoreFuzzyRefRange(vol, rankVec, ids[start:end], scores[start:end], termRunes, termASCII, termMask, tau, &sc)
		}(start, end)
	}
	wg.Wait()
	return scores
}

// scoreFuzzyRefRange scores one index range; every read (record, persisted
// lower name, rank vector) is immutable during a query, matching the existing
// parallel trigram verifiers.
func scoreFuzzyRefRange(vol *serviceVolumeIndex, rankVec []uint32, ids []int, scores []fuzzyRefScore, termRunes []rune, termASCII bool, termMask uint64, tau int, sc *fuzzyDistanceScratch) {
	for k, id := range ids {
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		// Score the name before building the entry: path reconstruction
		// dominates per-candidate cost, and piece postings bring in many
		// candidates that miss on distance. The persisted lower name avoids
		// the per-candidate ToLower copy whenever the index carries it. The
		// fused mask scan doubles as the ASCII test, so folding and the
		// distance fast-path choice need no second pass over the name.
		nameLower := vol.index.compactLowerNameOf(id, rec)
		nameMask, nameASCII := fuzzyLetterMaskASCII(nameLower)
		if !nameASCII {
			nameLower = foldFuzzyText(nameLower)
			nameMask, nameASCII = fuzzyLetterMaskASCII(nameLower)
		}
		if fuzzyLetterMaskMisses(termMask, nameMask) > tau {
			continue
		}
		dist, prefix, windowPos, okDist := fuzzyNameDistanceScratchKnown(nameLower, termRunes, termASCII, nameASCII, tau, sc)
		if !okDist || dist == 0 {
			continue
		}
		var rank uint64
		if id < len(rankVec) {
			rank = uint64(rankVec[id])
		}
		lenDiff := utf8.RuneCountInString(rec.Name) - len(termRunes)
		if lenDiff < 0 {
			lenDiff = -lenDiff
		}
		wholeDist := tau + 1
		if lenDiff <= tau {
			// sc.nameRunes still holds this candidate's decoded folded
			// name from the distance call above.
			wholeDist = damerauLevenshteinBoundedReuse(sc.nameRunes, termRunes, tau, sc)
		}
		scores[k] = fuzzyRefScore{ok: true, dist: dist, wholeDist: wholeDist, prefix: prefix, windowPos: windowPos, lenDiff: lenDiff, rank: rank}
	}
}

func fuzzyCandidateRankEqual(a, b fuzzyScoredCandidate) bool {
	return a.dist == b.dist && a.wholeDist == b.wholeDist && a.prefix == b.prefix &&
		a.windowPos == b.windowPos && a.lenDiff == b.lenDiff && a.rank == b.rank
}

// appendFuzzyServiceMatches tops up an underfilled exact result set with
// close-match entries gathered from the per-volume name trigram postings.
// It reports whether any fuzzy results were added; callers surface that via
// the response's fuzzy flag and planner mode.
func appendFuzzyServiceMatches(volumes []*serviceVolumeIndex, opts queryOptions, matches []Entry) ([]Entry, bool) {
	limit := normalizedLimit(opts.Limit, false)
	need := limit - len(matches)
	if need <= 0 {
		return matches, false
	}
	pq, err := parseQuery(opts)
	if err != nil || pq.CaseSensitive || pq.MatchPath || queryHasAnyContentLeaf(pq) || len(pq.Terms) != 1 || len(pq.OrGroups) > 0 || len(pq.Regexps) > 0 || len(pq.Globs) > 0 {
		return matches, false
	}
	// Name anchors describe exact-tier intent: a typo'd anchor is a
	// different question, and "close matches" that violate the stated
	// anchor would mislead. Quoted terms keep exact-match semantics and
	// never correct. Path globs constrain the path, which the name-based
	// fuzzy tier cannot verify.
	if len(pq.ExactTerms)+len(pq.Prefixes)+len(pq.Suffixes) > 0 || len(pq.PathGlobs) > 0 ||
		len(pq.NotUnders) > 0 || isQuotedTerm(pq, pq.Terms[0]) {
		return matches, false
	}
	// Auto-fuzzy: a query that matched nothing is the strongest typo signal,
	// so the fallback tier fires without needing the explicit ~ marker or
	// --fuzzy flag. Explicit markers additionally allow topping up partially
	// filled result sets. A partial exact set never auto-tops: appended rows
	// would not satisfy the query, so they read as false positives next to
	// exact hits. Typo queries whose misspelling still hits incidentally need
	// an explicit marker to reach the tier; the recall harness pins the
	// typo top-1 rates that must not regress.
	autoFuzzy := len(matches) == 0
	if !autoFuzzy && !pq.Fuzzy {
		return matches, false
	}
	term := foldFuzzyText(strings.ToLower(pq.Terms[0]))
	if utf8.RuneCountInString(term) < fuzzyMinTermRunes {
		return matches, false
	}
	tau := fuzzyThresholdForTerm(term)
	if tau > fuzzyMaxDistance {
		tau = fuzzyMaxDistance
	}
	// Depth-2 variants grow quadratically with term length; once the
	// pigeonhole pieces are long enough to be selective they cover two-edit
	// typos on their own, and depth 1 keeps the cheap transposition lookups.
	variantDepth := tau
	if tau > 1 && utf8.RuneCountInString(term) >= fuzzyLongTermRunes {
		variantDepth = 1
	}
	variants := fuzzyDeletionVariants(term, variantDepth)
	// Deterministic order: the candidate collection cap cuts off work, so
	// iteration order must not change which results users see.
	sort.Strings(variants)
	variants = append(variants, fuzzyPigeonholePieces(term, tau)...)
	termRunes := []rune(term)
	termASCII := asciiOnlyString(term)
	termMask := fuzzyLetterMask(term)

	pqNoTerms := pq
	pqNoTerms.Terms = nil

	existing := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		existing[m.Path] = struct{}{}
	}

	var out []fuzzyScoredCandidate
	var sc fuzzyDistanceScratch
	enough := need * fuzzyCollectFactor
	for volIndex, vol := range volumes {
		if vol == nil || vol.index == nil || vol.nameTrigramIndex() == nil {
			continue
		}
		rankVec := vol.rankForQuery(pqNoTerms)
		seen := make(map[int]struct{})
		var pending []int
		// flush scores one variant's pending ids and appends every
		// survivor in order. The enough cutoff is checked only at variant
		// boundaries exactly like the original serial loop: the in-flight
		// variant always over-collects past enough, and that tail
		// participates in the final sort, so stopping mid-variant would
		// change which survivors reach the top-N.
		flush := func() {
			if len(pending) == 0 {
				return
			}
			scores := scoreFuzzyRefs(vol, rankVec, pending, termRunes, termASCII, termMask, tau, &sc)
			for k, id := range pending {
				s := scores[k]
				if !s.ok {
					continue
				}
				out = append(out, fuzzyScoredCandidate{vol: volIndex, id: id, dist: s.dist, wholeDist: s.wholeDist, prefix: s.prefix, windowPos: s.windowPos, lenDiff: s.lenDiff, rank: s.rank})
			}
			pending = pending[:0]
		}
		for _, variant := range variants {
			ids, ok := vol.nameTrigramNameTermPostingLimited(variant, fuzzyCandidateCap)
			if !ok {
				continue
			}
			for _, id := range ids {
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				pending = append(pending, id)
			}
			flush()
			if len(out) >= enough {
				break
			}
		}
		if len(out) >= enough {
			break
		}
	}
	if len(out) == 0 {
		return matches, false
	}
	sort.Slice(out, func(i, j int) bool { return fuzzyCandidateRankLess(out[i], out[j]) })
	// Materialize entries lazily in rank order: only winners (and the tied
	// group straddling the limit, ordered by path exactly like the old
	// full sort) ever pay for path reconstruction. The existing-set and
	// entryMatches gates stay exactly where they were, on the entry.
	pathCaches := make([]map[int]string, len(volumes))
	appended := make([]Entry, 0, need)
	for i := 0; i < len(out) && len(appended) < need; {
		j := i + 1
		for j < len(out) && fuzzyCandidateRankEqual(out[j], out[i]) {
			j++
		}
		group := make([]Entry, 0, j-i)
		for _, scored := range out[i:j] {
			vol := volumes[scored.vol]
			rec := vol.index.compactRecord(scored.id)
			if pathCaches[scored.vol] == nil {
				pathCaches[scored.vol] = make(map[int]string)
			}
			entry := compactEntryFromRecord(vol.index, scored.id, rec, pathCaches[scored.vol], true)
			if entry.Path == "" {
				continue
			}
			if _, dupPath := existing[entry.Path]; dupPath {
				continue
			}
			if !entryMatches(entry, pqNoTerms, pq.MatchPath) {
				continue
			}
			group = append(group, entry)
		}
		sort.Slice(group, func(a, b int) bool { return group[a].Path < group[b].Path })
		// Usage/git/proximity signals reorder within this equal-distance
		// group only (rank_blend.go band rule); distance order never inverts,
		// and with no signals the path order above stands. Per-user: the
		// caller's store, not the process-shared one.
		if store := signalStoreFromOpts(opts); signalReorderActiveWithStore(pq, store) {
			reorderFuzzyGroup(group, buildSignalPageCtxWithStore(group, pq, store))
		}
		for _, entry := range group {
			if len(appended) >= need {
				break
			}
			appended = append(appended, entry)
		}
		i = j
	}
	if len(appended) == 0 {
		return matches, false
	}
	return append(matches, appended...), true
}

type fuzzyTermSubstitution struct {
	From string
	To   string
}

// fuzzySoloMatchCount counts visible names containing termLower as a substring
// across all volumes, stopping once stopAt matches are found. It returns
// (count, true) when at least one volume could evaluate the term and
// (0, false) when every lookup declined, so callers never mistake an
// unevaluated term for a healthy or broken one.
func fuzzySoloMatchCount(volumes []*serviceVolumeIndex, termLower string, stopAt int) (int, bool) {
	if utf8.RuneCountInString(termLower) < fuzzyMinTermRunes || strings.ContainsAny(termLower, `\/*?[]:`) {
		return stopAt, true
	}
	return fuzzySoloMatchCountAllowShort(volumes, termLower, stopAt)
}

// fuzzySoloMatchCountAllowShort is fuzzySoloMatchCount without the rune
// length gate, used by the short-term insertion path whose variants are
// full-length but whose metadata may be unavailable.
func fuzzySoloMatchCountAllowShort(volumes []*serviceVolumeIndex, termLower string, stopAt int) (int, bool) {
	count := 0
	evaluated := false
	for _, vol := range volumes {
		if vol == nil || vol.index == nil || vol.nameTrigramIndex() == nil {
			continue
		}
		pq := parsedQuery{Terms: []string{termLower}, Limit: stopAt}
		ids, ok := vol.nameTrigramCandidates(pq)
		if !ok {
			continue
		}
		evaluated = true
		for _, id := range ids {
			if !vol.nameTrigramCandidateMatches(id, termLower) {
				continue
			}
			count++
			if count >= stopAt {
				return count, true
			}
		}
	}
	return count, evaluated
}

// fuzzyTermMinGramCount scores how plausible a term is as a real filename
// substring using posting-count metadata only: the smallest (bottleneck) gram
// posting count across the term's grams, maximized across volumes. No posting
// blocks are decoded. ok is false when completeness metadata cannot judge
// (missing sections); a proven empty gram returns (0, true).
func fuzzyTermMinGramCount(vol *serviceVolumeIndex, termLower string) (int, bool) {
	if vol == nil || vol.index == nil {
		return 0, false
	}
	trigrams := vol.nameTrigramIndex()
	extraTrigrams := vol.index.Derived.SelfNameTrigrams
	if trigrams == nil && extraTrigrams == nil {
		return 0, false
	}
	minCount := -1
	for _, gram := range uniqueFixedGramKeysFoldASCII(strings.ToLower(termLower), 3) {
		_, count, _, state, exactEmpty := vol.nameGramPosting(gram)
		if exactEmpty {
			return 0, true
		}
		if state == "missing-section" {
			return 0, false
		}
		if count < 0 {
			count = 0
		}
		if minCount < 0 || count < minCount {
			minCount = count
		}
	}
	if minCount < 0 {
		return 0, false
	}
	return minCount, true
}

// fuzzyTopTermVariants returns up to n close-match variants of termLower
// ordered by plausibility: lowest edit distance first, then strongest
// gram-selectivity score (a stand-in for match count that stays cheap on
// popular terms whose posting lanes decline), then lexicographic order for
// determinism. Variants that are themselves too short to be fuzzed, or whose
// grams prove they cannot appear in any name, are skipped.
func fuzzyTopTermVariants(volumes []*serviceVolumeIndex, termLower string, n int) []string {
	if n <= 0 {
		return nil
	}
	tau := fuzzyThresholdForTerm(termLower)
	type scored struct {
		variant string
		dist    int
		seen    int // sampled names containing the variant; 0 = not seen
		score   int
	}
	var out []scored
	// Deletion variants never contain the corrected spelling of an interior
	// substitution or omission ("relese" -> "release"); real name windows
	// found through the pigeonhole pieces do, and are known to match.
	windows := fuzzyPieceWindowVariants(volumes, termLower, tau)
	variants := fuzzyDeletionVariants(termLower, tau)
	sort.Strings(variants)
	for _, variant := range variants {
		if variant == termLower {
			continue
		}
		if utf8.RuneCountInString(variant) < fuzzyMinTermRunes {
			continue
		}
		dist := damerauLevenshteinBounded([]rune(variant), []rune(termLower), tau)
		if dist <= 0 || dist > tau {
			continue
		}
		score := 0
		proven := false
		for _, vol := range volumes {
			s, ok := fuzzyTermMinGramCount(vol, variant)
			if !ok {
				continue // this volume cannot judge; other volumes may
			}
			if s > 0 {
				proven = true
				if s > score {
					score = s
				}
			}
		}
		if !proven || score <= 0 {
			continue
		}
		out = append(out, scored{variant: variant, dist: dist, seen: windows[variant], score: score})
	}
	deletion := make(map[string]struct{}, len(variants))
	for _, variant := range variants {
		deletion[variant] = struct{}{}
	}
	for variant, count := range windows {
		if _, dup := deletion[variant]; dup {
			continue
		}
		dist := damerauLevenshteinBounded([]rune(variant), []rune(termLower), tau)
		if dist <= 0 || dist > tau {
			continue
		}
		score := 0
		for _, vol := range volumes {
			if s, ok := fuzzyTermMinGramCount(vol, variant); ok && s > score {
				score = s
			}
		}
		out = append(out, scored{variant: variant, dist: dist, seen: count, score: score})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].dist != out[j].dist {
			return out[i].dist < out[j].dist
		}
		// A variant seen inside real names is guaranteed to match, and the
		// more names carry it the likelier it is the intended word; a
		// deletion variant whose grams merely all exist somewhere may not
		// match at all.
		if out[i].seen != out[j].seen {
			return out[i].seen > out[j].seen
		}
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].variant < out[j].variant
	})
	result := make([]string, 0, n)
	for _, s := range out {
		result = append(result, s.variant)
		if len(result) >= n {
			break
		}
	}
	return result
}

// fuzzyPieceWindowVariantBudget bounds how many candidate names one term's
// window search verifies; the variants only need to surface the common
// spellings, not every occurrence.
const fuzzyPieceWindowVariantBudget = 4096

// fuzzyPieceWindowVariants returns substrings of real names that sit within
// tau edits of termLower, found through the term's pigeonhole pieces. Each
// is the best-aligned window of one candidate name, so a rewrite to it is
// guaranteed to match at least that name.
//
// The returned map counts how many sampled names contained each window, the
// best available plausibility signal: "tentative" appears in thousands of
// names, a stray "tentive" in a handful.
func fuzzyPieceWindowVariants(volumes []*serviceVolumeIndex, termLower string, tau int) map[string]int {
	pieces := fuzzyPigeonholePieces(termLower, tau)
	if len(pieces) == 0 {
		return nil
	}
	termMask := fuzzyLetterMask(termLower)
	termRunes := []rune(termLower)
	termASCII := asciiOnlyString(termLower)
	edgeOK := func(r rune, termEdge rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r) || r == termEdge
	}
	out := make(map[string]int)
	seenIDs := make(map[[2]int]struct{})
	var sc fuzzyDistanceScratch
	// Each piece gets an equal share, sampled evenly across its matches: a
	// common piece ("tent" inside every "content") must not use up the
	// budget before the piece that reaches the intended word ("tive").
	perPiece := max(256, fuzzyPieceWindowVariantBudget/len(pieces))
	for volIndex, vol := range volumes {
		if vol == nil || vol.index == nil || vol.nameTrigramIndex() == nil {
			continue
		}
		for _, piece := range pieces {
			ids := vol.fuzzyPieceCandidateIDs(piece, fuzzyPieceWindowVariantBudget)
			stride := max(1, (len(ids)+perPiece-1)/perPiece)
			for i := 0; i < len(ids); i += stride {
				id := ids[i]
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				if _, dup := seenIDs[[2]int{volIndex, id}]; dup {
					continue
				}
				seenIDs[[2]int{volIndex, id}] = struct{}{}
				rec := vol.index.compactRecord(id)
				if rec.Deleted {
					continue
				}
				nameLower := vol.index.compactLowerNameOf(id, rec)
				nameMask, nameASCII := fuzzyLetterMaskASCII(nameLower)
				if !nameASCII {
					nameLower = foldFuzzyText(nameLower)
					nameMask, nameASCII = fuzzyLetterMaskASCII(nameLower)
				}
				if fuzzyLetterMaskMisses(termMask, nameMask) > tau {
					continue
				}
				dist, _, pos, ok := fuzzyNameDistanceScratchKnown(nameLower, termRunes, termASCII, nameASCII, tau, &sc)
				if !ok || dist == 0 {
					continue
				}
				window := fuzzyBestWindowAtReuse(sc.nameRunes, termRunes, pos, tau, &sc)
				if window == "" || strings.ContainsAny(window, " \t\"'\\/*?[]:") {
					continue
				}
				// "-opus" and "_opus" are the same correction as "opus"
				// with a separator attached; keep word-shaped windows.
				w := []rune(window)
				if !edgeOK(w[0], termRunes[0]) || !edgeOK(w[len(w)-1], termRunes[len(termRunes)-1]) {
					continue
				}
				out[window]++
			}
		}
	}
	return out
}

// fuzzyPieceCandidateIDs returns names containing piece. Pieces of common
// words ("exp", "cted") exceed the trigram candidate cap, and declining
// them left typos of exactly the most common words uncorrectable, so an
// over-cap piece is sampled from its rarest gram's posting instead: the
// window search only needs the plausible spellings, not every name.
func (vol *serviceVolumeIndex) fuzzyPieceCandidateIDs(piece string, budget int) []int {
	if budget <= 0 {
		return nil
	}
	if ids, ok := vol.nameTrigramNameTermPostingLimited(piece, fuzzyCandidateCap); ok {
		return ids
	}
	its, _, exactZero, complete := completeSelfNameGramIterators(vol.index, piece)
	if !complete || exactZero || len(its) == 0 {
		return nil
	}
	recordCount := vol.index.compactRecordCount()
	out := make([]int, 0, min(budget, 256))
	it := its[0]
	for len(out) < budget {
		block, _, ok := it.nextBlock()
		if !ok {
			break
		}
		for _, id32 := range block {
			id := int(id32)
			if id >= recordCount || !strings.Contains(vol.index.compactLowerNameAt(id), piece) {
				continue
			}
			out = append(out, id)
			if len(out) >= budget {
				break
			}
		}
	}
	return out
}

// fuzzyBestWindowAt returns the window of nameLower starting at start whose
// distance to term is smallest, preferring the term's own length on ties.
func fuzzyBestWindowAt(nameLower, term string, start, tau int) string {
	var sc fuzzyDistanceScratch
	return fuzzyBestWindowAtReuse([]rune(nameLower), []rune(term), start, tau, &sc)
}

// fuzzyBestWindowAtReuse is fuzzyBestWindowAt over pre-split runes with
// reusable DP rows.
func fuzzyBestWindowAtReuse(name, t []rune, start, tau int, sc *fuzzyDistanceScratch) string {
	offBy := func(width int) int {
		if width > len(t) {
			return width - len(t)
		}
		return len(t) - width
	}
	best, bestWidth := tau+1, 0
	for width := max(1, len(t)-tau); width <= len(t)+tau && start+width <= len(name); width++ {
		d := damerauLevenshteinBoundedReuse(name[start:start+width], t, tau, sc)
		if d < best || (d == best && offBy(width) < offBy(bestWidth)) {
			best, bestWidth = d, width
		}
	}
	if best > tau || bestWidth == 0 {
		return ""
	}
	return string(name[start : start+bestWidth])
}

// fuzzyRewriteTrial is one candidate query rewrite: a single term swapped for
// its best close-match variant, every other byte of the query untouched.
type fuzzyRewriteTrial struct {
	Opts queryOptions
	Sub  fuzzyTermSubstitution
}

// multiTermFuzzyRewriteTrials builds substitution candidates for an
// underfilled multi-term query. Terms with zero solo matches come first
// (strongest typo signal); terms that do match on their own still get a trial
// because incidental substrings (e.g. "reprot" inside "coreproto") can mask a
// typo. Trials are capped; callers execute them cheapest-first and keep the
// first that yields results.
func multiTermFuzzyRewriteTrials(volumes []*serviceVolumeIndex, opts queryOptions, pq parsedQuery) []fuzzyRewriteTrial {
	if len(pq.Terms) < 2 || pq.CaseSensitive || pq.MatchPath || queryHasAnyContentLeaf(pq) ||
		len(pq.OrGroups) > 0 || len(pq.Regexps) > 0 || len(pq.Globs) > 0 {
		return nil
	}
	const (
		maxTrials       = 6
		variantsPerTerm = 3
	)
	fields := strings.Fields(opts.Query)
	broken := make([]fuzzyRewriteTrial, 0, 2)
	masked := make([]fuzzyRewriteTrial, 0, 2)
	shortFirst := make([]fuzzyRewriteTrial, 0, 2)
	for _, term := range pq.Terms {
		low := foldFuzzyText(strings.ToLower(term))
		if strings.ContainsAny(low, `\/*?[]:`) {
			continue // ineligible terms stay exact; they never block the rewrite
		}
		if utf8.RuneCountInString(low) < fuzzyMinTermRunes {
			// A 1-2 rune term has no deletion neighborhood (the correct
			// spelling is longer, not shorter) and no gram posting of its
			// own.  Its plausible corrections are insertion variants:
			// "lg" -> "log" adds a rune.  Score insertion candidates with
			// gram-count metadata and rewrite the best proven one.  These
			// trials always rank first: a short term in a zero-result query
			// is the strongest possible typo signal.
			for _, trial := range fuzzyShortTermInsertionTrials(volumes, opts, term, low) {
				if trial.Sub.To == low {
					continue
				}
				if trial.Opts.Query == opts.Query {
					continue
				}
				shortFirst = append(shortFirst, trial)
				if len(shortFirst) >= 2 {
					break
				}
			}
			continue
		}
		// Solo health cannot be the gate here: typos like "llog" are real
		// substrings with dozens of unrelated matches, and popular terms'
		// lookups decline outright. In a zero-result query every eligible
		// term is a rewrite candidate; the ordered trials plus the exact
		// re-search decide what wins. Zero-match terms rank first.
		soloCount, soloOK := fuzzySoloMatchCount(volumes, low, 1)
		variants := fuzzyTopTermVariants(volumes, low, variantsPerTerm)
		for _, variant := range variants {
			out := make([]string, len(fields))
			replaced := false
			for i, field := range fields {
				//lint:ignore SA6005 ToLower compares the parser's normalization; EqualFold folds differently (e.g. ſ vs s) and would drop valid rewrites
				if !replaced && strings.ToLower(field) == strings.ToLower(term) {
					out[i] = variant
					replaced = true
				} else {
					out[i] = field
				}
			}
			if !replaced {
				continue
			}
			newOpts := opts
			newOpts.Query = strings.Join(out, " ")
			// The rewritten query is exact; do not let the recursive pass
			// open the explicit fuzzy tier on top of it.
			newOpts.Fuzzy = false
			trial := fuzzyRewriteTrial{Opts: newOpts, Sub: fuzzyTermSubstitution{From: term, To: variant}}
			if soloOK && soloCount == 0 {
				broken = append(broken, trial)
			} else {
				masked = append(masked, trial)
			}
			if len(broken)+len(masked) >= maxTrials {
				break
			}
		}
		if len(broken)+len(masked) >= maxTrials {
			break
		}
	}
	all := append(shortFirst, broken...)
	all = append(all, masked...)
	if len(all) > maxTrials {
		all = all[:maxTrials]
	}
	return all
}

// fuzzyShortTermInsertionTrials builds rewrite trials for a 1-2 rune term by
// inserting one ASCII letter in every position (2-rune) or appending/prepending
// one letter (1-rune), keeping candidates whose gram-count metadata proves a
// real filename substring presence.  Trials are ordered best-first: higher
// bottleneck gram count and (tie) alphabetically stable.  The rewritten query
// keeps every other option (limit, under, recent, ...) from the original.
func fuzzyShortTermInsertionTrials(volumes []*serviceVolumeIndex, opts queryOptions, origTerm, low string) []fuzzyRewriteTrial {
	runes := []rune(low)
	if len(runes) >= fuzzyMinTermRunes || len(runes) == 0 {
		return nil
	}
	var candidates []string
	if len(runes) == 2 {
		for c := 'a'; c <= 'z'; c++ {
			candidates = append(candidates, string(c)+low, string(runes[0])+string(c)+string(runes[1]), low+string(c))
		}
	} else {
		for c := 'a'; c <= 'z'; c++ {
			candidates = append(candidates, string(c)+low, low+string(c))
		}
	}
	type scored struct {
		variant string
		score   int
	}
	var out []scored
	for _, variant := range candidates {
		score := 0
		proven := false
		for _, vol := range volumes {
			s, ok := fuzzyTermMinGramCount(vol, variant)
			if !ok {
				continue
			}
			proven = true
			if s > score {
				score = s
			}
		}
		if !proven {
			// No volume's metadata could judge; fall back to a cheap
			// stop-at-first-match solo probe per variant.  The rune-length gate
			// inside fuzzySoloMatchCount is bypassed: the whole point of the
			// insertion path is 1-2 rune source terms, and the variants are
			// full-length words.
			if count, evaluated := fuzzySoloMatchCountAllowShort(volumes, variant, 1); evaluated && count > 0 {
				out = append(out, scored{variant: variant, score: 1})
			}
			continue
		}
		if score <= 0 {
			continue
		}
		out = append(out, scored{variant: variant, score: score})
	}
	// Deterministic order: score desc, then variant asc.
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].variant < out[j].variant
	})
	trials := make([]fuzzyRewriteTrial, 0, 2)
	for _, s := range out {
		if len(trials) >= 2 {
			break
		}
		if s.variant == low {
			continue
		}
		fields := strings.Fields(opts.Query)
		replaced := make([]string, len(fields))
		done := false
		for i, field := range fields {
			if !done && strings.ToLower(field) == origTerm {
				replaced[i] = s.variant
				done = true
			} else {
				replaced[i] = field
			}
		}
		if !done {
			continue
		}
		newOpts := opts
		newOpts.Query = strings.Join(replaced, " ")
		// The rewritten query is exact; the explicit fuzzy tier stays off.
		newOpts.Fuzzy = false
		trials = append(trials, fuzzyRewriteTrial{Opts: newOpts, Sub: fuzzyTermSubstitution{From: origTerm, To: s.variant}})
	}
	return trials
}

// tryMultiTermFuzzyRewrite runs the multi-term fuzzy chain against an
// underfilled result set. It reports whether the multi-term case was handled
// (including deciding there was nothing to do), so callers can skip the
// single-term fallback, which declines multi-term queries anyway.
func tryMultiTermFuzzyRewrite(volumes []*serviceVolumeIndex, opts *queryOptions, matches *[]Entry, fuzzied *bool) bool {
	pq, err := parseQuery(*opts)
	if err != nil || queryHasAnyContentLeaf(pq) || len(pq.Terms) < 2 {
		return false
	}
	if len(*matches) > 0 && !pq.Fuzzy {
		// Query rewrites are invasive: unlike the single-term tier, which
		// only appends below exact results, chaining fires on total misses
		// (or with an explicit marker). A query that already matched anything
		// is left alone.
		return true
	}
	for _, trial := range multiTermFuzzyRewriteTrials(volumes, *opts, pq) {
		rewritten, err := searchServiceVolumes(volumes, trial.Opts, false)
		if err != nil {
			continue
		}
		if len(rewritten) == 0 {
			continue
		}
		serviceLog("fuzzymulti rewrote q=%q %q->%q results=%d", opts.Query, trial.Sub.From, trial.Sub.To, len(rewritten))
		*matches = rewritten
		*fuzzied = true
		return true
	}
	return true // multi-term case decided; keep the original (underfilled) results
}
