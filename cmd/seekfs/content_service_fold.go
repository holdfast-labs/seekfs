package main

// PF-5a (PB2/WP1c): fold the in-memory content delta into the persisted `.gsx`
// and bound the delta (M1).
//
// A fold re-encodes the whole corpus (base docs merged with the delta) through
// the streaming assembler and writes it atomically, exactly like the
// service-owned build. It holds no lock across assembly: it snapshots the delta
// plus the observed USN under the coordinator lock, reads the base (an
// immutable heap image) and extracts no files, then publishes under indexMu with
// the build's generation/journal re-validation. Entries mutated after the
// snapshot are left in the delta, so a delete or edit racing the fold is never
// lost.

import (
	"errors"
	"os"
	"sort"
	"time"
)

// contentDeltaFoldMaxDocs / contentDeltaFoldMaxBytes bound the live delta
// between folds (M1). Exceeding either schedules a fold at the end of the next
// drain pass. Vars so tests can lower them.
var (
	contentDeltaFoldMaxDocs  = 8192
	contentDeltaFoldMaxBytes = int64(64 << 20)
)

// contentFoldRetryBackoff paces a fold retry after a failure. A var so tests
// run without the delay.
var contentFoldRetryBackoff = 5 * time.Second

// contentFoldCappedBackoff paces a fold retry after the encoded sidecar was
// refused for exceeding contentGSXMaxBytes. Re-running the whole-corpus
// assembly + encode on the normal cadence can never succeed while the live
// content is over cap, so a capped volume retries on a long cadence instead;
// this is the recovery hook for a cap that was raised. A var so tests run
// without the delay.
var contentFoldCappedBackoff = 15 * time.Minute

// contentDeltaHardMaxDocs / contentDeltaHardMaxBytes are the absolute ceiling
// on the resident delta while the sidecar is capped and no fold can publish.
// contentDeltaFoldMaxDocs/MaxBytes trigger a fold far earlier; the hard bound
// only bites when that fold cannot commit, so a distinct document past the
// ceiling is deferred and the volume surfaced incomplete rather than the delta
// growing without bound. Existing entries are still updated, so acknowledged
// content is never dropped; a deferred change stays replayable from the
// persisted base checkpoint once the cap is raised and the volume rebuilt. Vars
// so tests can lower them.
var (
	contentDeltaHardMaxDocs  = 32768
	contentDeltaHardMaxBytes = int64(256 << 20)
)

// contentFoldSave is contentSaveFile for the fold, a var so tests can inject a
// save failure and prove the delta is kept.
var contentFoldSave = contentSaveFile

// contentFoldSnapshotHook runs after the delta snapshot is taken and before any
// base text is read. Tests use it to mutate the delta mid-fold (an
// observeChanges delete) and prove the change is not lost.
var contentFoldSnapshotHook = func(*serviceVolumeIndex) {}

// contentFoldAssembleHook runs after the fold's snapshot and base reader are
// resolved and immediately before the streaming assembly. Tests use it to prove
// an over-cap fold is not re-attempted on the normal cadence (and so the
// whole-corpus assembly/encode does not run every drain tick).
var contentFoldAssembleHook = func() {}

// maybeFoldContentDelta folds the delta when it has grown past the thresholds.
// It is called from the drain loop after processQueue, so extraction work from
// the same tick is already in the delta.
func (s *goSearchService) maybeFoldContentDelta(vol *serviceVolumeIndex) {
	if vol == nil || vol.contentCoord == nil {
		return
	}
	if !vol.contentCoord.foldDue(contentDeltaFoldMaxDocs, contentDeltaFoldMaxBytes, vol.content.refreshPendingNow()) {
		return
	}
	s.runContentFold(vol)
}

