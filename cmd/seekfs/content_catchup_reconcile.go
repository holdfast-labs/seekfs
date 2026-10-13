package main

import (
	"os"
)

// Journal-wrap metadata reconcile: when the USN journal wraps under a
// finished content base — during its build, or while the service is stopped
// — the checkpoint catch-up cannot replay the lost range. On a busy volume
// with a default-sized journal this is routine, and the old code dropped the
// perfectly good index (stale + full rebuild) for it.
//
// Instead the reconcile diffs the live filename index against the content
// doc table by FRN, like a restart catch-up that reads records instead of
// the journal: changed and new FRNs enqueue into the drain, deleted ones
// tombstone, and the content checkpoint advances to the reconciled point.
// The filename index is authoritative here — the service keeps it current,
// and it does its own stale rebuild when its journal wraps — and FRN
// identity holds within one journal generation, so docs keep joining.
// Only a journal-ID change (a new journal, which is exactly when the
// filename side rebuilds and FRNs may recycle) forces a content rebuild.
//
// The attached base keeps serving throughout (state stays ready; health
// shows degraded/"reconciling" with scan progress) and a fold persists the
// reconciled delta. Sidecars predating the v6 file fingerprint cannot diff
// and rebuild instead.

// contentCatchUpWrapAttempts bounds metadata-reconcile retries when the base
// moves mid-scan. A var so tests run without delay.
var contentCatchUpWrapAttempts = 3

// contentCatchUpWrapCheck reports whether from misses a wrapped journal of
// the same generation: same journal ID, checkpoint below the first valid
// USN but not beyond the head. A new journal ID, a checkpoint past the head,
// or a query error is not a wrap (stale/rebuild, as before).
func contentCatchUpWrapCheck(volume string, journalID uint64, from int64) (usnJournalDataV0, bool) {
	journal, err := contentCatchUpJournal(volume)
	if err != nil {
		return usnJournalDataV0{}, false
	}
	if journal.UsnJournalID == 0 || journal.UsnJournalID != journalID {
		return journal, false
	}
	firstValid := journal.FirstUsn
	if journal.LowestValidUsn > firstValid {
		firstValid = journal.LowestValidUsn
	}
	if from < firstValid && from <= journal.NextUsn {
		return journal, true
	}
	return journal, false
}

// isSameJournalWrap reports the same condition against an already-queried
// journal, so the attach path does not query twice.
func isSameJournalWrap(journal usnJournalDataV0, journalID uint64, from int64) bool {
	if journal.UsnJournalID == 0 || journal.UsnJournalID != journalID {
		return false
	}
	firstValid := journal.FirstUsn
	if journal.LowestValidUsn > firstValid {
		firstValid = journal.LowestValidUsn
	}
	return from < firstValid && from <= journal.NextUsn
}

// contentCatchUpReconcileMetadata recovers a wrapped checkpoint by diffing
// the live filename index (base records plus the replay overlay, which holds
// creates/modifies/deletes the journal range no longer covers) against the
// attached doc table. Changed and new FRNs enqueue for extraction, deleted
// ones tombstone; the fold gate then opens at the reconciled volume
// checkpoint instead of the lost journal range.
func (s *goSearchService) contentCatchUpReconcileMetadata(vol *serviceVolumeIndex, idx *contentIndex) {
	if vol == nil || vol.content == nil || vol.contentCoord == nil || idx == nil {
		return
	}
	if s.contentStopped(vol) {
		return
	}
	if idx.Origin != contentOriginUSN {
		s.contentCatchUpFailed(vol, "metadata reconcile needs a USN-keyed base")
		return
	}
	// Sidecars predating the v6 file fingerprint cannot diff;
	// contentIndexDecode already refuses pre-v6 files, so an all-zero table
	// here means corruption, not age. Rebuild rather than guess.
	fingerprinted := false
	for i := range idx.Docs {
		if idx.Docs[i].FileSize != 0 || idx.Docs[i].FileModNano != 0 {
			fingerprinted = true
			break
		}
	}
	if len(idx.Docs) > 0 && !fingerprinted {
		s.contentCatchUpFailed(vol, "base predates file fingerprints")
		return
	}
	s.indexMu.RLock()
	count := 0
	if vol.index != nil {
		count = vol.index.compactRecordCount()
	}
	s.indexMu.RUnlock()
	vol.content.markReconciling(count)
	for attempt := 0; attempt < contentCatchUpWrapAttempts; attempt++ {
		if s.contentStopped(vol) {
			return
		}
		if s.reconcileMetadataPass(vol, idx) {
			return
		}
	}
	s.contentCatchUpFailed(vol, "base moved during metadata reconcile")
}

