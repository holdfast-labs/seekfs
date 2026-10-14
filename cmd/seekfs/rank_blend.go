package main

// Ranking integration: usage + git + proximity signals as BOUNDED
// tie-breakers (WORK_QUEUE item 1, gaps G7/G8).
//
// Rule (documented): signals reorder only WITHIN a match-quality band, never
// across bands. Bands, best first:
//
//   - Single-term filename queries (one plain term, no -path, no globs,
//     regexps or OR/NOT groups): exact name (0), name prefix (1), name
//     substring (2), matched-via-other-constraints only (3).
//   - Everything else (multi-term, -path, globs, regexps, groups): one band
//     per normalized file name (pure tie-break).
//   - Fuzzy top-up entries: one band per fuzzy-quality group (the materializer
//     reorders within equal (dist, wholeDist, prefix, windowPos, lenDiff)
//     groups only, so distance order never inverts).
//
// Within a band, keys apply in order: query-history combo (same normalized
// query -> same pick >= 3 times), near: hop distance, frecency (decayed open
// score), git dirt (dirty before clean), recent-commit membership,
// modification recency, current-file demotion (--current sinks the open
// file to the band bottom, fff current_file), then path as the
// deterministic final tie-break. An
// explicit sort: column, content queries (relevance owns their order), and
// counts never reorder.
//
// Result sets and counts never change (only order); with no signals anywhere
// (empty frecency store, no git: filter, no near: anchor, no --current)
// pages return untouched, so order is byte-identical to the base ranker and
// the extra cost is a few map lookups. The active path precomputes one
// evidence row per hit and sorts indices, so the comparator itself never allocates.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// signalHitKeys is one page hit's precomputed evidence.
type signalHitKeys struct {
	clean     string
	lower     string
	lowerName string
	kind      int
	comboHit  bool
	near      int
	score     float64
	git       int
	recent    bool
	biased    bool
	mod       int64
	// current marks the file the user already has open (--current): it
	// demotes to the bottom of its match-quality band (fff current_file).
	current bool
	name    string
}

// signalPageCtx holds one result page's precomputed evidence rows.
type signalPageCtx struct {
	pq       parsedQuery
	keys     []signalHitKeys
	useKinds bool
	hasBias  bool
	active   bool // any evidence distinguishes the page (or near/git-filter)
}

// signalReorderActive is the cheap gate: false means return the page
// untouched (byte-identical order, ~zero overhead).
func signalReorderActive(pq parsedQuery) bool {
	if pq.SortColumn != "" || queryHasAnyContentLeaf(pq) {
		return false
	}
	if len(pq.GitFilters) > 0 || pq.Near != "" || pq.Current != "" {
		return true
	}
	return !sharedSignalStore().empty()
}

