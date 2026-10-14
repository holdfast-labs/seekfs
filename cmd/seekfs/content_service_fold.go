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

// contentFoldSave streams one assembled fold to its sidecar path, a var so
// tests can inject a save failure and prove the delta is kept.
var contentFoldSave = contentSaveAssembled

// contentFoldSaveManifest publishes a sharded fold's manifest; a var for the
// same reason as contentFoldSave.
var contentFoldSaveManifest = contentSaveManifest

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
	if vol == nil {
		return
	}
	if !s.beginContentTask(vol) {
		return
	}
	defer endContentTask(vol)
	if vol.content == nil || vol.contentCoord == nil {
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
	reader, resolver := vol.content.readerResolverView()
	if reader == nil || reader.idx == nil {
		return
	}
	base := reader.idx
	contentFoldSnapshotHook(vol)

	// Tier-2 filter maintenance (§6y): delta docs for filter-covered keys
	// refresh the filter instead of joining tier 1 — they were dropped from
	// tier 1 by budget, and promoting them would grow tier 1 past its
	// allowance file by file. Partition the snapshot before assembly:
	// foldSnap feeds the existing tier-1 paths below, filterSnap recomputes
	// filter records from delta text once tier 1 publishes (or alone when
	// tier 1 has nothing to fold). Keys in the base stay tier 1 even if the
	// filter also names them (tier 1 wins); unknown keys join tier 1 exactly
	// as before, so a nil filter behaves byte-identically to today.
	fltOld := vol.content.filterView()
	var foldSnap, filterSnap []contentDeltaDoc
	if fltOld != nil && len(fltOld.Records) > 0 {
		for _, d := range snap {
			if _, inBase := contentLookupFRN(base.Docs, d.FRN); inBase {
				foldSnap = append(foldSnap, d)
				continue
			}
			if _, inFilter := fltOld.lookup(d.FRN); inFilter {
				filterSnap = append(filterSnap, d)
				continue
			}
			foldSnap = append(foldSnap, d)
		}
	} else {
		fltOld = nil
		foldSnap = snap
	}
	if len(foldSnap) == 0 {
		// Filter-only fold: tier 1 is unchanged, so no base is rewritten;
		// the refresh persists against the current base. The delta still
		// drains (commitFold) and the retry backoff clears, so a steady
		// stream of filter-only changes folds cheaply instead of rewriting
		// the base every drain tick. The persisted base checkpoint lags
		// until the next tier-1 fold; a restart replays the committed range
		// idempotently (fingerprints converge on the refreshed filter).
		if fltOld != nil && len(filterSnap) > 0 {
			s.publishFilterRefresh(vol, gen, base, fltOld, filterSnap)
		}
		vol.contentCoord.commitFold(snap)
		vol.contentCoord.foldSucceeded()
		return
	}

	tmpDir, err := contentMkdirWork(contentIndexPathForDB(vol.dbPath))
	if err != nil {
		vol.contentCoord.foldFailed(contentFoldRetryBackoff)
		return
	}
	defer os.RemoveAll(tmpDir)
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
	// The folded base keeps the prior extraction policy/caps so health stays
	// truthful; the snapshot already filtered the corpus the same way the base
	// was. A walk fold keeps the walk origin and restamps the wall-clock
	// watermark (there is no journal checkpoint to advance); a USN fold
	// advances the observed-USN checkpoint as before.
	header := &contentIndex{Origin: base.Origin, JournalID: base.JournalID, CheckpointUSN: checkpoint, Policy: base.Policy, ScopeHash: base.ScopeHash}
	if base.Origin == contentOriginWalk {
		if now := uint64(time.Now().UnixNano()); now > header.CheckpointUSN {
			header.CheckpointUSN = now
		}
	}
	// A scope reconcile converges here: persist the reconciled fingerprint
	// instead of the stale build-time one, so the sidecar restamps to the
	// selection it now represents. Without a reconcile the header keeps the
	// base hash, as before.
	if fp, ok := vol.content.scopeReconciledFP(); ok {
		header.ScopeHash = fp
	}
	gsx := contentIndexPathForDB(vol.dbPath)
	if gsx == "" {
		vol.contentCoord.foldFailed(contentFoldRetryBackoff)
		return
	}
	stopped := func() bool { return s.contentStopped(vol) }
	contentFoldAssembleHook()
	// foldShard assembles one fold source into a spooled shard plus the header
	// index the streaming save needs. The spooled section files, not a heap
	// index, are what gets saved; publish reloads the sidecar through the
	// mmap loader.
	foldShard := func(source contentBuildSource) (*contentStreamedBuild, *contentIndex, error) {
		guarded := func() (contentBuildDoc, bool, error) {
			if stopped() {
				return contentBuildDoc{}, false, errContentBuildAborted
			}
			return source()
		}
		built, err := assembleContentIndexStreamed(guarded, tmpDir)
		if err != nil {
			return nil, nil, err
		}
		if err := contentCheckDiskReserve(tmpDir); err != nil {
			// Surface it: a fold that cannot write keeps the delta growing.
			vol.content.setFoldError(err.Error())
			return nil, nil, err
		}
		shard := contentShardHeader(header, built)
		shard.Sections = map[uint32][]byte{contentSectionPaths: encodeContentPaths(built.paths)}
		return built, shard, nil
	}
	var rewritten int
	var saveErr error
	if base.shards == nil {
		source := newContentFoldSource(reader, foldSnap)
		built, shard, err := foldShard(source.next)
		if err != nil {
			serviceLog("content fold assemble failed volume=%s err=%v", vol.volume, err)
			vol.contentCoord.foldFailed(contentFoldRetryBackoff)
			return
		}
		if !s.contentBuildCurrent(vol, gen, base.JournalID) {
			// A rebuild/journal reset moved under the fold; keep the delta for
			// the next fold against the new generation rather than publish
			// stale.
			vol.contentCoord.foldFailed(contentFoldRetryBackoff)
			return
		}
		rewritten = len(built.docs)
		if built.textSize > uint64(2*contentShardTargetBytes) {
			// Grown well past one shard through folds: publish the single
			// file, reload it through the mmap loader, and split it into
			// shards rather than keep rewriting one ever-larger file. The
			// published single file is complete and correct, so a reshard
			// failure still publishes it instead of failing the fold.
			if saveErr = contentFoldSave(gsx, shard, contentBuildFiles(built)); saveErr == nil {
				var shards int
				if reloaded, rerr := contentLoadFile(gsx); rerr != nil {
					saveErr = rerr
					contentSweepShards(gsx)
				} else if shards, saveErr = contentReshardAndSave(gsx, reloaded, header, tmpDir); saveErr == nil {
					serviceLog("content fold resharded volume=%s shards=%d", vol.volume, shards)
					reloaded.Release()
				} else {
					reloaded.Release()
					contentSweepShards(gsx)
				}
			}
		} else {
			saveErr = contentFoldSave(gsx, shard, contentBuildFiles(built))
		}
	} else {
		// Sharded: rewrite only the shards the delta touches; the others are
		// named unchanged by the new manifest.
		plan := planContentShardedFold(reader, resolver, foldSnap)
		infos := make([]contentShardInfo, len(base.shards))
		for i, sh := range base.shards {
			infos[i] = sh.info
			if !plan.dirty[i] || saveErr != nil {
				continue
			}
			source := contentShardFoldSource(sh, plan.incoming[i], plan.drop)
			built, shard, err := foldShard(source)
			if err != nil {
				serviceLog("content fold assemble failed volume=%s shard=%s err=%v", vol.volume, sh.info.File, err)
				vol.contentCoord.foldFailed(contentFoldRetryBackoff)
				return
			}
			rewritten += len(built.docs)
			infos[i], saveErr = contentSaveShard(contentShardDir(gsx), shard, contentBuildFiles(built), sh.info.Prefixes)
		}
		if saveErr == nil {
			if !s.contentBuildCurrent(vol, gen, base.JournalID) {
				vol.contentCoord.foldFailed(contentFoldRetryBackoff)
				contentSweepShards(gsx)
				return
			}
			saveErr = contentFoldSaveManifest(gsx, header, infos)
		}
		if saveErr != nil {
			contentSweepShards(gsx) // drop shards no manifest names
		}
	}
	if err := saveErr; err != nil {
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
	// The fold just persisted every reconciled change (the queue drains
	// before a fold may run): converge the scope-reconcile tracking. The
	// stamped header already carries the fingerprint.
	vol.content.mu.Lock()
	vol.content.clearScopeReconcileLocked()
	vol.content.mu.Unlock()
	// ...and a wrap reconcile that forced this fold: the new checkpoint is
	// durable, so the reconciling display converges (the next drain returns
	// health to ready).
	vol.content.clearReconcileActive()
	contentSweepShards(gsx)
	// Tier-2 refresh (§6y): tombstoned filter keys drop their records and
	// changed keys recompute bits + fingerprints from the delta text the fold
	// just consumed. The new base's scope restamps the filter identity.
	if fltOld != nil && len(filterSnap) > 0 {
		if fp, ok := vol.content.scopeReconciledFP(); ok {
			header.ScopeHash = fp
		}
		s.publishFilterRefresh(vol, gen, header, fltOld, filterSnap)
	}
	serviceLog("content fold volume=%s rewritten_docs=%d checkpoint=%d", vol.volume, rewritten, checkpoint)
}

// publishFilterRefresh persists a filter generation and installs it under
// indexMu for writing, so no query reads the replaced mapping mid-swap. The
// file save is guarded by the same reserve; a save failure keeps the old
// filter (its stale records skip by fingerprint) and only logs — the delta
// still drained, so a lost refresh is re-covered by the file's next change.
// A generation that moved under the fold drops the refresh instead of
// installing it past a newer base.
func (s *goSearchService) publishFilterRefresh(vol *serviceVolumeIndex, gen uint64, header *contentIndex, old *contentFilter, refresh []contentDeltaDoc) {
	if vol == nil || old == nil && len(refresh) == 0 {
		return
	}
	var scope [32]byte
	var origin uint32
	var maxRaw, maxText int64
	if header != nil {
		scope, origin, maxRaw, maxText = header.ScopeHash, header.Origin, header.Policy.MaxRaw, header.Policy.MaxText
	} else if old != nil {
		scope, origin, maxRaw, maxText = old.ScopeHash, old.Origin, old.MaxRaw, old.MaxText
	}
	next := contentFilterApplyRefresh(old, refresh, origin, scope, maxRaw, maxText)
	if next == nil {
		return
	}
	gsx := contentIndexPathForDB(vol.dbPath)
	if gsx == "" {
		next.Release()
		return
	}
	if err := contentCheckDiskReserve(gsx); err != nil {
		serviceLog("content filter refresh skipped volume=%s: %v", vol.volume, err)
		next.Release()
		return
	}
	if err := contentSaveFilter(contentFilterPathForGSX(gsx), next); err != nil {
		serviceLog("content filter refresh save failed volume=%s: %v", vol.volume, err)
		next.Release()
		return
	}
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	// Re-validate the generation and journal under the write lock, inline:
	// contentBuildCurrent takes indexMu.RLock itself and must not be called
	// nested here (Go RWMutex is not reentrant — that self-deadlocks).
	// replayGen is atomic; journalID is safe under the write lock, exactly
	// as publishFoldedContent reads it.
	if s.contentStopped(vol) || vol.replayGen.Load() != gen || vol.journalID != header.JournalID {
		next.Release()
		return
	}
	vol.content.setFilter(next)
}

// contentFilterApplyRefresh folds delta docs into a filter generation: live
// docs for covered keys recompute bits + fingerprints from the delta text,
// tombstoned covered keys drop their records, and every other record carries
// over verbatim. The result restamps the given identity (the fold's header).
// It returns nil when there is nothing to carry (no old filter and no live
// refresh), so no empty sidecar is written.
func contentFilterApplyRefresh(old *contentFilter, refresh []contentDeltaDoc, origin uint32, scope [32]byte, maxRaw, maxText int64) *contentFilter {
	drop := make(map[uint64]bool, len(refresh))
	var live []contentDeltaDoc
	for _, d := range refresh {
		drop[d.FRN] = true
		if !d.Deleted {
			live = append(live, d)
		}
	}
	var kept []contentFilterRecord
	if old != nil {
		kept = make([]contentFilterRecord, 0, len(old.Records))
		for _, r := range old.Records {
			if !drop[r.Key] {
				kept = append(kept, r)
			}
		}
	}
	if len(kept) == 0 && len(live) == 0 {
		return nil
	}
	made := make([]contentFilterRecord, 0, len(live))
	for _, d := range live {
		stored := contentStoredText(d.Text)
		folded := contentFoldText(stored)
		grams := contentGramsOfU32(folded)
		class := contentFilterClassForGrams(len(grams))
		bits := make([]byte, contentFilterClassBits[class]/8)
		contentFilterBitsOfU32(bits, grams)
		made = append(made, contentFilterRecord{Key: d.FRN, FileSize: d.FileSize, FileModNano: d.FileModNano, TextLen: int64(len(d.Text)), Class: class, Path: d.Path, Bits: bits})
	}
	sort.Slice(made, func(i, j int) bool { return made[i].Key < made[j].Key })
	// Both halves are key-sorted: merge, with the refreshed records winning
	// ties (a key cannot be in both — the partition drops refreshed keys
	// from kept — but the merge stays total regardless).
	out := make([]contentFilterRecord, 0, len(kept)+len(made))
	i, j := 0, 0
	for i < len(kept) && j < len(made) {
		if kept[i].Key < made[j].Key {
			out = append(out, kept[i])
			i++
		} else {
			out = append(out, made[j])
			j++
		}
	}
	out = append(out, kept[i:]...)
	out = append(out, made[j:]...)
	if len(out) == 0 {
		return nil
	}
	root := ""
	if old != nil {
		root = old.Root
	}
	return &contentFilter{Version: contentFilterVersion, Origin: origin, MaxRaw: maxRaw, MaxText: maxText, ScopeHash: scope, Root: root, BuiltAt: time.Now(), Records: out}
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
	if idx.Origin == contentOriginWalk {
		if idx.JournalID != 0 || idx.CheckpointUSN == 0 {
			return false
		}
	} else if idx.Origin != contentOriginUSN || idx.JournalID != journalID || idx.CheckpointUSN == 0 {
		return false
	}
	reader, err := openContentReader(idx)
	if err != nil {
		return false
	}
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	if vol.state != "ready" || vol.index == nil {
		return false
	}
	if idx.Origin == contentOriginWalk {
		if vol.index.Source != "walk" {
			return false
		}
		frns, ids, ok := s.contentWalkKeys(vol)
		if !ok {
			return false
		}
		if old := vol.content.setReady(idx, reader, buildContentResolver(idx.Docs, frns, ids)); old != nil {
			old.Release()
		}
		attached = true
		return true
	}
	if vol.index.Source != "usn" {
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
	// Verify the large sections in the background, as attach does: the fold
	// just wrote this file, so a mismatch here means the write itself is
	// suspect and the base must not be served.
	if s.beginContentTask(vol) {
		contentVerifyAsync(func() { defer endContentTask(vol); s.verifyContentAfterAttach(vol, idx) })
	}
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
			return contentBuildDoc{frn: d.FRN, path: d.Path, text: d.Text, class: d.ContentType, version: d.ExtractorVersion, fileSize: d.FileSize, fileModNano: d.FileModNano}, true, nil
		case haveBase && (!haveDelta || baseFRN < deltaFRN):
			doc := m.base[m.i]
			m.i++
			text := m.reader.docText(doc.DocID)
			if len(text) == 0 {
				continue
			}
			return contentBuildDoc{frn: doc.FRN, path: m.reader.docPath(doc.DocID), text: text, modUnix: doc.ModUnix, class: doc.ContentType, version: doc.ExtractorVersion, fileSize: doc.FileSize, fileModNano: doc.FileModNano}, true, nil
		case haveBase && haveDelta:
			// Same FRN: the delta replaces the base doc; a tombstone drops it.
			d := m.delta[m.j]
			m.j++
			m.i++
			if d.Deleted {
				continue
			}
			return contentBuildDoc{frn: d.FRN, path: d.Path, text: d.Text, class: d.ContentType, version: d.ExtractorVersion, fileSize: d.FileSize, fileModNano: d.FileModNano}, true, nil
		default:
			return contentBuildDoc{}, false, nil
		}
	}
}
