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
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"sync/atomic"
	"time"
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

// contentDiskVerifyBudgetMax is the hard ceiling for a per-query disk
// verification budget (--content-verify-budget). The historical built-in
// default remains contentDiskVerifyBudget (512); larger explicit values are
// clamped here.
const contentDiskVerifyBudgetMax = 100_000

// contentDefaultBudgetMS is zero to preserve the existing no-implicit-deadline
// behavior. An explicit positive --budget arms a wall-clock bound; omitted and
// explicit zero both leave it unarmed. A var so tests can pin deadlines.
var contentDefaultBudgetMS = 0

// resolveContentBudget turns the CLI --budget duration into the wire selection
// (budgetMS, disabled). It keeps the three cases distinct: an omitted flag
// keeps the historical no-time-limit behavior; an explicit 0 disables the time
// bound; a positive value is the bound. Negative durations and positive
// sub-millisecond values are rejected rather than silently truncated to 0.
func resolveContentBudget(d time.Duration, provided bool) (ms int, disabled bool, err error) {
	if !provided {
		return 0, false, nil
	}
	if d < 0 {
		return 0, false, fmt.Errorf("--budget must be >= 0 (0 disables the time bound), got %s", d)
	}
	if d == 0 {
		return 0, true, nil
	}
	if d < time.Millisecond {
		return 0, false, fmt.Errorf("--budget must be 0 (disable) or at least 1ms, got %s", d)
	}
	return int(d.Milliseconds()), false, nil
}

// contentBudgetFromFlagSet preserves whether the CLI flag was explicitly set;
// flag.Duration's value alone cannot distinguish omitted from explicit zero.
func contentBudgetFromFlagSet(fs *flag.FlagSet, d time.Duration) (ms int, disabled bool, err error) {
	provided := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "budget" {
			provided = true
		}
	})
	return resolveContentBudget(d, provided)
}

// clampContentDiskVerifyBudget returns the effective per-query disk
// verification budget for an explicit request value. Values <= 0 keep the
// built-in default; larger values are clamped to contentDiskVerifyBudgetMax so
// an opt-in override can never read unboundedly.
func clampContentDiskVerifyBudget(v int) int {
	if v <= 0 {
		return contentDiskVerifyBudget
	}
	if v > contentDiskVerifyBudgetMax {
		return contentDiskVerifyBudgetMax
	}
	return v
}

// contentDiskVerifyBudgetOf returns the query's disk verification budget, or
// the built-in default when unset (<= 0). It is used where a parsedQuery is in
// hand; the verify path itself reads the pre-resolved value off the trace.
func contentDiskVerifyBudgetOf(pq parsedQuery) int {
	return clampContentDiskVerifyBudget(pq.ContentDiskVerifyBudget)
}

// boundPathCache resets a scan's path-reconstruction memo once it reaches
// contentScanPathCacheCap, keeping the scan's memory O(cap) rather than
// O(records visited). The memo is pure cache: dropping it only repeats
// parent-chain walks, never changes results. Shared by content and
// non-content scans alike.
func boundPathCache(cache map[int]string) map[int]string {
	if len(cache) >= contentScanPathCacheCap {
		return make(map[int]string, contentScanPathCacheCap)
	}
	return cache
}

// contentStatExists stats a path for the `exists:` predicate. It is a var so
// tests can substitute a fake filesystem: the differential lane tests keep
// every record path inside a deleted temp dir, which must still read as
// existing for the exists: shape to have a non-empty set on both arms.
var contentStatExists = os.Stat

// errContentIncomplete refuses a count whose evaluation was capped: a
// partial number would look exact. It covers both the candidate-superset cap
// and the v11 per-query disk verification cap; the operator action is the
// same either way (narrow the query).
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
	// needleASCII records, per term/phrase leaf, whether the folded needle
	// is pure ASCII. An ASCII needle against an ASCII document matches
	// exactly under ASCII case folding, so verification and snippets can
	// test it without allocating the full fold (asciiCaseIndex); anything
	// else takes the historical fold path with identical results.
	needleASCII map[int]bool
	// grams records, per term/phrase leaf, the folded distinct trigram keys
	// constraining the v11 per-file filter prefilter (empty for sub-3-byte
	// leaves: no constraint). regexGrams records, per regex leaf, the
	// required-literal grams per disjunct per run (nil = no sound prefilter:
	// no provable literals, or a disjunct a match may take unconstrained).
	// Both are computed once here so the per-record gate is bit tests only.
	// regexNeedle records, per regex leaf with provable literals, the
	// lowercased required runs of the currently cheapest disjunct (nil when
	// the query is case-sensitive or no disjunct is usable): a match must
	// contain every run, so verification skips the regexp engine when any
	// is absent — the same superset the tier-1 postings and tier-2 bloom
	// gate already enforce.
	grams        map[int][]uint32
	regexGrams   map[int][][][]uint32
	regexNeedles map[int][][]byte
	accentNeedle map[int]bool
	// sym holds the compiled definition patterns (content_sym.go) per leaf:
	// sym: leaves, and plain identifier-like term leaves for the
	// definition-line ranking bonus. Built once here; read-only after.
	sym map[int]*contentSymMatcher
}

// isASCIIBytes reports whether b has no byte >= 0x80.
func isASCIIBytes(b []byte) bool {
	for i := 0; i < len(b); i++ {
		if b[i] >= 0x80 {
			return false
		}
	}
	return true
}

