package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func serviceRequestFromOptions(opts queryOptions, countOnly bool) serviceRequest {
	return serviceRequest{
		Command:       "search",
		Query:         opts.Query,
		MatchPath:     opts.MatchPath || queryLooksLoosePathScoped(opts.Query),
		Limit:         opts.Limit,
		CountOnly:     countOnly,
		Under:         opts.Under,
		Exists:        opts.Exists,
		CWDBias:       opts.CWDBias,
		RootBias:      opts.RootBias,
		Recent:        opts.Recent,
		ModifiedAfter: opts.ModifiedAfter,
		CaseSensitive: opts.CaseSensitive,
		Fuzzy:         opts.Fuzzy,
		DeadlineUnix:  opts.DeadlineUnix,
		RequestSeq:    opts.RequestSeq,
	}
}

func requestToOptionsFromService(req serviceRequest) queryOptions {
	return queryOptions{
		Query:         req.Query,
		MatchPath:     req.MatchPath,
		Limit:         req.Limit,
		Under:         req.Under,
		Exists:        req.Exists,
		CWDBias:       req.CWDBias,
		RootBias:      req.RootBias,
		Recent:        req.Recent,
		ModifiedAfter: req.ModifiedAfter,
		CaseSensitive: req.CaseSensitive,
		Fuzzy:         req.Fuzzy,
		DeadlineUnix:  req.DeadlineUnix,
		RequestSeq:    req.RequestSeq,
	}
}

func search(idx *Index, opts queryOptions, countOnly bool) ([]Entry, error) {
	if idx.Compact {
		return searchCompact(idx, opts, countOnly)
	}
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, err
	}
	if pq.Impossible {
		opts.Trace.setPlannerMode("impossible-query")
		opts.Trace.setSource("impossible-query", 0)
		return []Entry{}, nil
	}
	if queryNeedsAttrs(pq) {
		return nil, errors.New("attrib: filters require a compact NTFS attribute-capable index")
	}
	order := idx.NameOrder
	if pq.MatchPath {
		order = idx.PathOrder
	}
	limit := normalizedLimit(opts.Limit, countOnly)
	if pq.RootBias != "" || pq.CWDBias != "" {
		order = biasOrderEntries(idx, order, firstNonEmpty(pq.CWDBias, pq.RootBias))
	}
	results := make([]Entry, 0, min(limit, 1024))
	for pos, entryIndex := range order {
		if pos&1023 == 0 && queryCanceled(pq) {
			return nil, errQueryCanceled
		}
		entry := idx.Entries[entryIndex]
		entry.IndexSource = idx.Source
		if entryMatches(entry, pq, pq.MatchPath) {
			results = append(results, entry)
			if !countOnly && len(results) >= limit {
				break
			}
		}
	}
	return filterImplicitUnderExisting(results, opts, countOnly), nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func indexInfoToJSON(idx *Index, path string) jsonInfoResponse {
	return jsonInfoResponse{
		OK:          true,
		Version:     idx.Version,
		Source:      idx.Source,
		BuiltAt:     idx.BuiltAt.Format(time.RFC3339Nano),
		Entries:     idx.entryCount(),
		Roots:       append([]string(nil), idx.Roots...),
		Volume:      idx.Volume,
		JournalID:   idx.JournalID,
		Checkpoint:  idx.Checkpoint,
		ContentHash: idx.ContentHash,
		Layout:      estimateIndexLayout(idx, path),
	}
}