// buildSignalPageCtx snapshots one evidence row per hit.
func buildSignalPageCtx(entries []Entry, pq parsedQuery) *signalPageCtx {
	ctx := &signalPageCtx{pq: pq}
	if len(entries) == 0 {
		return ctx
	}
	now := time.Now()
	store := sharedSignalStore()
	ctx.keys = make([]signalHitKeys, len(entries))
	ctx.useKinds = signalKindBandsUsable(pq)
	var kindTerm string
	if ctx.useKinds {
		kindTerm = nonVolumeTerms(pq.Terms)[0]
	}
	rawQuery := normalizeSignalQuery(pq.Raw)
	biasRoot := firstNonEmpty(pq.CWDBias, pq.RootBias)
	ctx.hasBias = biasRoot != ""
	needNear := pq.Near != ""
	needGitFilter := len(pq.GitFilters) > 0
	needCurrent := pq.Current != ""
	cleanPaths := make([]string, len(entries))
	for i := range entries {
		clean := cleanPathIfNeeded(entries[i].Path)
		cleanPaths[i] = clean
		k := &ctx.keys[i]
		k.clean = clean
		k.lower = entryLowerPath(entries[i])
		k.lowerName = entryLowerName(entries[i])
		if needCurrent && strings.EqualFold(clean, pq.Current) {
			k.current = true
		}
		if pq.CaseSensitive {
			k.lowerName = entries[i].Name
			if k.lowerName == "" {
				k.lowerName = filepath.Base(clean)
			}
		}
		k.mod = entries[i].ModUnix
		k.name = entries[i].Name
		k.git = 1
		if ctx.useKinds {
			k.kind = signalNameKindTerm(kindTerm, k.lowerName)
		}
		if ctx.hasBias {
			k.biased = pathUnder(clean, biasRoot)
		}
		if needNear {
			k.near = pathHopDistance(pq.Near, clean)
		}
	}
	if rawQuery != "" {
		combos := store.comboCounts(rawQuery, cleanPaths)
		for i := range entries {
			if combos[cleanPaths[i]] >= signalComboThreshold {
				ctx.keys[i].comboHit = true
			}
		}
	}
	scores := store.snapshotScores(cleanPaths, now)
	seenCombo, seenScore, seenGit, seenRecent := false, false, false, false
	for i := range entries {
		if ctx.keys[i].comboHit {
			seenCombo = true
		}
		if s := scores[cleanPaths[i]]; s > 0 {
			ctx.keys[i].score = s
			seenScore = true
		}
	}
	// Git evidence (stat + HEAD lookups) runs only when it can matter: a
	// git: filter is present, or usage signals already hit this page. This
	// keeps git wholly out of the hot path for unrelated queries. Roots
	// resolve once per directory; status + recency resolve once per hit.
	if needGitFilter || seenCombo || seenScore {
		roots := make(map[string]string, len(entries))
		for i := range entries {
			dir := filepath.Dir(cleanPaths[i])
			root, ok := roots[dir]
			if !ok {
				root, _ = gitRepoRootForPath(cleanPaths[i])
				roots[dir] = root
			}
			if root == "" {
				continue
			}
			isDir := entries[i].Mode&uint32(os.ModeDir) != 0
			boost, recent := gitBoostRecent(root, cleanPaths[i], isDir, pq.gitCtx)
			ctx.keys[i].git = boost
			if boost != 1 {
				seenGit = true
			}
			if recent {
				ctx.keys[i].recent = true
				seenRecent = true
			}
		}
	}
	ctx.active = needNear || needGitFilter || needCurrent || seenCombo || seenScore || seenGit || seenRecent
	return ctx
}

// applySignalReorder reorders one exact-tier result page within match-quality
// bands (see the package rule). It returns the input slice, possibly
// re-sorted; with no distinguishing signals it returns it untouched.
func applySignalReorder(entries []Entry, pq parsedQuery) []Entry {
	if len(entries) < 2 || !signalReorderActive(pq) {
		return entries
	}
	ctx := buildSignalPageCtx(entries, pq)
	if !ctx.active {
		return entries
	}
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return ctx.less(order[a], order[b]) < 0
	})
	sorted := make([]Entry, len(entries))
	for i, idx := range order {
		sorted[i] = entries[idx]
	}
	copy(entries, sorted)
	return entries
}

// reorderFuzzyGroup reorders one equal-fuzzy-quality group by signals only
// (no name band: the group already shares every distance key, so any order
// within it preserves distance ranking).
func reorderFuzzyGroup(group []Entry, ctx *signalPageCtx) {
	if len(group) < 2 || ctx == nil || !ctx.active {
		return
	}
	// The group is a slice of a larger page the ctx was built for; compare
	// by path-keyed evidence through fresh index rows is overkill here.
	// Groups are tiny (usually singletons), so build a minimal ctx.
	gctx := buildSignalPageCtx(group, ctx.pq)
	if !gctx.active {
		return
	}
	order := make([]int, len(group))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if n := compareSignalKeys(gctx, ia, ib); n != 0 {
			return n < 0
		}
		return compareSignalTiebreak(gctx, ia, ib) < 0
	})
	sorted := make([]Entry, len(group))
	for i, idx := range order {
		sorted[i] = group[idx]
	}
	copy(group, sorted)
}

// less orders two hits by index: bias partition, band, signal keys,
// tie-break.
func (ctx *signalPageCtx) less(i, j int) int {
	a, b := &ctx.keys[i], &ctx.keys[j]
	if ctx.hasBias && a.biased != b.biased {
		if a.biased {
			return -1
		}
		return 1
	}
	if ctx.useKinds {
		if a.kind != b.kind {
			return a.kind - b.kind
		}
	} else if a.lowerName != b.lowerName {
		if a.lowerName < b.lowerName {
			return -1
		}
		return 1
	}
	if n := compareSignalKeys(ctx, i, j); n != 0 {
		return n
	}
	return compareSignalTiebreak(ctx, i, j)
}

