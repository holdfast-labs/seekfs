package main

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func validateUSNCheckpoint(vol *serviceVolumeIndex, journal usnJournalDataV0) error {
	if vol.journalID != 0 && vol.journalID != journal.UsnJournalID {
		return staleIndexError{reason: fmt.Sprintf("journal id changed from %d to %d", vol.journalID, journal.UsnJournalID), rebuild: true}
	}
	firstValid := journal.FirstUsn
	if journal.LowestValidUsn > firstValid {
		firstValid = journal.LowestValidUsn
	}
	if vol.checkpoint < firstValid {
		return staleIndexError{reason: fmt.Sprintf("checkpoint %d is before first valid USN %d", vol.checkpoint, firstValid), rebuild: true}
	}
	if vol.checkpoint > journal.NextUsn {
		return staleIndexError{reason: fmt.Sprintf("checkpoint %d is after journal next USN %d", vol.checkpoint, journal.NextUsn), rebuild: true}
	}
	return nil
}

type staleIndexError struct {
	reason  string
	rebuild bool
}

func (e staleIndexError) Error() string {
	return e.reason
}

// staleRecoveryBlockedError reports that a stale index cannot be rebuilt right
// now for a reason that retrying will not fix in this process lifetime (the
// index file is memory-mapped/locked by another process that will not release
// it).  The stale-recovery loop stops retrying on this error so it does not
// burn CPU and build multi-gigabyte in-memory indexes on every backoff tick.
type staleRecoveryBlockedError struct{ reason string }

func (e staleRecoveryBlockedError) Error() string { return e.reason }

func shouldRebuildStaleIndex(err error) bool {
	var staleErr staleIndexError
	if errors.As(err, &staleErr) {
		return staleErr.rebuild
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "journal id changed") ||
		strings.Contains(text, "before first valid usn") ||
		strings.Contains(text, "after journal next usn")
}

func rebuildServiceVolumeIndex(vol *serviceVolumeIndex) (*serviceVolumeIndex, error) {
	if vol == nil || vol.volume == "" || vol.dbPath == "" {
		return nil, errors.New("stale index rebuild requires a volume and db path")
	}
	idx, err := indexUSNVolume(vol.volume)
	if err != nil {
		return nil, err
	}
	buildOrders(idx)
	if err := closeIndexMMapRecords(vol.index); err != nil {
		return nil, err
	}
	if err := saveIndex(vol.dbPath, idx); err != nil {
		return nil, err
	}
	releaseServiceMemoryAfterSave()
	if err := removeWAL(vol.dbPath); err != nil {
		serviceLog("wal cleanup error volume=%s db=%s err=%v", vol.volume, vol.dbPath, err)
	}
	loaded, err := loadIndexForService(vol.dbPath)
	if err != nil {
		return nil, err
	}
	rebuilt := newServiceVolumeIndex(vol.dbPath, loaded)
	rebuilt.state = "ready"
	rebuilt.staleReason = ""
	return rebuilt, nil
}

func rebuildWalkIndex(vol *serviceVolumeIndex) (*Index, error) {
	if vol == nil || vol.index == nil || len(vol.index.Roots) == 0 {
		return nil, errors.New("walk index rebuild requires roots")
	}
	idx := &Index{
		Version: vol.index.Version,
		Roots:   append([]string(nil), vol.index.Roots...),
		BuiltAt: time.Now(),
		Source:  "walk",
	}
	if idx.Version == 0 {
		idx.Version = indexVersion
	}
	for _, root := range idx.Roots {
		if err := walkRoot(root, idx); err != nil {
			serviceLog("walk watcher rebuild root error root=%s db=%s err=%v", root, vol.dbPath, err)
		}
	}
	buildOrders(idx)
	ensureCompactIndexForService(idx)
	return idx, nil
}