// runContentFold performs one delta fold for vol. On success the delta entries
// covered by the fold are removed and the new base is published; on failure the
// delta is kept verbatim so no persisted-and-acknowledged content is dropped.
func (s *goSearchService) runContentFold(vol *serviceVolumeIndex) {
	if vol == nil || vol.content == nil || vol.contentCoord == nil {
		return
	}
	// A fold needs a usable attached base. ready and degraded both have one —
	// degraded means the base is usable but incomplete, over cap, or mid
	// extractor refresh, and a fold is exactly how it recovers (this is also what
	// lets a delta parked at its hard ceiling replay once the fold can publish).
	// indexing/stale own the sidecar via a build/rebuild and must not be folded.
	switch vol.content.stateOf() {
	case contentStateReady, contentStateDegraded:
	default:
		return
	}
	// Serialize with the service-owned build: both publish the `.gsx`.
	if !vol.contentBuildBusy.CompareAndSwap(false, true) {
		return
	}
	defer func() {
		vol.contentBuildBusy.Store(false)
		// A journal reset (or base rebuild) may have called scheduleContentBuild
		// while this fold held the build slot, so its CAS failed and the rebuild
		// was dropped. Reschedule only for a state that actually needs a rebuild
		// (stale/unavailable): a degraded volume is usable and a fold is its
		// recovery, so re-attaching it here would clear the degradation signal
		// prematurely (and a rebuild would drop the usable base).
		if st := vol.content.stateOf(); st == contentStateStale || st == contentStateUnavailable {
			s.scheduleContentBuild(vol)
		}
	}()

	gen := vol.replayGen.Load()
	observedUSN, snap, ok := vol.contentCoord.foldSnapshot()
	if !ok {
		return
	}
	if len(snap) == 0 {
		return
	}
	reader, _ := vol.content.readerResolverView()
	if reader == nil || reader.idx == nil {
		return
	}
	base := reader.idx
	contentFoldSnapshotHook(vol)

	tmpDir, err := os.MkdirTemp("", "seekfs-content-fold-*")
	if err != nil {
		vol.contentCoord.foldFailed(contentFoldRetryBackoff)
		return
	}
	contentFoldAssembleHook()
	cidx, err := assembleContentIndexStream(newContentFoldSource(reader, snap).next, tmpDir)
	_ = os.RemoveAll(tmpDir)
	if err != nil {
		serviceLog("content fold assemble failed volume=%s err=%v", vol.volume, err)
		vol.contentCoord.foldFailed(contentFoldRetryBackoff)
		return
	}
	checkpoint := observedUSN
	if checkpoint < base.CheckpointUSN {
		checkpoint = base.CheckpointUSN
	}
	// Changes parked at the delta's hard ceiling are not in this fold's
	// snapshot; keep the persisted checkpoint below them so a restart replays
	// their USNs instead of skipping them. The in-process replay re-admits them
	// as the delta drains, so this is a redundant-but-safe re-extract on restart.
	if vol.contentCoord.hasDeferred() {
		checkpoint = base.CheckpointUSN
	}
	cidx.Origin = contentOriginUSN
	cidx.JournalID = base.JournalID
	cidx.CheckpointUSN = checkpoint
	// Carry the prior extraction policy/caps so health stays truthful; the
	// snapshot already filtered the corpus the same way the base was.
	cidx.Policy = base.Policy
	cidx.ScopeHash = base.ScopeHash

	if !s.contentBuildCurrent(vol, gen, base.JournalID) {
		// A rebuild/journal reset moved under the fold; keep the delta for the
		// next fold against the new generation rather than publish stale.
		vol.contentCoord.foldFailed(contentFoldRetryBackoff)
		return
	}
	gsx := contentIndexPathForDB(vol.dbPath)
	if gsx == "" {
		vol.contentCoord.foldFailed(contentFoldRetryBackoff)
		return
	}
	if err := contentFoldSave(gsx, cidx); err != nil {
		serviceLog("content fold save failed volume=%s err=%v", vol.volume, err)
		var sizeErr *contentGSXSizeError
		if errors.As(err, &sizeErr) {
			// The folded base would exceed the cap. Keep the previous base and
			// the delta (content stays correct/usable) and surface the size in
			// health instead of publishing a truncated sidecar.
			vol.content.markSidecarCapped(sizeErr.size, sizeErr.cap)
			// The same over-cap encode would fail every tick; retry on the long
			// capped cadence so the encoder is not run on the normal cadence.
			vol.contentCoord.foldFailed(contentFoldCappedBackoff)
			return
		}
		vol.contentCoord.foldFailed(contentFoldRetryBackoff)
		return
	}
	if !s.publishFoldedContent(vol, gen, base.JournalID) {
		serviceLog("content fold did not publish volume=%s (base changed)", vol.volume)
		vol.contentCoord.foldFailed(contentFoldRetryBackoff)
		return
	}
	vol.contentCoord.commitFold(snap)
	vol.contentCoord.foldSucceeded()
	serviceLog("content fold volume=%s docs=%d checkpoint=%d", vol.volume, len(cidx.Docs), checkpoint)
}

