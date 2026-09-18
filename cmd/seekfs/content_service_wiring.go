package main

// P2.5 service wiring: attach a loaded `.gsx` to a service volume, keep its
// resolver in step with base swaps, and drain changed files on a timer.
//
// Everything here is inert unless SEEKFS_CONTENT_SEARCH=1.

import (
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// contentIndexPathForDB returns the `.gsx` sidecar path for a `.gsi`.
func contentIndexPathForDB(dbPath string) string {
	if dbPath == "" {
		return ""
	}
	ext := filepath.Ext(dbPath)
	if ext == "" {
		return dbPath + ".gsx"
	}
	return strings.TrimSuffix(dbPath, ext) + ".gsx"
}

// contentBaseFRNColumns returns the persisted FRN column used to join content
// docs to record IDs.
func contentBaseFRNColumns(idx *Index) ([]uint64, []uint32, bool) {
	if idx == nil {
		return nil, nil, false
	}
	frns := idx.Derived.FRNs
	ids := idx.Derived.FRNRecordIDs
	if len(frns) == 0 || len(frns) != len(ids) {
		return nil, nil, false
	}
	return frns, ids, true
}

// attachContentForVolume loads the volume's `.gsx` and starts its drain loop.
// A missing or unreadable index leaves the volume in the `unavailable` state, so
// queries are refused rather than reported as no matches.
func (s *goSearchService) attachContentForVolume(vol *serviceVolumeIndex) {
	if vol == nil || !contentSearchEnabled() {
		return
	}
	// Only USN volumes can join content docs (keyed by FRN) to records. Walk
	// volumes use synthesized FRNs and a path-keyed `.gsx`, so attaching would
	// advertise `ready` with a resolver that maps nothing.
	if vol.index == nil || vol.index.Source != "usn" {
		return
	}
	if vol.contentCoord != nil && vol.contentCoord.drainEnabledNow() {
		return
	}
	gsx := contentIndexPathForDB(vol.dbPath)
	if gsx == "" {
		return
	}
	idx, err := contentLoadFile(gsx)
	if err != nil {
		serviceLog("content index unavailable for volume %s: %v", vol.volume, err)
		return
	}
	if idx.Origin != contentOriginUSN {
		// A path-keyed (walk) sidecar does not join to record FRNs; attaching it
		// would report `ready` while every document resolves to nothing.
		serviceLog("content index for volume %s is not FRN-keyed (origin=%d); leaving unavailable", vol.volume, idx.Origin)
		return
	}
	reader, err := openContentReader(idx)
	if err != nil {
		serviceLog("content index unreadable for volume %s: %v", vol.volume, err)
		return
	}
	// Read the record table under the same lock the search path uses, so a
	// concurrent rebuild cannot unmap it underneath us.
	s.indexMu.RLock()
	frns, ids, ok := contentBaseFRNColumns(vol.index)
	s.indexMu.RUnlock()
	if !ok {
		serviceLog("content index for volume %s has no FRN column; leaving unavailable", vol.volume)
		return
	}
	vol.content.setReady(idx, reader, buildContentResolver(idx.Docs, frns, ids))
	if vol.contentCoord != nil {
		vol.contentCoord.enableDrain()
		go s.contentDrainLoop(vol)
	}
	serviceLog("content index loaded for volume %s docs=%d", vol.volume, len(idx.Docs))
}

// contentDrainInterval is the quiet-window + drain cadence.
const contentDrainInterval = 2 * time.Second

func (s *goSearchService) contentDrainLoop(vol *serviceVolumeIndex) {
	ticker := time.NewTicker(contentDrainInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			if vol.contentCoord == nil {
				return
			}
			vol.contentCoord.promoteAll()
			// A private path cache: the drain must not touch vol.pathCache,
			// which the search path trims under searchMu (a different lock).
			pathCache := make(map[int]string, 64)
			vol.contentCoord.processQueue(func(frn uint64) (string, bool) {
				return s.contentResolvePath(vol, frn, pathCache)
			})
		}
	}
}

