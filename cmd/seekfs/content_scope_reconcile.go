package main

// Incremental scope reconcile: when the resolved content scope moves under a
// built sidecar (a repo appears or vanishes, a .gitignore edit admits or
// drops files, roots added or removed, extension/budget edits), the service
// reconciles the selection difference instead of discarding the finished
// build. On an active developer machine the auto-scope fingerprint (repo
// list plus ignore digest) can move during a long build; without this, every
// such build is refused at attach and rebuilt from scratch, potentially
// forever.
//
// The reconcile diffs the CURRENT selection (one fast scope scan over the
// filename index) against the sidecar's doc table: newly selected keys
// enqueue into the existing delta/drain pipeline for extraction, dropped
// keys tombstone for the next fold. The fold persists the reconciled
// fingerprint in the header, converging the sidecar without re-extracting
// the corpus. Only diffs past contentScopeReconcileRebuildFrac of base docs
// rebuild from scratch, plus the cases that never reconcile (extractor or
// format incompatibility, journal reset, unresolvable scope).

import (
	"sort"
	"time"
)

// contentScopeReconcileRebuildFrac bounds scope-reconcile diffs: past this
// fraction of base docs, a clean rebuild is cheaper than incremental
// extraction. A var so tests can shrink it.
var contentScopeReconcileRebuildFrac = 0.5

// contentScopeDriftCheckPeriod throttles steady-state fingerprint drift
// checks in the drain loop. Config/env/ignore edits move the scope without
// moving the filename generation, so the per-generation re-resolve alone
// would miss them; a full resolve every tick would rescan the repo markers.
var contentScopeDriftCheckPeriod = 30 * time.Second

// contentScopeDiffResult is the selection delta between the current scope
// and an attached base: keys to extract and keys to drop at the next fold.
type contentScopeDiffResult struct {
	enqueue []uint64
	drop    []uint64
}

// contentScopeMemberIgnored reports whether path is an in-repo ignore file
// under scope. Ignore files carry the rules but never content docs (no text
// extractor claims them; the drain declines them), so reconcile selection
// skips them: enqueueing them would burn threshold budget on keys that can
// never join the base, and a repo made only of ignore files would read as a
// total wipe.
func contentScopeMemberIgnored(scope *contentScopeResolved, path string) bool {
	if scope == nil || scope.Ignore == nil || !contentIsIgnoreFile(path) {
		return false
	}
	_, inRepo := scope.Ignore.repoFor(contentNormPath(path))
	return inRepo
}

// scopeSelectionKeys runs one fast selection scan and returns the selected
// key fingerprints. ok is false when the base moved mid-scan (caller
// reschedules) or the scan stopped.
func (s *goSearchService) scopeSelectionKeys(vol *serviceVolumeIndex, scope *contentScopeResolved, idx *Index, gen uint64, isWalk bool) (map[uint64]contentFilePrint, bool) {
	cfg := contentScopeScanConfig{
		scope:  scope,
		idx:    idx,
		isWalk: isWalk,
		prog:   nil,
		stopped: func() bool {
			return s.contentStopped(vol)
		},
		genChanged: func() bool {
			return vol.index != idx || vol.replayGen.Load() != gen
		},
	}
	lists, ok := scanScopeMembers(s, vol, cfg)
	if !ok {
		return nil, false
	}
	out := make(map[uint64]contentFilePrint)
	for _, l := range lists {
		for _, m := range l {
			if contentScopeMemberIgnored(scope, m.path) {
				continue
			}
			out[m.frn] = contentFilePrint{size: m.size, mod: m.mod}
		}
	}
	return out, true
}

