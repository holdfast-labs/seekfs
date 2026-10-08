package main

import (
	"container/heap"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func heapIDsByRank(h candidateRankMaxHeap, rankOf func(int) int) []int {
	if len(h) == 0 {
		return nil
	}
	out := make([]int, len(h))
	for i := range h {
		out[i] = h[i].id
	}
	sortIDsByRank(out, rankOf)
	return out
}

func (vol *serviceVolumeIndex) extensionShapedPathTermCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CaseSensitive ||
		pq.Under != "" || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.Type != "" || pq.HasModAfter || pq.Exists ||
		len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) != 1 {
		return nil, false
	}
	term := ""
	for _, candidate := range pq.Terms {
		if isVolumeQueryTerm(candidate) {
			continue
		}
		term = candidate
		break
	}
	ext, ok := extensionShapedPathTerm(term)
	if !ok {
		return nil, false
	}
	base := vol.extPosting(ext)
	if len(base) == 0 {
		return nil, false
	}
	nameMatches, ok := vol.nameTrigramNameTermPosting(term)
	if !ok {
		nameMatches = vol.nameTermPosting(term)
	}
	threshold := max(4096, pq.Limit*64)
	if len(base) > threshold || len(nameMatches) > threshold {
		return nil, false
	}
	estimated := len(base) + len(nameMatches)
	for _, id := range nameMatches {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		estimated += vol.estimatedDescendantOrSelfCount(id)
		if estimated > serviceComponentTrigramExpansionMaxIDs {
			return nil, false
		}
	}
	seen := make(map[int]struct{}, estimated)
	out := make([]int, 0, estimated)
	add := func(id int) {
		if id < 0 || id >= vol.index.compactRecordCount() {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, id := range base {
		add(id)
	}
	for _, id := range nameMatches {
		add(id)
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		for _, childID := range vol.underDescendants(id) {
			add(int(childID))
			if len(out) > serviceComponentTrigramExpansionMaxIDs {
				return nil, false
			}
		}
	}
	sort.Ints(out)
	return out, true
}

func (vol *serviceVolumeIndex) extensionShapedPathTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 ||
		pq.CaseSensitive || pq.Under != "" || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.Type != "" || pq.HasModAfter || pq.Exists ||
		pq.SortColumn != "" ||
		len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) != 1 || pq.hiddenBlocksTruncation() {
		return nil, false
	}
	term := ""
	for _, candidate := range pq.Terms {
		if isVolumeQueryTerm(candidate) {
			continue
		}
		term = candidate
		break
	}
	ext, ok := extensionShapedPathTerm(term)
	if !ok {
		return nil, false
	}
	nameMatches, ok := vol.nameTrigramNameTermPosting(term)
	if !ok {
		return nil, false
	}
	if len(nameMatches) >= pq.Limit && !vol.hasDirectoryCandidate(nameMatches) {
		return topCandidateIDsByRank(append([]int(nil), nameMatches...), pq.Limit, vol.index, vol.rankForQuery(pq)), true
	}
	ids, _ := vol.extTopPosting(ext, pq.Limit, pq)
	seen := make(map[int]struct{}, len(nameMatches)+len(ids))
	out := make([]int, 0, len(nameMatches)+len(ids))
	addNameMatch := func(id int) {
		if id < 0 || id >= vol.index.compactRecordCount() {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	addPathMatch := func(id int) {
		if id < 0 || id >= vol.index.compactRecordCount() {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !vol.index.compactPathContainsTerm(id, term) {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, id := range nameMatches {
		addNameMatch(id)
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		for _, childID := range vol.underDescendants(id) {
			addPathMatch(int(childID))
			if len(out) > serviceComponentTrigramExpansionMaxIDs {
				return nil, false
			}
		}
	}
	for _, id := range ids {
		addPathMatch(id)
	}
	if len(out) < pq.Limit {
		return nil, false
	}
	return topCandidateIDsByRank(out, pq.Limit, vol.index, vol.rankForQuery(pq)), true
}

func (vol *serviceVolumeIndex) bareExtensionMultiPathTopCandidates(pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || !pq.MatchPath || pq.CountOnly || pq.Limit <= 0 ||
		pq.CaseSensitive || pq.Under != "" || len(pq.Dirs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 ||
		pq.Type != "" || pq.HasModAfter || pq.Exists ||
		pq.SortColumn != "" ||
		len(pq.Exts) > 0 || len(pq.Globs) > 0 ||
		len(pq.SizeFilters) > 0 || len(pq.DateFilters) > 0 || len(pq.AttrFilters) > 0 ||
		pq.CWDBias != "" || pq.RootBias != "" ||
		countNonVolumeTerms(pq.Terms) < 2 || pq.hiddenBlocksTruncation() {
		return nil, false
	}
	hasAnchor := false
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) || strings.ContainsAny(term, `\/*?[]:`) {
			continue
		}
		if vol.pathTermIsUsableExtensionCandidate(term) {
			continue
		}
		if len(term) >= 4 {
			hasAnchor = true
			break
		}
	}
	if !hasAnchor {
		return nil, false
	}
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		ext, ok := pathExtensionCandidateTerm(term)
		if !ok {
			continue
		}
		if !strings.HasPrefix(term, ".") {
			candidate, ok := vol.extPostingCountCandidate(ext)
			if !ok || candidate.len() == 0 {
				continue
			}
		}
		if candidates, ok := vol.extTopPathTermCandidates(ext, pq.Terms, pq.Limit); ok {
			return candidates, true
		}
		if candidates, ok := vol.extPathTermPostingCandidates(ext, pq.Terms, pq.Limit); ok {
			return candidates, true
		}
		if candidates, ok := vol.extPostingPathTermCandidates(ext, pq.Terms, pq.Limit); ok {
			return candidates, true
		}
	}
	return nil, false
}

func (vol *serviceVolumeIndex) pathTermIsUsableExtensionCandidate(term string) bool {
	ext, ok := pathExtensionCandidateTerm(term)
	if !ok {
		return false
	}
	if strings.HasPrefix(term, ".") {
		return true
	}
	candidate, ok := vol.extPostingCountCandidate(ext)
	return ok && candidate.len() > 0
}

func (vol *serviceVolumeIndex) hasDirectoryCandidate(ids []int) bool {
	if vol == nil || vol.index == nil {
		return true
	}
	recordCount := vol.index.compactRecordCount()
	for _, id := range ids {
		if id < 0 || id >= recordCount {
			continue
		}
		rec := vol.index.compactRecord(id)
		if !rec.Deleted && rec.Mode&uint32(os.ModeDir) != 0 {
			return true
		}
	}
	return false
}

func extensionShapedPathTerm(term string) (string, bool) {
	if len(term) < 2 || term[0] != '.' || strings.ContainsAny(term, `\/*?[]:`) {
		return "", false
	}
	ext := strings.TrimPrefix(term, ".")
	if ext == "" || strings.Contains(ext, ".") {
		return "", false
	}
	return ext, true
}

func bareExtensionCandidateTerm(term string) bool {
	if len(term) < 2 || len(term) > 8 || strings.ContainsAny(term, `.\/*?[]:`) {
		return false
	}
	for _, r := range term {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func pathExtensionCandidateTerm(term string) (string, bool) {
	if ext, ok := extensionShapedPathTerm(term); ok {
		return ext, true
	}
	if bareExtensionCandidateTerm(term) {
		return term, true
	}
	return "", false
}

func (vol *serviceVolumeIndex) extPathTermPostingCandidates(ext string, terms []string, limit int) ([]int, bool) {
	if vol == nil || vol.index == nil || ext == "" || limit <= 0 || len(terms) == 0 {
		return nil, false
	}
	best := []int(nil)
	bestSet := false
	checkedAnchor := false
	for _, term := range terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		if vol.pathTermIsUsableExtensionCandidate(term) {
			continue
		}
		if strings.ContainsAny(term, `\/*?[]:`) {
			continue
		}
		checkedAnchor = true
		ids, ok := vol.boundedPathTermPlanSource(term)
		if !ok {
			if expanded, expandedOK := vol.pathTermPostingForExtFilter(term, serviceComponentMultiTermScanMaxIDs); expandedOK {
				ids = expanded
				ok = true
			}
		}
		if !ok {
			if vol.pathTermDefinitelyEmpty(term) {
				return []int{}, true
			}
			continue
		}
		if !bestSet || len(ids) < len(best) {
			best = ids
			bestSet = true
		}
	}
	if checkedAnchor && !bestSet {
		return nil, false
	}
	if !bestSet {
		return nil, false
	}
	out := make([]int, 0, min(limit, len(best)))
	for _, id := range best {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		actual := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
		if !strings.EqualFold(actual, ext) || !vol.index.compactPathContainsAll(id, terms) {
			continue
		}
		out = append(out, id)
	}
	return topCandidateIDsByRank(out, limit, vol.index, vol.nameOrderRanks()), true
}

func (vol *serviceVolumeIndex) pathTermDefinitelyEmpty(term string) bool {
	if vol == nil || vol.index == nil || term == "" || isVolumeQueryTerm(term) {
		return false
	}
	if len(vol.pathComponentRootIDs(term)) > 0 {
		return false
	}
	if ids, ok := vol.nameTrigramNameTermPosting(term); ok {
		return len(ids) == 0
	}
	return len(vol.nameTermPosting(term)) == 0
}

func (vol *serviceVolumeIndex) pathTermPostingForExtFilter(term string, maxIDs int) ([]int, bool) {
	if vol == nil || vol.index == nil || term == "" || maxIDs <= 0 {
		return nil, false
	}
	seen := make(map[int]struct{})
	out := make([]int, 0, 256)
	add := func(id int) bool {
		if id < 0 || id >= vol.index.compactRecordCount() {
			return true
		}
		if _, exists := seen[id]; exists {
			return true
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			return true
		}
		seen[id] = struct{}{}
		out = append(out, id)
		return len(out) <= maxIDs
	}
	for _, root := range vol.pathComponentRootIDs(term) {
		if root < 0 || root >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(root)
		if rec.Deleted {
			continue
		}
		if rec.Mode&uint32(os.ModeDir) == 0 {
			if !add(root) {
				return nil, false
			}
			continue
		}
		if !vol.hasDescendantIndex() || vol.estimatedDescendantOrSelfCount(root) > maxIDs {
			return nil, false
		}
		for _, childID := range vol.underDescendantsLimited(root, maxIDs+1) {
			if !add(int(childID)) {
				return nil, false
			}
		}
	}
	nameMatches, ok := vol.completeNameTrigramNameTermPostingLimited(term, servicePathNameTrigramCandidateMaxIDs)
	if !ok {
		nameMatches = vol.nameTermPosting(term)
		if len(nameMatches) > servicePathNameTrigramCandidateMaxIDs {
			return nil, false
		}
	}
	if len(nameMatches) == 0 && len(out) == 0 {
		return []int{}, true
	}
	estimated := 0
	for _, id := range nameMatches {
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		if rec.Mode&uint32(os.ModeDir) == 0 {
			estimated++
		} else {
			if !vol.hasDescendantIndex() {
				return nil, false
			}
			estimated += vol.estimatedDescendantOrSelfCount(id)
		}
		if estimated > maxIDs {
			return nil, false
		}
	}
	for _, id := range nameMatches {
		if !add(id) {
			return nil, false
		}
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) == 0 {
			continue
		}
		if !vol.hasDescendantIndex() {
			return nil, false
		}
		for _, childID := range vol.underDescendantsLimited(id, maxIDs+1) {
			if !add(int(childID)) {
				return nil, false
			}
		}
	}
	sort.Ints(out)
	return out, true
}

func (vol *serviceVolumeIndex) pathPlanTermPostingLimited(term string, pq parsedQuery) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.CountOnly || pq.Limit <= 0 || !pq.MatchPath ||
		pq.Under != "" || pq.Type != "" || pq.CaseSensitive || pq.CWDBias != "" || pq.RootBias != "" ||
		len(pq.Exts) > 0 || len(pq.Dirs) > 0 || len(pq.Globs) > 0 || len(pq.Regexps) > 0 ||
		len(pq.OrGroups) > 0 || len(pq.NotGroups) > 0 || pq.HasModAfter || pq.Exists ||
		countNonVolumeTerms(pq.Terms) != 1 || term == "" || strings.ContainsAny(term, `\/*?[]:`) {
		return nil, false
	}
	roots := vol.pathTermRootIDs(term)
	if len(roots) == 0 || len(vol.subtreeStart) == 0 || len(vol.subtreeEnd) == 0 || len(vol.subtreeOrder) == 0 {
		return nil, false
	}
	nameMatches, ok := vol.nameTrigramNameTermPostingLimited(term, servicePathNameTrigramCandidateMaxIDs)
	if !ok {
		nameMatches = vol.nameTermPosting(term)
	}
	if len(nameMatches) > max(128, pq.Limit*4) {
		return nil, false
	}
	intervals := make([]interval, 0, len(roots))
	recordCount := vol.index.compactRecordCount()
	for _, rootID := range roots {
		if rootID < 0 || rootID >= recordCount || rootID >= len(vol.subtreeStart) || rootID >= len(vol.subtreeEnd) {
			continue
		}
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start == ^uint32(0) || start > end || int(end) > len(vol.subtreeOrder) {
			continue
		}
		intervals = append(intervals, interval{start: int(start), end: int(end)})
	}
	if len(intervals) == 0 {
		return nil, false
	}
	intervals = mergeIntervals(intervals)
	if out, ok := vol.smallPathComponentExpansion(term, pq, intervals, nameMatches); ok {
		return out, true
	}
	return vol.topPathComponentExpansion(term, pq, intervals, nameMatches)
}

func (vol *serviceVolumeIndex) smallPathComponentExpansion(term string, pq parsedQuery, intervals []interval, nameMatches []int) ([]int, bool) {
	total := 0
	for _, iv := range intervals {
		if iv.end > iv.start {
			total += iv.end - iv.start
		}
	}
	threshold := max(4096, pq.Limit*64)
	if total > threshold || len(nameMatches) > threshold {
		return nil, false
	}
	seen := make(map[int]struct{}, total+len(nameMatches))
	out := make([]int, 0, total+len(nameMatches))
	add := func(id int) {
		if id < 0 || id >= vol.index.compactRecordCount() {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !compactRecordPrecheck(rec, pq, true) {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, id := range nameMatches {
		add(id)
	}
	for _, iv := range intervals {
		for pos := iv.start; pos < iv.end; pos++ {
			if pos < 0 || pos >= len(vol.subtreeOrder) {
				continue
			}
			add(int(vol.subtreeOrder[pos]))
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	sort.Ints(out)
	return out, true
}

func (vol *serviceVolumeIndex) topPathComponentExpansion(term string, pq parsedQuery, intervals []interval, nameMatches []int) ([]int, bool) {
	if vol == nil || vol.index == nil || pq.Limit <= 0 {
		return nil, false
	}
	rankOf := candidateRanker(vol.index, vol.rankForQuery(pq))
	seen := make(map[int]struct{}, pq.Limit*4)
	h := make(candidateRankMaxHeap, 0, pq.Limit)
	add := func(id int) {
		if id < 0 || id >= vol.index.compactRecordCount() {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted || !compactRecordPrecheck(rec, pq, true) {
			return
		}
		seen[id] = struct{}{}
		item := candidateRankItem{id: id, rank: rankOf(id)}
		if len(h) < pq.Limit {
			heap.Push(&h, item)
			return
		}
		if item.rank < h[0].rank {
			h[0] = item
			heap.Fix(&h, 0)
		}
	}
	for _, id := range nameMatches {
		if id >= 0 && id < vol.index.compactRecordCount() && strings.Contains(vol.index.compactLowerNameAt(id), term) {
			add(id)
		}
	}
	for _, iv := range intervals {
		for pos := iv.start; pos < iv.end; pos++ {
			if pos < 0 || pos >= len(vol.subtreeOrder) {
				continue
			}
			add(int(vol.subtreeOrder[pos]))
		}
	}
	if len(h) == 0 {
		return nil, false
	}
	out := make([]int, len(h))
	for i := range h {
		out[i] = h[i].id
	}
	sort.Slice(out, func(i, j int) bool {
		return rankOf(out[i]) < rankOf(out[j])
	})
	return out, true
}

func countNonVolumeTerms(terms []string) int {
	count := 0
	for _, term := range terms {
		if isVolumeQueryTerm(term) {
			continue
		}
		count++
	}
	return count
}

func mergeIntervals(intervals []interval) []interval {
	if len(intervals) <= 1 {
		return intervals
	}
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].start == intervals[j].start {
			return intervals[i].end < intervals[j].end
		}
		return intervals[i].start < intervals[j].start
	})
	out := intervals[:1]
	for _, iv := range intervals[1:] {
		last := &out[len(out)-1]
		if iv.start <= last.end {
			if iv.end > last.end {
				last.end = iv.end
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

func intervalContainsPosition(intervals []interval, pos int) bool {
	if pos < 0 {
		return false
	}
	i := sort.Search(len(intervals), func(i int) bool {
		return intervals[i].end > pos
	})
	return i < len(intervals) && intervals[i].start <= pos && pos < intervals[i].end
}
