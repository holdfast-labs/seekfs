package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"seekfs/feature"
)

type featureVolumeView struct {
	meta      feature.Volume
	view      *serviceVolumeIndex
	gen       uint64
	baseCount int
	records   []CompactRecord
	latest    map[uint64]int32
	hidden    hiddenBaseIDs
	prepared  bool
	excludes  []string
}

func (s *goSearchService) captureFeatureVolume(vol *serviceVolumeIndex) (*featureVolumeView, error) {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	vol.mu.Lock()
	defer vol.mu.Unlock()
	if !s.featureVolumeLoaded(vol) {
		return nil, fmt.Errorf("feature volume is no longer loaded")
	}
	if vol.featureFeed == nil || vol.index == nil || vol.index.Source != "usn" || !vol.index.Compact || vol.state != "ready" || vol.journalID == 0 || vol.checkpoint <= 0 {
		return nil, fmt.Errorf("feature volume %s unavailable (requires a ready NTFS index with a journal checkpoint)", vol.volume)
	}
	f := vol.featureFeed
	f.mu.Lock()
	defer f.mu.Unlock()
	v := &featureVolumeView{meta: feature.Volume{ID: vol.volume, JournalID: vol.journalID, Generation: f.epoch, Checkpoint: vol.checkpoint, Cursor: f.seq}, view: snapshotServiceVolumeForSearch(vol), gen: vol.snapshotGen.Load(), baseCount: vol.index.compactRecordCount()}
	key, err := filepath.Abs(vol.dbPath)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(key))))
	v.meta.IndexID = hex.EncodeToString(hash[:])
	for _, root := range []string{defaultSeekFSDir(), s.contentCfg.SeekFSDir} {
		if root != "" {
			if absolute, err := filepath.Abs(root); err == nil {
				v.excludes = append(v.excludes, absolute)
			}
		}
	}
	if snap := vol.snap.Load(); snap != nil {
		v.records = snap.records[:min(max(0, int(snap.watermark)), len(snap.records))]
	}
	return v, nil
}

func (s *goSearchService) featureVolumeLoaded(vol *serviceVolumeIndex) bool {
	for _, loaded := range s.volumes {
		if loaded == vol {
			return true
		}
	}
	return false
}

func (v *featureVolumeView) excluded(path string) bool {
	for _, root := range v.excludes {
		if pathUnder(path, root) {
			return true
		}
	}
	return false
}

// Copy the overlay only when a sync/query needs records, never on an idle tick.
// Names in overlay records are heap-owned. Base names are copied per IPC page.
func (s *goSearchService) prepareFeatureView(vol *serviceVolumeIndex, v *featureVolumeView) error {
	if v.prepared {
		return nil
	}
	s.indexMu.RLock()
	vol.mu.Lock()
	if !s.featureVolumeCurrent(vol, v) {
		vol.mu.Unlock()
		s.indexMu.RUnlock()
		return fmt.Errorf("feature view changed; retry required")
	}
	if len(v.records) > feature.MaxQueryMatches {
		vol.mu.Unlock()
		s.indexMu.RUnlock()
		return fmt.Errorf("feature overlay exceeds snapshot memory budget; wait for filename compaction")
	}
	v.records = append([]CompactRecord(nil), v.records...)
	if snap := v.view.snap.Load(); snap != nil {
		private := *snap
		private.records = v.records
		v.view.snap.Store(&private)
	}
	vol.mu.Unlock()
	s.indexMu.RUnlock()
	v.latest = latestOverlaySlotsByFRN(v.records)
	v.hidden = v.view.snapshotHiddenBaseIDs()
	v.prepared = true
	return nil
}

func (s *goSearchService) featureVolumeCurrent(vol *serviceVolumeIndex, v *featureVolumeView) bool {
	// Caller holds indexMu and vol.mu. snapshotGen covers file metadata and
	// overlay changes; feed epoch also invalidates same-journal base swaps.
	if !s.featureVolumeLoaded(vol) || vol.index != v.view.index || vol.snapshotGen.Load() != v.gen || vol.featureFeed == nil || vol.checkpoint != v.meta.Checkpoint || vol.state != "ready" {
		return false
	}
	vol.featureFeed.mu.Lock()
	defer vol.featureFeed.mu.Unlock()
	return vol.featureFeed.epoch == v.meta.Generation && vol.featureFeed.seq == v.meta.Cursor
}

func (s *goSearchService) featureSnapshotCurrent(vol *serviceVolumeIndex, v *featureVolumeView) bool {
	if !s.featureVolumeLoaded(vol) || vol.index != v.view.index || vol.journalID != v.meta.JournalID || vol.featureFeed == nil || vol.state != "ready" {
		return false
	}
	vol.featureFeed.mu.Lock()
	defer vol.featureFeed.mu.Unlock()
	return vol.featureFeed.epoch == v.meta.Generation
}