// scopeDiffSelection diffs the current selection against a base doc table
// plus the resident delta: selected keys with neither a doc nor a live
// delta entry enqueue for extraction; base keys outside the selection drop
// at the next fold. Keys remembered by the unindexable skip-set at the same
// fingerprint are not re-enqueued. Keys are FRNs on USN volumes, stable path
// hashes on walk volumes; the machinery is key-agnostic.
func scopeDiffSelection(selected map[uint64]contentFilePrint, base []contentDoc, delta *contentDelta, declined func(uint64, contentFilePrint) bool) contentScopeDiffResult {
	var out contentScopeDiffResult
	baseKeys := make(map[uint64]struct{}, len(base))
	for i := range base {
		if base[i].FRN != 0 {
			baseKeys[base[i].FRN] = struct{}{}
		}
	}
	for key, print := range selected {
		if _, ok := baseKeys[key]; ok {
			continue
		}
		if delta != nil {
			if doc, ok := delta.docFor(key); ok && !doc.Deleted {
				continue
			}
		}
		if declined != nil && declined(key, print) {
			continue
		}
		out.enqueue = append(out.enqueue, key)
	}
	for key := range baseKeys {
		if _, ok := selected[key]; !ok {
			out.drop = append(out.drop, key)
		}
	}
	sort.Slice(out.enqueue, func(i, j int) bool { return out.enqueue[i] < out.enqueue[j] })
	sort.Slice(out.drop, func(i, j int) bool { return out.drop[i] < out.drop[j] })
	return out
}

// scopeDiffOverThreshold reports whether reconciling diff against a base of
// baseDocs documents should rebuild from scratch instead.
func scopeDiffOverThreshold(diff contentScopeDiffResult, baseDocs int) bool {
	if baseDocs <= 0 {
		return len(diff.enqueue) > 0 || len(diff.drop) > 0
	}
	return float64(len(diff.enqueue)+len(diff.drop)) > contentScopeReconcileRebuildFrac*float64(baseDocs)
}

// applyScopeDiff tombstones dropped keys and enqueues new ones, then records
// the reconciled fingerprint and pending count for health and the next fold.
func (s *goSearchService) applyScopeDiff(vol *serviceVolumeIndex, diff contentScopeDiffResult, fp [32]byte) {
	if vol == nil || vol.content == nil {
		return
	}
	if delta := vol.content.deltaView(); delta != nil {
		for _, key := range diff.drop {
			delta.delete(key)
		}
	}
	if len(diff.enqueue) > 0 && vol.contentCoord != nil {
		vol.contentCoord.enqueueFRNs(diff.enqueue)
	}
	vol.content.markScopeReconcile(fp, len(diff.enqueue)+len(diff.drop))
	if len(diff.enqueue)+len(diff.drop) > 0 {
		serviceLog("content scope reconcile volume=%s enqueue=%d drop=%d", vol.volume, len(diff.enqueue), len(diff.drop))
	}
}

// attachScopeMismatchedBase reconciles a sidecar built under a different
// scope at attach time: diff, threshold gate, apply. Unlike the steady-state
// path below it does not require the volume to be ready (attach publishes
// afterwards). It returns false when the caller must rebuild from scratch.
func (s *goSearchService) attachScopeMismatchedBase(vol *serviceVolumeIndex, idx *contentIndex, scope contentScopeResolved, isWalk bool) bool {
	return s.scopeReconcile(vol, idx, scope, isWalk, false)
}

// contentScopeReconcileVolume reconciles an attached base to scope: diff,
// threshold gate, apply. It returns false when the caller must rebuild from
// scratch (scope unresolvable, moved base, or over-threshold diff). idx is
// the attached base; scope the current resolution; isWalk selects the key
// space. The caller holds no locks; the scan takes its own.
func (s *goSearchService) contentScopeReconcileVolume(vol *serviceVolumeIndex, idx *contentIndex, scope contentScopeResolved, isWalk bool) bool {
	return s.scopeReconcile(vol, idx, scope, isWalk, true)
}