// reconcileMetadataPass runs one diff pass; true means done (converged or
// terminally failed), false means the base moved and the caller retries.
func (s *goSearchService) reconcileMetadataPass(vol *serviceVolumeIndex, idx *contentIndex) bool {
	if !s.beginContentTask(vol) {
		return true
	}
	defer endContentTask(vol)
	s.indexMu.RLock()
	if vol.index == nil || vol.index.Source != "usn" {
		s.indexMu.RUnlock()
		return true
	}
	base := vol.index
	gen := vol.replayGen.Load()
	journalID := vol.journalID
	s.indexMu.RUnlock()
	scope, ok := s.resolvedContentScope(vol)
	if !ok {
		s.contentCatchUpFailed(vol, "scope unresolvable during metadata reconcile")
		return true
	}
	// Selection scan over the base records (fast path, with progress), then
	// the overlay merge under the same read lock below.
	cfg := contentScopeScanConfig{
		scope:  &scope,
		idx:    base,
		isWalk: false,
		prog: func(scanned, total int) {
			vol.content.setReconcileProgress(scanned, total)
		},
		stopped: func() bool {
			return s.contentStopped(vol)
		},
		genChanged: func() bool {
			return vol.index != base || vol.replayGen.Load() != gen || vol.journalID != journalID
		},
	}
	lists, ok := scanScopeMembers(s, vol, cfg)
	if !ok {
		if s.contentStopped(vol) {
			return true
		}
		return false
	}
	live := make(map[uint64]contentFilePrint)
	for _, l := range lists {
		for _, m := range l {
			if contentScopeMemberIgnored(&scope, m.path) {
				continue
			}
			live[m.frn] = contentFilePrint{size: m.size, mod: m.mod}
		}
	}
	if len(live) == 0 && len(idx.Docs) > 0 {
		// An empty selection against a populated base is indistinguishable
		// from total deletion; tombstoning the whole base on a scope
		// misconfiguration would be wrong either way, so fail toward a
		// budgeted rebuild instead.
		s.contentCatchUpFailed(vol, "metadata reconcile selected nothing")
		return true
	}
	// Merge the replay overlay: it holds every create/modify/delete the
	// wrapped range covered, and wins over the base record. Deleted FRNs
	// leave the live set so their base docs tombstone below.
	s.indexMu.RLock()
	if vol.index != base || vol.replayGen.Load() != gen || vol.journalID != journalID {
		s.indexMu.RUnlock()
		return false
	}
	vol.mu.Lock()
	if vol.overlay != nil {
		latest := latestOverlaySlotsByFRN(vol.overlay.records)
		for frn, slot := range latest {
			if slot < 0 || int(slot) >= len(vol.overlay.records) {
				continue
			}
			rec := vol.overlay.records[slot]
			if frn == 0 || rec.FRN == 0 {
				continue
			}
			if rec.Deleted {
				delete(live, frn)
				continue
			}
			if rec.Mode&uint32(os.ModeDir) != 0 {
				delete(live, frn)
				continue
			}
			live[frn] = contentFilePrint{size: rec.Size, mod: rec.ModUnix}
		}
	}
	target := vol.checkpoint
	vol.mu.Unlock()
	s.indexMu.RUnlock()
	// Base docs outside the live set (deleted, or selected out) drop at the
	// next fold; live files without a matching doc enqueue. The drain
	// re-resolves every path and re-checks scope at extraction, so an entry
	// that moved again mid-pass converges on the next tick instead of
	// misindexing. This diff runs without service locks (pure maps plus the
	// delta/coordinator's own locks); the generation is re-validated before
	// anything publishes below.
	delta := vol.content.deltaView()
	basePrints := make(map[uint64]contentFilePrint, len(idx.Docs))
	for i := range idx.Docs {
		if idx.Docs[i].FRN != 0 {
			basePrints[idx.Docs[i].FRN] = contentFilePrint{size: idx.Docs[i].FileSize, mod: idx.Docs[i].FileModNano}
		}
	}
	var enqueue, drop []uint64
	for key, want := range live {
		if doc, ok := delta.docFor(key); ok && !doc.Deleted &&
			doc.FileSize == want.size && doc.FileModNano == want.mod {
			continue
		}
		if vol.content.walkDeclinedSkipped(key, want) {
			continue
		}
		got, inBase := basePrints[key]
		if !inBase {
			if want.size != 0 {
				enqueue = append(enqueue, key)
			}
			continue
		}
		if got != want {
			enqueue = append(enqueue, key)
		}
	}
	for key := range basePrints {
		if _, ok := live[key]; !ok {
			drop = append(drop, key)
		}
	}
	// A base swap or journal reset under the diff would join enqueues to
	// the wrong generation: re-validate before publishing anything.
	s.indexMu.RLock()
	moved := vol.index != base || vol.replayGen.Load() != gen || vol.journalID != journalID
	s.indexMu.RUnlock()
	if moved {
		return false
	}
	if len(enqueue) == 0 && len(drop) == 0 {
		// Nothing differed: converge immediately instead of parking the
		// volume in reconciling until a fold that may never come (an empty
		// delta never forces one). The checkpoint still advances below.
		vol.contentCoord.noteReconciledCheckpoint(uint64(target))
		vol.content.markCatchUpComplete()
		vol.contentBuildAttempts.Store(0)
		vol.content.clearReconcileActive()
		vol.content.markDrained()
		serviceLog("content metadata reconcile volume=%s converged checkpoint=%d",
			vol.volume, target)
		return true
	}
	for _, key := range drop {
		delta.delete(key)
	}
	if len(enqueue) > 0 {
		vol.contentCoord.enqueueFRNs(enqueue)
	}
	// The live index as merged above is complete as of the volume
	// checkpoint, and every difference is now queued or tombstoned: the
	// fold may durably claim it exactly like a fully replayed catch-up
	// range. The live stream keeps observing past this point, so later
	// changes queue normally and the fold gate (empty queue) holds until
	// they drain.
	vol.contentCoord.noteReconciledCheckpoint(uint64(target))
	vol.content.markCatchUpComplete()
	vol.contentBuildAttempts.Store(0)
	vol.content.setReconcileProgress(len(live), len(live))
	serviceLog("content metadata reconcile volume=%s enqueue=%d drop=%d checkpoint=%d",
		vol.volume, len(enqueue), len(drop), target)
	return true
}