func (s *goSearchService) rebuildWalkVolumeInPlace(vol *serviceVolumeIndex, reason string) error {
	if vol == nil || vol.dbPath == "" {
		return errors.New("walk index rebuild requires a db path")
	}
	vol.walkMu.Lock()
	defer vol.walkMu.Unlock()
	idx, err := rebuildWalkIndex(vol)
	if err != nil {
		return err
	}
	tmp, err := stageIndexFile(vol.dbPath, idx)
	if err != nil {
		return err
	}
	s.indexMu.Lock()
	if err := closeIndexMMapRecords(vol.index); err != nil {
		s.indexMu.Unlock()
		_ = os.Remove(tmp)
		return err
	}
	if err := commitStageIndexFile(vol.dbPath, tmp); err != nil {
		s.indexMu.Unlock()
		return err
	}
	loaded, err := loadIndexForService(vol.dbPath)
	if err != nil {
		s.indexMu.Unlock()
		return err
	}
	rebuilt := newServiceVolumeIndex(vol.dbPath, loaded)
	rebuilt.state = "ready"
	rebuilt.staleReason = ""
	vol.replayGen.Add(1)
	vol.persistGen.Add(1)
	replaceServiceVolumeContents(vol, rebuilt)
	for i, existing := range s.volumes {
		if existing == vol {
			s.indexes[i] = vol.index
			break
		}
	}
	s.indexMu.Unlock()
	releaseServiceMemoryAfterSave()
	s.startBackgroundNameTrigramBuilds([]*serviceVolumeIndex{vol})
	serviceLog("rebuilt walk index db=%s entries=%d reason=%s", vol.dbPath, loaded.entryCount(), reason)
	return nil
}

func (s *goSearchService) rebuildVolumeInPlace(vol *serviceVolumeIndex) error {
	if vol == nil || vol.volume == "" || vol.dbPath == "" {
		return errors.New("stale index rebuild requires a volume and db path")
	}
	idx, err := indexUSNVolume(vol.volume)
	if err != nil {
		return err
	}
	buildOrders(idx)

	// The freshly built in-memory index can be several gigabytes.  Release it
	// explicitly on every failure path so the stale-recovery retry loop does
	// not accumulate one large unreclaimed index per attempt.
	release := func() { idx = nil; releaseServiceMemoryAfterSave() }

	tmp, err := stageIndexFile(vol.dbPath, idx)
	if err != nil {
		release()
		return err
	}
	s.indexMu.Lock()
	if err := closeIndexMMapRecords(vol.index); err != nil {
		s.indexMu.Unlock()
		_ = os.Remove(tmp)
		release()
		return err
	}
	if err := commitStageIndexFile(vol.dbPath, tmp); err != nil {
		s.indexMu.Unlock()
		release()
		// If the target index file is still memory-mapped or otherwise locked
		// by another process, retrying in this process is futile and only
		// rebuilds the whole index on every backoff tick.  Stop the recovery
		// loop instead of burning memory indefinitely.
		if isIndexFileLockedError(err) {
			return staleRecoveryBlockedError{reason: fmt.Sprintf("index file %s is locked by another process; cannot rebuild stale volume %s: %v", vol.dbPath, vol.volume, err)}
		}
		return err
	}
	if err := removeWAL(vol.dbPath); err != nil {
		serviceLog("wal cleanup error volume=%s db=%s err=%v", vol.volume, vol.dbPath, err)
	}
	loaded, loadErr := loadIndexForService(vol.dbPath)
	if loadErr != nil {
		s.indexMu.Unlock()
		release()
		return loadErr
	}
	rebuilt := newServiceVolumeIndex(vol.dbPath, loaded)
	rebuilt.state = "ready"
	rebuilt.staleReason = ""
	vol.replayGen.Add(1)
	vol.persistGen.Add(1)
	replaceServiceVolumeContents(vol, rebuilt)
	for i, existing := range s.volumes {
		if existing == vol {
			s.indexes[i] = vol.index
			break
		}
	}
	s.indexMu.Unlock()
	releaseServiceMemoryAfterSave()
	s.startBackgroundNameTrigramBuilds([]*serviceVolumeIndex{vol})
	serviceLog("rebuilt stale index volume=%s db=%s entries=%d", rebuilt.volume, rebuilt.dbPath, rebuilt.index.entryCount())
	// WP1e/PB4: content must self-heal after a filename rebuild (a journal
	// reset invalidated the FRN-keyed content index). Re-attach a still-valid
	// sidecar, or schedule the background content rebuild.
	s.featuresVolumeReady(vol)
	return nil
}

