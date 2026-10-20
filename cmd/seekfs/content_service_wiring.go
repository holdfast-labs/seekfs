package main

// P2.5 service wiring: attach a loaded `.gsx` to a service volume, keep its
// resolver in step with base swaps, and drain changed files on a timer.
//
// Everything here is inert unless the optional content feature is enabled.

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
	s.attachContentForVolumeReplace(vol, false)
}

// attachContentForVolumeReplace is attachContentForVolume with an explicit
// replace decision. A completed rebuild passes replace=true so its fresh
// sidecar is published even while the previous base is still attached and
// serving with the stale flag. Every other caller passes false and keeps the
// early return that avoids re-attaching a base already in use.
func (s *goSearchService) attachContentForVolumeReplace(vol *serviceVolumeIndex, replace bool) {
	if vol == nil {
		return
	}
	if !s.beginContentTask(vol) {
		return
	}
	defer endContentTask(vol)
	if !contentSearchEnabled() || vol.content == nil {
		return
	}
	if contentScopeForVolume(s.contentConfig(), vol.volume).disabled() {
		return
	}
	// A volume joins content docs on its origin's key: the NTFS FRN for USN
	// volumes, the stable path hash for walk volumes (whose record FRNs are
	// positional and renumber on every rescan). Anything else has no join
	// column and stays unavailable.
	s.indexMu.RLock()
	var wantOrigin uint32
	switch {
	case vol.index != nil && vol.index.Source == "usn":
		wantOrigin = contentOriginUSN
	case vol.index != nil && vol.index.Source == "walk":
		wantOrigin = contentOriginWalk
	}
	s.indexMu.RUnlock()
	if wantOrigin == contentOriginUnknown {
		return
	}
	isWalk := wantOrigin == contentOriginWalk
	s.acquireContentVolumeLock(vol)
	// A volume that already has a usable base attached is done, unless this is
	// a rebuild publishing its replacement. This replaced the old "drain
	// enabled" guard: after a journal reset invalidates the base the drain
	// stays enabled, so keying on it would skip the re-attach of a freshly
	// rebuilt sidecar (PF-3/WP1e).
	if !replace && vol.content.readerView() != nil {
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
	if idx.Origin != wantOrigin {
		// A sidecar from the other key space does not join to this volume's
		// records; attaching it would report `ready` while every document
		// resolves to nothing.
		serviceLog("content index for volume %s is not keyed for this volume (origin=%d, want=%d); leaving unavailable", vol.volume, idx.Origin, wantOrigin)
		return
	}
	resolvedScope, ok := s.resolvedContentScope(vol)
	if !ok {
		reason := "content scope unresolvable"
		serviceLog("content index for volume %s %s; leaving stale", vol.volume, reason)
		vol.content.markStale(reason)
		return
	}
	if idx.ScopeHash == ([32]byte{}) || idx.ScopeHash != resolvedScope.fingerprint() {
		// The scope moved under a finished sidecar (a repo appeared or
		// vanished, an ignore rule changed, roots/exts/budget edited) —
		// or the sidecar predates scope identity. Reconcile the selection
		// difference incrementally instead of discarding the build; only
		// a wholesale change rebuilds from scratch.
		if !s.attachScopeMismatchedBase(vol, idx, resolvedScope, isWalk) {
			reason := "content scope changed beyond reconcile; rebuilding"
			serviceLog("content index for volume %s %s", vol.volume, reason)
			vol.content.markStale(reason)
			return
		}
	} else {
		// A matching sidecar converges any earlier reconcile tracking.
		vol.content.mu.Lock()
		vol.content.clearScopeReconcileLocked()
		vol.content.mu.Unlock()
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
			if isWalk {
				// contentWalkKeys needs indexMu; attach holds no lock here
				// (unlike the drain/reconcile paths, which already hold it).
				s.indexMu.RLock()
				defer s.indexMu.RUnlock()
				return s.contentResolveWalkPath(vol, frn, pathCache)
			}
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
	if !isWalk && (idx.JournalID == 0 || idx.CheckpointUSN == 0) {
		reason := "content base has no USN watermark"
		serviceLog("content index for volume %s %s; leaving stale", vol.volume, reason)
		vol.content.markStale(reason)
		return
	}
	// A walk-origin base carries no journal: JournalID must be 0 (anything
	// else is a foreign sidecar) and CheckpointUSN holds the wall-clock
	// reconcile watermark the service-owned build stamped. A zero watermark
	// is a hand-built or offline sidecar the service did not build; rebuild
	// it under the service's key convention and scope instead of serving it.
	if isWalk && (idx.JournalID != 0 || idx.CheckpointUSN == 0) {
		reason := "content base has no walk watermark"
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
	journalID := vol.journalID
	var frns []uint64
	var ids []uint32
	if isWalk {
		// Walk records carry positional FRNs, so the persisted FRN column
		// cannot join: build the stable path-hash join column from the live
		// records instead.
		var ok bool
		frns, ids, ok = s.contentWalkKeys(vol)
		if !ok {
			s.indexMu.Unlock()
			serviceLog("content index for volume %s has no walk records; leaving unavailable", vol.volume)
			return
		}
	} else {
		var ok bool
		frns, ids, ok = contentBaseFRNColumns(vol.index)
		if !ok {
			s.indexMu.Unlock()
			serviceLog("content index for volume %s has no FRN column; leaving unavailable", vol.volume)
			return
		}
	}
	// WP0/PB3: a base built against a different USN journal generation can
	// never be joined to this volume's current FRNs. Refuse it (stale) rather
	// than advertise ready with docs that resolve to nothing; PF-3 schedules
	// the rebuild. Walk volumes have no journal generation; their join is
	// validated by the zero-join guard below instead.
	if !isWalk && idx.JournalID != journalID {
		s.indexMu.Unlock()
		reason := fmt.Sprintf("content base journal %d != volume journal %d", idx.JournalID, journalID)
		serviceLog("content index journal mismatch for volume %s: %s", vol.volume, reason)
		vol.content.markStale(reason)
		return
	}
	resolver := buildContentResolverSeq(idx.Docs, frns, ids, vol.index.seqColumn())
	// A content index whose docs join none of the base's FRNs would serve every
	// query as an empty result set while advertising `ready` (the candidate path
	// drops unmapped docs and still returns ok). Refuse to publish it: a
	// zero-join base is a fault (built against a different base/journal), not an
	// empty corpus.
	if len(idx.Docs) > 0 && resolver.liveDocCount() == 0 {
		s.indexMu.Unlock()
		reason := "content docs do not join the base key column"
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
		if startDrain && s.beginContentTask(vol) {
			go func() { defer endContentTask(vol); s.contentDrainLoop(vol) }()
		}
		if isWalk {
			// No journal to replay: restart freshness is a metadata reconcile
			// of the persisted filename index against the base, run inline
			// (it opens no files) before the fold gate opens. Disk changes
			// newer than the persisted index arrive via the watcher's next
			// filename rebuild, which reconciles in turn.
			s.contentReconcileWalkVolume(vol, true)
			vol.content.markCatchUpComplete()
			vol.contentBuildAttempts.Store(0)
			// Same background integrity pass as the USN path below: the
			// small sections (including the doc table) were verified at
			// load; the large text/posting sections hash off-path.
			if s.beginContentTask(vol) {
				contentVerifyAsync(func() { defer endContentTask(vol); s.verifyContentAfterAttach(vol, idx) })
			}
			serviceLog("content index loaded for volume %s docs=%d (walk)", vol.volume, len(idx.Docs))
			return
		}
		// Catch up OFF the startup path: a far-behind base must not block
		// startup or grow an unbounded delta. Catch-up runs after the drain is
		// enabled so the FRNs it enqueues are actually processed; files changed
		// while the service was stopped become findable in content, matching
		// filename search.
		if s.beginContentTask(vol) {
			contentCatchUpAsync(func() { defer endContentTask(vol); s.contentCatchUpAfterAttach(vol, idx) })
		}
	}
	// Verify the large sections in the background: the small ones were checked
	// at load, and hashing gigabytes synchronously would stall startup. A
	// mismatch drops the base and schedules a rebuild, so corrupt bytes are
	// never served.
	if s.beginContentTask(vol) {
		contentVerifyAsync(func() { defer endContentTask(vol); s.verifyContentAfterAttach(vol, idx) })
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
	if s.contentStopped(vol) {
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
		// A wrapped journal of the same generation keeps FRN identity: the
		// filename side rebuilds or replays around it, and content follows
		// with a metadata reconcile instead of dropping a good base. Only
		// a new journal generation (or a checkpoint past the head) rebuilds.
		if isSameJournalWrap(journal, idx.JournalID, from) {
			serviceLog("content catch-up journal wrapped volume=%s from=%d; reconciling metadata", vol.volume, from)
			s.contentCatchUpReconcileMetadata(vol, idx)
			return
		}
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
		if s.contentStopped(vol) {
			return
		}
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
			// The journal may have wrapped under a long catch-up: same
			// generation still joins, so reconcile instead of rebuilding.
			if _, wrapped := contentCatchUpWrapCheck(vol.volume, idx.JournalID, cur); wrapped {
				serviceLog("content catch-up journal wrapped mid-read volume=%s from=%d; reconciling metadata", vol.volume, cur)
				s.contentCatchUpReconcileMetadata(vol, idx)
				return
			}
			serviceLog("content catch-up read error volume=%s from=%d err=%v", vol.volume, cur, err)
			s.contentCatchUpFailed(vol, "read error")
			return
		}
		if next <= cur {
			s.contentCatchUpFailed(vol, "journal read made no progress")
			return
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
	serviceLog("content catch-up failed volume=%s: %s; scheduling a rebuild", vol.volume, reason)
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
		case <-contentActivationDone(vol):
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
			// Steady-state scope drift (config/env/ignore edits move the
			// selection without moving the filename generation): reconcile
			// instead of rebuilding. Throttled to the drift period; the
			// generation-gated re-resolve above covers rebuild-driven
			// drift, and ignore edits force a check below.
			s.contentScopeDriftCheck(vol, false)
			// Walk volumes have no USN stream: reconcile the filename index
			// against the content base first (a no-op unless the filename
			// generation moved or the safety-net period elapsed), so changed
			// files are extracted on this tick.
			s.indexMu.RLock()
			isWalkDrain := vol.contentWalkOriginAttached()
			s.indexMu.RUnlock()
			if isWalkDrain {
				s.contentReconcileWalkVolume(vol, false)
			}
			// Feed deterministic declines into the unindexable skip-set, so
			// files that extract to nothing are not retried every pass. The
			// coordinator is per-volume, so no other volume's drain
			// can observe this.
			vol.contentCoord.declined = func(key uint64, size, modNano int64) {
				vol.content.walkDeclinedNote(key, contentFilePrint{size: size, mod: modNano})
			}
			vol.contentCoord.promoteAll()
			// A private path cache: the drain must not touch vol.pathCache,
			// which the search path trims under searchMu (a different lock).
			pathCache := make(map[int]string, 64)
			ignoreChanged := false
			// Hand the tick's scope to extraction for the repo-aware caps.
			vol.contentCoord.scopeHint = scope
			// Stamp the record's current sequence on each extracted delta doc so
			// a reused record's doc never joins its previous occupant's row.
			vol.contentCoord.seqHint = func(frn uint64) uint16 {
				s.indexMu.RLock()
				defer s.indexMu.RUnlock()
				vol.mu.Lock()
				defer vol.mu.Unlock()
				return vol.knownFRNSeq(frn)
			}
			vol.contentCoord.processQueue(func(frn uint64) (string, bool) {
				var path string
				var ok bool
				if isWalkDrain {
					// The walk join column needs indexMu; the tick holds no
					// lock here.
					s.indexMu.RLock()
					path, ok = s.contentResolveWalkPath(vol, frn, pathCache)
					s.indexMu.RUnlock()
				} else {
					path, ok = s.contentResolvePath(vol, frn, pathCache)
				}
				if !ok {
					return "", false
				}
				if scope != nil {
					if scope.Ignore != nil && contentIsIgnoreFile(path) {
						if _, inRepo := scope.Ignore.repoFor(contentNormPath(path)); inRepo {
							ignoreChanged = true
						}
					}
					if _, _, allowed := scope.allows(path); !allowed {
						return "", false
					}
				}
				return path, true
			})
			if scope != nil && scope.Ignore != nil && scope.Ignore.globalChanged() {
				// The global excludes file lives outside the volume, so the
				// USN stream never reports its edits; poll its stat instead.
				// Same rebuild as an edited in-volume ignore file.
				ignoreChanged = true
			}
			if ignoreChanged {
				// Files the edit newly ignores or un-ignores are not in the
				// change stream; reconcile the selection difference under
				// the new rules instead of rebuilding the sidecar. The
				// fold persists the reconciled fingerprint.
				serviceLog("content ignore rules changed volume=%s; reconciling", vol.volume)
				scope.Ignore.invalidate()
				scope = nil
				s.contentScopeDriftCheck(vol, true)
			}
			vol.contentCoord.declined = nil
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
// because the resident FRN index was not built. For walk-origin volumes the
// key is the stable path hash, resolved through the cached walk join column
// (walk FRNs are positional and renumber on every rescan). Call with indexMu
// held (read or write); the walk set is immutable and pointer-keyed, so a
// concurrent base swap can only make it miss, never misjoin.
func (vol *serviceVolumeIndex) recordIDForFRN(frn uint64) (int, bool) {
	if vol == nil || vol.index == nil || frn == 0 {
		return 0, false
	}
	if id, ok := vol.idForFRN(frn); ok {
		return id, true
	}
	if set := vol.walkKeys; set != nil && set.idx == vol.index {
		if i, found := contentLookupFRNColumn(set.keys, frn); found && i < len(set.ids) {
			return int(set.ids[i]), true
		}
		return 0, false
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
// FRN, so a persist/compaction swap must never re-extract. Walk record IDs
// renumber on every rescan, so a walk rebind also rebuilds the stable-key
// join column first — rebinding positional FRNs would resolve results to the
// wrong records.
func rebindContentAfterBaseSwap(vol *serviceVolumeIndex) {
	if vol == nil || vol.content == nil {
		return
	}
	vol.content.mu.RLock()
	origin := uint32(contentOriginUnknown)
	if vol.content.idx != nil {
		origin = vol.content.idx.Origin
	}
	vol.content.mu.RUnlock()
	if origin == contentOriginWalk {
		keys, ids := buildContentWalkKeys(vol.index)
		vol.walkKeys = &walkKeySet{idx: vol.index, keys: keys, ids: ids}
		vol.content.rebuildResolver(keys, ids, nil)
		// A base whose docs join nothing (e.g. the roots changed out from
		// under the sidecar) must not serve empty as ready. The next
		// ensureContentBuild (walk rebuild path, runtime swap) re-attaches
		// or rebuilds it.
		vol.content.mu.RLock()
		joined := vol.content.resolver.liveDocCount()
		docs := len(vol.content.idx.Docs)
		vol.content.mu.RUnlock()
		if docs > 0 && joined == 0 {
			vol.content.markStale("content docs do not join the rebuilt walk index")
		}
		return
	}
	frns, ids, ok := contentBaseFRNColumns(vol.index)
	if !ok {
		return
	}
	vol.content.rebuildResolver(frns, ids, vol.index.seqColumn())
}
