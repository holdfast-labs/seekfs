package main

import (
	"cmp"
	"slices"
	"sort"
	"strings"
	"time"
)

type scalarRange struct {
	order      []uint32
	start, end int
	modified   bool
}

type scalarExecutionStats struct {
	driver          string
	interval        int
	recordsVerified int
}

// globalScalarRangeIterator is intentionally not a globalIDIterator. Its
// records are ordered by a scalar value, while globalIDIterator requires
// global record-ID order for SeekGE and set operations.
type globalScalarRangeIterator struct {
	volume   int
	order    []uint32
	pos, end int
}

func newGlobalScalarRangeIterator(volume int, scalar scalarRange) *globalScalarRangeIterator {
	return &globalScalarRangeIterator{
		volume: volume,
		order:  scalar.order,
		pos:    scalar.start,
		end:    scalar.end,
	}
}

func (it *globalScalarRangeIterator) CountHint() int {
	if it == nil || it.pos >= it.end {
		return 0
	}
	return it.end - it.pos
}

func (it *globalScalarRangeIterator) Next() (globalRecordID, bool) {
	if it == nil || it.pos >= it.end || it.pos >= len(it.order) {
		return globalRecordID{}, false
	}
	id := globalRecordID{volume: it.volume, local: int(it.order[it.pos])}
	it.pos++
	return id, true
}

func scalarOrderForRange(vol *serviceVolumeIndex, modified bool) []uint32 {
	if vol == nil {
		return nil
	}
	if vol.queryIndex != nil {
		if modified && len(vol.queryIndex.modOrder) > 0 && len(vol.queryIndex.modRank) > 0 {
			return vol.queryIndex.modOrder
		}
		if !modified && len(vol.queryIndex.sizeOrder) > 0 && len(vol.queryIndex.sizeRank) > 0 {
			return vol.queryIndex.sizeOrder
		}
	}
	if vol.index == nil {
		return nil
	}
	if modified {
		if len(vol.index.Derived.ModOrder) > 0 && len(vol.index.Derived.ModRank) > 0 {
			return vol.index.Derived.ModOrder
		}
		return nil
	}
	if len(vol.index.Derived.SizeOrder) > 0 && len(vol.index.Derived.SizeRank) > 0 {
		return vol.index.Derived.SizeOrder
	}
	return nil
}

func scalarOrderValue(order []uint32, idx *Index, pos int) (int64, bool) {
	if idx == nil || pos < 0 || pos >= len(order) {
		return 0, false
	}
	id := int(order[pos])
	if id < 0 || id >= idx.compactRecordCount() {
		return 0, false
	}
	rec := idx.compactRecord(id)
	return rec.Size, true
}