// isIndexFileLockedError reports whether err indicates the index file could not
// be replaced because another process holds it (memory-mapped or open).  These
// failures will not resolve while that process runs, so the stale-recovery loop
// stops retrying instead of rebuilding the index on every backoff tick.
func isIndexFileLockedError(err error) bool {
	if err == nil {
		return false
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		if pathErr.Err == windows.ERROR_ACCESS_DENIED || pathErr.Err == windows.ERROR_SHARING_VIOLATION {
			return true
		}
		if errors.Is(pathErr.Err, os.ErrPermission) {
			return true
		}
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "access is denied") || strings.Contains(text, "sharing violation")
}

func (s *goSearchService) startWalkWatchers(vol *serviceVolumeIndex) {
	if vol == nil || vol.index == nil || vol.index.Source != "walk" {
		return
	}
	for _, root := range vol.index.Roots {
		root := root
		go s.watchWalkRoot(vol, root)
	}
}

func (s *goSearchService) watchWalkRoot(vol *serviceVolumeIndex, root string) {
	handle, err := openDirectoryForChanges(root)
	if err != nil {
		serviceLog("walk watcher disabled root=%s db=%s err=%v", root, vol.dbPath, err)
		return
	}
	defer windows.CloseHandle(handle)
	buffer := make([]byte, 64*1024)
	mask := uint32(windows.FILE_NOTIFY_CHANGE_FILE_NAME |
		windows.FILE_NOTIFY_CHANGE_DIR_NAME |
		windows.FILE_NOTIFY_CHANGE_ATTRIBUTES |
		windows.FILE_NOTIFY_CHANGE_SIZE |
		windows.FILE_NOTIFY_CHANGE_LAST_WRITE |
		windows.FILE_NOTIFY_CHANGE_CREATION)
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		var bytesReturned uint32
		err := windows.ReadDirectoryChanges(handle, &buffer[0], uint32(len(buffer)), true, mask, &bytesReturned, nil, 0)
		if err != nil {
			if errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
				return
			}
			if errors.Is(err, windows.ERROR_NOTIFY_ENUM_DIR) {
				_ = s.rebuildWalkVolumeInPlace(vol, "watch-overflow")
				continue
			}
			serviceLog("walk watcher read error root=%s db=%s err=%v", root, vol.dbPath, err)
			select {
			case <-s.stop:
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		reason := "watch-change"
		if bytesReturned == 0 {
			reason = "watch-overflow"
		}
		select {
		case <-s.stop:
			return
		case <-time.After(250 * time.Millisecond):
		}
		if err := s.rebuildWalkVolumeInPlace(vol, reason); err != nil {
			serviceLog("walk watcher rebuild error root=%s db=%s err=%v", root, vol.dbPath, err)
		}
	}
}

func openDirectoryForChanges(root string) (windows.Handle, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return windows.InvalidHandle, err
	}
	ptr, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(
		ptr,
		windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
}