// publishFoldedContent attaches the freshly saved `.gsx` under indexMu. Unlike
// attachContentForVolume it does not early-return on a ready volume: the fold
// replaces a ready base, so it must reload. It re-validates the generation and
// journal under the same locks a base swap takes, so a rebuild/journal reset or
// a newer content base is never overwritten with this fold.
func (s *goSearchService) publishFoldedContent(vol *serviceVolumeIndex, gen uint64, journalID uint64) bool {
	if vol == nil || vol.content == nil {
		return false
	}
	if !s.contentBuildCurrent(vol, gen, journalID) {
		return false
	}
	idx, err := contentLoadFile(contentIndexPathForDB(vol.dbPath))
	if err != nil {
		return false
	}
	attached := false
	defer func() {
		if !attached {
			idx.Release()
		}
	}()
	if idx.Origin != contentOriginUSN || idx.JournalID != journalID || idx.CheckpointUSN == 0 {
		return false
	}
	reader, err := openContentReader(idx)
	if err != nil {
		return false
	}
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	if vol.state != "ready" || vol.index == nil || vol.index.Source != "usn" {
		return false
	}
	if vol.journalID != journalID {
		return false
	}
	frns, ids, ok := contentBaseFRNColumns(vol.index)
	if !ok {
		return false
	}
	if old := vol.content.setReady(idx, reader, buildContentResolver(idx.Docs, frns, ids)); old != nil {
		// Replace the previous base's mapping while holding indexMu for writing:
		// no query can be reading it (queries hold indexMu.RLock throughout), so
		// the unmap cannot race a read.
		//
		// The fold itself reads the base mapping WITHOUT indexMu (it captures the
		// reader and streams docText/docPath lock-free for the whole assembly).
		// That is safe because this Release is the only unmap that can occur while
		// a fold runs, and it happens only here, AFTER the fold's base reads are
		// complete. A concurrent markStale→setReady cannot reach setReady during a
		// fold: the content state is ready (so attachContentForVolume early-returns)
		// and a journal reset bumps vol.journalID, failing the check above. The
		// finalizer is also a non-issue: the fold's local reader keeps reader.idx
		// reachable for the whole assembly.
		old.Release()
	}
	attached = true
	return true
}

// contentFoldSource merges the base doc table with the delta snapshot in
// ascending (frn, path) order, one document at a time. Both inputs are already
// resident; the merge adds no corpus-sized buffer.
type contentFoldSource struct {
	reader *contentReader
	base   []contentDoc
	delta  []contentDeltaDoc
	i      int
	j      int
}

func newContentFoldSource(reader *contentReader, snap []contentDeltaDoc) *contentFoldSource {
	delta := append([]contentDeltaDoc(nil), snap...)
	sort.Slice(delta, func(i, j int) bool {
		if delta[i].FRN != delta[j].FRN {
			return delta[i].FRN < delta[j].FRN
		}
		return delta[i].Path < delta[j].Path
	})
	return &contentFoldSource{reader: reader, base: reader.idx.Docs, delta: delta}
}

func (m *contentFoldSource) next() (contentBuildDoc, bool, error) {
	for {
		haveBase := m.i < len(m.base)
		haveDelta := m.j < len(m.delta)
		var baseFRN, deltaFRN uint64
		if haveBase {
			baseFRN = m.base[m.i].FRN
		}
		if haveDelta {
			deltaFRN = m.delta[m.j].FRN
		}
		switch {
		case haveDelta && (!haveBase || deltaFRN < baseFRN):
			d := m.delta[m.j]
			m.j++
			if d.Deleted {
				continue
			}
			return contentBuildDoc{frn: d.FRN, path: d.Path, text: d.Text, class: d.ContentType, version: d.ExtractorVersion}, true, nil
		case haveBase && (!haveDelta || baseFRN < deltaFRN):
			doc := m.base[m.i]
			m.i++
			text := m.reader.docText(doc.DocID)
			if len(text) == 0 {
				continue
			}
			return contentBuildDoc{frn: doc.FRN, path: m.reader.docPath(doc.DocID), text: text, modUnix: doc.ModUnix, class: doc.ContentType, version: doc.ExtractorVersion}, true, nil
		case haveBase && haveDelta:
			// Same FRN: the delta replaces the base doc; a tombstone drops it.
			d := m.delta[m.j]
			m.j++
			m.i++
			if d.Deleted {
				continue
			}
			return contentBuildDoc{frn: d.FRN, path: d.Path, text: d.Text, class: d.ContentType, version: d.ExtractorVersion}, true, nil
		default:
			return contentBuildDoc{}, false, nil
		}
	}
}