func estimateIndexLayout(idx *Index, path string) *indexLayout {
	if idx == nil || !idx.Compact {
		return nil
	}
	recordCount := idx.compactRecordCount()
	layout := &indexLayout{RecordCount: recordCount}
	if info, err := os.Stat(path); err == nil {
		layout.FileBytes = info.Size()
	}
	unique := make(map[string]struct{}, recordCount/2)
	var nameBlobBytes int64
	for i := 0; i < recordCount; i++ {
		rec := idx.compactRecord(i)
		if _, ok := unique[rec.Name]; ok {
			continue
		}
		unique[rec.Name] = struct{}{}
		nameBlobBytes += int64(len(rec.Name))
	}
	layout.UniqueNames = len(unique)
	layout.NameBlobBytes = nameBlobBytes
	layout.NameTableBytes = int64(layout.UniqueNames * 6)
	layout.RecordBytes = int64(layout.RecordCount * compactDiskRecordBytesForCounts(layout.RecordCount, layout.UniqueNames))
	if layout.FileBytes > 0 {
		layout.OtherBytes = layout.FileBytes - layout.RecordBytes - layout.NameBlobBytes - layout.NameTableBytes
		layout.BytesPerRecord = float64(layout.FileBytes) / float64(max(layout.RecordCount, 1))
	}
	return layout
}

func compactNeedsWideDiskRecords(recordCount, tokenCount int) bool {
	return recordCount > int(compactNarrowMaxRecordRef)+1 || tokenCount > int(compactNarrowMaxRecordRef)+1
}

func compactDiskRecordBytesForCounts(recordCount, tokenCount int) int {
	if compactNeedsWideDiskRecords(recordCount, tokenCount) {
		return compactWideDiskRecordBytes
	}
	return compactDiskRecordBytes
}

func entriesToJSON(entries []Entry) []jsonResult {
	out := make([]jsonResult, len(entries))
	for i, entry := range entries {
		out[i] = entryToJSON(entry)
	}
	return out
}

func pathsToJSON(paths []string) []jsonResult {
	out := make([]jsonResult, len(paths))
	for i, path := range paths {
		out[i] = jsonResult{
			Path:        filepath.Clean(path),
			Name:        filepath.Base(path),
			Volume:      filepath.VolumeName(path),
			IndexSource: "service",
		}
	}
	return out
}

func entryToJSON(entry Entry) jsonResult {
	result := jsonResult{
		Path:        filepath.Clean(entry.Path),
		Name:        entry.Name,
		Volume:      filepath.VolumeName(entry.Path),
		IsDir:       entry.Mode&uint32(os.ModeDir) != 0,
		IndexSource: entry.IndexSource,
		Snippet:     entry.Snippet,
	}
	if result.Name == "" {
		result.Name = filepath.Base(result.Path)
	}
	size := entry.Size
	result.Size = &size
	if entry.ModUnix != 0 {
		result.Modified = time.Unix(0, entry.ModUnix).Format(time.RFC3339Nano)
	}
	return result
}

