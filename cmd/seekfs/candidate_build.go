package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (vol *serviceVolumeIndex) plannedCandidates(pq parsedQuery) ([]int, bool) {
	if out, ok := vol.exactTopPlannedCandidates(pq); ok {
		if len(pq.Exts) == 1 {
			volume := ""
			if vol != nil && vol.index != nil {
				volume = vol.index.Volume
			}
			pq.Trace.addTerm(traceTerm{Term: pq.Exts[0], Kind: "extension", Source: "planned:ext-top", CountHint: len(out), Exact: true, Volume: volume})
		}
		pq.Trace.setSource("planned:ext-top", len(out))
		return out, true
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		return nil, false
	}
	if out, scanned, ok := plan.executeTop(pq); ok {
		pq.Trace.addTerms(plan.traceTerms())
		pq.Trace.setSource("planned:or-group-lazy-top", scanned)
		return out, true
	}
	out := plan.execute()
	if compactCandidateCanSkipEntryMatches(pq, true) && pq.Limit > 0 {
		out = topCandidateIDsByRank(out, pq.Limit, vol.index, vol.rankForQuery(pq))
	}
	// topCandidateIDsByRank intentionally only knows persisted ranks.  Apply
	// the requested Entry comparator afterward so equal/rankless candidates
	// retain deterministic path/name tie ordering.
	sortCandidateIDs(out, pq, vol.index, vol.rankForQuery(pq))
	pq.Trace.addTerms(plan.traceTerms())
	pq.Trace.setSource("planned:"+plan.sourceSummary(), len(out))
	return out, true
}

func (vol *serviceVolumeIndex) exactTopPlannedCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.queryIndex == nil || pq.Limit <= 0 ||
		len(pq.Exts) != 1 || len(pq.Globs) > 0 || len(pq.Dirs) > 0 ||
		pq.Type != "" || pq.Under != "" || pq.HasModAfter || pq.Exists ||
		(pq.SortColumn != "" && pq.SortColumn != "size" && pq.SortColumn != "modified" && pq.SortColumn != "extension" && pq.SortColumn != "type" && pq.SortColumn != "path") ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 {
		return nil, false
	}
	terms := nonVolumeTerms(pq.Terms)
	if len(terms) > 0 {
		return nil, false
	}
	ids, ok := vol.extTopPosting(pq.Exts[0], pq.Limit, pq)
	if !ok {
		return nil, false
	}
	return ids, true
}

func nonVolumeTerms(terms []string) []string {
	out := make([]string, 0, len(terms))
	for _, term := range terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		out = append(out, term)
	}
	return out
}

func (vol *serviceVolumeIndex) plannedCount(pq parsedQuery) (int, bool) {
	return vol.plannedCountHidden(pq, hiddenBaseIDs{})
}

