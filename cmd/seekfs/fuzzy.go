package main

import (
	"sort"
	"strings"
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
	// Auto-fuzzy fires when exact search returns fewer than this many
	// results: zero hits are the strongest typo signal, but a handful of
	// incidental substring hits (e.g. "reprot" inside "coreproto") can also
	// mask the intended match, so a small threshold catches those too.
	fuzzyAutoResultThreshold = 10
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

// foldFuzzyText normalizes text for fuzzy comparison: fullwidth forms fold to
// their ASCII equivalents and combining marks are stripped so "sodanco"
// matches "Só Danço". Comparison-side only; index data is untouched.
func foldFuzzyText(s string) string {
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

// fuzzyNameDistance finds the best (lowest) bounded Damerau-Levenshtein
// distance between the term and any window of the lowercased name whose rune
// length is within tau of the term length. prefix reports whether the best
// alignment starts at the beginning of the name. ok is false when even the
// best window exceeds tau.
func fuzzyNameDistance(nameLower, term string, tau int) (dist int, prefix bool, windowPos int, ok bool) {
	nameRunes := []rune(nameLower)
	termRunes := []rune(term)
	lt, ln := len(termRunes), len(nameRunes)
	if ln == 0 {
		return tau + 1, false, 0, false
	}
	best := tau + 1
	prefixBest := false
	bestStart := ln
	end := ln - lt + tau
	if end > ln {
		end = ln
	}
	for start := 0; start <= end; start++ {
		maxWidth := lt + tau
		if maxWidth > ln-start {
			maxWidth = ln - start
		}
		minWidth := lt - tau
		if minWidth < 1 {
			minWidth = 1
		}
		for width := minWidth; width <= maxWidth; width++ {
			d := damerauLevenshteinBounded(nameRunes[start:start+width], termRunes, tau)
			atPrefix := start == 0
			if d < best || (d == best && start < bestStart) {
				if d < best {
					best = d
					prefixBest = atPrefix
				}
				bestStart = start
			} else if d == best && atPrefix {
				prefixBest = true
			}
			if best == 0 {
				break
			}
		}
		if best == 0 {
			break
		}
	}
	prefix = prefixBest
	if best > tau {
		return best, false, bestStart, false
	}
	return best, prefix, bestStart, true
}

type fuzzyScoredEntry struct {
	entry     Entry
	dist      int
	prefix    bool
	windowPos int
	rank      uint64
	path      string
	lenDiff   int
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
	// Auto-fuzzy: a query that matched nothing is the strongest typo signal,
	// so the fallback tier fires without needing the explicit ~ marker or
	// --fuzzy flag. Explicit markers additionally allow topping up partially
	// filled result sets.
	autoFuzzy := len(matches) < fuzzyAutoResultThreshold
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
	variants := fuzzyDeletionVariants(term, tau)
	// Deterministic order: the candidate collection cap cuts off work, so
	// iteration order must not change which results users see.
	sort.Strings(variants)
	termRunes := int64(utf8.RuneCountInString(term))

	pqNoTerms := pq
	pqNoTerms.Terms = nil

	existing := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		existing[m.Path] = struct{}{}
	}

	var out []fuzzyScoredEntry
	enough := need * fuzzyCollectFactor
	for _, vol := range volumes {
		if vol == nil || vol.index == nil || vol.nameTrigramIndex() == nil {
			continue
		}
		rankVec := vol.rankForQuery(pqNoTerms)
		pathCache := make(map[int]string)
		seen := make(map[int]struct{})
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
				rec := vol.index.compactRecord(id)
				if rec.Deleted {
					continue
				}
				entry := compactEntryFromRecord(vol.index, id, rec, pathCache, true)
				if _, dupPath := existing[entry.Path]; dupPath {
					continue
				}
				nameLower := foldFuzzyText(strings.ToLower(entry.Name))
				dist, prefix, windowPos, okDist := fuzzyNameDistance(nameLower, term, tau)
				if !okDist || dist == 0 {
					continue
				}
				if !entryMatches(entry, pqNoTerms, pq.MatchPath) {
					continue
				}
				var rank uint64
				if id < len(rankVec) {
					rank = uint64(rankVec[id])
				}
				lenDiff := utf8.RuneCountInString(entry.Name) - int(termRunes)
				if lenDiff < 0 {
					lenDiff = -lenDiff
				}
				out = append(out, fuzzyScoredEntry{entry: entry, dist: dist, prefix: prefix, windowPos: windowPos, rank: rank, path: entry.Path, lenDiff: lenDiff})
			}
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
	sort.Slice(out, func(i, j int) bool {
		if out[i].dist != out[j].dist {
			return out[i].dist < out[j].dist
		}
		if out[i].prefix != out[j].prefix {
			return out[i].prefix
		}
		if out[i].windowPos != out[j].windowPos {
			return out[i].windowPos < out[j].windowPos
		}
		if out[i].lenDiff != out[j].lenDiff {
			return out[i].lenDiff < out[j].lenDiff
		}
		if out[i].rank != out[j].rank {
			return out[i].rank < out[j].rank
		}
		return out[i].path < out[j].path
	})
	if len(out) > need {
		out = out[:need]
	}
	appended := make([]Entry, 0, len(out))
	for _, scored := range out {
		appended = append(appended, scored.entry)
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
		score   int
	}
	var out []scored
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
		out = append(out, scored{variant: variant, dist: dist, score: score})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].dist != out[j].dist {
			return out[i].dist < out[j].dist
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
		maxTrials       = 4
		variantsPerTerm = 2
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