// asciiCaseIndex finds the folded ASCII needle in raw under ASCII case
// folding, without allocating. The caller must guarantee needle is folded
// and pure ASCII (needleASCII); raw may be anything.
//
// A non-negative return is always a true match at that raw byte offset
// (ASCII folding preserves offsets, so no mapping is needed). ascii=false
// means raw contains non-ASCII bytes: the caller must fall back to the
// full fold, which decides (folding can only reveal matches involving
// non-ASCII runes, e.g. İ folding to i̇, which imply non-ASCII raw).
// ascii=true with -1 means raw is pure ASCII and needle is genuinely
// absent. Either way the outcome equals fold-then-substring.
func asciiCaseIndex(raw, needle []byte) (idx int, ascii bool) {
	if len(needle) == 0 || len(raw) < len(needle) {
		return -1, isASCIIBytes(raw)
	}
	first := needle[0]
	upper := first
	if first >= 'a' && first <= 'z' {
		upper = first - ('a' - 'A')
	}
	ascii = true
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c >= 0x80 {
			ascii = false
			continue
		}
		if i+len(needle) > len(raw) {
			// Past the last match start: no comparison, but the byte
			// still counts for the ASCII verdict (checked above).
			continue
		}
		if c != first && c != upper {
			continue
		}
		match := true
		for j := 1; j < len(needle); j++ {
			d := raw[i+j]
			if d >= 0x80 {
				ascii = false
				match = false
				break
			}
			if d >= 'A' && d <= 'Z' {
				d += 'a' - 'A'
			}
			if d != needle[j] {
				match = false
				break
			}
		}
		if match {
			// A true ASCII match — but the caller still needs the
			// verdict: earlier non-ASCII bytes mean the fold could
			// match even earlier (inside a fold expansion like İ→i̇),
			// so only a clean scan authorizes the fast offset.
			return i, ascii
		}
	}
	return -1, ascii
}