// compareSignalKeys applies the within-band precedence: combo, near,
// frecency, git dirt, recent-commit, modification recency.
func compareSignalKeys(ctx *signalPageCtx, i, j int) int {
	a, b := &ctx.keys[i], &ctx.keys[j]
	if a.comboHit != b.comboHit {
		if a.comboHit {
			return -1
		}
		return 1
	}
	if a.near != b.near {
		return a.near - b.near
	}
	if a.score != b.score {
		if a.score > b.score {
			return -1
		}
		return 1
	}
	if a.git != b.git {
		return a.git - b.git
	}
	if a.recent != b.recent {
		if a.recent {
			return -1
		}
		return 1
	}
	if a.mod != b.mod {
		// Modification recency is a tie-break only when signals are active
		// (the gate above); with an empty store the page returns untouched.
		if a.mod > b.mod {
			return -1
		}
		return 1
	}
	// Current-file demotion (fff current_file, --current): the file the
	// user already has open sinks to the bottom of its band, after every
	// other signal. Bands never invert: this only orders within one.
	if a.current != b.current {
		if a.current {
			return 1
		}
		return -1
	}
	return 0
}

// compareSignalTiebreak is the deterministic final order: lower path, path,
// name (mirrors compareSearchAllEntryPath plus the name).
func compareSignalTiebreak(ctx *signalPageCtx, i, j int) int {
	a, b := &ctx.keys[i], &ctx.keys[j]
	if a.lower != b.lower {
		if a.lower < b.lower {
			return -1
		}
		return 1
	}
	if a.clean != b.clean {
		if a.clean < b.clean {
			return -1
		}
		return 1
	}
	return strings.Compare(a.name, b.name)
}

// signalKindBandsUsable reports whether name-match-kind bands apply: exactly
// one plain term, filename matching, no name-quality-changing constraints.
func signalKindBandsUsable(pq parsedQuery) bool {
	if pq.MatchPath {
		return false
	}
	if len(nonVolumeTerms(pq.Terms)) != 1 {
		return false
	}
	if len(pq.Globs) != 0 || len(pq.Regexps) != 0 || len(pq.RegexTerms) != 0 ||
		len(pq.OrGroups) != 0 || len(pq.NotGroups) != 0 {
		return false
	}
	return true
}

// signalNameKindTerm classifies one hit's name-match quality: exact (0),
// prefix (1), substring (2), other-constraint (3).
func signalNameKindTerm(term, name string) int {
	switch {
	case name == term:
		return 0
	case strings.HasPrefix(name, term):
		return 1
	case strings.Contains(name, term):
		return 2
	default:
		return 3
	}
}

// signalNameKind classifies one hit for pq (term extracted once by callers
// that loop; here for single-shot use).
func signalNameKind(pq parsedQuery, e Entry) int {
	terms := nonVolumeTerms(pq.Terms)
	if len(terms) != 1 {
		return 0
	}
	name := entryLowerName(e)
	if pq.CaseSensitive {
		name = e.Name
		if name == "" {
			name = filepath.Base(e.Path)
		}
	}
	return signalNameKindTerm(terms[0], name)
}

// pathHopDistance counts path hops from the near anchor to target: parent
// hops up plus component hops down (0 when identical). Pure string math on
// cleaned paths; the anchor need not exist. A file anchor measures from its
// own directory, so near:<file> favors its siblings.
func pathHopDistance(anchor, target string) int {
	anchor = cleanPathIfNeeded(anchor)
	target = cleanPathIfNeeded(target)
	if anchor == "" || target == "" {
		return 1 << 30
	}
	if strings.EqualFold(anchor, target) {
		return 0
	}
	anchorDir := anchor
	if !strings.HasSuffix(anchor, `\`) && !strings.HasSuffix(anchor, "/") {
		// A file anchor measures from its own directory so near:<file>
		// favors its siblings; an existing directory measures from itself.
		// One stat per query (not per candidate) keeps this exact.
		if fi, err := os.Stat(anchor); err != nil || !fi.IsDir() {
			anchorDir = filepath.Dir(anchor)
		}
	}
	rel, err := filepath.Rel(anchorDir, filepath.Dir(target))
	if err != nil {
		return 1 << 30
	}
	if rel == "." {
		return 0
	}
	return len(strings.Split(rel, string(filepath.Separator)))
}