func searchAll(indexes []*Index, opts queryOptions, countOnly bool) ([]Entry, error) {
	if pq, err := parseQuery(opts); err == nil && queryHasFeatureLeaf(pq) {
		return nil, fmt.Errorf("feature queries require the resident seekfs service")
	}
	if pq, err := parseQuery(opts); err == nil && queryHasAnyContentLeaf(pq) {
		// Content is only queryable through the service's FRN-keyed volumes;
		// a direct-index search has no content resolver. Refuse rather than
		// silently ignore the content leaf.
		return nil, contentUnavailableError()
	}
	if len(indexes) == 1 {
		matches, err := search(indexes[0], opts, countOnly)
		if err != nil {
			return nil, err
		}
		return filterImplicitUnderExisting(matches, opts, countOnly), nil
	}
	limit := normalizedLimit(opts.Limit, countOnly)
	results := make([]Entry, 0, min(limit, 1024))
	for _, idx := range indexes {
		childOpts := opts
		childOpts.Limit = limit
		if !countOnly {
			childOpts.Limit = max(limit, idx.compactRecordCount())
		}
		matches, err := search(idx, childOpts, countOnly)
		if err != nil {
			return nil, err
		}
		results = append(results, matches...)
	}
	results = filterImplicitUnderExisting(results, opts, countOnly)
	if !countOnly && entriesSpanMultipleVolumes(results) {
		pq, err := parseQuery(opts)
		if err != nil {
			return nil, err
		}
		sortSearchAllEntries(results, pq)
	}
	if !countOnly && limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func entriesSpanMultipleVolumes(entries []Entry) bool {
	first := ""
	for _, entry := range entries {
		vol := filepath.VolumeName(entry.Path)
		if first == "" {
			first = vol
			continue
		}
		if vol != first {
			return true
		}
	}
	return false
}

func sortSearchAllEntries(entries []Entry, pq parsedQuery) {
	sort.SliceStable(entries, func(i, j int) bool {
		return compareSearchAllEntries(entries[i], entries[j], pq) < 0
	})
}

func compareSearchAllEntries(a, b Entry, pq parsedQuery) int {
	if pq.SortColumn == "size" {
		if a.Size != b.Size {
			if a.Size < b.Size {
				return -1
			}
			return 1
		}
		return compareSearchAllEntryNamePath(a, b)
	}
	if pq.SortColumn == "modified" {
		if a.ModUnix != b.ModUnix {
			if a.ModUnix == 0 {
				return 1
			}
			if b.ModUnix == 0 {
				return -1
			}
			if a.ModUnix > b.ModUnix {
				return -1
			}
			return 1
		}
		return compareSearchAllEntryNamePath(a, b)
	}
	if pq.SortColumn == "extension" {
		ae, be := entryLowerExt(a), entryLowerExt(b)
		if ae != be {
			if ae < be {
				return -1
			}
			return 1
		}
		return compareSearchAllEntryNamePath(a, b)
	}
	if pq.SortColumn == "type" {
		at, bt := entryTypeRank(a), entryTypeRank(b)
		if at != bt {
			return at - bt
		}
		return compareSearchAllEntryNamePath(a, b)
	}
	if pq.SortColumn == "path" {
		return compareSearchAllEntryPath(a, b)
	}
	return compareSearchAllEntryNamePath(a, b)
}

func compareSearchAllEntryNamePath(a, b Entry) int {
	an, bn := entryLowerName(a), entryLowerName(b)
	if an != bn {
		if an < bn {
			return -1
		}
		return 1
	}
	return compareSearchAllEntryPath(a, b)
}

func compareSearchAllEntryPath(a, b Entry) int {
	ap, bp := entryLowerPath(a), entryLowerPath(b)
	if ap != bp {
		if ap < bp {
			return -1
		}
		return 1
	}
	if a.Path != b.Path {
		if a.Path < b.Path {
			return -1
		}
		return 1
	}
	return strings.Compare(a.Name, b.Name)
}

func entryLowerName(entry Entry) string {
	if entry.LowerName != "" {
		return entry.LowerName
	}
	if entry.Name != "" {
		return strings.ToLower(entry.Name)
	}
	return strings.ToLower(filepath.Base(entry.Path))
}

func entryLowerPath(entry Entry) string {
	if entry.LowerPath != "" {
		return entry.LowerPath
	}
	return strings.ToLower(entry.Path)
}

func entryLowerExt(entry Entry) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(entry.Name), "."))
}

func entryTypeRank(entry Entry) int {
	if entry.Mode&uint32(os.ModeDir) != 0 {
		return 0
	}
	return 1
}