func featureRecord(entry Entry) feature.Record {
	size := entry.Size
	if entry.Mode&uint32(os.ModeDir) != 0 {
		size = 0
	} // Directory aggregate sizes are not snapshot-stable file metadata.
	return feature.Record{FRN: entry.FRN, Path: strings.Clone(entry.Path), Name: strings.Clone(entry.Name), Size: size, ModUnix: entry.ModUnix, Mode: entry.Mode}
}

func (v *featureVolumeView) entryForFRN(frn uint64, cache map[int]string) (Entry, bool) {
	if slot, ok := v.latest[frn]; ok {
		rec := v.records[slot]
		if rec.Deleted {
			return Entry{}, false
		}
		path := v.currentPath(frn, cache)
		if path == "" || v.excluded(path) {
			return Entry{}, false
		}
		return Entry{FRN: frn, Path: path, Name: rec.Name, Size: rec.Size, Mode: rec.Mode, ModUnix: rec.ModUnix, LowerName: strings.ToLower(rec.Name), LowerPath: strings.ToLower(path), IndexSource: v.view.index.Source}, true
	}
	if id, ok := v.view.recordIDForFRN(frn); ok && !v.hidden.contains(id) {
		rec := v.view.index.compactRecord(id)
		if !rec.Deleted {
			path := v.currentPath(frn, cache)
			if path == "" || v.excluded(path) {
				return Entry{}, false
			}
			entry := compactEntryFromRecord(v.view.index, id, rec, cache, true)
			entry.Path, entry.LowerPath = path, strings.ToLower(path)
			return entry, true
		}
	}
	return Entry{}, false
}