// scopeReconcile is the shared attach/drift reconcile. needReady requires
// the volume to be serving (steady state); attach reconciles first and
// publishes after.
func (s *goSearchService) scopeReconcile(vol *serviceVolumeIndex, idx *contentIndex, scope contentScopeResolved, isWalk, needReady bool) bool {
	if s == nil || vol == nil || vol.content == nil || idx == nil {
		return false
	}
	if !s.beginContentTask(vol) {
		return false
	}
	defer endContentTask(vol)
	s.indexMu.RLock()
	if vol.index == nil || (needReady && vol.state != "ready") {
		s.indexMu.RUnlock()
		return false
	}
	base := vol.index
	gen := vol.replayGen.Load()
	s.indexMu.RUnlock()
	fp := scope.fingerprint()
	selected, ok := s.scopeSelectionKeys(vol, &scope, base, gen, isWalk)
	if !ok {
		return false
	}
	diff := scopeDiffSelection(selected, idx.Docs, vol.content.deltaView(), vol.content.walkDeclinedSkipped)
	if scopeDiffOverThreshold(diff, len(idx.Docs)) {
		serviceLog("content scope diff volume=%s enqueue=%d drop=%d base=%d exceeds rebuild fraction; rebuilding",
			vol.volume, len(diff.enqueue), len(diff.drop), len(idx.Docs))
		return false
	}
	s.applyScopeDiff(vol, diff, fp)
	return true
}

// contentScopeDriftCheckedAt returns the last steady-state drift check time.
func (vol *serviceVolumeIndex) contentScopeDriftCheckedAt() time.Time {
	if vol == nil || vol.content == nil {
		return time.Time{}
	}
	vol.content.mu.RLock()
	defer vol.content.mu.RUnlock()
	return vol.content.scopeDriftCheckedAt
}

// contentScopeDriftNoteChecked records a drift check.
func (vol *serviceVolumeIndex) contentScopeDriftNoteChecked() {
	if vol == nil || vol.content == nil {
		return
	}
	vol.content.mu.Lock()
	defer vol.content.mu.Unlock()
	vol.content.scopeDriftCheckedAt = time.Now()
}

// contentScopeDriftCheck compares the current scope fingerprint against the
// selection the attached base represents (reconciled-to, else stamped) and
// reconciles on drift. It is throttled and skipped without an attached base;
// the build path owns baseless volumes. force bypasses the throttle (an
// ignore-file edit just invalidated the rules). Returns true when a
// reconcile ran.
func (s *goSearchService) contentScopeDriftCheck(vol *serviceVolumeIndex, force bool) bool {
	if s == nil || vol == nil || vol.content == nil {
		return false
	}
	if !contentSearchEnabled() {
		return false
	}
	if !force && time.Since(vol.contentScopeDriftCheckedAt()) < contentScopeDriftCheckPeriod {
		return false
	}
	vol.contentScopeDriftNoteChecked()
	s.indexMu.RLock()
	if vol.index == nil || (vol.index.Source != "usn" && vol.index.Source != "walk") {
		s.indexMu.RUnlock()
		return false
	}
	reader, resolver := vol.content.readerResolverView()
	if reader == nil || reader.idx == nil || resolver == nil {
		s.indexMu.RUnlock()
		return false
	}
	base := reader.idx
	if base.Origin != contentOriginUSN && base.Origin != contentOriginWalk {
		s.indexMu.RUnlock()
		return false
	}
	effective := base.ScopeHash
	if fp, ok := vol.content.scopeReconciledFP(); ok {
		effective = fp
	}
	s.indexMu.RUnlock()
	scope, ok := s.resolvedContentScope(vol)
	if !ok {
		return false
	}
	if scope.fingerprint() == effective {
		return false
	}
	if !s.contentScopeReconcileVolume(vol, base, scope, base.Origin == contentOriginWalk) {
		// Over-threshold or moved base: the usable base stays attached
		// while a clean rebuild replaces it (never drop good content for
		// a scope change alone).
		vol.content.markScopeReconcile(scope.fingerprint(), 0)
		s.scheduleContentRebuild(vol)
		return false
	}
	return true
}
