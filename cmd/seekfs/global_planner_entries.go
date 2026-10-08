package main

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

func addGlobalComponentTraceTerms(trace *searchTrace, pq parsedQuery, countHint int) {
	if trace == nil {
		return
	}
	for _, term := range nonVolumeTerms(pq.Terms) {
		trace.addTerm(traceTerm{Term: term, Kind: "path-substring", Source: "global:component-subtree", CountHint: countHint, Exact: false})
	}
	for _, dir := range pq.Dirs {
		trace.addTerm(traceTerm{Term: dir, Kind: "directory-component", Source: "global:dir", CountHint: countHint, Exact: false})
	}
	if globalRegexLiteralSupported(pq) {
		trace.addTerm(traceTerm{Term: pq.RegexTerms[0], Kind: "regex-literal", Source: "global:regex-literal", CountHint: countHint, Exact: false})
	}
	for _, parent := range pq.Parents {
		trace.addTerm(traceTerm{Term: parent, Kind: "parent", Source: "global:parent", CountHint: countHint, Exact: true})
	}
	if pq.Under != "" {
		trace.addTerm(traceTerm{Term: pq.Under, Kind: "under", Source: "global:under", CountHint: countHint, Exact: true})
	}
	if pq.Type != "" {
		trace.addTerm(traceTerm{Term: pq.Type, Kind: "type", Source: "global:type", CountHint: countHint, Exact: true})
	}
	for _, mask := range pq.AttrFilters {
		trace.addTerm(traceTerm{Term: attribMaskString(mask), Kind: "attribute", Source: "global:attribute", CountHint: countHint, Exact: true})
	}
	extFilters, _ := globalExtPostingFilters(pq)
	for _, extFilter := range extFilters {
		trace.addTerm(traceTerm{Term: extFilter.ext, Kind: "extension", Source: "global:" + extFilter.source, CountHint: countHint, Exact: true})
	}
}

func globalComponentQueryIDs(volumes []*serviceVolumeIndex, pq parsedQuery, trace *searchTrace) ([]globalRecordID, bool) {
	var ids []globalRecordID
	haveIDs := false
	var underRoots []globalRecordID
	underPending := false
	if pq.Under != "" {
		var ok bool
		underRoots, ok = globalUnderRoots(volumes, pq.Under)
		if !ok {
			return nil, false
		}
		underPending = true
	}
	intersect := func(next []globalRecordID) {
		if underPending {
			ids = filterGlobalIDsBySubtrees(volumes, underRoots, next)
			underPending = false
			haveIDs = true
			return
		}
		if !haveIDs {
			ids = next
			haveIDs = true
			return
		}
		left := newGlobalIDSliceIterator(ids)
		right := newGlobalIDSliceIterator(next)
		ids = intersectGlobalIterators(&left, &right, 0)
	}
	materializeUnder := func() bool {
		if !underPending {
			return true
		}
		var ok bool
		ids, ok = globalSubtreeIDs(volumes, underRoots, 0)
		underPending = false
		haveIDs = ok
		return ok
	}
	if terms := nonVolumeTerms(pq.Terms); len(terms) > 0 {
		var ok bool
		termIDs, ok := globalComponentPathIDs(volumes, terms)
		if !ok {
			return nil, false
		}
		intersect(termIDs)
	}
	for _, group := range pq.OrGroups {
		var groupIDs []globalRecordID
		for altIndex, alt := range group {
			altIDs, ok := globalComponentSubqueryIDs(volumes, alt, trace)
			if !ok {
				return nil, false
			}
			if altIndex == 0 {
				groupIDs = altIDs
				continue
			}
			left := newGlobalIDSliceIterator(groupIDs)
			right := newGlobalIDSliceIterator(altIDs)
			groupIDs = unionGlobalIterators(&left, &right, 0)
		}
		intersect(groupIDs)
		if len(ids) == 0 {
			break
		}
	}
	if len(pq.NotGroups) > 0 && !materializeUnder() {
		return nil, false
	}
	for _, neg := range pq.NotGroups {
		negIDs, ok := globalComponentSubqueryIDs(volumes, neg, trace)
		if !ok {
			return nil, false
		}
		if haveIDs {
			left := newGlobalIDSliceIterator(ids)
			right := newGlobalIDSliceIterator(negIDs)
			ids = excludeGlobalIterator(&left, &right, 0)
		}
		if len(ids) == 0 {
			break
		}
	}
	for _, dir := range pq.Dirs {
		dirIDs, ok := globalPathTermIDs(volumes, dir)
		if !ok {
			return nil, false
		}
		intersect(dirIDs)
		if len(ids) == 0 {
			break
		}
	}
	if globalRegexLiteralSupported(pq) {
		regexIDs, ok := globalRegexLiteralIDs(volumes, pq)
		if !ok {
			return nil, false
		}
		intersect(regexIDs)
	}
	for _, parent := range pq.Parents {
		parentIDs, ok := globalParentIDs(volumes, parent)
		if !ok {
			return nil, false
		}
		intersect(parentIDs)
		if len(ids) == 0 {
			break
		}
	}
	if len(pq.AttrFilters) > 0 {
		attrIDs, ok := globalAttrIDs(volumes, pq.AttrFilters)
		if !ok {
			return nil, false
		}
		intersect(attrIDs)
	}
	extFilters, _ := globalExtPostingFilters(pq)
	for _, extFilter := range extFilters {
		extIDs, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, trace)
		if !ok {
			return nil, false
		}
		intersect(extIDs)
		if len(ids) == 0 {
			break
		}
	}
	if !materializeUnder() || !haveIDs {
		return nil, false
	}
	return ids, true
}