func (vol *serviceVolumeIndex) applyUSNChanges(changes []usnChange) {
	// Refresh a changed file's metadata once per batch, at its last change, so
	// a file written across many DATA_EXTEND events is statted a single time.
	lastChange := make(map[uint64]int, len(changes))
	for i, change := range changes {
		if change.FRN != 0 {
			lastChange[change.FRN] = i
		}
	}
	// Changes that actually apply to the indexed set: own-artifact churn is
	// consumed below and must not reach the content coordinator either, or the
	// content store would feed its own writes back. Built only when content is
	// enabled so the default replay path pays nothing.
	var applied []usnChange
	if vol.contentCoord != nil || vol.featureFeed != nil {
		applied = make([]usnChange, 0, len(changes))
	}
	for i, change := range changes {
		if change.FRN == 0 {
			continue
		}
		// Changes inside directories that hold seekfs's own artifacts (the
		// seekfs dir, the name-gram spool, and the builder's random scratch
		// dirs) are consumed without entering the overlay.  The offline walk
		// refuses those directories too, and replaying their churn used to
		// inflate the overlay watermark until a persist was due the moment the
		// previous one finished.  The checkpoint still advances (below and in
		// every caller) so replay never re-reads the skipped records, and the
		// WAL keeps the full stream so checkpoints stay derivable.
		if vol.consumeOwnedReplayChange(change) {
			if change.USN > vol.checkpoint {
				vol.checkpoint = change.USN
			}
			continue
		}
		vol.recordOverlayChange(change, i == lastChange[change.FRN] && change.Reason&usnReasonNeedsInfoRefresh != 0)
		if vol.contentCoord != nil || vol.featureFeed != nil {
			applied = append(applied, change)
		}
		if change.USN > vol.checkpoint {
			vol.checkpoint = change.USN
		}
	}
	vol.index.Checkpoint = vol.checkpoint
	vol.dirty = true
	vol.publishDirSizeDelta()
	vol.publishSnapshot()
	// Content search consumes the same applied change stream as the record
	// overlay (own-artifact churn already excluded). Nil unless the feature is
	// enabled, so the default path is untouched.
	vol.observeFeatureChanges(applied)
}

// filterOwnedReplayChanges drops changes that fall inside directories holding
// seekfs's own artifacts.  Applying it before the WAL append and the overlay
// apply keeps the multi-GB staged index, the service log, and the external
// builder's spill files out of both, so their churn can no longer fill the WAL
// or inflate the overlay watermark until a persist is due the moment the
// previous one finished.  The checkpoint is advanced past the dropped records
// by the caller, so replay never re-reads them.  Callers must derive any
// checkpoint from the unfiltered batch first.  The returned slice may alias
// changes.
func (vol *serviceVolumeIndex) filterOwnedReplayChanges(changes []usnChange) []usnChange {
	if vol == nil || len(changes) == 0 {
		return changes
	}
	if len(vol.ownedDirFRNs) == 0 && !vol.hasDynamicOwnedDirs() {
		return changes
	}
	kept := changes[:0]
	for _, change := range changes {
		if vol.consumeOwnedReplayChange(change) {
			continue
		}
		kept = append(kept, change)
	}
	return kept
}

// consumeOwnedReplayChange reports whether a USN change belongs inside a
// directory that holds seekfs's own artifacts and must not be indexed.  It also
// maintains the dynamic owned-dir set: a directory created under an owned
// directory (the external builder's MkdirTemp scratch dirs) joins the set so
// its children are filtered, and a directory that is removed or moved out
// retires so a reused reference cannot hide real files.  A nil or empty owned
// set keeps this to a map lookup, so volumes without artifacts on them (any
// volume other than the one holding the seekfs dir) are unaffected.
func (vol *serviceVolumeIndex) consumeOwnedReplayChange(change usnChange) bool {
	if vol == nil || change.FRN == 0 {
		return false
	}
	if vol.ownedReplayParent(change.ParentFRN) {
		vol.noteOwnedChildDir(change)
		return true
	}
	if vol.hasDynamicOwnedDirs() {
		vol.retireOwnedDir(change.FRN)
	}
	return false
}

// ownedReplayParent reports whether parent is an owned artifact directory.
// The static set is built once at load and never mutated, so it is read
// without the lock; only the dynamic set needs synchronization.
func (vol *serviceVolumeIndex) ownedReplayParent(parent uint64) bool {
	if parent == 0 {
		return false
	}
	if _, ok := vol.ownedDirFRNs[parent]; ok {
		return true
	}
	vol.ownedDirMu.RLock()
	_, ok := vol.ownedDynamicDirFRNs[parent]
	vol.ownedDirMu.RUnlock()
	return ok
}

func (vol *serviceVolumeIndex) hasDynamicOwnedDirs() bool {
	vol.ownedDirMu.RLock()
	defer vol.ownedDirMu.RUnlock()
	return len(vol.ownedDynamicDirFRNs) > 0
}

