package main

// P2.5 service wiring: attach a loaded `.gsx` to a service volume, keep its
// resolver in step with base swaps, and drain changed files on a timer.
//
// Everything here is inert unless SEEKFS_CONTENT_SEARCH=1.

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/windows"
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
	if vol == nil || !contentSearchEnabled() || vol.content == nil {
		return
	}
	if contentScopeForVolume(s.contentCfg, vol.volume).disabled() {
		return
	}
	// Only USN volumes can join content docs (keyed by FRN) to records. Walk
	// volumes use synthesized FRNs and a path-keyed `.gsx`, so attaching would
	// advertise `ready` with a resolver that maps nothing.
	s.indexMu.RLock()
	eligible := vol.index != nil && vol.index.Source == "usn"
	s.indexMu.RUnlock()
	if !eligible {
		return
	}
	s.acquireContentVolumeLock(vol)
	// A volume that already has a usable base attached is done. This replaced
	// the old "drain enabled" guard: after a journal reset invalidates the base
	// the drain stays enabled, so keying on it would skip the re-attach of a
	// freshly rebuilt sidecar (PF-3/WP1e).
	if vol.content.readerView() != nil {
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
	// Until this base is attached it owns the only reference to a mapped
	// sidecar; release it on any path that declines to attach so the mapping is
	// not held by a dead index (the finalizer would eventually, but this is
	// prompt).
	attached := false
	defer func() {
		if !attached {
			idx.Release()
		}
	}()
	if idx.Origin != contentOriginUSN {
		// A path-keyed (walk) sidecar does not join to record FRNs; attaching it
		// would report `ready` while every document resolves to nothing.
		serviceLog("content index for volume %s is not FRN-keyed (origin=%d); leaving unavailable", vol.volume, idx.Origin)
		return
	}
	// P1/WP11: a base whose docs were produced by an older/other extractor
	// version (or written before the class was populated) would serve stale
	// text. A registered-class version bump is refreshed in place: the stale
	// FRNs are enqueued for the drain to re-extract with the current extractor
	// (re-stamping class/version in the delta, then the base via the fold). The
	// old text is still served — a valid, if older, extraction — with a degraded
	// refresh signal until the fold publishes the refreshed base. An
	// unregistered class, an unrefreshable old doc, or an over-large stale set
	// falls back to the full rebuild. See contentPlanStaleExtractorRefresh.
	var refreshFRNs []uint64
	if reason, mismatch := contentIndexExtractorMismatch(idx); mismatch {
		if vol.contentCoord == nil {
			serviceLog("content index for volume %s %s; leaving stale (no coordinator)", vol.volume, reason)
			vol.content.markStale(reason)
			return
		}
		pathCache := make(map[int]string, 64)
		plan := contentPlanStaleExtractorRefresh(idx, func(frn uint64) (string, bool) {
			return s.contentResolvePath(vol, frn, pathCache)
		})
		if plan.rebuild {
			serviceLog("content index for volume %s %s; leaving stale", vol.volume, plan.reason)
			vol.content.markStale(plan.reason)
			return
		}
		refreshFRNs = plan.refreshFRNs
		serviceLog("content index for volume %s %s; refreshing %d doc(s) in place", vol.volume, reason, len(refreshFRNs))
	}
	// A USN-origin base with no journal generation or no checkpoint watermark
	// cannot be joined or caught up: a zero watermark would attach `ready` and
	// never catch up. Treat it as stale/needs-rebuild instead; PF-3 schedules
	// the rebuild.
	if idx.JournalID == 0 || idx.CheckpointUSN == 0 {
		reason := "content base has no USN watermark"
		serviceLog("content index for volume %s %s; leaving stale", vol.volume, reason)
		vol.content.markStale(reason)
		return
	}
	reader, err := openContentReader(idx)
	if err != nil {
		serviceLog("content index unreadable for volume %s: %v", vol.volume, err)
		return
	}
	// Validate and publish under indexMu for writing: every base rebuild and
	// content invalidation runs under indexMu.Lock, and queries hold
	// indexMu.RLock for their whole duration (serviceCommandSearch). Holding the
	// write lock here both rejects a base swapped or marked stale while this
	// attach was in flight and guarantees no query is reading the replaced base,
	// so its mapping can be released immediately below without a use-after-unmap.
	s.indexMu.Lock()
	if vol.state != "ready" {
		state := vol.state
		s.indexMu.Unlock()
		// A non-ready volume is owned by a rebuild (stale recovery). Do not
		// publish content for it. Re-attaching after the rebuild finishes is
		// PF-3 (service-owned build + rebuild scheduling); until then the
		// volume's content stays unavailable until the next restart.
		serviceLog("content attach skipped volume=%s state=%s; let the rebuild finish (PF-3 re-attach)", vol.volume, state)
		return
	}
	frns, ids, ok := contentBaseFRNColumns(vol.index)
	journalID := vol.journalID
	if !ok {
		s.indexMu.Unlock()
		serviceLog("content index for volume %s has no FRN column; leaving unavailable", vol.volume)
		return
	}
	// WP0/PB3: a base built against a different USN journal generation can
	// never be joined to this volume's current FRNs. Refuse it (stale) rather
	// than advertise ready with docs that resolve to nothing; PF-3 schedules
	// the rebuild.
	if idx.JournalID != journalID {
		s.indexMu.Unlock()
		reason := fmt.Sprintf("content base journal %d != volume journal %d", idx.JournalID, journalID)
		serviceLog("content index journal mismatch for volume %s: %s", vol.volume, reason)
		vol.content.markStale(reason)
		return
	}
	resolver := buildContentResolver(idx.Docs, frns, ids)
	// A content index whose docs join none of the base's FRNs would serve every
	// query as an empty result set while advertising `ready` (the candidate path
	// drops unmapped docs and still returns ok). Refuse to publish it: a
	// zero-join base is a fault (built against a different base/journal), not an
	// empty corpus.
	if len(idx.Docs) > 0 && resolver.liveDocCount() == 0 {
		s.indexMu.Unlock()
		reason := "content docs do not join the base FRN column"
		serviceLog("content index for volume %s %s; leaving stale", vol.volume, reason)
		vol.content.markStale(reason)
		return
	}
	if old := vol.content.setReady(idx, reader, resolver); old != nil {
		// The replaced base's Sections may alias a mapping; no query can be
		// reading them under the write lock, so unmap now. Release is once-only,
		// so a concurrent finalizer is harmless.
		old.Release()
	}
	attached = true
	s.indexMu.Unlock()
	if vol.contentCoord != nil {
		// Gate folds until catch-up finishes: observedUSN spans the live replay
		// and catch-up streams, so a fold before this completes could persist a
		// checkpoint ahead of the delta's coverage (PB2/WP1c).
		vol.content.markCatchUpPending()
		// Start the drain loop only once per volume; a re-attach after a
		// rebuild must not spawn a second loop. enableDrain stays idempotent.
		startDrain := !vol.contentCoord.drainEnabledNow()
		vol.contentCoord.enableDrain()
		// A targeted extractor refresh enqueues the stale base FRNs before the
		// drain loop starts, so the first tick re-extracts them. The queue is a
		// set, so a duplicate from the USN stream collapses.
		if len(refreshFRNs) > 0 {
			vol.content.markExtractorRefresh(len(refreshFRNs))
			vol.contentCoord.enqueueFRNs(refreshFRNs)
		}
		if startDrain {
			go s.contentDrainLoop(vol)
		}
		// Catch up OFF the startup path: a far-behind base must not block
		// startup or grow an unbounded delta. Catch-up runs after the drain is
		// enabled so the FRNs it enqueues are actually processed; files changed
		// while the service was stopped become findable in content, matching
		// filename search.
		contentCatchUpAsync(func() { s.contentCatchUpAfterAttach(vol, idx) })
	}
	serviceLog("content index loaded for volume %s docs=%d", vol.volume, len(idx.Docs))
}