func newContentLeafMatcher(pq parsedQuery) *contentLeafMatcher {
	leaves := contentAllLeaves(pq)
	m := &contentLeafMatcher{leaves: leaves, caseSensitive: pq.contentCaseSensitive()}
	for _, leaf := range leaves {
		// Fuzzy variants (WholeWord, not Sym) skip this: up to 64 per word.
		if leaf.Sym || leaf.Kind == contentLeafTerm && !leaf.WholeWord && !leaf.Fuzzy && contentLooksLikeIdentifier(leaf.Text) {
			if m.sym == nil {
				m.sym = make(map[int]*contentSymMatcher)
			}
			m.sym[leaf.LeafID] = newContentSymMatcher(leaf.Text)
		}
	}
	for i := range leaves {
		if leaves[i].LeafID+1 > m.size {
			m.size = leaves[i].LeafID + 1
		}
	}
	for _, leaf := range leaves {
		if leaf.Kind == contentLeafRegex {
			pat := leaf.Text
			if !pq.contentCaseSensitive() {
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
			// Prefilter grams for the v11 filter gate: per disjunct, the
			// trigram-safe runs' grams (runs arrive lowercased, matching
			// the folded grams the builder indexed). A disjunct with no
			// safe run constrains nothing, so the whole leaf is
			// unconstrained — mirroring contentRegexLeafStream exactly.
			// The required runs double as the verify-time literal
			// prefilter below (regexNeedlesFor): a match must contain
			// every run of its disjunct, so a memmem miss decides without
			// the regexp engine. Runs arrive lowercased from the
			// extractor, matching bytes.Index over the folded text for a
			// case-insensitive query (or the raw text when sensitive).
			var disjuncts [][][]uint32
			constrained := true
			var alts [][]string
			for _, runs := range regexRequiredLiteralAlternatives(leaf.Text) {
				var rg [][]uint32
				var safe []string
				for _, run := range runs {
					if !contentTermTrigramSafe(run) {
						continue
					}
					rg = append(rg, contentFilterGramsOf(contentFoldAccent([]byte(run))))
					safe = append(safe, string(contentFoldAccent([]byte(run))))
				}
				if len(rg) == 0 {
					constrained = false
					break
				}
				disjuncts = append(disjuncts, rg)
				alts = append(alts, safe)
			}
			if constrained && len(disjuncts) > 0 {
				if m.regexGrams == nil {
					m.regexGrams = make(map[int][][][]uint32)
				}
				m.regexGrams[leaf.LeafID] = disjuncts
				if needles := contentRegexVerifyNeedles(alts); len(needles) > 0 {
					if m.regexNeedles == nil {
						m.regexNeedles = make(map[int][][]byte)
					}
					m.regexNeedles[leaf.LeafID] = needles
				}
			}
			continue
		}
		if m.needle == nil {
			m.needle = make(map[int][]byte)
		}
		b := []byte(leaf.Text)
		if m.accentNeedle == nil {
			m.accentNeedle = make(map[int]bool)
		}
		m.accentNeedle[leaf.LeafID] = !pq.contentCaseSensitive() && string(contentFoldAccent(b)) == string(contentFoldText(b))
		if !pq.contentCaseSensitive() {
			if m.accentNeedle[leaf.LeafID] {
				b = contentFoldAccent(b)
			} else {
				b = contentFoldCaseExact(b)
			}
		}
		m.needle[leaf.LeafID] = b
		if m.needleASCII == nil {
			m.needleASCII = make(map[int]bool)
		}
		m.needleASCII[leaf.LeafID] = isASCIIBytes(b)
		if m.grams == nil {
			m.grams = make(map[int][]uint32)
		}
		// The prefilter folds every leaf (a raw match implies the folded
		// match), so the grams always derive from the folded needle.
		m.grams[leaf.LeafID] = contentFilterGramsOf(contentFoldAccent([]byte(leaf.Text)))
	}
	return m
}

// leafGrams returns the precomputed filter grams constraining a term/phrase
// leaf (nil-safe: no matcher state means no constraint).
func (m *contentLeafMatcher) filterGramsFor(leafID int) []uint32 {
	if m == nil {
		return nil
	}
	return m.grams[leafID]
}

// regexGrams returns the precomputed required-literal grams for a regex leaf
// (nil = no sound prefilter).
func (m *contentLeafMatcher) filterRegexGramsFor(leafID int) [][][]uint32 {
	if m == nil {
		return nil
	}
	return m.regexGrams[leafID]
}

// regexNeedlesFor returns the precomputed verify-time literal needles for
// a regex leaf (nil-safe: no needles means no literal prefilter).
func (m *contentLeafMatcher) regexNeedlesFor(leafID int) [][]byte {
	if m == nil {
		return nil
	}
	return m.regexNeedles[leafID]
}

// contentRegexVerifyNeedles picks the cheapest disjunct's required runs as
// verify-time literal needles: the fewest runs, breaking ties by longest
// total length. A match must contain every run of its disjunct, so any
// disjunct's runs are a sound prefilter; the cheapest minimizes memmem
// work. Runs arrive lowercased from the extractor: case-insensitive
// verification tests them against the folded text, case-sensitive against
// the raw text (runs are exact there only when the pattern is already
// lowercase, else no needles — never a false negative).
func contentRegexVerifyNeedles(alts [][]string) [][]byte {
	best := -1
	for i, runs := range alts {
		if len(runs) == 0 {
			continue
		}
		if best < 0 || len(runs) < len(alts[best]) {
			best = i
			continue
		}
		if len(runs) == len(alts[best]) {
			a, b := 0, 0
			for _, r := range runs {
				a += len(r)
			}
			for _, r := range alts[best] {
				b += len(r)
			}
			if a > b {
				best = i
			}
		}
	}
	if best < 0 {
		return nil
	}
	out := make([][]byte, 0, len(alts[best]))
	for _, run := range alts[best] {
		out = append(out, []byte(run))
	}
	return out
}

// contentRegexNeedlesHold reports whether hay holds every needle (vacuously
// true with no needles: the leaf has no literal prefilter).
func contentRegexNeedlesHold(hay []byte, needles [][]byte) bool {
	for _, n := range needles {
		if !bytes.Contains(hay, n) {
			return false
		}
	}
	return true
}

// contentRegexLineAnchored reports whether the pattern carries a line anchor
// (^, $, \b is not one) outside a character class that Go's text-boundary
// semantics would misplace: (?m) is absent (patterns never carry it — the
// compiler adds only (?i)), so ^/$ bind the whole text and must be
// re-tested per line for ripgrep semantics.
func contentRegexLineAnchored(pattern string) bool {
	escaped, inClass := false, false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if escaped {
			escaped = false
			continue
		}
		switch c {
		case '\\':
			escaped = true
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '^', '$':
			if !inClass {
				return true
			}
		}
	}
	return false
}