// noteOwnedChildDir tracks a directory created or removed directly under an
// owned directory.  A create/rename-in adds its reference; a delete or
// rename-out retires it.  Non-directory changes and plain deletes of files are
// no-ops.
func (vol *serviceVolumeIndex) noteOwnedChildDir(change usnChange) {
	if change.Attr&fileAttributeDir == 0 {
		return
	}
	created := change.Reason&(usnReasonFileCreate|usnReasonRenameNew) != 0
	removed := change.Reason&usnReasonFileDelete != 0 ||
		change.Reason&usnReasonRenameOld != 0 && change.Reason&usnReasonRenameNew == 0
	if !created && !removed {
		return
	}
	vol.ownedDirMu.Lock()
	defer vol.ownedDirMu.Unlock()
	if created {
		if vol.ownedDynamicDirFRNs == nil {
			vol.ownedDynamicDirFRNs = make(map[uint64]struct{})
		}
		vol.ownedDynamicDirFRNs[change.FRN] = struct{}{}
		return
	}
	delete(vol.ownedDynamicDirFRNs, change.FRN)
}

// retireOwnedDir drops frn from the dynamic set.  Called for changes whose
// parent is not owned: a tracked directory that now lives outside the owned
// tree (or whose reference was reused) must stop filtering its children.
func (vol *serviceVolumeIndex) retireOwnedDir(frn uint64) {
	vol.ownedDirMu.Lock()
	delete(vol.ownedDynamicDirFRNs, frn)
	vol.ownedDirMu.Unlock()
}

func (vol *serviceVolumeIndex) recordOverlayChange(change usnChange, refreshInfo bool) {
	if vol == nil {
		return
	}
	if vol.overlay == nil {
		vol.overlay = newOverlaySegment()
	}
	overlay := vol.overlay
	if change.Reason&usnReasonRenameOld != 0 && change.Reason&usnReasonRenameNew == 0 {
		return
	}
	// Snapshot the file's current effective size and parent so the ancestor
	// folder-size deltas can subtract the old contribution and add the new one.
	beforeSize := vol.effectiveAccountingSize(change.FRN)
	oldParent := vol.effectiveParentFRN(change.FRN)
	if change.Reason&usnReasonFileDelete != 0 {
		rec := CompactRecord{FRN: change.FRN, ParentFRN: change.ParentFRN, Name: change.Name, Deleted: true}
		// Only directories can have descendants, so the O(overlay) cascade
		// below is skipped when the deleted FRN is PROVABLY a plain file:
		// some record for it exists (base or prior live overlay slot) and
		// no available record says directory. The overlay slot is fresher
		// than the base record (it shadows it), so a live overlay dir
		// create on a reused FRN must veto stale base file evidence. If
		// dir-ness cannot be determined at all, cascade conservatively:
		// correctness wins. This keeps mass file-delete churn (e.g. a
		// node_modules removal WITH per-child USN records) at O(deletes)
		// on the apply path instead of O(deletes x overlay).
		baseID, hasBase := vol.idForFRN(change.FRN)
		provenFile := hasBase && vol.index.compactRecord(baseID).Mode&uint32(os.ModeDir) == 0
		if slot, ok := overlay.byFRN[change.FRN]; ok && slot >= 0 && int(slot) < len(overlay.records) {
			prev := overlay.records[slot]
			if rec.Name == "" {
				rec.Name = prev.Name
			}
			if rec.ParentFRN == 0 {
				rec.ParentFRN = prev.ParentFRN
			}
			if !prev.Deleted {
				if prev.Mode&uint32(os.ModeDir) != 0 {
					provenFile = false
				} else if !hasBase {
					provenFile = true
				}
			}
		}
		slot := int32(len(overlay.records))
		overlay.byFRN[change.FRN] = slot
		overlay.records = append(overlay.records, rec)
		if hasBase {
			vol.tombstoneBaseSubtree(baseID)
		}
		if !provenFile {
			vol.cascadeOverlayDelete(change.FRN)
		}
		overlay.watermark.Store(int32(len(overlay.records)))
		if beforeSize != 0 {
			if parent := firstNonZeroFRN(rec.ParentFRN, oldParent); parent != 0 {
				vol.addAncestorDirDelta(parent, -beforeSize)
			}
		}
		return
	}
	rec := CompactRecord{
		FRN:       change.FRN,
		ParentFRN: change.ParentFRN,
		Parent:    -1,
		Name:      change.Name,
		Mode:      modeFromAttrs(change.Attr),
	}
	vol.index.CompactAttrs = true
	if baseID, ok := vol.idForFRN(change.FRN); ok {
		overlay.shadowed.add(int32(baseID))
		base := vol.index.compactRecord(baseID)
		if rec.Name == "" {
			rec.Name = base.Name
		}
		if rec.ParentFRN == 0 {
			rec.ParentFRN = base.ParentFRN
		}
		// A USN record carries neither size nor timestamp.  Preserve the base
		// values so a rename or modify does not report a zero size; the live
		// refresh below replaces them when the change affects file data.
		rec.Size = base.Size
		rec.ModUnix = base.ModUnix
	}
	slot := int32(len(overlay.records))
	overlay.byFRN[change.FRN] = slot
	overlay.records = append(overlay.records, rec)
	overlay.watermark.Store(int32(len(overlay.records)))
	if refreshInfo {
		vol.refreshOverlayFileInfo(int(slot))
	}
	afterSize := vol.effectiveAccountingSize(change.FRN)
	afterParent := overlay.records[slot].ParentFRN
	if beforeSize != 0 && oldParent != 0 {
		vol.addAncestorDirDelta(oldParent, -beforeSize)
	}
	if afterSize != 0 {
		if parent := firstNonZeroFRN(afterParent, oldParent); parent != 0 {
			vol.addAncestorDirDelta(parent, afterSize)
		}
	}
}