// acquireContentVolumeLock records this service as the owner of vol's content
// sidecar (M10) by taking the `.gsx.lock` advisory lock, held for the process
// lifetime. It is idempotent and best-effort: a lock already held for the path
// is reused, and a path owned by another process is logged (once) and skipped so
// the service still attaches (the atomic sidecar write keeps the file intact,
// and the CLI refuses when it finds the service's lock). A later attempt is made
// from the drain tick so the service eventually owns the lock after a
// startup-race loss (M10 residual). A nil receiver or volume is a no-op.
func (s *goSearchService) acquireContentVolumeLock(vol *serviceVolumeIndex) {
	if s == nil || vol == nil {
		return
	}
	gsx := contentIndexPathForDB(vol.dbPath)
	if gsx == "" {
		return
	}
	s.contentLocksMu.Lock()
	defer s.contentLocksMu.Unlock()
	if _, ok := s.contentLocks[gsx]; ok {
		return
	}
	lk, err := acquireContentVolumeLock(gsx)
	if err != nil {
		if !s.contentLockWarned[gsx] {
			serviceLog("content sidecar lock volume=%s path=%s not acquired: %v", vol.volume, gsx, err)
			if s.contentLockWarned == nil {
				s.contentLockWarned = make(map[string]bool)
			}
			s.contentLockWarned[gsx] = true
		}
		return
	}
	delete(s.contentLockWarned, gsx)
	if s.contentLocks == nil {
		s.contentLocks = make(map[string]*contentVolumeLock)
	}
	s.contentLocks[gsx] = lk
}

