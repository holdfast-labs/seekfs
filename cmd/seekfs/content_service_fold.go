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
	"context"
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
	// The folded base keeps the prior inclusion stats plus the drain-time
	// declines since the last fold/publish (taken and reset here on the
	// drain goroutine, the only writer). Dedupe totals are restamped
	// after assembly below (the merge recomputes them).
	foldInclusion := base.Inclusion
	foldInclusion.merge(vol.contentCoord.inclusion)
	vol.contentCoord.inclusion = contentInclusionStats{}
	// A scope reconcile between builds rescanned the current selection; its
	// tallies replace the build's stale reasons so the fold persists the
	// live scope (e.g. no `root` for files a new auto root admitted).
	vol.content.applyScopeInclusion(&foldInclusion)
	header := &contentIndex{Origin: base.Origin, JournalID: base.JournalID, CheckpointUSN: checkpoint, Policy: base.Policy, ScopeHash: base.ScopeHash, Inclusion: foldInclusion}
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
	// v11 stores no text: the fold re-extracts base docs from disk (the
	// delta already holds changed files' text). Filter identity carries
	// forward from the base section; caps carry forward from the header.
	settings := contentServiceExtractSettings()
	filterID := contentFilterID{MaxRaw: header.Policy.MaxRaw, MaxText: header.Policy.MaxText, Encoding: contentServiceEncodingLabel()}
	if reader.filter != nil {
		// The re-framed section keeps the base's caps identity (explicit
		// caps stay exact at verification; policy defaults stay
		// repo-aware fail-open), root and encoding.
		filterID = contentFilterID{MaxRaw: header.Policy.MaxRaw, MaxText: header.Policy.MaxText, CapsExplicit: reader.filter.CapsExplicit, Root: reader.filter.Root, Encoding: reader.filter.Encoding}
		if filterID.Encoding == "" {
			filterID.Encoding = contentServiceEncodingLabel()
		}
	}
	fctx := contentWithExtractSettings(context.Background(), settings)
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
		built, err := assembleContentIndexStreamed(guarded, tmpDir, filterID)
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
	// basePaths parallels the base doc table by DocID for disk re-reads.
	basePaths := make([]string, len(base.Docs))
	for i := range base.Docs {
		basePaths[i] = reader.docPath(base.Docs[i].DocID)
	}
	// livePath resolves a doc's current record-index path under a brief read
	// lock (rename fallback for re-reads whose stored path went stale).
	foldPathCache := make(map[int]string, 1024)
	livePath := func(frn uint64) string {
		s.indexMu.RLock()
		defer s.indexMu.RUnlock()
		id, ok := vol.recordIDForFRN(frn)
		if !ok || vol.index == nil {
			return ""
		}
		return vol.index.reconstructCompactPathCached(id, foldPathCache)
	}
	var rewritten int
	var saveErr error
	if base.shards == nil {
		src := newContentFoldDiskSource(vol, base.Docs, basePaths, snap, settings, fctx, livePath)
		defer src.stop()
		// Growth is projected up front from metadata (no text reads): base
		// extracted bytes, minus tombstoned/replaced docs, plus delta docs.
		// Past twice the shard target the fold shards directly instead of
		// assembling a single file it would only split again.
		baseLens := make(map[uint64]int64, len(base.Docs))
		var projected int64
		for i := range base.Docs {
			baseLens[base.Docs[i].FRN] = int64(base.Docs[i].TextLen)
			projected += int64(base.Docs[i].TextLen)
		}
		for _, d := range snap {
			if old, ok := baseLens[d.FRN]; ok {
				projected -= old
				delete(baseLens, d.FRN)
			}
			if !d.Deleted {
				projected += int64(len(d.Text))
			}
		}
		if projected > int64(2*contentShardTargetBytes) {
			serviceLog("content fold resharding volume=%s projected=%d", vol.volume, projected)
			items := contentFoldMergeItems(base.Docs, basePaths, snap)
			var shards int
			shards, saveErr = contentReshardAndSave(gsx, items, foldReadDoc(vol, settings, fctx, livePath), header, filterID, tmpDir)
			if saveErr == nil {
				serviceLog("content fold resharded volume=%s shards=%d", vol.volume, shards)
			} else {
				contentSweepShards(gsx)
			}
			rewritten = len(items)
		} else {
			built, shard, err := foldShard(src.next)
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
			// Stamp the shard save, not the header: foldShard already
			// snapshotted header into shard above.
			shard.Inclusion.DedupeFiles = built.dedupeFiles
			shard.Inclusion.DedupeBytes = built.dedupeBytes
			saveErr = contentFoldSave(gsx, shard, contentBuildFiles(built))
		}
	} else {
		// Sharded: rewrite only the shards the delta touches; the others are
		// named unchanged by the new manifest. Each dirty shard re-reads its
		// surviving base docs from disk (v11: no stored text) merged with its
		// incoming delta docs.
		plan := planContentShardedFold(reader, resolver, snap)
		infos := make([]contentShardInfo, len(base.shards))
		var foldBuilt []*contentStreamedBuild
		var stops []func()
		defer func() {
			for _, stop := range stops {
				stop()
			}
		}()
		for i, sh := range base.shards {
			infos[i] = sh.info
			if !plan.dirty[i] || saveErr != nil {
				continue
			}
			src := contentShardFoldDiskSource(vol, sh, plan.incoming[i], plan.drop, settings, fctx, livePath)
			stops = append(stops, src.stop)
			built, shard, err := foldShard(src.next)
			if err != nil {
				serviceLog("content fold assemble failed volume=%s shard=%s err=%v", vol.volume, sh.info.File, err)
				vol.contentCoord.foldFailed(contentFoldRetryBackoff)
				return
			}
			rewritten += len(built.docs)
			foldBuilt = append(foldBuilt, built)
			infos[i], saveErr = contentSaveShard(contentShardDir(gsx), shard, contentBuildFiles(built), sh.info.Prefixes)
		}
		if saveErr == nil {
			// Restamp the dedupe totals over the new corpus: rewritten
			// shards report their fresh assembler counts, retained
			// shards keep their loaded ones.
			var df, db int64
			for i, sh := range base.shards {
				if plan.dirty[i] {
					continue
				}
				f, b := contentDedupeTotalsForIndex(sh.idx)
				df += f
				db += b
			}
			for _, b := range foldBuilt {
				df += b.dedupeFiles
				db += b.dedupeBytes
			}
			header.Inclusion.DedupeFiles = df
			header.Inclusion.DedupeBytes = db
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
	serviceLog("content fold volume=%s rewritten_docs=%d checkpoint=%d", vol.volume, rewritten, checkpoint)
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
	if old := vol.content.setReady(idx, reader, buildContentResolverSeq(idx.Docs, frns, ids, vol.index.seqColumn())); old != nil {
		// Replace the previous base's mapping while holding indexMu for writing:
		// no query can be reading it (queries hold indexMu.RLock throughout), so
		// the unmap cannot race a read.
		//
		// The fold itself reads the base mapping WITHOUT indexMu (it captures the
		// reader and streams doc paths lock-free for the whole assembly; base
		// text comes from disk, not the mapping).
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

// contentFoldItem is one merged corpus member for grouping/resharding:
// its key, path, and extracted-text size — all known without reading text.
type contentFoldItem struct {
	frn  uint64
	path string
	size int64
	// seq carries the doc's NTFS sequence through a re-extract so a fold never
	// downgrades a known sequence back to unknown.
	seq uint16
}

// contentFoldMergeItems merges the base doc table (FRN-sorted, with parallel
// DocID-ordered paths) and the delta snapshot into the ordered merged corpus
// without reading any text: delta docs win ties, tombstones vanish. Sizes
// come from the doc table (base) and the delta text lengths.
func contentFoldMergeItems(base []contentDoc, paths []string, snap []contentDeltaDoc) []contentFoldItem {
	delta := append([]contentDeltaDoc(nil), snap...)
	sort.Slice(delta, func(i, j int) bool {
		if delta[i].FRN != delta[j].FRN {
			return delta[i].FRN < delta[j].FRN
		}
		return delta[i].Path < delta[j].Path
	})
	var out []contentFoldItem
	i, j := 0, 0
	for i < len(base) || j < len(delta) {
		var baseFRN, deltaFRN uint64
		haveBase, haveDelta := i < len(base), j < len(delta)
		if haveBase {
			baseFRN = base[i].FRN
		}
		if haveDelta {
			deltaFRN = delta[j].FRN
		}
		switch {
		case haveDelta && (!haveBase || deltaFRN < baseFRN):
			if !delta[j].Deleted {
				out = append(out, contentFoldItem{frn: delta[j].FRN, path: delta[j].Path, size: int64(len(delta[j].Text)), seq: delta[j].FRNSeq})
			}
			j++
		case haveBase && (!haveDelta || baseFRN < deltaFRN):
			p := ""
			if i < len(paths) {
				p = paths[i]
			}
			out = append(out, contentFoldItem{frn: base[i].FRN, path: p, size: int64(base[i].TextLen), seq: base[i].FRNSeq})
			i++
		case haveBase && haveDelta:
			d := delta[j]
			j++
			i++
			if d.Deleted {
				continue
			}
			out = append(out, contentFoldItem{frn: d.FRN, path: d.Path, size: int64(len(d.Text)), seq: d.FRNSeq})
		}
	}
	return out
}

// foldReadDoc returns a serial disk reader for merged items (the reshard
// growth path, which is too rare to parallelize): each item re-extracts
// through the same safe path as the build, stamping fresh extractor
// identity and fingerprints. Unreadable items report false and are skipped;
// stale stored paths fall back to the live record-index path like the
// parallel source.
func foldReadDoc(vol *serviceVolumeIndex, settings contentExtractSettings, ctx context.Context, livePath func(frn uint64) string) func(contentFoldItem) (contentBuildDoc, bool) {
	cctx := contentWithExtractSettings(ctx, settings)
	return func(it contentFoldItem) (contentBuildDoc, bool) {
		if it.path == "" {
			return contentBuildDoc{}, false
		}
		// Fail open to the in-repo caps (the drain's precedent): the fold
		// carries no scope matcher, and outside-repo caps would truncate
		// in-repo files the build indexed whole.
		if doc, _, ok := contentBuildDocSafe(cctx, contentBuildItem{frn: it.frn, path: it.path, inRepo: true, seq: it.seq}); ok {
			return doc, true
		}
		if livePath != nil {
			if lp := livePath(it.frn); lp != "" && lp != it.path {
				doc, _, ok := contentBuildDocSafe(cctx, contentBuildItem{frn: it.frn, path: lp, inRepo: true, seq: it.seq})
				return doc, ok
			}
		}
		return contentBuildDoc{}, false
	}
}

// contentFoldDiskSource merges base docs (re-read from disk in parallel,
// in order) with delta docs (from memory) in ascending (frn, path) order.
// v11 stores no text, so a fold re-extracts every base doc the delta does
// not cover; the delta wins for covered keys and tombstones drop docs.
// Unreadable base docs are skipped (logged): a file deleted mid-fold is
// correctly dropped, and its drain tombstone converges the rest. A transient
// read failure loses the doc until its next change (documented residual);
// folds never publish partial text. The merge adds no corpus-sized buffer
// (one metadata item per base doc); text flows one document at a time.
type contentFoldDiskSource struct {
	items    []contentFoldItem
	delta    []contentDeltaDoc
	pull     func() (contentExtracted, bool)
	halt     func()
	settings contentExtractSettings
	livePath func(frn uint64) string
	i        int
	j        int
	skips    int
	vol      string
}

// newContentFoldDiskSource builds the source over base docs (parallel docs
// and paths) merged with snap. settings is the extraction policy the
// re-reads use (the service policy, matching the drain). vol names the
// volume for skip logging. Call stop when assembly ends or aborts.
func newContentFoldDiskSource(vol *serviceVolumeIndex, base []contentDoc, paths []string, snap []contentDeltaDoc, settings contentExtractSettings, ctx context.Context, livePath func(frn uint64) string) *contentFoldDiskSource {
	delta := append([]contentDeltaDoc(nil), snap...)
	sort.Slice(delta, func(i, j int) bool {
		if delta[i].FRN != delta[j].FRN {
			return delta[i].FRN < delta[j].FRN
		}
		return delta[i].Path < delta[j].Path
	})
	items := make([]contentFoldItem, 0, len(base))
	for i := range base {
		p := ""
		if i < len(paths) {
			p = paths[i]
		}
		if p == "" {
			// No path, no re-read: the prefetch yields exactly one result
			// per submitted item, so unsubmittable docs must not occupy
			// merge positions (that would misalign every later result).
			// Stored paths are never empty in practice; a hole drops the
			// doc until its next change re-adds it through the delta.
			continue
		}
		items = append(items, contentFoldItem{frn: base[i].FRN, path: p, size: int64(base[i].TextLen), seq: base[i].FRNSeq})
	}
	bitems := make([]contentBuildItem, 0, len(items))
	for _, it := range items {
		// Fail open to the in-repo caps (see foldReadDoc): the fold has
		// no scope matcher, and outside-repo caps would truncate in-repo
		// files the build indexed whole.
		bitems = append(bitems, contentBuildItem{frn: it.frn, path: it.path, inRepo: true, seq: it.seq})
	}
	cctx := contentWithExtractSettings(ctx, settings)
	next, stop := contentPrefetchExtract(cctx, bitems, contentBuildWorkerCount(), contentBuildDocSafe)
	name := ""
	if vol != nil {
		name = vol.volume
	}
	return &contentFoldDiskSource{items: items, delta: delta, pull: next, halt: stop, settings: settings, livePath: livePath, vol: name}
}

// stop frees the prefetch workers and open handles.
func (m *contentFoldDiskSource) stop() {
	if m == nil || m.halt == nil {
		return
	}
	m.halt()
}

// next serves the merged stream in (frn, path) order. Base docs covered by
// the delta come from the delta; tombstoned docs vanish; unreadable base
// docs are skipped with a bounded log.
func (m *contentFoldDiskSource) next() (contentBuildDoc, bool, error) {
	for {
		// Advance the base cursor past delta-covered keys without consuming
		// prefetch results out of order: the prefetch yields in item order,
		// so results are consumed strictly front-to-back.
		for m.i < len(m.items) && m.j < len(m.delta) && m.delta[m.j].FRN < m.items[m.i].frn {
			d := m.delta[m.j]
			m.j++
			if d.Deleted {
				continue
			}
			return contentBuildDoc{frn: d.FRN, path: d.Path, text: d.Text, class: d.ContentType, version: d.ExtractorVersion, fileSize: d.FileSize, fileModNano: d.FileModNano, seq: d.FRNSeq}, true, nil
		}
		if m.j < len(m.delta) && (m.i >= len(m.items) || m.delta[m.j].FRN == m.items[m.i].frn) {
			// Same FRN: the delta replaces the base doc; consume both sides.
			d := m.delta[m.j]
			m.j++
			if m.i < len(m.items) {
				m.i++
				if _, live := m.pull(); !live {
					return contentBuildDoc{}, false, errors.New("content fold re-read stopped")
				}
			}
			if d.Deleted {
				continue
			}
			return contentBuildDoc{frn: d.FRN, path: d.Path, text: d.Text, class: d.ContentType, version: d.ExtractorVersion, fileSize: d.FileSize, fileModNano: d.FileModNano, seq: d.FRNSeq}, true, nil
		}
		if m.i >= len(m.items) {
			// Drain any remaining delta-only tail.
			for m.j < len(m.delta) {
				d := m.delta[m.j]
				m.j++
				if d.Deleted {
					continue
				}
				return contentBuildDoc{frn: d.FRN, path: d.Path, text: d.Text, class: d.ContentType, version: d.ExtractorVersion, fileSize: d.FileSize, fileModNano: d.FileModNano, seq: d.FRNSeq}, true, nil
			}
			return contentBuildDoc{}, false, nil
		}
		got, live := m.pull()
		if !live {
			return contentBuildDoc{}, false, errors.New("content fold re-read stopped")
		}
		m.i++
		doc, res, ok := got.doc, got.res, got.ok
		if !ok && m.livePath != nil {
			// The stored path is stale (most often an intra-volume rename,
			// which keeps the FRN but moves the file): resolve the live
			// record-index path and retry once. A rename without content
			// change is not re-extracted by the drain, so without this the
			// doc would be dropped until its next change.
			if lp := m.livePath(m.items[m.i-1].frn); lp != "" && lp != m.items[m.i-1].path {
				doc, res, ok = contentBuildDocSafe(contentWithExtractSettings(context.Background(), m.settings), contentBuildItem{frn: m.items[m.i-1].frn, path: lp})
			}
		}
		if !ok {
			// Skip unreadable/unextractable base docs (a mid-fold delete is
			// correctly dropped); bound the log, not the skip.
			m.skips++
			if m.skips <= 10 {
				reason := res.Reason
				if reason == "" {
					reason = "unreadable"
				}
				serviceLog("content fold volume=%s skip %s: %s", m.vol, m.items[m.i-1].path, reason)
			}
			continue
		}
		_ = res
		return doc, true, nil
	}
}