// effectiveAccountingSize returns the size a record currently contributes to
// its ancestors' recursive totals: its own size for a file, the recursive
// subtree total for a directory, and 0 when it is deleted or unknown.  A live
// overlay slot wins over the base record.
func (vol *serviceVolumeIndex) effectiveAccountingSize(frn uint64) int64 {
	if vol == nil || frn == 0 {
		return 0
	}
	if vol.overlay != nil {
		if slot, ok := vol.overlay.byFRN[frn]; ok && slot >= 0 && int(slot) < len(vol.overlay.records) {
			rec := vol.overlay.records[slot]
			if rec.Deleted {
				return 0
			}
			if rec.Mode&uint32(os.ModeDir) != 0 {
				if id, ok := vol.idForFRN(frn); ok {
					return vol.baseDirRecursiveSize(id)
				}
				return 0
			}
			return rec.Size
		}
	}
	if id, ok := vol.idForFRN(frn); ok {
		rec := vol.index.compactRecord(id)
		if rec.Mode&uint32(os.ModeDir) != 0 {
			return vol.baseDirRecursiveSize(id)
		}
		return rec.Size
	}
	return 0
}

func (vol *serviceVolumeIndex) effectiveParentFRN(frn uint64) uint64 {
	if vol == nil || frn == 0 {
		return 0
	}
	if vol.overlay != nil {
		if slot, ok := vol.overlay.byFRN[frn]; ok && slot >= 0 && int(slot) < len(vol.overlay.records) {
			return vol.overlay.records[slot].ParentFRN
		}
	}
	if id, ok := vol.idForFRN(frn); ok {
		return vol.index.compactRecord(id).ParentFRN
	}
	return 0
}

func (vol *serviceVolumeIndex) baseDirRecursiveSize(id int) int64 {
	if id < 0 || id >= len(vol.subtreeBytes) {
		return 0
	}
	size := int64(vol.subtreeBytes[id])
	if vol.dirSizeDelta != nil {
		size += vol.dirSizeDelta[id]
	}
	if size < 0 {
		return 0
	}
	return size
}