// contentResolvePath maps an FRN to its current path, preferring the overlay
// (recently changed files that are not yet folded into the base) and falling
// back to the base record table.
func (s *goSearchService) contentResolvePath(vol *serviceVolumeIndex, frn uint64, pathCache map[int]string) (string, bool) {
	if vol == nil {
		return "", false
	}
	// The index and path cache are replaced (and the mmap closed) under
	// indexMu during persist/rebuild; take the same read lock the search path
	// holds, ordered before vol.mu, so a drain tick cannot read an unmapped
	// table.
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	vol.mu.Lock()
	defer vol.mu.Unlock()
	if vol.index == nil {
		return "", false
	}
	if vol.overlay != nil {
		latest := latestOverlaySlotsByFRN(vol.overlay.records)
		if slot, ok := latest[frn]; ok {
			seen := make(map[int32]struct{}, 4)
			if p := vol.overlayRecordPath(vol.overlay.records, latest, int(slot), seen, pathCache); p != "" {
				return p, true
			}
		}
	}
	if id, ok := vol.idForFRN(frn); ok {
		if p := vol.index.reconstructCompactPathCached(id, pathCache); p != "" {
			return p, true
		}
	}
	// Low-memory mode does not build vol.frns, so fall back to the persisted FRN
	// column; otherwise a changed file would resolve to false and be dropped
	// from the delta.
	derived := vol.index.Derived
	if i, ok := contentLookupFRNColumn(derived.FRNs, frn); ok && i < len(derived.FRNRecordIDs) {
		if p := vol.index.reconstructCompactPathCached(int(derived.FRNRecordIDs[i]), pathCache); p != "" {
			return p, true
		}
	}
	return "", false
}

// contentLookupFRNColumn binary-searches the persisted FRN column (sorted by
// FRN) for a file reference.
func contentLookupFRNColumn(frns []uint64, frn uint64) (int, bool) {
	i := sort.Search(len(frns), func(i int) bool { return frns[i] >= frn })
	if i < len(frns) && frns[i] == frn {
		return i, true
	}
	return 0, false
}

// recordIDForFRN resolves an FRN to a base record id, falling back to the
// persisted FRN column when the resident arrays are absent (low-memory mode).
// Content delta docs and candidate mapping must never drop a record just
// because the resident FRN index was not built.
func (vol *serviceVolumeIndex) recordIDForFRN(frn uint64) (int, bool) {
	if vol == nil || vol.index == nil || frn == 0 {
		return 0, false
	}
	if id, ok := vol.idForFRN(frn); ok {
		return id, true
	}
	derived := vol.index.Derived
	if i, ok := contentLookupFRNColumn(derived.FRNs, frn); ok && i < len(derived.FRNRecordIDs) {
		return int(derived.FRNRecordIDs[i]), true
	}
	return 0, false
}

// invalidateContentAfterBaseReset drops a volume's decoded content index and
// any pending content work when the base was rebuilt against a different USN
// journal generation. The content docs are FRN-keyed against the old journal,
// so continuing to serve them would be stale; the volume is left stale (not
// ready, not silently empty) until a full content rebuild.
func invalidateContentAfterBaseReset(vol *serviceVolumeIndex, reason string) {
	if vol == nil || vol.content == nil {
		return
	}
	if vol.contentCoord != nil {
		vol.contentCoord.invalidate(reason)
		return
	}
	vol.content.markStale(reason)
}

// rebindContentAfterBaseSwap rebuilds a volume's content->record resolver
// against its new record table. Content itself is untouched: docs are keyed on
// FRN, so a persist/compaction swap must never re-extract.
func rebindContentAfterBaseSwap(vol *serviceVolumeIndex) {
	if vol == nil || vol.content == nil {
		return
	}
	frns, ids, ok := contentBaseFRNColumns(vol.index)
	if !ok {
		return
	}
	vol.content.rebuildResolver(frns, ids)
}