func globalComponentSubqueryIDs(volumes []*serviceVolumeIndex, pq parsedQuery, trace *searchTrace) ([]globalRecordID, bool) {
	var ids []globalRecordID
	haveIDs := false
	intersect := func(next []globalRecordID) {
		if !haveIDs {
			ids = next
			haveIDs = true
			return
		}
		left := newGlobalIDSliceIterator(ids)
		right := newGlobalIDSliceIterator(next)
		ids = intersectGlobalIterators(&left, &right, 0)
	}
	if terms := nonVolumeTerms(pq.Terms); len(terms) > 0 {
		termIDs, ok := globalComponentPathIDs(volumes, terms)
		if !ok {
			return nil, false
		}
		intersect(termIDs)
	}
	for _, dir := range pq.Dirs {
		dirIDs, ok := globalPathTermIDs(volumes, dir)
		if !ok {
			return nil, false
		}
		intersect(dirIDs)
		if len(ids) == 0 {
			break
		}
	}
	if globalRegexLiteralSupported(pq) {
		regexIDs, ok := globalRegexLiteralIDs(volumes, pq)
		if !ok {
			return nil, false
		}
		intersect(regexIDs)
	}
	for _, parent := range pq.Parents {
		parentIDs, ok := globalParentIDs(volumes, parent)
		if !ok {
			return nil, false
		}
		intersect(parentIDs)
		if len(ids) == 0 {
			break
		}
	}
	if len(pq.AttrFilters) > 0 {
		attrIDs, ok := globalAttrIDs(volumes, pq.AttrFilters)
		if !ok {
			return nil, false
		}
		intersect(attrIDs)
	}
	extFilters, _ := globalExtPostingFilters(pq)
	for _, extFilter := range extFilters {
		extIDs, ok := globalExtPostingIDs(volumes, extFilter.ext, 0, trace)
		if !ok {
			return nil, false
		}
		intersect(extIDs)
		if len(ids) == 0 {
			break
		}
	}
	if pq.Type != "" {
		if !haveIDs {
			return nil, false
		}
		ids = filterGlobalIDsByType(volumes, ids, pq.Type)
	}
	if !haveIDs {
		return nil, false
	}
	return ids, true
}

func globalUnderIDs(volumes []*serviceVolumeIndex, under string) ([]globalRecordID, bool) {
	roots, ok := globalUnderRoots(volumes, under)
	if !ok || len(roots) == 0 {
		return roots, ok
	}
	return globalSubtreeIDs(volumes, roots, 0)
}

func globalUnderRoots(volumes []*serviceVolumeIndex, under string) ([]globalRecordID, bool) {
	if under == "" {
		return nil, false
	}
	under = filepath.Clean(under)
	underVolume := strings.ToUpper(filepath.VolumeName(under))
	roots := make([]globalRecordID, 0, 1)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return nil, false
		}
		if underVolume != "" && vol.index.Volume != "" && !strings.EqualFold(vol.index.Volume, underVolume) {
			continue
		}
		for _, rootID := range vol.underRootIDs(under) {
			roots = append(roots, globalRecordID{volume: volumeIndex, local: rootID})
		}
	}
	if len(roots) == 0 {
		return []globalRecordID{}, true
	}
	return roots, true
}