// plannedCountHidden is plannedCount plus an id-level exclusion set (base
// tombstoned/shadowed ids from the active v9 overlay snapshot). It never
// materializes an Entry unless path reconstruction is unavoidable, and it
// filters candidate ids against hidden before evaluating them so counts
// stay exact while an overlay is active (review G7 / plan R2.6).
func (vol *serviceVolumeIndex) plannedCountHidden(pq parsedQuery, hidden hiddenBaseIDs) (int, bool) {
	pq.CountOnly = true
	// Single bare term with no path scope and no other filters: count directly
	// over records in parallel without materializing the candidate slice.  A
	// short/broad term like `x` matches millions of names, so building the full
	// name posting just to count it is wasteful.
	if terms := nonVolumeTerms(pq.Terms); len(terms) == 1 && !pq.MatchPath &&
		pq.Type == "" && pq.Under == "" && !pq.Exists && !pq.HasModAfter &&
		len(pq.Exts) == 0 && len(pq.Dirs) == 0 && len(pq.Globs) == 0 &&
		len(pq.Regexps) == 0 && len(pq.RegexTerms) == 0 && len(pq.Parents) == 0 &&
		len(pq.SizeFilters) == 0 && len(pq.DateFilters) == 0 && len(pq.AttrFilters) == 0 &&
		len(pq.OrGroups) == 0 && len(pq.NotGroups) == 0 &&
		pq.CWDBias == "" && pq.RootBias == "" && !pq.CaseSensitive {
		if count, ok := vol.countBareTermParallel(terms[0], hidden, pq); ok {
			pq.Trace.addTerm(traceTerm{Term: terms[0], Kind: "name-substring", Source: "parallel-name-count", CountHint: count, Exact: true})
			pq.Trace.setSource("parallel-name-count", count)
			pq.Trace.setComplete(true)
			return count, true
		}
	}
	// Name-memo count first: bit checks plus scalar record reads, no
	// posting decode and no path reconstruction at all. Hidden ids are
	// skipped inline; overlay records are merged by the caller, exactly as
	// with the plan-execute path below.
	if count, ok := vol.memoCount(pq, hidden); ok {
		pq.Trace.setSource("memo-count", count)
		pq.Trace.setComplete(true)
		return count, true
	}
	plan, ok := vol.buildCandidatePlan(pq)
	if !ok {
		return 0, false
	}
	if plan.empty {
		return 0, true
	}
	if count, scanned, ok := plan.executeUnionCount(pq, hidden); ok {
		pq.Trace.addTerms(plan.traceTerms())
		pq.Trace.setSource("planned:or-group-lazy-count", scanned)
		return count, true
	}
	ids := plan.execute()
	pq.Trace.addTerms(plan.traceTerms())
	count := 0

	// Fast path: when the query can be decided from the record alone (no path
	// substring matching, no path-scoped filters), count without reconstructing
	// the full path or allocating an Entry per candidate. This is the common
	// case for `count ext:md`, `count type:file ext:go`, etc., and is where
	// Everything's -get-result-count was beating us.
	if !queryNeedsPath(pq) {
		for _, id := range ids {
			if id < 0 || id >= vol.index.compactRecordCount() {
				continue
			}
			if !hidden.empty() && hidden.contains(id) {
				continue
			}
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			if vol.recordMatchesNonPath(id, rec, pq) {
				count++
			}
		}
		return count, true
	}

	pathCache := make(map[int]string)
	for _, id := range ids {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		if !hidden.empty() && hidden.contains(id) {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !compactRecordPrecheck(rec, pq, pq.MatchPath) {
			continue
		}
		path := vol.index.reconstructCompactPathCached(id, pathCache)
		entry := Entry{
			Path:        path,
			Name:        rec.Name,
			LowerPath:   strings.ToLower(path),
			LowerName:   vol.index.compactLowerNameAt(id),
			Mode:        rec.Mode,
			Size:        rec.Size,
			ModUnix:     rec.ModUnix,
			IndexSource: vol.index.Source,
		}
		if entryMatches(entry, pq, pq.MatchPath) {
			count++
		}
	}
	return count, true
}

// queryNeedsPath reports whether deciding a match requires the reconstructed
// full path rather than just the record's own fields.
func queryNeedsPath(pq parsedQuery) bool {
	if pq.MatchPath && len(pq.Terms) > 0 {
		return true
	}
	if len(pq.Dirs) > 0 || len(pq.Regexps) > 0 || len(pq.Parents) > 0 {
		return true
	}
	if pq.Under != "" || pq.Exists {
		return true
	}
	for _, group := range pq.OrGroups {
		for _, alt := range group {
			if queryNeedsPath(alt) {
				return true
			}
		}
	}
	for _, neg := range pq.NotGroups {
		if queryNeedsPath(neg) {
			return true
		}
	}
	return false
}

// recordMatchesNonPath verifies a record against a query that does not require
// path reconstruction. It mirrors entryMatches but operates on the compact
// record's own name/size/mtime/mode fields.
func (vol *serviceVolumeIndex) recordMatchesNonPath(id int, rec CompactRecord, pq parsedQuery) bool {
	cmpName := normalizeCase(rec.Name, pq.CaseSensitive)
	if !pq.MatchPath && !containsAll(cmpName, pq.Terms) {
		return false
	}
	if pq.Type == "file" && rec.Mode&uint32(os.ModeDir) != 0 {
		return false
	}
	if pq.Type == "dir" && rec.Mode&uint32(os.ModeDir) == 0 {
		return false
	}
	if !attrFiltersMatch(rec.Mode, pq.AttrFilters) {
		return false
	}
	if pq.HasModAfter {
		if rec.ModUnix == 0 || !time.Unix(0, rec.ModUnix).After(pq.ModifiedAfter) {
			return false
		}
	}
	for _, ext := range pq.Exts {
		actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
		if normalizeCase(actual, pq.CaseSensitive) != ext {
			return false
		}
	}
	for _, glob := range pq.Globs {
		ok, err := filepath.Match(glob, cmpName)
		if err != nil || !ok {
			return false
		}
	}
	for _, sf := range pq.SizeFilters {
		if !sf.matches(rec.Size) {
			return false
		}
	}
	for _, df := range pq.DateFilters {
		if !df.matches(rec.ModUnix) {
			return false
		}
	}
	for _, group := range pq.OrGroups {
		matched := false
		for _, alt := range group {
			if vol.recordMatchesNonPath(id, rec, alt) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, neg := range pq.NotGroups {
		if vol.recordMatchesNonPath(id, rec, neg) {
			return false
		}
	}
	return true
}

func (vol *serviceVolumeIndex) buildCandidatePlan(pq parsedQuery) (candidatePlan, bool) {
	plan := candidatePlan{vol: vol, pq: pq}
	if vol == nil || vol.index == nil || pq.CaseSensitive {
		return plan, false
	}
	var underRoots []int
	underEstimatedSize := -1
	addRequired := func(name string, ids []int) bool {
		if len(ids) == 0 {
			plan.empty = true
			return false
		}
		plan.sources = append(plan.sources, candidatePlanSource{
			name: name,
			ids:  uniqueSortedInts(append([]int(nil), ids...)),
		})
		return true
	}
	addPostingRequired := func(name string, candidate postingCountCandidate) bool {
		if candidate.len() == 0 {
			plan.empty = true
			return false
		}
		plan.sources = append(plan.sources, candidatePlanSource{
			name:       name,
			posting:    candidate,
			hasPosting: true,
		})
		return true
	}

	if pq.Under != "" {
		under := filepath.Clean(pq.Under)
		if vol.index.Volume != "" && !strings.EqualFold(filepath.VolumeName(under), vol.index.Volume) {
			plan.empty = true
			return plan, true
		}
		underRoots = vol.underRootIDs(under)
		if len(underRoots) == 0 {
			plan.underPathFallback = under
		}
		if len(underRoots) > 0 {
			underEstimatedSize = vol.estimateUnderDescendantCount(underRoots)
		}
	}

	for _, ext := range pq.Exts {
		if candidate, ok := vol.extPostingCountCandidate(ext); ok {
			if !addPostingRequired("ext:"+ext, candidate) {
				return plan, true
			}
			continue
		}
		if !addRequired("ext:"+ext, vol.extPosting(ext)) {
			return plan, true
		}
	}
	globExts, globsOK := simpleGlobExts(pq.Globs)
	if globsOK {
		for _, ext := range globExts {
			if candidate, ok := vol.extPostingCountCandidate(ext); ok {
				if !addPostingRequired("glob-ext:"+ext, candidate) {
					return plan, true
				}
				continue
			}
			if !addRequired("glob-ext:"+ext, vol.extPosting(ext)) {
				return plan, true
			}
		}
	} else {
		for _, ext := range complexGlobExts(pq.Globs) {
			if candidate, ok := vol.extPostingCountCandidate(ext); ok {
				if !addPostingRequired("glob-ext:"+ext, candidate) {
					return plan, true
				}
				continue
			}
			if !addRequired("glob-ext:"+ext, vol.extPosting(ext)) {
				return plan, true
			}
		}
	}
	if pq.Type == "dir" {
		if vol.queryIndex != nil && vol.queryIndex.dirsReady {
			if !addPostingRequired("type:dir", postingCountCandidate{ids: vol.queryIndex.dirs}) {
				return plan, true
			}
		}
	}
	for _, parent := range pq.Parents {
		if !addRequired("parent:"+parent, vol.parentIDs(parent)) {
			return plan, true
		}
	}
	for _, mask := range pq.AttrFilters {
		ids, ok := vol.attrIDsForMask(mask)
		if !ok {
			continue
		}
		if !addRequired("attrib:"+attribMaskString(mask), ids) {
			return plan, true
		}
	}
	for _, dir := range pq.Dirs {
		if !vol.pathComponentPostingAvailable(dir) {
			continue
		}
		roots := vol.pathComponentRootIDs(dir)
		if len(roots) == 0 {
			plan.empty = true
			return plan, true
		}
		plan.sources = append(plan.sources, candidatePlanSource{
			name:  "dir:" + dir,
			vol:   vol,
			roots: uniqueSortedInts(roots),
		})
	}
	// OR groups: a record must match at least one alternative, so the candidate
	// source is the union of each alternative's posting. We only build a posting
	// source when every alternative is cheaply postable (ext/glob-ext/term);
	// otherwise the group is verified later against the full candidate set.
	for _, group := range pq.OrGroups {
		source, ok := vol.orGroupPlanSource(group, pq.MatchPath)
		if !ok {
			continue
		}
		if source.len() == 0 {
			plan.empty = true
			return plan, true
		}
		plan.sources = append(plan.sources, source)
	}

	// Cheap structural filters above are verified against the full query later.
	// Add bounded path/name term postings for remaining terms so the plan drives
	// off the smallest source and intersects the rest lazily.  This keeps a
	// loose multi-term query like `Dataset trainingdata nrrd` from materializing
	// a huge promoted extension posting and verifying every other term against
	// it.  Only selective (bounded) postings are added; a broad term that cannot
	// be bounded stays verification-only so correctness never depends on a cap.
	if pq.MatchPath && hasNonVolumeTerm(pq.Terms) {
		for _, term := range pathPlanProbeTerms(pq.Terms) {
			ids, ok := vol.boundedPathTermPlanSource(term)
			if !ok {
				continue
			}
			if !addRequired("path-term:"+term, ids) {
				return plan, true
			}
		}
		if len(plan.sources) == 0 && len(underRoots) == 0 {
			// Path mode with no usable source at all: decline so the search
			// uses the streaming name-order scan instead of materializing a
			// broad posting on every call.
			return plan, false
		}
	} else if !pq.MatchPath {
		for _, term := range pq.Terms {
			if !addRequired("term:"+term, vol.namePlanTermPosting(term)) {
				return plan, true
			}
		}
	}
	// Glob literals are a safe name-substring prefilter for complex globs (a
	// record matching `glob:*foo*` necessarily has `foo` in its name), so add
	// them as sources regardless of path scope.  This narrows the candidate set
	// before the full glob is verified, avoiding a full-volume scan for broad
	// globs that are not reducible to a single extension posting.
	if !globsOK {
		for _, term := range globLiteralTerms(pq.Globs, pq.CaseSensitive) {
			if list := vol.nameTermPosting(term); len(list) > 0 {
				if !addRequired("glob-literal:"+term, list) {
					return plan, true
				}
			}
		}
	}

	if len(underRoots) > 0 && shouldUseUnderPlanSource(underEstimatedSize, plan.sources) {
		plan.sources = append(plan.sources, candidatePlanSource{
			name:  "under",
			vol:   vol,
			roots: uniqueSortedInts(underRoots),
		})
	}

	if len(plan.sources) == 0 {
		return plan, false
	}
	return plan, true
}