// retryContentVolumeLock makes one bounded re-acquisition attempt for vol's
// sidecar lock if it is not yet held. The drain tick calls it, so a service that
// lost the startup `.gsx.lock` race to a CLI build takes ownership once that
// build releases it, instead of never re-acquiring (M10 residual). The attempt
// is a single non-blocking LockFileEx and is a map lookup once the lock is held.
func (s *goSearchService) retryContentVolumeLock(vol *serviceVolumeIndex) {
	if s == nil || vol == nil {
		return
	}
	gsx := contentIndexPathForDB(vol.dbPath)
	if gsx == "" {
		return
	}
	s.contentLocksMu.Lock()
	_, held := s.contentLocks[gsx]
	s.contentLocksMu.Unlock()
	if held {
		return
	}
	s.acquireContentVolumeLock(vol)
}

// contentCatchUpMaxChanges and contentCatchUpMaxBytes bound restart catch-up so
// a base far behind a busy journal cannot make startup do unbounded work or
// queue an unbounded delta (a record count alone permits 1M files times the
// 16 MiB text cap). Hitting either cap marks the volume incomplete/degraded
// rather than silently serving a partial delta. Vars so tests can lower them.
var (
	contentCatchUpMaxChanges = 1 << 20
	contentCatchUpMaxBytes   = int64(256 << 20)
)

// contentCatchUpAsync schedules restart catch-up off the startup path, like the
// drain loop. It is a package var so tests can run catch-up synchronously and
// deterministically.
var contentCatchUpAsync = func(fn func()) { go fn() }

// contentCatchUpJournal returns the live journal bounds for a volume. It is a
// package var so tests can drive the wrap/generation-mismatch branches without
// a real journal.
var contentCatchUpJournal = func(volume string) (usnJournalDataV0, error) {
	handle, err := openVolume(volume)
	if err != nil {
		return usnJournalDataV0{}, err
	}
	defer windows.CloseHandle(handle)
	return queryUSNJournal(handle)
}