func filterGlobalIDsBySubtrees(volumes []*serviceVolumeIndex, roots, ids []globalRecordID) []globalRecordID {
	base := newGlobalIDSliceIterator(ids)
	filtered := newGlobalSubtreeFilterIterator(&base, volumes, roots)
	return collectGlobalIterator(&filtered, 0)
}

func filterGlobalIDsByType(volumes []*serviceVolumeIndex, ids []globalRecordID, typ string) []globalRecordID {
	if typ == "" {
		return ids
	}
	out := ids[:0]
	for _, id := range ids {
		if id.volume < 0 || id.volume >= len(volumes) {
			continue
		}
		vol := volumes[id.volume]
		if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
			continue
		}
		isDir := vol.index.compactRecord(id.local).Mode&uint32(os.ModeDir) != 0
		if (typ == "file" && !isDir) || (typ == "dir" && isDir) {
			out = append(out, id)
		}
	}
	return out
}

func globalComponentPathIDs(volumes []*serviceVolumeIndex, terms []string) ([]globalRecordID, bool) {
	probes := pathPlanProbeTerms(terms)
	if len(probes) == 0 {
		return nil, true
	}
	ids, ok := globalPathTermIDs(volumes, probes[0])
	if !ok {
		return nil, false
	}
	for _, term := range probes[1:] {
		base := newGlobalIDSliceIterator(ids)
		filtered := newGlobalPathTermFilterIterator(&base, volumes, term)
		ids = collectGlobalIterator(&filtered, 0)
		if len(ids) == 0 {
			break
		}
	}
	return ids, true
}

func globalPathTermIDs(volumes []*serviceVolumeIndex, term string) ([]globalRecordID, bool) {
	if term == "" {
		return nil, false
	}
	out := make([]globalRecordID, 0)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil || !vol.pathComponentPostingAvailable(term) {
			return nil, false
		}
		for _, id := range vol.pathPlanTermPosting(term) {
			out = append(out, globalRecordID{volume: volumeIndex, local: id})
		}
	}
	sortGlobalRecordIDs(out)
	return out, true
}

func globalRegexLiteralIDs(volumes []*serviceVolumeIndex, pq parsedQuery) ([]globalRecordID, bool) {
	if !globalRegexLiteralSupported(pq) {
		return nil, false
	}
	return globalPathTermIDs(volumes, pq.RegexTerms[0])
}

func globalParentIDs(volumes []*serviceVolumeIndex, parent string) ([]globalRecordID, bool) {
	if parent == "" || strings.ContainsAny(parent, `\/:*?[]`) {
		return nil, false
	}
	out := make([]globalRecordID, 0)
	for volumeIndex, vol := range volumes {
		if vol == nil || vol.index == nil {
			return nil, false
		}
		for _, id := range vol.parentIDs(parent) {
			out = append(out, globalRecordID{volume: volumeIndex, local: id})
		}
	}
	sortGlobalRecordIDs(out)
	return out, true
}

func globalAttrIDs(volumes []*serviceVolumeIndex, filters []uint32) ([]globalRecordID, bool) {
	if len(filters) == 0 {
		return nil, false
	}
	var ids []globalRecordID
	haveIDs := false
	for _, mask := range filters {
		maskIDs := make([]globalRecordID, 0)
		for volumeIndex, vol := range volumes {
			if vol == nil || vol.index == nil {
				return nil, false
			}
			localIDs, ok := vol.attrIDsForMask(mask)
			if !ok {
				return nil, false
			}
			for _, id := range localIDs {
				maskIDs = append(maskIDs, globalRecordID{volume: volumeIndex, local: id})
			}
		}
		sortGlobalRecordIDs(maskIDs)
		if !haveIDs {
			ids = maskIDs
			haveIDs = true
			continue
		}
		left := newGlobalIDSliceIterator(ids)
		right := newGlobalIDSliceIterator(maskIDs)
		ids = intersectGlobalIterators(&left, &right, 0)
		if len(ids) == 0 {
			break
		}
	}
	if !haveIDs {
		return nil, false
	}
	return ids, true
}

