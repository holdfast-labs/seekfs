package main

// Accent-insensitive name matching (name-search lessons, fsearch parity).
//
// foldAccentLower maps a name or query term to its accent-insensitive,
// case-insensitive form: NFD decompose, strip combining marks (Mn),
// fullwidth fold, lowercase. Both NFC and NFD input normalize identically,
// so `resume` finds `résumé` regardless of normalization form.
//
// Comparison-side only: index data (LOWR blobs, trigram postings, name
// order) is untouched, so no format change is needed. Posting lanes serve
// the unfolded term and the accent union in nameTermPosting adds the folded
// matches; verification below folds both sides.
//
// Exact-match semantics are preserved for `case:`/quoted terms: callers
// skip folding there and compare unfolded bytes.

import (
	"os"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// foldOffKillSwitch disables accent folding process-wide when
// SEEKFS_FOLD=0 (diagnostic A/B and escape hatch, mirroring
// SEEKFS_NAME_MEMO=0). Folding is on by default.
var foldOffKillSwitch = os.Getenv("SEEKFS_FOLD") == "0"

// nameNeedsFold reports whether s can fold differently than strings.ToLower:
// any non-ASCII byte. ASCII names fold to their lowercase form exactly.
func nameNeedsFold(s string) bool {
	return !asciiOnlyString(s)
}

// foldAccentLower returns the accent-insensitive lowercase form of s.
// ASCII input returns strings.ToLower(s) without any NFD work.
func foldAccentLower(s string) string {
	if asciiOnlyString(s) {
		return strings.ToLower(s)
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
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// nameContainsFolded reports whether the lowercased name contains term
// under the query folding rule: unfolded bytes first, then accent-folded
// (comparison-side only; index data is untouched). exact disables folding
// (`case:`/quoted).
func nameContainsFolded(lowerName, term string, exact bool) bool {
	if exact {
		return strings.Contains(lowerName, term)
	}
	var foldedTerm string
	if nameNeedsFold(term) {
		foldedTerm = foldAccentLower(term)
		if strings.Contains(lowerName, foldedTerm) {
			return true
		}
	} else {
		if strings.Contains(lowerName, term) {
			return true
		}
		foldedTerm = term
	}
	if !nameNeedsFold(lowerName) {
		return false
	}
	return strings.Contains(foldAccentLower(lowerName), foldedTerm)
}

// accentTermAddsMatches reports whether the accent union would add at least
// one base-record candidate for a single normalized term: some foldable
// name fold-contains it without containing it unfolded. Lane gates use this
// (and its query-level form) so queries unrelated to any accent fold keep
// their exact legacy lanes; only queries that actually gain matches reroute.
func accentTermAddsMatches(idx *Index, term string) bool {
	if idx == nil || term == "" || isVolumeQueryTerm(term) {
		return false
	}
	ids := accentRecordIDs(idx)
	if len(ids) == 0 {
		return false
	}
	foldedTerm := foldAccentLower(term)
	recordCount := idx.compactRecordCount()
	for _, id32 := range ids {
		id := int(id32)
		if id < 0 || id >= recordCount {
			continue
		}
		lower := idx.compactLowerNameAt(id)
		if strings.Contains(lower, term) {
			continue
		}
		if strings.Contains(foldAccentLower(lower), foldedTerm) {
			return true
		}
	}
	return false
}

// accentQueryAddsMatches reports whether the union would add matches for
// any fold-eligible plain term of pq. Conservative (OR over terms): an
// unnecessary reroute only costs speed on accent-bearing indexes, never
// correctness.
func accentQueryAddsMatches(idx *Index, pq parsedQuery) bool {
	if pq.CaseSensitive || idx == nil {
		return false
	}
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) || isQuotedTerm(pq, term) {
			continue
		}
		if accentTermAddsMatches(idx, term) {
			return true
		}
	}
	return false
}

// needsPathVerification reports whether pq carries constraints verifiable
// only against the reconstructed full path: path globs (`glob:a/**/*.cc`,
// `src/**/*.{c,h}`) and negated `in:` roots. Lanes that verify at the
// record level, or trust postings exactly, must decline these shapes; the
// entry gate (entryMatches) enforces them. Name anchors (`'exact`,
// `^prefix`, `suffix$`) need no decline: the record verifiers
// (compactRecordPrecheck, recordMatchesNonPath) enforce them.
func needsPathVerification(pq parsedQuery) bool {
	return len(pq.PathGlobs) > 0 || len(pq.NotUnders) > 0
}

// accentUnionActive reports whether the accent union can add true matches
// for pq on this volume: some foldable name fold-matches a fold-eligible
// plain term. Lane gates use it to decline proposal shapes built from
// unfolded bytes (trigram/component postings, rank-truncated scans); ASCII
// corpora — and queries unrelated to any accent fold — always report false,
// so those lanes keep their exact legacy behavior there.
func (vol *serviceVolumeIndex) accentUnionActive(pq parsedQuery) bool {
	if vol == nil || vol.index == nil || foldOffKillSwitch {
		return false
	}
	return accentQueryAddsMatches(vol.index, pq)
}

// anyVolumeAccentActive reports whether the accent union can add true
// matches on any volume: global lanes built from unfolded proposal bytes
// decline these queries so exhaustive (scan or posting-intersection) paths
// answer instead.
func anyVolumeAccentActive(volumes []*serviceVolumeIndex, pq parsedQuery) bool {
	for _, vol := range volumes {
		if vol.accentUnionActive(pq) {
			return true
		}
	}
	return false
}

// accentRecordIDs returns the record IDs whose lowercased name actually
// changes under accent folding (combining marks or fullwidth forms that
// strip). Names that merely contain non-ASCII bytes but fold to themselves
// (e.g. CJK) are excluded, so the union scan stays proportional to names
// folding can reveal. Built once per index generation and reset with the
// name identity; nil on ASCII corpora.
func accentRecordIDs(idx *Index) []int32 {
	if idx == nil {
		return nil
	}
	idx.nameIdentMu.Lock()
	defer idx.nameIdentMu.Unlock()
	if idx.accentBuilt && idx.accentCount == idx.compactRecordCount() {
		return idx.accentIDs
	}
	n := idx.compactRecordCount()
	var out []int32
	for id := 0; id < n; id++ {
		lower := idx.compactLowerNameAt(id)
		if !nameNeedsFold(lower) {
			continue
		}
		if foldAccentLower(lower) == lower {
			continue
		}
		out = append(out, int32(id))
	}
	idx.accentIDs = out
	idx.accentCount = n
	idx.accentBuilt = true
	return out
}