// contentCatchUpUSNRead reads one USN batch for restart catch-up. It is a
// package var so tests can drive catch-up without a real NTFS journal. The
// production path opens a fresh volume handle per batch (it does not hold one
// reader open across the catch-up).
var contentCatchUpUSNRead = func(volume string, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
	handle, err := openVolume(volume)
	if err != nil {
		return startUSN, nil, err
	}
	defer windows.CloseHandle(handle)
	return readUSNChanges(handle, journalID, startUSN, buffer)
}

// contentCatchUpAfterAttach replays the USN records the base missed while the
// service was stopped, enqueueing each changed FRN for extraction. The drain
// must already be enabled (attach does this first). A base with no USN
// watermark, or one already at/ahead of the volume checkpoint, needs no work.
func (s *goSearchService) contentCatchUpAfterAttach(vol *serviceVolumeIndex, idx *contentIndex) {
	if vol == nil || vol.contentCoord == nil || idx == nil || vol.volume == "" {
		return
	}
	from := int64(idx.CheckpointUSN)
	if idx.JournalID == 0 || from <= 0 {
		// Not USN-valid (a hand-built or walk-derived base). Leave it alone;
		// PF-3 owns the rebuild that would stamp a checkpoint. Nothing to
		// replay, so the fold gate may clear.
		vol.content.markCatchUpComplete()
		vol.contentBuildAttempts.Store(0)
		return
	}
	// Validate the base's watermark against the live journal before reading. A
	// wrapped journal (checkpoint below FirstUsn/LowestValidUsn) or a reset
	// generation must be stale/needs-rebuild, never a normal read that silently
	// misses records. validateUSNCheckpoint is the same bound check the record
	// index uses (service_replay.go).
	journal, err := contentCatchUpJournal(vol.volume)
	if err != nil {
		serviceLog("content catch-up journal query error volume=%s err=%v", vol.volume, err)
		s.contentCatchUpFailed(vol, "journal query error")
		return
	}
	probe := &serviceVolumeIndex{journalID: idx.JournalID, checkpoint: from}
	if err := validateUSNCheckpoint(probe, journal); err != nil {
		serviceLog("content catch-up checkpoint invalid volume=%s from=%d err=%v", vol.volume, from, err)
		vol.content.markStale(err.Error())
		return
	}
	vol.mu.Lock()
	target := vol.checkpoint
	volumeJournal := vol.journalID
	vol.mu.Unlock()
	if volumeJournal != 0 && volumeJournal != idx.JournalID {
		// A journal reset raced the attach; the base is not this generation.
		vol.content.markStale(fmt.Sprintf("content base journal %d != volume journal %d", idx.JournalID, volumeJournal))
		return
	}
	if from >= target {
		vol.content.markCatchUpComplete()
		vol.contentBuildAttempts.Store(0)
		return
	}
	buffer := make([]byte, 4*1024*1024)
	cur := from
	processed := 0
	var processedBytes int64
	for cur < target {
		if !s.contentCatchUpGenerationCurrent(vol, idx.JournalID) {
			// A journal reset or base swap landed mid-loop; the rest of the
			// read is for a generation these FRNs can no longer join. Abort
			// instead of enqueueing wasted extraction work.
			reason := fmt.Sprintf("content base journal %d no longer current", idx.JournalID)
			serviceLog("content catch-up aborted volume=%s %s", vol.volume, reason)
			vol.content.markStale(reason)
			return
		}
		next, changes, err := contentCatchUpUSNRead(vol.volume, idx.JournalID, cur, buffer)
		if err != nil {
			serviceLog("content catch-up read error volume=%s from=%d err=%v", vol.volume, cur, err)
			s.contentCatchUpFailed(vol, "read error")
			return
		}
		if next <= cur {
			break
		}
		processed += len(changes)
		processedBytes += s.contentCatchUpBatchBytes(vol, changes)
		if processed > contentCatchUpMaxChanges || processedBytes > contentCatchUpMaxBytes {
			serviceLog("content catch-up cap hit volume=%s from=%d target=%d processed=%d bytes=%d", vol.volume, from, target, processed, processedBytes)
			s.contentCatchUpFailed(vol, "hit the record/byte cap")
			return
		}
		vol.contentCoord.observeChanges(changes)
		vol.contentCoord.promoteAll()
		cur = next
	}
	// Every record from the base checkpoint to the volume checkpoint has been
	// observed and enqueued; the fold gate may open. A capped/errored catch-up
	// returned above without reaching here, so it stays gated and degraded.
	vol.content.markCatchUpComplete()
	vol.contentBuildAttempts.Store(0)
}