func entriesFromGlobalIDs(volumes []*serviceVolumeIndex, ids []globalRecordID, pq parsedQuery) ([]Entry, error) {
	ranked, err := rankedEntriesFromGlobalIDs(volumes, ids, pq, nil)
	if err != nil {
		return nil, err
	}
	return globalRankedEntriesToEntries(ranked), nil
}

func countVerifiedGlobalIDs(volumes []*serviceVolumeIndex, ids []globalRecordID, pq parsedQuery) (int, error) {
	return countVerifiedGlobalIDsContent(volumes, ids, pq, nil)
}

func countVerifiedGlobalIDsContent(volumes []*serviceVolumeIndex, ids []globalRecordID, pq parsedQuery, matcher *contentLeafMatcher) (int, error) {
	pathCaches := make([]map[int]string, len(volumes))
	count := 0
	for _, id := range ids {
		if queryCanceled(pq) {
			return 0, errQueryCanceled
		}
		if id.volume < 0 || id.volume >= len(volumes) {
			continue
		}
		vol := volumes[id.volume]
		if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
			continue
		}
		if pathCaches[id.volume] == nil {
			pathCaches[id.volume] = make(map[int]string)
		}
		pathCaches[id.volume] = boundPathCache(pathCaches[id.volume])
		volumePQ := pq
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		if _, ok := compactCandidateEntryIfMatchIn(vol, vol.index, volumePQ, id.local, pathCaches[id.volume], true, false, matcher); ok {
			count++
		}
	}
	return count, nil
}

func rankedEntriesFromGlobalIDs(volumes []*serviceVolumeIndex, ids []globalRecordID, pq parsedQuery, matcher *contentLeafMatcher) ([]globalRankedEntry, error) {
	pathCaches := make([]map[int]string, len(volumes))
	rankers := make([]func(int) int, len(volumes))
	for i, vol := range volumes {
		if vol != nil && vol.index != nil {
			rankers[i] = candidateRanker(vol.index, vol.rankForQuery(pq))
		}
	}
	results := make([]globalRankedEntry, 0, len(ids))
	for _, id := range ids {
		if queryCanceled(pq) {
			return nil, errQueryCanceled
		}
		if id.volume < 0 || id.volume >= len(volumes) {
			continue
		}
		vol := volumes[id.volume]
		if vol == nil || vol.index == nil {
			continue
		}
		if pathCaches[id.volume] == nil {
			pathCaches[id.volume] = make(map[int]string)
		}
		pathCaches[id.volume] = boundPathCache(pathCaches[id.volume])
		volumePQ := pq
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		entry, ok := compactCandidateEntryIfMatchIn(vol, vol.index, volumePQ, id.local, pathCaches[id.volume], true, compactCandidateCanSkipEntryMatches(volumePQ, matcher == nil), matcher)
		if ok {
			rank := int(^uint(0) >> 1)
			if rankers[id.volume] != nil {
				rank = rankers[id.volume](id.local)
			}
			results = append(results, globalRankedEntry{entry: entry, rank: rank, volume: id.volume, tie: entry.Path})
		}
	}
	return results, nil
}

func globalRankedEntriesToEntries(ranked []globalRankedEntry) []Entry {
	out := make([]Entry, len(ranked))
	for i, item := range ranked {
		out[i] = item.entry
	}
	return out
}