// addAncestorDirDelta adds delta to every base-directory ancestor of dirFRN.
// Overlay-created directories are traversed but not recorded: they are not
// base-index records and have no persisted size of their own.
func (vol *serviceVolumeIndex) addAncestorDirDelta(dirFRN uint64, delta int64) {
	if vol == nil || delta == 0 || dirFRN == 0 {
		return
	}
	for depth := 0; depth < 4096 && dirFRN != 0; depth++ {
		if vol.overlay != nil {
			if slot, ok := vol.overlay.byFRN[dirFRN]; ok && slot >= 0 && int(slot) < len(vol.overlay.records) {
				dirFRN = vol.overlay.records[slot].ParentFRN
				continue
			}
		}
		id, ok := vol.idForFRN(dirFRN)
		if !ok {
			return
		}
		if vol.dirSizeDelta == nil {
			vol.dirSizeDelta = make(map[int]int64)
		}
		vol.dirSizeDelta[id] += delta
		next := vol.index.compactRecord(id).ParentFRN
		if next == dirFRN {
			return
		}
		dirFRN = next
	}
}

func firstNonZeroFRN(values ...uint64) uint64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

// publishDirSizeDelta stores an immutable copy of the working delta map so
// lock-free readers see a stable view.
func (vol *serviceVolumeIndex) publishDirSizeDelta() {
	if vol == nil || vol.index == nil {
		return
	}
	if len(vol.dirSizeDelta) == 0 {
		vol.index.dirSizeDelta.Store(nil)
		return
	}
	snapshot := make(map[int]int64, len(vol.dirSizeDelta))
	for id, delta := range vol.dirSizeDelta {
		snapshot[id] = delta
	}
	vol.index.dirSizeDelta.Store(&snapshot)
}

// refreshOverlayFileInfo re-reads a just-recorded file's size and modification
// time from the live filesystem.  The USN journal reports changes but carries
// no metadata, so without this the overlay (and any base it is later folded
// into) reports a zero size for every created or modified file.  This mirrors
// Everything, which re-reads file metadata when the journal reports a change.
func (vol *serviceVolumeIndex) refreshOverlayFileInfo(slot int) {
	if vol == nil || vol.overlay == nil || vol.volume == "" {
		return
	}
	if slot < 0 || slot >= len(vol.overlay.records) {
		return
	}
	path := vol.overlayRecordPath(vol.overlay.records, vol.overlay.byFRN, slot, map[int32]struct{}{}, make(map[int]string))
	if path == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	vol.overlay.records[slot].Size = info.Size()
	vol.overlay.records[slot].ModUnix = info.ModTime().UnixNano()
}

func (vol *serviceVolumeIndex) tombstoneBaseSubtree(rootID int) {
	if vol == nil || vol.overlay == nil || vol.index == nil || rootID < 0 || rootID >= vol.index.compactRecordCount() {
		return
	}
	if rootID < len(vol.subtreeStart) && rootID < len(vol.subtreeEnd) && len(vol.subtreeOrder) > 0 {
		start, end := vol.subtreeStart[rootID], vol.subtreeEnd[rootID]
		if start != ^uint32(0) && start <= end && int(end) <= len(vol.subtreeOrder) {
			for _, id32 := range vol.subtreeOrder[start:end] {
				vol.overlay.tombstone.add(int32(id32))
			}
			return
		}
	}
	stack := []int{rootID}
	for len(stack) > 0 {
		last := len(stack) - 1
		id := stack[last]
		stack = stack[:last]
		if id < 0 || id >= vol.index.compactRecordCount() || vol.overlay.tombstone.contains(int32(id)) {
			continue
		}
		vol.overlay.tombstone.add(int32(id))
		for _, childID := range vol.childIDsForRecord(id) {
			stack = append(stack, int(childID))
		}
	}
}