// contentCatchUpFailed handles a capped or errored restart catch-up. It keeps
// the volume visibly incomplete/degraded, then schedules a full rebuild so the
// volume self-heals instead of staying gated forever with an ever-growing
// delta. contentBuildAttempts bounds retries, so a volume whose catch-up can
// never keep up gives up with a BuildError rather than spinning rebuilds; a
// successful catch-up resets the budget.
func (s *goSearchService) contentCatchUpFailed(vol *serviceVolumeIndex, reason string) {
	if vol == nil {
		return
	}
	vol.content.markCatchUpIncomplete()
	if vol.contentBuildAttempts.Add(1) > contentBuildMaxAttempts {
		vol.content.markBuildFailed(fmt.Sprintf("content catch-up %s; rebuild gave up after %d attempts", reason, contentBuildMaxAttempts))
		return
	}
	s.scheduleContentRebuild(vol)
}

// contentCatchUpGenerationCurrent reports whether the volume's live journal
// generation still matches the base catch-up is reading. A base swap with the
// same generation is fine (content is generation-independent); a journal reset
// is not, so the loop aborts. indexMu is taken before vol.mu, the documented
// order, and is the lock a base swap holds while it rewrites vol.journalID.
func (s *goSearchService) contentCatchUpGenerationCurrent(vol *serviceVolumeIndex, journalID uint64) bool {
	if vol == nil {
		return false
	}
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	vol.mu.Lock()
	defer vol.mu.Unlock()
	return vol.journalID == 0 || vol.journalID == journalID
}

// contentCatchUpUnknownFileBytes is the conservative per-file byte budget used
// when a changed file has no size in the base or the overlay: a create or
// move-in the base predates whose metadata refresh did not run. It is the
// extractor's per-document text cap, a true upper bound on what the delta can
// retain, so an all-create catch-up (base records absent) is still bounded
// instead of counting every file as 0 bytes. A var so tests can lower it.
var contentCatchUpUnknownFileBytes int64 = contentExtractMaxTextBytes

// contentCatchUpBatchBytes estimates the extracted-text volume a catch-up batch
// could enqueue from each changed file's size, so catch-up is bounded by bytes
// as well as records. A file with no base or overlay record (created/moved in
// while the service was stopped) is charged the conservative per-file bound.
func (s *goSearchService) contentCatchUpBatchBytes(vol *serviceVolumeIndex, changes []usnChange) int64 {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	if vol.index == nil {
		return 0
	}
	vol.mu.Lock()
	defer vol.mu.Unlock()
	var total int64
	for i := range changes {
		ch := &changes[i]
		if ch.FRN == 0 || ch.Reason&(usnReasonNeedsContentRefresh|usnReasonRenameNew) == 0 {
			continue
		}
		if !contentEligibleForExtraction(0, ch.Attr) {
			continue
		}
		if contentChangeExcluded(ch.Name) {
			continue
		}
		size, known := vol.contentFRNSizeLocked(ch.FRN)
		if !known {
			size = contentCatchUpUnknownFileBytes
		}
		// Only files an extractor could actually index count toward the budget:
		// a binary with no registered extractor would be skipped by extraction,
		// so charging its full size would spuriously hit the catch-up cap and
		// force a rebuild. An extensionless file is sniffable, so it still
		// counts. Cap the per-file charge at the text cap (an extractable file
		// contributes at most that much text).
		if contentPathExtension(ch.Name) != "" && !contentPathExtractableByRegistry(ch.Name) {
			continue
		}
		if size > contentExtractMaxTextBytes {
			size = contentExtractMaxTextBytes
		}
		total += size
	}
	return total
}