func mergeGlobalOverlayEntries(volumes []*serviceVolumeIndex, snapshots []*volumeSnapshot, base []globalRankedEntry, pq parsedQuery, limit int) []globalRankedEntry {
	if len(snapshots) == 0 {
		sortGlobalRankedEntries(base, pq)
		if limit > 0 && len(base) > limit {
			return base[:limit]
		}
		return base
	}
	out := append([]globalRankedEntry(nil), base...)
	for volumeIndex, snap := range snapshots {
		if snap == nil || volumeIndex < 0 || volumeIndex >= len(volumes) {
			continue
		}
		vol := volumes[volumeIndex]
		if vol == nil {
			continue
		}
		pathCache := make(map[int]string)
		for _, overlay := range vol.overlayRankedMatches(snap, pq, pathCache) {
			out = append(out, globalRankedEntry{entry: overlay.entry, rank: overlay.rank, volume: volumeIndex, tie: overlay.entry.Path, overlay: true})
		}
	}
	sortGlobalRankedEntries(out, pq)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func sortGlobalRankedEntries(entries []globalRankedEntry, pq parsedQuery) {
	global := globalRankedEntriesSpanMultipleVolumes(entries)
	for _, entry := range entries {
		if entry.overlay {
			global = true
			break
		}
	}
	slices.SortStableFunc(entries, func(a, b globalRankedEntry) int {
		if global {
			// Once multiple volumes participate, persisted ranks are local to a
			// volume.  Compare actual entries before rank/tie fallbacks so an
			// overlay or equal local rank cannot change global deterministic order.
			if n := compareSearchAllEntries(a.entry, b.entry, pq); n != 0 {
				return n
			}
		}
		if n := cmp.Compare(a.rank, b.rank); n != 0 {
			return n
		}
		if n := cmp.Compare(a.volume, b.volume); n != 0 {
			return n
		}
		return strings.Compare(a.tie, b.tie)
	})
}

func globalRankedEntriesSpanMultipleVolumes(entries []globalRankedEntry) bool {
	first := -1
	for _, entry := range entries {
		if first < 0 {
			first = entry.volume
			continue
		}
		if entry.volume != first {
			return true
		}
	}
	return false
}

func globalOverlayMatchCount(volumes []*serviceVolumeIndex, snapshots []*volumeSnapshot, pq parsedQuery) int {
	total := 0
	for i, snap := range snapshots {
		if snap == nil || i < 0 || i >= len(volumes) || volumes[i] == nil {
			continue
		}
		total += volumes[i].overlayLiveMatchCount(snap, pq)
	}
	return total
}

func globalOverlaySnapshots(volumes []*serviceVolumeIndex) ([]*volumeSnapshot, bool) {
	snapshots := make([]*volumeSnapshot, len(volumes))
	for i, vol := range volumes {
		if vol == nil || !vol.hasActiveOverlay() {
			continue
		}
		snap := vol.snap.Load()
		if snap == nil {
			return nil, false
		}
		snapshots[i] = snap
	}
	return snapshots, true
}

func globalSnapshotsHaveHidden(snapshots []*volumeSnapshot) bool {
	for _, snap := range snapshots {
		if snap != nil && (len(snap.tombstoneIDs) > 0 || len(snap.shadowedIDs) > 0) {
			return true
		}
	}
	return false
}

func globalSnapshotsHaveOverlayRecords(snapshots []*volumeSnapshot) bool {
	for _, snap := range snapshots {
		if snap != nil && len(snap.records) > 0 {
			return true
		}
	}
	return false
}

func globalVolumesHaveRankForQuery(volumes []*serviceVolumeIndex, pq parsedQuery) bool {
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			return false
		}
		recordCount := vol.index.compactRecordCount()
		if len(vol.rankForQuery(pq)) >= recordCount {
			continue
		}
		if pq.SortColumn == "" && len(vol.index.CompactNameOrder) >= recordCount {
			continue
		}
		return false
	}
	return true
}

func globalHiddenContains(snapshots []*volumeSnapshot, id globalRecordID) bool {
	if id.volume < 0 || id.volume >= len(snapshots) || snapshots[id.volume] == nil {
		return false
	}
	hidden := hiddenBaseIDs{tombstone: snapshots[id.volume].tombstoneIDs, shadowed: snapshots[id.volume].shadowedIDs}
	return hidden.contains(id.local)
}

func filterGlobalIDsHidden(ids []globalRecordID, snapshots []*volumeSnapshot) []globalRecordID {
	if !globalSnapshotsHaveHidden(snapshots) {
		return ids
	}
	out := ids[:0]
	for _, id := range ids {
		if !globalHiddenContains(snapshots, id) {
			out = append(out, id)
		}
	}
	return out
}

func globalRankerForVolumes(volumes []*serviceVolumeIndex, pq parsedQuery) func(globalRecordID) int {
	rankers := make([]func(int) int, len(volumes))
	for i, vol := range volumes {
		if vol != nil && vol.index != nil {
			rankers[i] = candidateRanker(vol.index, vol.rankForQuery(pq))
		}
	}
	return func(id globalRecordID) int {
		if id.volume < 0 || id.volume >= len(rankers) || rankers[id.volume] == nil {
			return int(^uint(0) >> 1)
		}
		return rankers[id.volume](id.local)
	}
}

func (it *globalIDSliceIterator) CountHint() int {
	if it == nil || it.pos >= len(it.ids) {
		return 0
	}
	return len(it.ids) - it.pos
}