// cascadeOverlayDelete handles the case a plain base-subtree tombstone
// cannot: overlay-only descendants (created purely via USN, never
// persisted) whose parent chain passes through deletedFRN or through a
// base id that tombstoneBaseSubtree just marked. Those descendants have
// no overlay slot of their own that got tombstoned by the delete branch
// above (only deletedFRN's own slot did), and overlayRecordPath falls
// back to the base index for any ancestor FRN without a live overlay
// slot Ã¢â‚¬â€ which does not consult vol.overlay.tombstone. So without this
// cascade an overlay-only child parented (directly or transitively)
// under a deleted base directory stays visible forever.
//
// This runs at apply time, once per delete, and is append-only: for
// every live overlay slot whose ancestor chain is doomed, it appends a
// new Deleted record for that FRN (mirroring the manual delete branch),
// never mutating overlay.records in place. Cost is O(overlay slots x
// chain depth) with per-FRN memoization, which is acceptable because the
// overlay is bounded (~64k slots before compaction) and directory
// deletes are rare; this trades a bounded amount of apply-time work for
// avoiding any read-path (per-query) traversal, which is the failure
// mode reviews F1/G1 flagged.
func (vol *serviceVolumeIndex) cascadeOverlayDelete(deletedFRN uint64) {
	if vol == nil || vol.overlay == nil || vol.index == nil {
		return
	}
	overlay := vol.overlay
	if len(overlay.records) == 0 {
		return
	}
	latest := latestOverlaySlotsByFRN(overlay.records)

	// doomed memoizes, per FRN visited during this cascade, whether that
	// FRN's own identity (i.e. treating it as an ancestor) is dead: either
	// it IS deletedFRN, or it resolves (via live overlay slot, else base
	// id) to a base id in vol.overlay.tombstone, or its own parent chain
	// is doomed.
	doomed := make(map[uint64]bool, 8)
	var resolve func(frn uint64, seen map[uint64]struct{}) bool
	resolve = func(frn uint64, seen map[uint64]struct{}) bool {
		if frn == 0 {
			return false
		}
		if frn == deletedFRN {
			return true
		}
		if v, ok := doomed[frn]; ok {
			return v
		}
		if _, cyc := seen[frn]; cyc {
			// Cycle in parent chain (shouldn't happen); treat as not doomed
			// rather than infinite-looping.
			return false
		}
		seen[frn] = struct{}{}

		result := false
		if slot, ok := latest[frn]; ok && slot >= 0 && int(slot) < len(overlay.records) {
			rec := overlay.records[slot]
			if rec.Deleted {
				result = true
			} else if rec.ParentFRN != 0 && rec.ParentFRN != frn {
				result = resolve(rec.ParentFRN, seen)
			}
		} else if baseID, ok := vol.idForFRN(frn); ok {
			if overlay.tombstone.contains(int32(baseID)) {
				result = true
			} else if baseID >= 0 && baseID < vol.index.compactRecordCount() {
				rec := vol.index.compactRecord(baseID)
				if rec.ParentFRN != 0 && rec.ParentFRN != frn {
					result = resolve(rec.ParentFRN, seen)
				}
			}
		}
		doomed[frn] = result
		return result
	}

	type victim struct {
		frn  uint64
		name string
	}
	var victims []victim
	for frn, slot := range latest {
		if frn == 0 || frn == deletedFRN {
			continue
		}
		rec := overlay.records[slot]
		if rec.Deleted {
			continue
		}
		if rec.ParentFRN == 0 || rec.ParentFRN == frn {
			continue
		}
		if resolve(rec.ParentFRN, map[uint64]struct{}{frn: {}}) {
			victims = append(victims, victim{frn: frn, name: rec.Name})
		}
	}
	if len(victims) == 0 {
		return
	}
	// Deterministic append order so replay from WAL is reproducible.
	sort.Slice(victims, func(i, j int) bool { return victims[i].frn < victims[j].frn })
	for _, v := range victims {
		slot := latest[v.frn]
		prev := overlay.records[slot]
		newSlot := int32(len(overlay.records))
		overlay.byFRN[v.frn] = newSlot
		overlay.records = append(overlay.records, CompactRecord{
			FRN:       v.frn,
			ParentFRN: prev.ParentFRN,
			Name:      v.name,
			Deleted:   true,
		})
	}
}

func (vol *serviceVolumeIndex) replayWAL() error {
	return vol.replayWALWithLimit(serviceStartupWALRebuildBytes)
}