func searchServiceVolumes(volumes []*serviceVolumeIndex, opts queryOptions, countOnly bool) ([]Entry, error) {
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, err
	}
	if queryHasFeatureLeaf(pq) {
		return nil, fmt.Errorf("feature queries require the service feature dispatcher")
	}
	if pq.Impossible {
		opts.Trace.setPlannerMode("impossible-query")
		opts.Trace.setSource("impossible-query", 0)
		return []Entry{}, nil
	}
	volumes, err = serviceVolumesForQuery(volumes, opts)
	if err != nil {
		return nil, err
	}
	if len(volumes) == 0 {
		opts.Trace.setEligibleVolumes(volumes)
		opts.Trace.setPlannerMode("volume-empty")
		opts.Trace.setSource("volume-empty", 0)
		return []Entry{}, nil
	}
	volumes = prioritizeServiceVolumesForPathTerms(volumes, opts)
	opts.Trace.setEligibleVolumes(volumes)
	if contentGlobalLaneEnabled() && queryHasPositiveContentLeaf(pq) {
		snapshot := newGlobalQuerySnapshot(volumes, opts.Trace)
		if matches, handled, err := searchServiceVolumesGlobalContentComponentsSnapshot(snapshot, opts, countOnly, pq); handled {
			return matches, err
		}
	}
	if queryHasAnyContentLeaf(pq) {
		// Content has its own candidate + post-filter path. It runs on every
		// content-usable volume; an unusable volume degrades the result rather
		// than bricking the whole service, but if none is usable the query is
		// refused instead of reported as no matches. A query that remains
		// answerable without content still gets the unusable volume's filename
		// matches (PF-7b); an unanswerable one skips it.
		usable, skipped := contentUsableVolumes(volumes, pq)
		if len(usable) == 0 && !filenameAnswerable(pq) {
			return nil, contentUnavailableError()
		}
		markContentQueryDegraded(opts.Trace, skipped)
		markContentQueryIncomplete(opts.Trace, usable)
		return searchContentServiceVolumes(volumes, opts, countOnly, pq)
	}
	snapshot := newGlobalQuerySnapshot(volumes, opts.Trace)
	if matches, handled, err := searchServiceVolumesGlobalExtOnlySnapshot(snapshot, opts, countOnly); handled {
		return matches, err
	}
	if matches, handled, err := searchServiceVolumesGlobalComponentsOnlySnapshot(snapshot, opts, countOnly); handled {
		return matches, err
	}
	if matches, handled, err := searchServiceVolumesGlobalNameSnapshot(snapshot, opts, countOnly); handled {
		return matches, err
	}
	if matches, handled, err := searchServiceVolumesGlobalScalarSnapshot(snapshot, opts, countOnly); handled {
		return matches, err
	}
	if matches, handled, err := searchServiceVolumesGlobalBoundedFallbackSnapshot(snapshot, opts, countOnly); handled {
		return matches, err
	}
	if len(volumes) > 1 {
		return nil, globalMultiVolumePlannerDeclineError(opts)
	}
	if len(volumes) == 1 {
		if opts.Trace != nil && strings.HasPrefix(opts.Trace.Decline, "global-") {
			opts.Trace.setFallback("service-single-volume")
		}
		opts.Trace.setPlannerMode("service-single-volume")
		vol := volumes[0]
		locked, ok := lockVolumeSearch(vol, opts)
		if !ok {
			return nil, errQueryCanceled
		}
		pathCache := make(map[int]string)
		hidden := vol.snapshotHiddenBaseIDs()
		candidateFn := vol.nameTermCandidates
		if vol.hasActiveOverlay() && !countOnly {
			if hidden.empty() {
				candidateFn = vol.overlayAwareNameTermCandidates
			} else {
				candidateFn = nil
			}
		}
		baseOpts := opts
		if vol.hasActiveOverlay() && !countOnly {
			// The overlay may outrank a base entry that was just outside the
			// base top-N. Retain one bounded base window per live overlay slot;
			// mergeOverlayMatches applies the actual Entry comparator before the
			// caller's final limit.
			overlayMatches := 0
			if snap := vol.snap.Load(); snap != nil {
				pq, parseErr := parseQuery(opts)
				if parseErr != nil {
					return nil, parseErr
				}
				var countErr error
				overlayMatches, countErr = vol.overlayLiveMatchCountCancellable(snap, pq)
				if countErr != nil {
					return nil, countErr
				}
			}
			baseOpts.Limit = normalizedLimit(opts.Limit, false) + overlayMatches
			if opts.Trace != nil {
				opts.Trace.OverlayBaseWindow = baseOpts.Limit
			}
		}
		matches, err := searchCompactWithCacheHidden(vol.index, baseOpts, countOnly, pathCache, candidateFn, hidden, nil)
		matches = vol.mergeOverlayMatches(matches, opts, countOnly, pathCache)
		vol.trimSearchCachesLocked()
		if locked {
			vol.searchMu.Unlock()
		}
		matches = filterImplicitUnderExisting(matches, opts, countOnly)
		if err == nil && len(matches) == 0 {
			if fallback, ok, complete := filesystemUnderFallbackSearch(opts, countOnly); ok {
				opts.Trace.setFallback("filesystem-under-fallback")
				opts.Trace.setSource("filesystem-under-fallback", len(fallback))
				opts.Trace.setComplete(complete)
				return fallback, nil
			}
		}
		return matches, err
	}
	return nil, nil
}