// Resolve the current parent chain, including unmodified base children beneath
// renamed overlay directories. Negative cache keys identify overlay slots.
func (v *featureVolumeView) currentPath(frn uint64, cache map[int]string) string {
	if len(v.latest) == 0 {
		if id, ok := v.view.recordIDForFRN(frn); ok {
			return v.view.index.reconstructCompactPathCached(id, cache)
		}
		return ""
	}
	type component struct {
		key  int
		name string
	}
	var components []component
	seen := make(map[uint64]struct{}, 8)
	path := v.meta.ID + `\`
	for depth := 0; frn != 0; depth++ {
		if depth >= 1024 {
			return ""
		} // Same parent-chain bound as filename search.
		if _, ok := seen[frn]; ok {
			return ""
		}
		seen[frn] = struct{}{}
		var rec CompactRecord
		var key int
		if slot, ok := v.latest[frn]; ok {
			rec, key = v.records[slot], -int(slot)-1
		} else if id, ok := v.view.recordIDForFRN(frn); ok {
			rec, key = v.view.index.compactRecord(id), id
		} else {
			break
		}
		if rec.Deleted {
			return ""
		}
		if cached, ok := cache[key]; ok {
			path = cached
			break
		}
		components = append(components, component{key, rec.Name})
		if rec.ParentFRN == frn {
			break
		}
		frn = rec.ParentFRN
	}
	for i := len(components) - 1; i >= 0; i-- {
		c := components[i]
		if c.name != "" && c.name != "." {
			path = joinOverlayPath(path, c.name)
		}
		cache[c.key] = path
	}
	return path
}

// Each page holds the filename mmap lock only while copying bounded metadata.
// A changing snapshot is discarded rather than committing a torn corpus.
func (s *goSearchService) featureSnapshotPage(vol *serviceVolumeIndex, v *featureVolumeView, start int) ([]feature.Record, int, error) {
	if err := s.prepareFeatureView(vol, v); err != nil {
		return nil, start, err
	}
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	vol.mu.Lock()
	defer vol.mu.Unlock()
	if !s.featureSnapshotCurrent(vol, v) {
		return nil, start, fmt.Errorf("feature snapshot changed; retry required")
	}
	end := min(start+feature.PageSize, v.baseCount+len(v.records))
	cache := make(map[int]string, feature.PageSize)
	out := make([]feature.Record, 0, end-start)
	for id := start; id < end; id++ {
		var entry Entry
		var ok bool
		if id < v.baseCount {
			if v.hidden.contains(id) {
				continue
			}
			rec := v.view.index.compactRecord(id)
			if rec.Deleted || rec.FRN == 0 {
				continue
			}
			entry, ok = v.entryForFRN(rec.FRN, cache)
		} else {
			slot := id - v.baseCount
			if latest, found := v.latest[v.records[slot].FRN]; !found || int(latest) != slot {
				continue
			}
			entry, ok = v.entryForFRN(v.records[slot].FRN, cache)
		}
		if ok && entry.FRN != 0 {
			out = append(out, featureRecord(entry))
		}
	}
	return out, end, nil
}

func (s *goSearchService) featureChanges(vol *serviceVolumeIndex, v *featureVolumeView, previous feature.Volume) ([]feature.Record, bool, error) {
	if err := s.prepareFeatureView(vol, v); err != nil {
		return nil, false, err
	}
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	vol.mu.Lock()
	defer vol.mu.Unlock()
	if !s.featureVolumeCurrent(vol, v) {
		return nil, false, fmt.Errorf("feature changes moved; retry required")
	}
	feed := vol.featureFeed
	feed.mu.Lock()
	defer feed.mu.Unlock()
	if previous.Generation != v.meta.Generation || previous.JournalID != v.meta.JournalID || previous.Cursor > feed.seq || feed.seq-previous.Cursor > uint64(len(feed.changes)) {
		return nil, false, nil
	}
	changes := feed.changes[len(feed.changes)-int(feed.seq-previous.Cursor):]
	seen := make(map[uint64]struct{}, len(changes))
	cache := make(map[int]string, featureChangeWindow)
	out := make([]feature.Record, 0, len(changes))
	for _, ch := range changes {
		if _, ok := seen[ch.FRN]; ok {
			continue
		}
		seen[ch.FRN] = struct{}{}
		entry, ok := v.entryForFRN(ch.FRN, cache)
		if ok {
			out = append(out, featureRecord(entry))
		} else {
			out = append(out, feature.Record{FRN: ch.FRN, Deleted: true})
		}
		cache = boundContentPathCache(cache)
	}
	return out, true, nil
}

func (f *companionFeature) syncVolume(ctx context.Context, vol *serviceVolumeIndex) (*featureVolumeView, error) {
	v, err := f.service.captureFeatureVolume(vol)
	if err != nil {
		return nil, err
	}
	previous, has := f.cursors[vol]
	if has && (previous.ID != v.meta.ID || previous.IndexID != v.meta.IndexID) {
		if _, err := f.call(ctx, feature.Request{Op: "volume_remove", Volume: &previous}); err != nil {
			return nil, err
		}
		delete(f.cursors, vol)
		has = false
	}
	if has && previous == v.meta {
		return v, nil
	}
	var records []feature.Record
	incremental := false
	if has {
		records, incremental, err = f.service.featureChanges(vol, v, previous)
		if err != nil {
			return nil, err
		}
	}
	prefix := "snapshot"
	if incremental {
		prefix = "changes"
	}
	begin := feature.Request{Op: prefix + "_begin", Volume: &v.meta}
	if incremental {
		begin.Previous = &previous
	}
	if _, err = f.call(ctx, begin); err != nil {
		return nil, err
	}
	if incremental {
		if err = f.sendRecords(ctx, &v.meta, records); err != nil {
			return nil, err
		}
	} else {
		for start := 0; start < v.baseCount+len(v.records); {
			var next int
			records, next, err = f.service.featureSnapshotPage(vol, v, start)
			if err != nil {
				return nil, err
			}
			if err = f.sendRecords(ctx, &v.meta, records); err != nil {
				return nil, err
			}
			start = next
		}
	}
	f.service.indexMu.RLock()
	vol.mu.Lock()
	current := f.service.featureSnapshotCurrent(vol, v)
	vol.mu.Unlock()
	f.service.indexMu.RUnlock()
	if !current {
		return nil, fmt.Errorf("feature sync changed before commit; retry required")
	}
	if _, err = f.call(ctx, feature.Request{Op: prefix + "_commit", Volume: &v.meta}); err != nil {
		return nil, err
	}
	f.cursors[vol] = v.meta
	f.failures = 0
	f.setHealth("ready", nil)
	return v, nil
}

func (f *companionFeature) query(ctx context.Context, vol *serviceVolumeIndex, term string, caseSensitive bool) (map[uint64]struct{}, *featureVolumeView, error) {
	if err := f.lock(ctx); err != nil {
		return nil, nil, err
	}
	defer func() { <-f.gate }()
	if err := f.start(ctx); err != nil {
		return nil, nil, err
	}
	if !f.supportsQuery {
		return nil, nil, fmt.Errorf("feature %s does not advertise query capability", f.name)
	}
	v, err := f.syncVolume(ctx, vol)
	if err != nil {
		return nil, nil, err
	}
	resp, err := f.call(ctx, feature.Request{Op: "query", Volume: &v.meta, Query: term, CaseSensitive: caseSensitive, Limit: feature.MaxQueryMatches})
	if err != nil {
		return nil, nil, err
	}
	if !resp.Complete {
		err := fmt.Errorf("feature %s query is incomplete; narrow the query or wait for indexing: %s", f.name, resp.Message)
		f.setHealth("incomplete", err)
		return nil, nil, err
	}
	set := make(map[uint64]struct{}, len(resp.FRNs))
	for _, frn := range resp.FRNs {
		if frn == 0 {
			err := fmt.Errorf("feature %s returned invalid FRN zero", f.name)
			f.failed(err)
			return nil, nil, err
		}
		set[frn] = struct{}{}
	}
	return set, v, nil
}