func (it *globalIDSliceIterator) Next() (globalRecordID, bool) {
	if it == nil || it.pos >= len(it.ids) {
		return globalRecordID{}, false
	}
	id := it.ids[it.pos]
	it.pos++
	return id, true
}

func (it *globalIDSliceIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil {
		return globalRecordID{}, false
	}
	for it.pos < len(it.ids) && compareGlobalRecordID(it.ids[it.pos], target) < 0 {
		it.pos++
	}
	return it.Next()
}

func (it *globalHiddenIterator) CountHint() int {
	if it == nil || it.base == nil {
		return 0
	}
	return it.base.CountHint()
}

func (it *globalHiddenIterator) Next() (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	for {
		id, ok := it.base.Next()
		if !ok {
			return globalRecordID{}, false
		}
		if !globalHiddenContains(it.snapshots, id) {
			return id, true
		}
	}
}

func (it *globalHiddenIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	id, ok := it.base.SeekGE(target)
	for ok && globalHiddenContains(it.snapshots, id) {
		id, ok = it.base.Next()
	}
	return id, ok
}

func (it *globalSubtreeFilterIterator) CountHint() int {
	if it == nil || it.base == nil {
		return 0
	}
	return it.base.CountHint()
}

func (it *globalSubtreeFilterIterator) contains(id globalRecordID) bool {
	if id.volume < 0 || id.volume >= len(it.volumes) {
		return false
	}
	vol := it.volumes[id.volume]
	if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
		return false
	}
	for _, root := range it.roots[id.volume] {
		if vol.isDescendantOrSelf(id.local, root) {
			return true
		}
	}
	return false
}

func (it *globalSubtreeFilterIterator) Next() (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	for {
		id, ok := it.base.Next()
		if !ok {
			return globalRecordID{}, false
		}
		if it.contains(id) {
			return id, true
		}
	}
}

func (it *globalSubtreeFilterIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	id, ok := it.base.SeekGE(target)
	if ok && it.contains(id) {
		return id, true
	}
	return it.Next()
}

func (it *globalPathTermFilterIterator) CountHint() int {
	if it == nil || it.base == nil {
		return 0
	}
	return it.base.CountHint()
}

func (it *globalPathTermFilterIterator) contains(id globalRecordID) bool {
	if id.volume < 0 || id.volume >= len(it.volumes) {
		return false
	}
	vol := it.volumes[id.volume]
	if vol == nil || vol.index == nil || id.local < 0 || id.local >= vol.index.compactRecordCount() {
		return false
	}
	if set := it.nameIDs[id.volume]; len(set) > 0 {
		if _, ok := set[id.local]; ok {
			return true
		}
	}
	if it.fast[id.volume] {
		// PCMP represents exact directory components.  For a filtered
		// candidate, walk only its parent chain instead of copying the full
		// root posting (Users can have millions of descendants).
		if strings.Contains(vol.index.compactLowerNameAt(id.local), it.term) {
			return true
		}
		for current := id.local; current >= 0 && current < vol.index.compactRecordCount(); {
			if current != id.local && strings.EqualFold(vol.index.compactLowerNameAt(current), it.term) {
				return true
			}
			rec := vol.index.compactRecord(current)
			if rec.Parent < 0 || int(rec.Parent) == current {
				break
			}
			current = int(rec.Parent)
		}
		return false
	}
	if roots := it.roots[id.volume]; len(roots) > 0 {
		if id.local < len(vol.subtreeStart) {
			pos := vol.subtreeStart[id.local]
			if pos == ^uint32(0) {
				return false
			}
			idx := sort.Search(len(roots), func(i int) bool { return roots[i].start > pos }) - 1
			if idx >= 0 && pos < roots[idx].end {
				return true
			}
		}
		return false
	}
	return vol.index.compactPathContainsTerm(id.local, it.term)
}

func (it *globalPathTermFilterIterator) Next() (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	for {
		id, ok := it.base.Next()
		if !ok {
			return globalRecordID{}, false
		}
		if it.contains(id) {
			return id, true
		}
	}
}

func (it *globalPathTermFilterIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	if it == nil || it.base == nil {
		return globalRecordID{}, false
	}
	id, ok := it.base.SeekGE(target)
	if ok && it.contains(id) {
		return id, true
	}
	return it.Next()
}