func queryTermPromotedToExtension(term string, exts []string) bool {
	for _, ext := range exts {
		if strings.TrimPrefix(ext, ".") == term {
			return true
		}
	}
	return false
}

func (vol *serviceVolumeIndex) mergeOverlayMatches(base []Entry, opts queryOptions, countOnly bool, pathCache map[int]string) []Entry {
	if vol == nil {
		return base
	}
	if opts.contentCount != nil {
		// Count mode: the compact scan tallied base matches in place and
		// countContentVolume adds the overlay count (where cancellation is
		// surfaced). Materializing entries here would defeat that.
		return base
	}
	snap := vol.snap.Load()
	if snap == nil || len(snap.records) == 0 {
		return base
	}
	pq, err := parseQuery(opts)
	if err != nil {
		return base
	}
	limit := normalizedLimit(opts.Limit, countOnly)
	if queryHasAnyContentLeaf(pq) {
		// Inline content verification owns the per-volume limit already;
		// truncating the merged set here would drop a true match that the
		// caller's post-merge limit has not yet considered.
		limit = 0
	}
	overlay := vol.overlayRankedMatches(snap, pq, pathCache)
	if !countOnly {
		merged := make([]Entry, 0, len(base)+len(overlay))
		merged = append(merged, base...)
		for _, item := range overlay {
			merged = append(merged, item.entry)
		}
		sort.SliceStable(merged, func(i, j int) bool {
			return compareSearchAllEntries(merged[i], merged[j], pq) < 0
		})
		if limit > 0 && len(merged) > limit {
			merged = merged[:limit]
		}
		return merged
	}
	out := vol.mergeRankedOverlayEntries(base, overlay, limit, countOnly, pq)
	return out
}

func (vol *serviceVolumeIndex) overlayRankedMatches(snap *volumeSnapshot, pq parsedQuery, pathCache map[int]string) []rankedOverlayEntry {
	if vol == nil || snap == nil || len(snap.records) == 0 {
		return nil
	}
	watermark := int(snap.watermark)
	records := snap.records
	if watermark > len(records) {
		watermark = len(records)
	}
	records = records[:watermark]
	if len(records) == 0 {
		return nil
	}
	latest := latestOverlaySlotsByFRN(records)
	var contentMatcher *contentLeafMatcher
	if queryHasAnyContentLeaf(pq) {
		contentMatcher = newContentLeafMatcher(pq)
	}
	overlay := make([]rankedOverlayEntry, 0, len(records))
	for slot := 0; slot < len(records); slot++ {
		entry, ok := vol.overlayEntry(records, latest, slot, map[int32]struct{}{}, pathCache)
		if !ok {
			continue
		}
		if entryMatchesWithContentMatcher(vol, entry, pq, pq.MatchPath, contentMatcher) {
			overlay = append(overlay, rankedOverlayEntry{entry: entry, rank: vol.overlayEntryRank(entry, pq)})
		}
	}
	sort.SliceStable(overlay, func(i, j int) bool {
		if overlay[i].rank == overlay[j].rank {
			if pq.SortColumn == "path" {
				ip, jp := overlay[i].entry.LowerPath, overlay[j].entry.LowerPath
				if ip == "" {
					ip = strings.ToLower(overlay[i].entry.Path)
				}
				if jp == "" {
					jp = strings.ToLower(overlay[j].entry.Path)
				}
				if ip == jp {
					return overlay[i].entry.Path < overlay[j].entry.Path
				}
				return ip < jp
			}
			in, jn := overlay[i].entry.LowerName, overlay[j].entry.LowerName
			if in == "" {
				in = strings.ToLower(overlay[i].entry.Name)
			}
			if jn == "" {
				jn = strings.ToLower(overlay[j].entry.Name)
			}
			if in == jn {
				return overlay[i].entry.Path < overlay[j].entry.Path
			}
			return in < jn
		}
		return overlay[i].rank < overlay[j].rank
	})
	return overlay
}