func lowerBoundSize(order []uint32, idx *Index, value int64) (int, bool) {
	lo, hi := 0, len(order)
	for lo < hi {
		mid := lo + (hi-lo)/2
		got, ok := scalarOrderValue(order, idx, mid)
		if !ok {
			return 0, false
		}
		if got < value {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, true
}

func upperBoundSize(order []uint32, idx *Index, value int64) (int, bool) {
	lo, hi := 0, len(order)
	for lo < hi {
		mid := lo + (hi-lo)/2
		got, ok := scalarOrderValue(order, idx, mid)
		if !ok {
			return 0, false
		}
		if got <= value {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, true
}

func firstModifiedZero(order []uint32, idx *Index) (int, bool) {
	lo, hi := 0, len(order)
	for lo < hi {
		mid := lo + (hi-lo)/2
		id := int(order[mid])
		if id < 0 || id >= idx.compactRecordCount() {
			return 0, false
		}
		if idx.compactRecord(id).ModUnix == 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo, true
}

// firstModifiedBoundary returns the first position that must be excluded from
// a newest-first order. Real modification times precede zero timestamps.
func firstModifiedBoundary(order []uint32, idx *Index, bound time.Time, strict bool) (int, bool) {
	nonZeroEnd, ok := firstModifiedZero(order, idx)
	if !ok {
		return 0, false
	}
	if bound.IsZero() {
		return nonZeroEnd, true
	}
	want := bound.UnixNano()
	lo, hi := 0, nonZeroEnd
	for lo < hi {
		mid := lo + (hi-lo)/2
		id := int(order[mid])
		if id < 0 || id >= idx.compactRecordCount() {
			return 0, false
		}
		mod := idx.compactRecord(id).ModUnix
		// Inclusive dm: bound excludes values below it. Strict
		// --modified-after: bound excludes values at or below it.
		exclude := mod < want
		if strict {
			exclude = mod <= want
		}
		if exclude {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo, true
}

func scalarRangeForVolume(vol *serviceVolumeIndex, pq parsedQuery) (scalarRange, bool) {
	if vol == nil || vol.index == nil || len(pq.OrGroups) != 0 || len(pq.NotGroups) != 0 ||
		len(pq.Terms) != 0 || len(pq.Exts) != 0 || len(pq.Dirs) != 0 || len(pq.Globs) != 0 ||
		len(pq.Regexps) != 0 || len(pq.RegexTerms) != 0 || len(pq.Parents) != 0 || pq.Under != "" || pq.Exists {
		return scalarRange{}, false
	}
	if len(pq.SizeFilters) == 1 && len(pq.DateFilters) == 0 && !pq.HasModAfter {
		order := scalarOrderForRange(vol, false)
		if len(order) == 0 {
			return scalarRange{}, false
		}
		sf := pq.SizeFilters[0]
		start, end := 0, len(order)
		var ok bool
		switch sf.op {
		case ">":
			start, ok = upperBoundSize(order, vol.index, sf.bytes)
		case ">=":
			start, ok = lowerBoundSize(order, vol.index, sf.bytes)
		case "<":
			end, ok = lowerBoundSize(order, vol.index, sf.bytes)
		case "<=":
			end, ok = upperBoundSize(order, vol.index, sf.bytes)
		default:
			start, ok = lowerBoundSize(order, vol.index, sf.bytes)
			if ok {
				end, ok = upperBoundSize(order, vol.index, sf.bytes)
			}
		}
		if !ok {
			return scalarRange{}, false
		}
		return scalarRange{order: order, start: start, end: end}, start <= end
	}
	if len(pq.SizeFilters) != 0 || len(pq.DateFilters) > 1 || (len(pq.DateFilters) == 1 && pq.HasModAfter) ||
		(len(pq.DateFilters) == 0 && !pq.HasModAfter) {
		return scalarRange{}, false
	}
	order := scalarOrderForRange(vol, true)
	if len(order) == 0 {
		return scalarRange{}, false
	}
	var after, before time.Time
	if pq.HasModAfter {
		after = pq.ModifiedAfter
	} else {
		after = pq.DateFilters[0].after
		before = pq.DateFilters[0].before
	}
	start, ok := firstModifiedBoundary(order, vol.index, before, false)
	if !ok {
		return scalarRange{}, false
	}
	end, ok := firstModifiedBoundary(order, vol.index, after, pq.HasModAfter)
	if !ok {
		return scalarRange{}, false
	}
	if before.IsZero() {
		start = 0
	}
	if after.IsZero() {
		end, ok = firstModifiedBoundary(order, vol.index, time.Time{}, false)
		if !ok {
			return scalarRange{}, false
		}
	}
	if end < start {
		return scalarRange{order: order, start: 0, end: 0, modified: true}, true
	}
	return scalarRange{order: order, start: start, end: end, modified: true}, true
}

// globalScalarQuerySupported covers source-less metadata queries that would
// otherwise fall back to independent per-volume scans. Simple scalar ranges
// use persisted/resident order arrays; type-only and unsupported compound
// shapes retain the complete global scan fallback.
func globalScalarQuerySupported(pq parsedQuery) bool {
	if queryHasAnyContentLeaf(pq) {
		return false
	}
	if pq.Exists || pq.Under != "" || len(nonVolumeTerms(pq.Terms)) != 0 ||
		len(pq.Exts) != 0 || len(pq.Dirs) != 0 || len(pq.Globs) != 0 ||
		len(pq.Regexps) != 0 || len(pq.RegexTerms) != 0 || len(pq.Parents) != 0 ||
		len(pq.AttrFilters) != 0 || len(pq.OrGroups) != 0 || len(pq.NotGroups) != 0 {
		return false
	}
	if pq.Type != "" && pq.Type != "file" && pq.Type != "dir" {
		return false
	}
	return pq.Type != "" || pq.HasModAfter || len(pq.SizeFilters) != 0 || len(pq.DateFilters) != 0
}

func searchServiceVolumesGlobalScalarSnapshot(snapshot globalQuerySnapshot, opts queryOptions, countOnly bool) ([]Entry, bool, error) {
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, true, err
	}
	if !globalScalarQuerySupported(pq) {
		return nil, false, nil
	}
	if !globalPlannerSnapshotReady(snapshot, opts.Trace, "global-scalar") {
		return nil, false, nil
	}
	limit := normalizedLimit(opts.Limit, countOnly)
	if countOnly {
		limit = 0
	}
	base, baseCount, rangeUsed, scanUsed, stats, err := executeGlobalScalarBase(snapshot, pq, true, limit)
	if err != nil {
		return nil, true, err
	}
	ranked := mergeGlobalVerifiedEntries(snapshot, base, pq, limit)
	setGlobalScalarTrace(opts.Trace, pq, baseCount, false, rangeUsed, scanUsed, stats)
	return globalRankedEntriesToEntries(ranked), true, nil
}

func mergeGlobalVerifiedEntries(snapshot globalQuerySnapshot, base []globalRankedEntry, pq parsedQuery, limit int) []globalRankedEntry {
	out := append([]globalRankedEntry(nil), base...)
	for volumeIndex, snap := range snapshot.overlays {
		if snap == nil || volumeIndex >= len(snapshot.volumes) || snapshot.volumes[volumeIndex] == nil {
			continue
		}
		vol := snapshot.volumes[volumeIndex]
		for _, overlay := range vol.overlayRankedMatches(snap, pq, make(map[int]string)) {
			out = append(out, globalRankedEntry{entry: overlay.entry, rank: overlay.rank, volume: volumeIndex, tie: overlay.entry.Path, overlay: true})
		}
	}
	slices.SortStableFunc(out, func(a, b globalRankedEntry) int {
		if n := compareSearchAllEntries(a.entry, b.entry, pq); n != 0 {
			return n
		}
		if n := cmp.Compare(a.volume, b.volume); n != 0 {
			return n
		}
		if n := cmp.Compare(a.rank, b.rank); n != 0 {
			return n
		}
		return strings.Compare(a.tie, b.tie)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func countServiceVolumesGlobalScalarSnapshot(snapshot globalQuerySnapshot, opts queryOptions) (int, bool, error) {
	pq, err := parseQuery(opts)
	if err != nil {
		return 0, true, err
	}
	if !globalScalarQuerySupported(pq) {
		return 0, false, nil
	}
	if !globalPlannerSnapshotReady(snapshot, opts.Trace, "global-scalar") {
		return 0, false, nil
	}
	_, baseCount, rangeUsed, scanUsed, stats, err := executeGlobalScalarBase(snapshot, pq, false, 0)
	if err != nil {
		return 0, true, err
	}
	overlayCount := globalOverlayMatchCount(snapshot.volumes, snapshot.overlays, pq)
	setGlobalScalarTrace(opts.Trace, pq, baseCount, true, rangeUsed, scanUsed, stats)
	return baseCount + overlayCount, true, nil
}

func executeGlobalScalarBase(snapshot globalQuerySnapshot, pq parsedQuery, collect bool, limit int) ([]globalRankedEntry, int, bool, bool, scalarExecutionStats, error) {
	var out []globalRankedEntry
	count := 0
	rangeUsed := false
	scanUsed := false
	stats := scalarExecutionStats{}
	pathCaches := make([]map[int]string, len(snapshot.volumes))
	for volumeIndex, vol := range snapshot.volumes {
		if err := checkQueryCapabilities(pq, vol.index); err != nil {
			return nil, 0, rangeUsed, scanUsed, stats, err
		}
		volumePQ := pq
		dropSatisfiedVolumeTerms(&volumePQ, vol.index.Volume)
		hidden := hiddenBaseIDs{}
		if volumeIndex < len(snapshot.overlays) && snapshot.overlays[volumeIndex] != nil {
			snap := snapshot.overlays[volumeIndex]
			hidden = hiddenBaseIDs{tombstone: snap.tombstoneIDs, shadowed: snap.shadowedIDs}
		}
		rankOf := candidateRanker(vol.index, vol.rankForQuery(pq))
		recordCount := vol.index.compactRecordCount()
		order := []uint32(nil)
		start, end := 0, recordCount
		volumeLimit := 0
		scalar, ranged := scalarRangeForVolume(vol, volumePQ)
		if ranged {
			rangeUsed = true
			stats.interval += max(0, scalar.end-scalar.start)
			order, start, end = scalar.order, scalar.start, scalar.end
			if !collect && scalarPredicateOnly(volumePQ) {
				count += countVisibleScalarRange(scalar, hidden)
				setScalarDriver(&stats, "interval-cardinality")
				continue
			}
			scalarOrder := (scalar.modified && volumePQ.SortColumn == "modified") ||
				(!scalar.modified && volumePQ.SortColumn == "size")
			if collect && limit > 0 && volumePQ.RootBias == "" && volumePQ.CWDBias == "" {
				if scalarOrder {
					volumeLimit = limit
					setScalarDriver(&stats, "scalar-order")
				} else if candidateOrder := vol.orderForQuery(volumePQ); len(candidateOrder) >= recordCount &&
					(scalar.end-scalar.start) > max(limit*4, 1024) {
					// The scalar interval is only a predicate. For default/name or
					// another persisted order, verify that order and stop once this
					// volume has supplied its bounded top-N contribution.
					order, start, end = candidateOrder, 0, len(candidateOrder)
					volumeLimit = limit
					setScalarDriver(&stats, "persisted-order")
				} else {
					setScalarDriver(&stats, "scalar-range")
				}
			} else {
				setScalarDriver(&stats, "scalar-range")
			}
		} else {
			scanUsed = true
			setScalarDriver(&stats, "record-order-scan")
			if collect && limit > 0 {
				candidateOrder := vol.orderForQuery(volumePQ)
				if len(candidateOrder) >= recordCount && pq.RootBias == "" && pq.CWDBias == "" {
					order = candidateOrder
					volumeLimit = limit
				}
			}
			end = compactUint32OrderLen(order, recordCount)
		}
		volumeMatches := 0
		processed := 0
		var rangeIt *globalScalarRangeIterator
		if ranged {
			rangeIt = newGlobalScalarRangeIterator(volumeIndex, scalar)
		}
		for {
			if processed&1023 == 0 && queryCanceled(pq) {
				return nil, 0, rangeUsed, scanUsed, stats, errQueryCanceled
			}
			var id int
			if ranged {
				globalID, ok := rangeIt.Next()
				if !ok {
					break
				}
				id = globalID.local
			} else {
				if start >= end {
					break
				}
				id = compactUint32OrderAt(order, start)
				start++
			}
			processed++
			stats.recordsVerified++
			if !hidden.empty() && hidden.contains(id) {
				continue
			}
			rec := vol.index.compactRecord(id)
			if rec.Deleted || !vol.recordMatchesNonPath(id, rec, volumePQ) {
				continue
			}
			count++
			volumeMatches++
			if !collect {
				continue
			}
			if pathCaches[volumeIndex] == nil {
				pathCaches[volumeIndex] = make(map[int]string)
			}
			path := vol.index.reconstructCompactPathCached(id, pathCaches[volumeIndex])
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
			out = append(out, globalRankedEntry{entry: entry, rank: rankOf(id), volume: volumeIndex, tie: path})
			if volumeLimit > 0 && volumeMatches >= volumeLimit {
				break
			}
		}
	}
	return out, count, rangeUsed, scanUsed, stats, nil
}

func setScalarDriver(stats *scalarExecutionStats, driver string) {
	if stats == nil || driver == "" {
		return
	}
	if stats.driver == "" {
		stats.driver = driver
	} else if stats.driver != driver {
		stats.driver = "mixed"
	}
}

func scalarPredicateOnly(pq parsedQuery) bool {
	return pq.Type == "" && len(pq.AttrFilters) == 0 && len(pq.SizeFilters) == 1 &&
		len(pq.DateFilters) == 0 && !pq.HasModAfter
}

func countVisibleScalarRange(scalar scalarRange, hidden hiddenBaseIDs) int {
	count := max(0, scalar.end-scalar.start)
	if count == 0 || hidden.empty() {
		return count
	}
	seen := make(map[int32]struct{}, len(hidden.tombstone)+len(hidden.shadowed))
	for _, ids := range [2][]int32{hidden.tombstone, hidden.shadowed} {
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			pos := sort.Search(len(scalar.order), func(pos int) bool { return scalar.order[pos] >= uint32(id) })
			if pos >= scalar.start && pos < scalar.end && pos < len(scalar.order) && scalar.order[pos] == uint32(id) {
				count--
			}
		}
	}
	return max(0, count)
}

func globalPlannerSnapshotReady(snapshot globalQuerySnapshot, trace *searchTrace, source string) bool {
	if !snapshot.overlaysOK {
		trace.replaceDecline(source + ":overlay-snapshot-missing")
		return false
	}
	for _, vol := range snapshot.volumes {
		if vol == nil || vol.index == nil {
			trace.addDeclineForVolume(source+":missing-volume", "")
			return false
		}
	}
	return true
}

func setGlobalScalarTrace(trace *searchTrace, pq parsedQuery, candidates int, count bool, rangeUsed, scanUsed bool, stats scalarExecutionStats) {
	if trace == nil {
		return
	}
	mode := "global-scalar"
	if count {
		mode = "global-count-scalar"
	}
	trace.setPlannerMode(mode)
	source := "global:scalar-scan"
	if rangeUsed && !scanUsed {
		source = "global:scalar-range"
	} else if rangeUsed {
		source = "global:scalar-range+scan"
	}
	trace.setSource(source, candidates)
	trace.ScalarDriver = stats.driver
	trace.ScalarInterval = stats.interval
	trace.ScalarRecordsVerified = stats.recordsVerified
	if pq.Type != "" {
		trace.addTerm(traceTerm{Term: pq.Type, Kind: "type", Source: source, CountHint: candidates, Exact: true})
	}
	for range pq.SizeFilters {
		trace.addTerm(traceTerm{Term: "size", Kind: "size", Source: source, CountHint: candidates, Exact: true})
	}
	for range pq.DateFilters {
		trace.addTerm(traceTerm{Term: "dm", Kind: "modified", Source: source, CountHint: candidates, Exact: true})
	}
	if pq.HasModAfter {
		trace.addTerm(traceTerm{Term: "modified-after", Kind: "modified", Source: source, CountHint: candidates, Exact: true})
	}
	trace.setComplete(true)
}