// contentRegexMatchLines tests the pattern per line (split after \n,
// keeping a trailing \r on the line like ripgrep): ^/$ then anchor at
// line boundaries. Unanchored patterns never reach here — match tests
// them on the whole text — so results are identical for those, and
// anchored patterns gain ripgrep's line semantics.
func contentRegexMatchLines(re *regexp.Regexp, hay []byte) bool {
	start := 0
	for i := 0; i <= len(hay); i++ {
		if i == len(hay) || hay[i] == '\n' {
			line := hay[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if re.Match(line) {
				return true
			}
			start = i + 1
		}
	}
	return false
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
		if m.accentNeedle[leaf.LeafID] {
			b = contentFoldAccent(b)
		} else {
			b = contentFoldCaseExact(b)
		}
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

// match tests one leaf against a document. Regex leaves prefilter on
// their required literals (contentRegexVerifyNeedles) before running the
// engine: any miss decides without it. Regex leaves always run on the raw
// (case-preserving) text; the compiled pattern carries the (?i) flag when the
// query is case-insensitive. Go's regexp reports ^/$ at text boundaries;
// ripgrep line semantics anchor them per line, so a leaf whose pattern
// carries a line anchor is re-tested per line — narrowing only narrows the
// prefilter-free tail down to the lines, and never changes unanchored
// results.
func (m *contentLeafMatcher) match(raw, folded []byte, leaf contentLeaf) bool {
	if leaf.WholeWord {
		// content~ variants are dictionary terms: match them as whole words
		// (contentTermsOf boundaries) on the same haystack as plain terms.
		if !m.caseSensitive && folded == nil {
			folded = contentFoldAccent(raw)
		}
		return contentHasWholeWord(m.haystackFor(raw, folded), m.needleOf(leaf))
	}
	if leaf.Kind == contentLeafRegex {
		re := m.regex[leaf.LeafID]
		if re == nil {
			return false
		}
		// The needles are lowercased literal runs: a necessary condition on
		// the fold for case-insensitive leaves. Case-sensitive leaves skip
		// the prefilter (testing raw text against lowercased runs would
		// reject true matches).
		if needles := m.regexNeedlesFor(leaf.LeafID); len(needles) > 0 && !m.caseSensitive {
			if folded == nil {
				folded = contentFoldAccent(raw)
			}
			if !contentRegexNeedlesHold(folded, needles) {
				return false
			}
		}
		if contentRegexLineAnchored(leaf.Text) {
			return contentRegexMatchLines(re, raw)
		}
		return re.Match(raw)
	}
	if m.caseSensitive {
		return bytes.Contains(raw, m.needleOf(leaf))
	}
	if m.accentNeedle[leaf.LeafID] {
		// folded is the caller's once-per-entry contentFoldAccent(raw).
		if folded == nil {
			folded = contentFoldAccent(raw)
		}
		return bytes.Contains(folded, m.needleOf(leaf))
	}
	return bytes.Contains(contentFoldCaseExact(raw), m.needleOf(leaf))
}

// matchRawFirst is match with a lazy fold: a case-insensitive leaf whose
// lowercased needle already appears verbatim in the raw text is a match without
// folding. Otherwise, when both needle and text are ASCII, the match is
// decided by asciiCaseIndex with no allocation at all; only text containing
// non-ASCII bytes pays for the once-per-entry fold (cached through
// folded/foldedReady). A corpus of already-lowercase documents (the common
// case) then never folds at all. The match decision is identical to match:
// asciiCaseIndex finds exactly the ASCII matches and defers to the fold
// whenever non-ASCII bytes could hide one.
func (m *contentLeafMatcher) matchRawFirst(raw []byte, folded *[]byte, foldedReady *bool, leaf contentLeaf) bool {
	if leaf.WholeWord {
		if !m.caseSensitive && !*foldedReady {
			*folded, *foldedReady = contentFoldAccentAppend((*folded)[:0], raw), true
		}
		return m.match(raw, *folded, leaf)
	}
	if leaf.Kind == contentLeafRegex {
		re := m.regex[leaf.LeafID]
		if re == nil {
			return false
		}
		if needles := m.regexNeedlesFor(leaf.LeafID); len(needles) > 0 && !m.caseSensitive {
			// The needles are lowercased runs, so test them against the
			// fold — but a verbatim raw hit authorizes skipping the fold
			// (raw contains folded needles only when already lowercase).
			// Either way the verdict equals match: a miss on the fold
			// decides, a hold falls through to the engine below.
			rawHit := true
			for _, n := range needles {
				if !bytes.Contains(raw, n) {
					rawHit = false
					break
				}
			}
			if !rawHit {
				if !*foldedReady {
					// Shared per-entry cache: always the accent fold.
					*folded = contentFoldAccentAppend((*folded)[:0], raw)
					*foldedReady = true
				}
				if !contentRegexNeedlesHold(*folded, needles) {
					return false
				}
			}
		}
		if contentRegexLineAnchored(leaf.Text) {
			return contentRegexMatchLines(re, raw)
		}
		return re.Match(raw)
	}
	needle := m.needleOf(leaf)
	if bytes.Contains(raw, needle) {
		return true
	}
	if m.caseSensitive {
		return false
	}
	if m.needleASCII[leaf.LeafID] {
		if idx, ascii := asciiCaseIndex(raw, needle); ascii {
			return idx >= 0
		}
	}
	if !*foldedReady {
		*folded = contentFoldAccentAppend((*folded)[:0], raw)
		*foldedReady = true
	}
	if m.accentNeedle[leaf.LeafID] {
		return bytes.Contains(*folded, needle)
	}
	return bytes.Contains(contentFoldCaseExact(raw), needle)
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
	// Scalar short-circuit: the joint predicate ANDs the scalar and content
	// constraints (entryMatchesContentAt fails fast on entryMatchesScalar),
	// so an entry its name/scalar side already rejects must not pay a
	// block decompress for contentTextForEntry. This only skips work a later
	// check would reject anyway — results are identical. It matters for
	// compounds with a selective filename side (ext:cc + content:mutex skips
	// every non-.cc candidate's text fetch).
	if !entryMatchesScalar(entry, pq, matchPath) {
		return false
	}
	if m == nil {
		m = newContentLeafMatcher(pq)
	}
	// v11 filter gate (§6y): the per-file bloom bits decide whether the
	// positive content constraints can hold before any disk read. A gate
	// miss evaluates the predicate with all content leaves false — exact,
	// because the gate only fails an unsatisfiable positive constraint (a
	// content-free OR alternative still matches, NOT exclusions still hold;
	// see contentFilterGateOk). The gate is skipped for delta-covered keys:
	// delta text is fresher than the filter bits, so stale bits must never
	// reject fresh text.
	if contentFilterGateRejects(vol, entry, pq, m) {
		return entryMatchesContentAt(entry, pq, matchPath, make([]bool, m.size))
	}
	matched := make([]bool, m.size)
	if text, ok := vol.contentTextForEntryVerification(&entry, pq); ok {
		// Fold the document at most once for the whole entry, and only if a
		// case-insensitive leaf actually needs it; case-sensitive leaves read
		// the raw text. The fold lands in a pooled scratch buffer so each
		// verify reuses one buffer instead of allocating a per-file copy.
		var folded []byte
		if !m.caseSensitive {
			folded = contentFoldScratchGet()
			defer func() { contentFoldScratchPut(folded) }()
		}
		foldedReady := false
		for _, leaf := range m.leaves {
			if leaf.Sym {
				matched[leaf.LeafID] = m.sym[leaf.LeafID].defines(entry.Path, text)
			} else {
				matched[leaf.LeafID] = m.matchRawFirst(text, &folded, &foldedReady, leaf)
			}
		}
	}
	return entryMatchesContentAt(entry, pq, matchPath, matched)
}

// contentFilterGateRejects reports whether a volume's per-file filter proves
// the entry cannot satisfy the query's positive content constraints, so the
// caller evaluates the predicate with content-false instead of reading the
// file. It never rejects a delta-covered key (fresh text beats stale bits),
// a key outside the base docs (a new file the delta may cover), or a volume
// without a usable filter — those fall through to verification, which
// decides exactly.
func contentFilterGateRejects(vol *serviceVolumeIndex, entry Entry, pq parsedQuery, m *contentLeafMatcher) bool {
	if vol == nil || vol.content == nil || m == nil || !queryHasAnyContentLeaf(pq) {
		return false
	}
	key := entry.FRN
	if vol.contentWalkOriginAttached() {
		var ok bool
		key, ok = contentWalkKeyOfEntry(&entry)
		if !ok {
			return false
		}
	} else if key == 0 {
		return false
	}
	if delta := vol.content.deltaView(); delta != nil {
		if _, found, _ := delta.textFor(key); found {
			return false
		}
	}
	reader, resolver := vol.content.readerResolverView()
	if reader == nil || resolver == nil {
		return false
	}
	docIndex, ok := resolver.docForFRN(key)
	if !ok {
		return false
	}
	row, ok := reader.filterRow(uint32(docIndex))
	if !ok {
		// No filter row: verify from disk without narrowing (a builder that
		// never wrote the section still answers, only slower).
		return false
	}
	return !contentFilterGateOk(row, pq, m)
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
// wins over disk for a changed key; a tombstoned delta doc means absent.
// Otherwise (v11: no stored text) the file is read fresh from disk and
// extracted with the service policy — verify-direct freshness: a size/mtime
// mismatch against the indexed fingerprint means stale postings/filter, but
// the bytes on disk still decide, and the drain re-extracts the file through
// the existing pipeline. A missing/unreadable/unextractable file reads as
// absent, never an error.
//
// Disk reads count against the query's trace budget (contentDiskVerifyBudget;
// delta hits never count): past it verification short-circuits to absent and
// the trace is marked capped+incomplete, so the result is a visible subset
// (search) or a refused tally (count), never silent. A nil trace disables
// the cap for bounded callers (snippet attach runs post-limit).
func (vol *serviceVolumeIndex) contentTextForEntryTrace(entry *Entry, trace *searchTrace) ([]byte, bool) {
	return vol.contentTextForEntryPath(entry, "", trace, false, nil)
}

func (vol *serviceVolumeIndex) contentTextForEntryVerification(entry *Entry, pq parsedQuery) ([]byte, bool) {
	return vol.contentTextForEntryPath(entry, "", pq.Trace, false, pq.contentVerifyState)
}

// contentTextForEntryOptional is for presentation-only enrichment after the
// match set is fixed. Its reads share the query deadline/cap, but a skipped
// snippet never changes match completeness or removes the verified hit.
func (vol *serviceVolumeIndex) contentTextForEntryOptional(entry *Entry, trace *searchTrace) ([]byte, bool) {
	return vol.contentTextForEntryPath(entry, "", trace, true, nil)
}

// contentTextForEntry is contentTextForEntryTrace without a trace (uncapped,
// for bounded callers).
func (vol *serviceVolumeIndex) contentTextForEntry(entry *Entry) ([]byte, bool) {
	return vol.contentTextForEntryTrace(entry, nil)
}

// contentTextForEntryPath resolves an entry's text, verifying livePath
// instead of the entry path when non-empty (callers that resolved a fresher
// path than the entry carries).
func (vol *serviceVolumeIndex) contentTextForEntryPath(entry *Entry, livePath string, trace *searchTrace, optional bool, verifyState *atomic.Bool) ([]byte, bool) {
	if vol == nil || vol.content == nil {
		return nil, false
	}
	key := entry.FRN
	if vol.contentWalkOriginAttached() {
		var ok bool
		key, ok = contentWalkKeyOfEntry(entry)
		if !ok {
			return nil, false
		}
	} else if key == 0 {
		return nil, false
	}
	if delta := vol.content.deltaView(); delta != nil {
		if text, found, deleted := delta.textFor(key); found {
			if deleted {
				return nil, false
			}
			return text, true
		}
	}
	path := livePath
	if path == "" {
		path = entry.Path
	}
	if path == "" {
		return nil, false
	}
	read := func(p string) ([]byte, bool) {
		if trace != nil && optional && !trace.acquireOptionalDiskRead() {
			return nil, false
		}
		if trace != nil && !optional && !trace.acquireDiskRead() {
			if verifyState != nil {
				verifyState.Store(true)
			}
			// The wall-clock deadline (--budget) or the exact disk cap is
			// spent: stop reading, keep the hits found so far as a visible
			// partial page (or a refused tally). acquireDiskRead latched the
			// shared reservation and marked the query incomplete.
			return nil, false
		}
		if trace != nil && trace.deadlineReached() {
			if optional {
				trace.markOptionalEnrichmentSkipped()
			} else {
				trace.markBudgetExceeded()
				if verifyState != nil {
					verifyState.Store(true)
				}
			}
			return nil, false
		}
		return contentDiskReadForVerify(p, contentServiceExtractSettings())
	}
	if text, ok := read(path); ok {
		return text, true
	}
	if optional && trace != nil && trace.optionalEnrichmentSkipped() {
		return nil, false
	}
	// The live path is missing: fall back to the doc table's stored path.
	// This heals rename races where the record index lags the content index
	// (the record still names the old path while the fold already carries
	// the new one) and vice versa. Only a missing file falls back — an
	// extraction failure of a present file is final, so a corrupt file can
	// never serve another path's bytes.
	if _, serr := contentStatExists(path); serr != nil && os.IsNotExist(serr) {
		if docPath := vol.contentDocPathForKey(key); docPath != "" && docPath != path {
			if text, ok := read(docPath); ok {
				return text, true
			}
			if optional && trace != nil && trace.optionalEnrichmentSkipped() {
				return nil, false
			}
		}
	}
	return nil, false
}

// contentDocPathForKey returns the base doc table's stored path for a
// content key ("", when the key has no doc). It backs the rename-race
// fallback in contentTextForEntryPath.
func (vol *serviceVolumeIndex) contentDocPathForKey(key uint64) string {
	if vol == nil || vol.content == nil {
		return ""
	}
	reader, resolver := vol.content.readerResolverView()
	if reader == nil || resolver == nil {
		return ""
	}
	docIndex, ok := resolver.docForFRN(key)
	if !ok {
		return ""
	}
	return reader.docPath(uint32(docIndex))
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
	rawOff, _ := contentFoldSpanToRaw(raw, folded, off, off)
	return rawOff
}

// contentFoldSpanToRaw maps both boundaries of a folded-range match back to
// the raw text in a single rune walk (two contentFoldOffsetToRaw calls would
// each re-walk the shared prefix). off/end clamp exactly like the single
// mapper, so the pair is identical to mapping each boundary separately.
func contentFoldSpanToRaw(raw, folded []byte, off, end int) (rawOff, rawEnd int) {
	if off < 0 {
		off = 0
	}
	if off > len(folded) {
		off = len(folded)
	}
	rawOff = contentFoldRunesToRaw(raw, utf8.RuneCount(folded[:off]))
	if end <= off {
		return rawOff, rawOff
	}
	if end > len(folded) {
		end = len(folded)
	}
	// Continue from rawOff: the runes between off and end advance both Texts.
	rawEnd = rawOff
	for n := utf8.RuneCount(folded[off:end]); n > 0 && rawEnd < len(raw); n-- {
		_, size := utf8.DecodeRune(raw[rawEnd:])
		rawEnd += size
	}
	return rawOff, rawEnd
}

// contentFoldAccentWithOffsets maps each output byte back to the source rune
// that produced it. The returned final boundary maps to len(raw).
func contentFoldAccentWithOffsets(raw []byte) ([]byte, []int) {
	folded := contentFoldAccent(raw)
	offsets := make([]int, 0, len(folded)+1)
	for i := 0; i < len(raw); {
		_, size := utf8.DecodeRune(raw[i:])
		if size < 1 {
			size = 1
		}
		part := contentFoldAccent(raw[i : i+size])
		for range part {
			offsets = append(offsets, i)
		}
		i += size
	}
	offsets = append(offsets, len(raw))
	return folded, offsets
}

func contentFoldCaseExactWithOffsets(raw []byte) ([]byte, []int) {
	folded := contentFoldCaseExact(raw)
	offsets := make([]int, 0, len(folded)+1)
	for i := 0; i < len(raw); {
		_, size := utf8.DecodeRune(raw[i:])
		if size < 1 {
			size = 1
		}
		part := contentFoldCaseExact(raw[i : i+size])
		for range part {
			offsets = append(offsets, i)
		}
		i += size
	}
	offsets = append(offsets, len(raw))
	return folded, offsets
}

func contentAccentSpanToRaw(raw []byte, offsets []int, off, end int) (int, int) {
	if off < 0 {
		off = 0
	}
	if end < off {
		end = off
	}
	if off >= len(offsets) {
		return len(raw), len(raw)
	}
	if end >= len(offsets) {
		end = len(offsets) - 1
	}
	return offsets[off], offsets[end]
}

// contentFoldRunesToRaw advances n runes into raw.
func contentFoldRunesToRaw(raw []byte, n int) int {
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
func contentSnippet(path string, raw, folded []byte, positive []contentLeaf, m *contentLeafMatcher) string {
	for _, leaf := range positive {
		if leaf.Sym {
			// The snippet is the definition line itself.
			if ls, le, ok := m.sym[leaf.LeafID].definitionLine(path, raw); ok {
				return contentSnippetWindow(raw, ls, le-ls)
			}
			continue
		}
		if leaf.Kind == contentLeafRegex || !m.match(raw, folded, leaf) {
			continue
		}
		needle := m.needleOf(leaf)
		// An ASCII needle in ASCII text windows the raw bytes directly:
		// folding preserves ASCII offsets, so this renders exactly what
		// the fold-and-map path below produces, with no allocation.
		if !m.caseSensitive && m.needleASCII[leaf.LeafID] {
			if off, ascii := asciiCaseIndex(raw, needle); ascii {
				if off < 0 {
					continue
				}
				return contentSnippetWindow(raw, off, len(needle))
			}
		}
		hay := m.haystackFor(raw, folded)
		var offsets []int
		if !m.caseSensitive && m.accentNeedle[leaf.LeafID] {
			hay, offsets = contentFoldAccentWithOffsets(raw)
		}
		if !m.caseSensitive && !m.accentNeedle[leaf.LeafID] {
			hay, offsets = contentFoldCaseExactWithOffsets(raw)
		}
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
		var rawOff, rawEnd int
		if offsets != nil {
			rawOff, rawEnd = contentAccentSpanToRaw(raw, offsets, off, off+len(needle))
		} else {
			rawOff, rawEnd = contentFoldSpanToRaw(raw, hay, off, off+len(needle))
		}
		return contentSnippetWindow(raw, rawOff, rawEnd-rawOff)
	}
	return ""
}

// attachContentSnippets fills Entry.Snippet for content-query results. It is
// called after the result set is bounded by the limit, so the work is O(results).
// The matcher and positive-leaf list are built once per query by the caller.
func attachContentSnippets(entries []Entry, volByPath map[string]*serviceVolumeIndex, positive []contentLeaf, m *contentLeafMatcher, trace *searchTrace) {
	if len(entries) == 0 || len(volByPath) == 0 || m == nil || len(positive) == 0 {
		return
	}
	for i := range entries {
		vol := volByPath[entries[i].Path]
		if vol == nil {
			continue
		}
		if text, ok := vol.contentTextForEntryOptional(&entries[i], trace); ok {
			var folded []byte
			if !m.caseSensitive {
				folded = contentFoldAccentAppend(contentFoldScratchGet()[:0], text)
			}
			entries[i].Snippet = contentSnippet(entries[i].Path, text, folded, positive, m)
			if !m.caseSensitive {
				contentFoldScratchPut(folded)
			}
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
	// A sharded base only reads the shards that can hold docs under the
	// query's folder scope.
	mask := reader.shardMaskUnder(pq.Under)
	// Regex leaves always constrain through their required literals
	// (regexRequiredLiteralAlternatives): a match must take one disjunct
	// and contain every run of that disjunct, so the union of the
	// per-disjunct intersections is an exact superset of the match set —
	// the same narrowing term/phrase leaves get from trigram postings —
	// and the unchanged verification still decides. Mixed term+regex
	// queries AND the regex's literal streams with the terms' streams.
	// A regex with no provable literals stays unconstrained. Pure-regex
	// queries keep the historical scan-order candidates below, so their
	// results stay byte-identical.
	if len(pq.Content) > 0 {
		// A top-level Content leaf is a conjunct, so its postings alone are a
		// superset of the whole query; OR groups only narrow it further.
		for _, leaf := range pq.Content {
			if leaf.Kind == contentLeafRegex {
				// A regex match must contain one disjunct's required
				// literals (see contentRegexLeafStream); intersecting
				// them only narrows the superset, so verification still
				// decides exactly. A regex with no provable literals
				// stays unconstrained, as before.
				if s := contentRegexLeafStream(reader, leaf, mask); s != nil {
					streams = append(streams, s)
				}
				continue
			}
			term := string(contentFoldAccent([]byte(leaf.Text)))
			if !contentTermTrigramSafe(term) {
				// A gram the builder skipped (control byte) would look up
				// empty and falsely report zero; fall back to a scan.
				return nil, false, false
			}
			streams = append(streams, reader.candidateStreamIn(term, mask))
		}
	} else {
		// No top-level Content: coverage must come from content-driven OR
		// groups. A non-content alternative means the query can match without
		// content, so no postings superset exists; fall back.
		driven := false
		for _, group := range pq.OrGroups {
			groupStream, groupDriven, ok := contentOrGroupCandidateStream(reader, group, mask)
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
	// §6z dedupe: aliases own no postings, so expand primaries to their
	// duplicates BEFORE the cap accounts the set. The expanded set then
	// equals the pre-dedupe posting set exactly (an alias shares its
	// primary's text, hence its trigrams, both directions), so every
	// lane/cap/budget decision behaves as if nothing were deduplicated —
	// only the stored bytes shrink. A capped set still falls back to the
	// covering scan, which enumerates aliases directly. The resolver maps
	// every alias DocID through its own FRN, and the final sort restores
	// order.
	candidates = reader.expandAliasDocIDs(candidates)
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
// the union is unconstrained (a regex alternative with no prefilter).
// It is the same k-way union as the historical pairwise unionSortedDocIDs merge,
// but it materializes no alternative: the caller's budget+1 cap stops it before
// any full list decodes.
func contentOrGroupCandidateStream(reader *contentReader, group []parsedQuery, mask []bool) (stream func() (uint32, bool), driven, ok bool) {
	var streams []func() (uint32, bool)
	for i := range group {
		altStream, altDriven, altOK := contentAltCandidateStream(reader, group[i], mask)
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
// unconstrained (a regex leaf with no provable literals). Every source is a
// posting-backed stream, so an alternative is never materialized in full.
// contentLeavesAllRegex reports whether every positive content leaf in the
// query tree is a regex leaf: top-level Content and OR-nested alternatives.
// NOT leaves are exclusion and contribute no candidates, so they do not
// affect the verdict. A pure verdict keeps the historical scan-order
// candidates in nameTermCandidates; mixed term+regex queries order by rank.
func contentLeavesAllRegex(pq parsedQuery) bool {
	found := false
	var walk func(q parsedQuery) bool
	walk = func(q parsedQuery) bool {
		for _, leaf := range q.Content {
			if leaf.Kind != contentLeafRegex {
				return false
			}
			found = true
		}
		for _, g := range q.OrGroups {
			for i := range g {
				if !walk(g[i]) {
					return false
				}
			}
		}
		return true
	}
	return walk(pq) && found
}

// contentRegexLeafStream returns a lazy candidate stream for a regex leaf's
// required literals (see regexRequiredLiteralAlternatives): per disjunct,
// the intersection of its runs' trigram streams, unioned across disjuncts.
// A match must take one disjunct and contain every run of that disjunct, so
// the union is an exact superset of the match set and the unchanged
// verification still decides. Nil when no sound prefilter exists (a
// disjunct with no trigram-safe run, an unrelaxable construct, or too many
// disjuncts): the leaf stays unconstrained, exactly as before.
func contentRegexLeafStream(reader *contentReader, leaf contentLeaf, mask []bool) func() (uint32, bool) {
	if reader == nil || leaf.Kind != contentLeafRegex {
		return nil
	}
	disjuncts := regexRequiredLiteralAlternatives(leaf.Text)
	if len(disjuncts) == 0 {
		return nil
	}
	var alts []func() (uint32, bool)
	for _, runs := range disjuncts {
		var runStreams []func() (uint32, bool)
		for _, run := range runs {
			run = string(contentFoldAccent([]byte(run)))
			if !contentTermTrigramSafe(run) {
				// A gram the builder skipped (control byte) cannot
				// constrain; the disjunct's remaining runs still do.
				continue
			}
			// Runs arrive lowercased from the extractor, matching the
			// folded grams the builder indexed.
			runStreams = append(runStreams, reader.candidateStreamIn(run, mask))
		}
		if len(runStreams) == 0 {
			// This disjunct is unconstrained: a match may take it while
			// containing none of the other disjuncts' runs, so no sound
			// prefilter exists for the leaf.
			return nil
		}
		alts = append(alts, intersectDocIDStreams(runStreams))
	}
	if len(alts) == 1 {
		return alts[0]
	}
	return unionDocIDStreams(alts)
}

func contentAltCandidateStream(reader *contentReader, alt parsedQuery, mask []bool) (stream func() (uint32, bool), driven, ok bool) {
	var streams []func() (uint32, bool)
	for _, leaf := range alt.Content {
		driven = true
		if leaf.Kind == contentLeafRegex {
			if s := contentRegexLeafStream(reader, leaf, mask); s != nil {
				streams = append(streams, s)
			}
			continue
		}
		term := string(contentFoldAccent([]byte(leaf.Text)))
		if !contentTermTrigramSafe(term) {
			return nil, false, false
		}
		streams = append(streams, reader.candidateStreamIn(term, mask))
	}
	for _, group := range alt.OrGroups {
		groupStream, groupDriven, groupOK := contentOrGroupCandidateStream(reader, group, mask)
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
	if pq.MatchPath || pq.Under != "" || pq.lateVerified() ||
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
	if opts.Trace != nil && opts.Trace.deadlineReached() {
		opts.Trace.markBudgetExceeded()
	}
	// A disk verification cap hit during the tally means the count is
	// partial: refuse it outright, like the candidate-budget overrun — never
	// a silent partial number.
	if opts.Trace != nil && opts.Trace.stopDiskVerify() {
		return 0, errContentIncomplete
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
	// Deadline is checked before optional snippets: a timed-out enrichment must
	// not make an otherwise complete verified match set incomplete.
	if opts.Trace != nil && opts.Trace.deadlineReached() {
		opts.Trace.markBudgetExceeded()
	}
	// Snippets are attached last so the work is bounded by the returned set.
	if postVerify {
		attachContentSnippets(results, volByPath, positive, contentMatcher, opts.Trace)
	}
	opts.Trace.setPlannerMode("service-content")
	opts.Trace.setComplete(opts.Trace == nil || (!opts.Trace.ContentPartial && !opts.Trace.contentIncomplete()))
	return results, nil
}

// contentHasWholeWord reports whether word occurs in hay delimited by non-word
// bytes, with contentTermsOf's word bytes (ASCII alphanumerics, '_', and any
// byte >= 0x80), so it agrees with the indexed terms without tokenizing the
// whole document per leaf.
func contentHasWholeWord(hay, word []byte) bool {
	if len(word) == 0 {
		return false
	}
	isWord := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b >= 0x80
	}
	for off := 0; off < len(hay); {
		i := bytes.Index(hay[off:], word)
		if i < 0 {
			return false
		}
		i += off
		end := i + len(word)
		if (i == 0 || !isWord(hay[i-1])) && (end == len(hay) || !isWord(hay[end])) {
			return true
		}
		off = i + 1
	}
	return false
}