func (vol *serviceVolumeIndex) snapshotHiddenBaseIDs() hiddenBaseIDs {
	if vol == nil {
		return hiddenBaseIDs{}
	}
	snap := vol.snap.Load()
	if snap == nil {
		return hiddenBaseIDs{}
	}
	return hiddenBaseIDs{tombstone: snap.tombstoneIDs, shadowed: snap.shadowedIDs}
}

// serviceWatchDelta returns the file-change events for a watch client since
// each volume's overlay watermark. It reads each volume's published snapshot
// (base + overlay records + watermark published atomically together) and
// emits, per FRN touched after that volume's cursor, the transition between
// the state at the cursor boundary and the state now, filtered through the
// watch query. This lets a watch client pay only for changed records instead
// of re-running the full query every tick, and it never takes vol.mu (which a
// background persist can hold for minutes during the multi-GB compaction).
//
// The cursor is the overlay watermark (len of overlay.records at snapshot
// time). On background persist the overlay is rebuilt and the watermark
// resets to 0; a requested cursor beyond the current watermark means that
// volume's cursor is stale, and the caller must re-baseline that volume.
func serviceWatchDelta(volumes []*serviceVolumeIndex, since []watchVolumeCursor, pq parsedQuery, trace *searchTrace) ([]watchVolumeCursor, []watchDeltaEvent, error) {
	if len(volumes) == 0 {
		return since, nil, nil
	}
	sinceMap := make(map[string]uint64, len(since))
	for _, c := range since {
		sinceMap[c.Volume] = c.Seq
	}
	out := make([]watchDeltaEvent, 0, 32)
	cursors := make([]watchVolumeCursor, 0, len(volumes))
	for _, vol := range volumes {
		if vol == nil || vol.index == nil {
			continue
		}
		snap := vol.snap.Load()
		watermark := uint64(0)
		records := []CompactRecord(nil)
		if snap != nil {
			watermark = uint64(snap.watermark)
			records = snap.records
			if watermark > uint64(len(records)) {
				watermark = uint64(len(records))
			}
		}
		sinceSeq, known := sinceMap[vol.volume]
		if !known {
			// New volume: baseline it silently at its current watermark.
			cursors = append(cursors, watchVolumeCursor{Volume: vol.volume, Seq: watermark})
			continue
		}
		if sinceSeq > watermark {
			// Overlay rebuilt (persist) after this volume's cursor. Signal
			// reset; the client re-baselines this volume.
			cursors = append(cursors, watchVolumeCursor{Volume: vol.volume, Seq: watermark, Reset: true})
			continue
		}
		cursors = append(cursors, watchVolumeCursor{Volume: vol.volume, Seq: watermark})
		if sinceSeq >= watermark {
			continue
		}
		out = append(out, vol.overlayDelta(records, watermark, sinceSeq, pq, trace)...)
	}
	return cursors, out, nil
}