// contentFRNSizeLocked returns the best-known byte size for an FRN and whether
// any record (live overlay slot or base record) describes it. The overlay is
// consulted first: a file created or moved in while the service was stopped has
// no base record, but startup replay stat'd its size into the overlay, so it
// must not budget as 0. Caller holds indexMu.RLock and vol.mu.
func (vol *serviceVolumeIndex) contentFRNSizeLocked(frn uint64) (int64, bool) {
	if vol == nil || vol.index == nil || frn == 0 {
		return 0, false
	}
	if vol.overlay != nil {
		if slot, ok := vol.overlay.byFRN[frn]; ok && slot >= 0 && int(slot) < len(vol.overlay.records) {
			rec := vol.overlay.records[slot]
			if rec.Deleted {
				return 0, true
			}
			return rec.Size, true
		}
	}
	id, ok := vol.recordIDForFRN(frn)
	if !ok || id < 0 || id >= vol.index.compactRecordCount() {
		return 0, false
	}
	size := vol.index.compactRecord(id).Size
	if size < 0 {
		size = 0
	}
	return size, true
}

// contentDrainInterval is the quiet-window + drain cadence.
const contentDrainInterval = 2 * time.Second

func (s *goSearchService) contentDrainLoop(vol *serviceVolumeIndex) {
	if vol == nil || vol.contentCoord == nil {
		return
	}
	var scope *contentScopeResolved
	var scopeGen uint64
	// Capture the per-volume done channel once: a volume swap that installs a
	// different coordinator must not let this loop observe a new channel and
	// outlive its volume (M10/P6-4). The loop ends at either s.stop (shutdown)
	// or this volume's done (replaced/removed).
	done := vol.contentCoord.done
	ticker := time.NewTicker(contentDrainInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-done:
			return
		case <-ticker.C:
			if vol.contentCoord == nil {
				return
			}
			// Re-resolve only when the filename index generation changes; a
			// full repo-marker scan on every drain tick would be expensive.
			s.indexMu.RLock()
			hasIndex := vol.index != nil
			s.indexMu.RUnlock()
			if hasIndex && (scope == nil || scopeGen != vol.replayGen.Load()) {
				resolved, ok := s.resolvedContentScope(vol)
				if !ok {
					return
				}
				scope = &resolved
				scopeGen = vol.replayGen.Load()
			}
			// M10 residual: keep trying to own the sidecar lock on the drain
			// cadence, so a service that lost the startup race acquires it once
			// the CLI build releases it.
			s.retryContentVolumeLock(vol)
			vol.contentCoord.promoteAll()
			// A private path cache: the drain must not touch vol.pathCache,
			// which the search path trims under searchMu (a different lock).
			pathCache := make(map[int]string, 64)
			vol.contentCoord.processQueue(func(frn uint64) (string, bool) {
				path, ok := s.contentResolvePath(vol, frn, pathCache)
				if !ok {
					return "", false
				}
				if scope != nil {
					if _, _, allowed := scope.allows(path); !allowed {
						return "", false
					}
				}
				return path, true
			})
			// M1: after the tick's extractions, fold if the delta has grown
			// past its bound. A fold failure keeps the delta and backs off.
			s.maybeFoldContentDelta(vol)
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

// stopContentDrain retires vol's content drain loop, if any. It is the
// per-volume cancel: a volume that is replaced by a different one (or removed)
// stops its own loop without disturbing s.stop, while a plain in-place base swap
// keeps the coordinator (and therefore its drain). Idempotent and nil-safe.
func (vol *serviceVolumeIndex) stopContentDrain() {
	if vol == nil || vol.contentCoord == nil {
		return
	}
	vol.contentCoord.stopDrain()
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