// overlayDelta emits the changed-overlay events for slots [since, watermark).
// It compares each FRN's state at the cursor boundary against its state now
// so created/modified/deleted exactly match a poll-and-diff against the
// previous snapshot, while only walking changed slots.
func (vol *serviceVolumeIndex) overlayDelta(records []CompactRecord, watermark, since uint64, pq parsedQuery, trace *searchTrace) []watchDeltaEvent {
	if vol == nil || len(records) == 0 || since >= watermark {
		return nil
	}
	if int(since) >= len(records) {
		return nil
	}
	window := records[since:watermark]
	if len(window) == 0 {
		return nil
	}
	latest := latestOverlaySlotsByFRN(records)
	// prevSlots: last slot per FRN at or before the cursor boundary.
	prevSlots := make(map[uint64]int32)
	for i := int(since) - 1; i >= 0; i-- {
		frn := records[i].FRN
		if frn == 0 {
			continue
		}
		if _, exists := prevSlots[frn]; !exists {
			prevSlots[frn] = int32(i)
		}
	}
	// curSlots: last slot per FRN in the window (touched since cursor).
	curSlots := make(map[uint64]int32, len(window))
	for i := 0; i < len(window); i++ {
		if frn := window[i].FRN; frn != 0 {
			curSlots[frn] = int32(i)
		}
	}
	pathCache := make(map[int]string)
	out := make([]watchDeltaEvent, 0, len(curSlots))
	for frn, curOffset := range curSlots {
		curSlot := int(curOffset) + int(since)
		curRec := records[curSlot]
		curEntry, curOK := vol.overlaySlotEntry(records, latest, curSlot, make(map[int32]struct{}), pathCache)
		prevSlot, hadPrev := prevSlots[frn]
		var prevEntry Entry
		prevOK := false
		if hadPrev {
			prevEntry, prevOK = vol.overlaySlotEntry(records, latest, int(prevSlot), make(map[int32]struct{}), pathCache)
		}
		if !prevOK && !curOK && curRec.Deleted {
			// Created and deleted entirely within the window: nothing to
			// report, matching poll-and-diff (absent both before and after).
			continue
		}
		var ev *watchDeltaEvent
		switch {
		case prevOK && !curOK:
			if entryMatches(prevEntry, pq, pq.MatchPath) {
				ev = &watchDeltaEvent{Volume: vol.volume, Event: "deleted", Path: prevEntry.Path}
			}
		case !prevOK && curOK:
			if entryMatches(curEntry, pq, pq.MatchPath) {
				ev = &watchDeltaEvent{Volume: vol.volume, Event: "created", Path: curEntry.Path}
			}
		case prevOK && curOK:
			if prevEntry.Path != curEntry.Path {
				// Rename: emit deleted(old)+created(new), matching poll-diff.
				if entryMatches(prevEntry, pq, pq.MatchPath) {
					out = append(out, watchDeltaEvent{Volume: vol.volume, Event: "deleted", Path: prevEntry.Path})
				}
				if entryMatches(curEntry, pq, pq.MatchPath) {
					ev = &watchDeltaEvent{Volume: vol.volume, Event: "created", Path: curEntry.Path}
				}
			} else if entryMatches(curEntry, pq, pq.MatchPath) && (prevEntry.Size != curEntry.Size || prevEntry.ModUnix != curEntry.ModUnix) {
				ev = &watchDeltaEvent{Volume: vol.volume, Event: "modified", Path: curEntry.Path}
			}
		}
		if ev == nil {
			continue
		}
		entry := curEntry
		if ev.Event == "deleted" {
			entry = prevEntry
		}
		if entry.Size != 0 {
			ev.Size = entry.Size
		}
		if entry.ModUnix != 0 {
			ev.Mtime = time.Unix(0, entry.ModUnix).UTC().Format(time.RFC3339)
		}
		out = append(out, *ev)
	}
	var created, modified, deleted []watchDeltaEvent
	for _, ev := range out {
		switch ev.Event {
		case "created":
			created = append(created, ev)
		case "modified":
			modified = append(modified, ev)
		default:
			deleted = append(deleted, ev)
		}
	}
	sortEvents := func(list []watchDeltaEvent) {
		sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
	}
	sortEvents(created)
	sortEvents(modified)
	sortEvents(deleted)
	ordered := make([]watchDeltaEvent, 0, len(out))
	ordered = append(ordered, created...)
	ordered = append(ordered, modified...)
	ordered = append(ordered, deleted...)
	return ordered
}
