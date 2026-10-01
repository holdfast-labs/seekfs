package main

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

func (s *goSearchService) runStandalone() error {
	if s.processMode == "" {
		s.processMode = "standalone"
	}
	applyServiceRuntimeMemoryTuning()
	fmt.Fprintf(os.Stderr, "seekfs privileged service listening on %s\n", s.pipeName)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				serviceLog("startup index load panic: %v\n%s", r, string(debug.Stack()))
			}
		}()
		if err := s.loadConfiguredIndexes(); err != nil {
			serviceLog("startup index load error: %v", err)
		}
	}()
	s.servePrivileged()
	return nil
}

func (s *goSearchService) loadConfiguredIndexes() error {
	s.initializeFeatures()
	s.indexMu.Lock()
	s.loading = true
	s.loadErr = ""
	s.indexMu.Unlock()
	defer func() {
		s.indexMu.Lock()
		s.loading = false
		s.indexMu.Unlock()
	}()
	if len(s.dbs) == 0 {
		serviceLog("service started without search databases")
		return nil
	}
	start := time.Now()
	indexes, volumes, total, err := loadConfiguredVolumes(s.dbs)
	if err != nil {
		s.indexMu.Lock()
		s.loadErr = err.Error()
		s.indexMu.Unlock()
		return err
	}
	s.indexMu.Lock()
	for _, vol := range volumes {
		s.prepareFeatureVolume(vol)
	}
	s.indexes = indexes
	s.volumes = volumes
	s.loadErr = ""
	s.indexMu.Unlock()
	debug.FreeOSMemory()
	s.sweepStaleIndexTempFiles()
	s.startBackgroundNameOrderBuilds(volumes)
	s.startBackgroundNameTrigramBuilds(volumes)
	for _, vol := range volumes {
		if vol.state == "ready" && vol.index.Compact && vol.index.Source == "usn" {
			go s.replayVolumeLoop(vol)
			go s.persistVolumeLoop(vol)
		} else if vol.state == "ready" && vol.index.Source == "walk" {
			s.startWalkWatchers(vol)
		} else if vol.state == "stale" && vol.index != nil && vol.index.Compact && vol.index.Source == "usn" {
			// A volume that started stale (failed startup rebuild, journal
			// anomaly, transient open failure) must not stay broken until
			// restart: keep retrying the rebuild in the background with
			// backoff until it recovers.
			go s.staleRecoveryLoop(vol)
		}
	}
	for _, vol := range volumes {
		s.featuresVolumeReady(vol)
	}
	go s.replayStallWatchdog()
	serviceLog("loaded %d dbs entries=%d elapsed=%s", len(indexes), total, time.Since(start).Round(time.Millisecond))
	return nil
}

type startupVolumeResult struct {
	idx *Index
	vol *serviceVolumeIndex
	err error
}

func loadConfiguredVolumes(dbs []string) ([]*Index, []*serviceVolumeIndex, int, error) {
	results := make([]startupVolumeResult, len(dbs))
	workers := serviceStartupWorkerCount(len(dbs))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, dbPath := range dbs {
		i, dbPath := i, dbPath
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			idx, vol, err := loadConfiguredVolume(dbPath)
			results[i] = startupVolumeResult{idx: idx, vol: vol, err: err}
		}()
	}
	wg.Wait()
	indexes := make([]*Index, 0, len(results))
	volumes := make([]*serviceVolumeIndex, 0, len(results))
	total := 0
	for _, result := range results {
		if result.err != nil {
			return nil, nil, 0, result.err
		}
		indexes = append(indexes, result.idx)
		volumes = append(volumes, result.vol)
		total += result.idx.entryCount()
	}
	return indexes, volumes, total, nil
}

func serviceStartupWorkerCount(dbCount int) int {
	if dbCount <= 1 {
		return 1
	}
	raw := strings.TrimSpace(os.Getenv("SEEKFS_STARTUP_WORKERS"))
	if raw != "" {
		n, err := strconv.Atoi(raw)
		if err == nil && n > 0 {
			return min(n, dbCount)
		}
	}
	return min(serviceStartupDefaultWorkers, dbCount)
}

func loadConfiguredVolume(dbPath string) (*Index, *serviceVolumeIndex, error) {
	idx, err := loadIndexForService(dbPath)
	if err != nil {
		return nil, nil, err
	}
	idx.DBPath = dbPath
	vol := newServiceVolumeIndex(dbPath, idx)
	if err := vol.replayWAL(); err != nil {
		serviceLog("startup wal replay skipped volume=%s db=%s err=%v", vol.volume, vol.dbPath, err)
		vol.state = "stale"
		vol.staleReason = err.Error()
		if shouldRebuildStaleIndex(err) {
			if rebuilt, rebuildErr := rebuildServiceVolumeIndex(vol); rebuildErr == nil {
				serviceLog("startup wal rebuild complete volume=%s db=%s entries=%d", rebuilt.volume, rebuilt.dbPath, rebuilt.index.entryCount())
				vol = rebuilt
				idx = rebuilt.index
			} else {
				serviceLog("startup wal rebuild failed volume=%s db=%s err=%v", vol.volume, vol.dbPath, rebuildErr)
			}
		}
	}
	if err := catchUpServiceVolume(vol); err != nil {
		serviceLog("startup catch-up skipped volume=%s db=%s err=%v", vol.volume, vol.dbPath, err)
		if shouldRebuildStaleIndex(err) {
			if rebuilt, rebuildErr := rebuildServiceVolumeIndex(vol); rebuildErr == nil {
				serviceLog("startup stale rebuild complete volume=%s db=%s entries=%d", rebuilt.volume, rebuilt.dbPath, rebuilt.index.entryCount())
				vol = rebuilt
				idx = rebuilt.index
			} else {
				serviceLog("startup stale rebuild failed volume=%s db=%s err=%v", vol.volume, vol.dbPath, rebuildErr)
			}
		}
	}
	if serviceLowMemoryMode() && idx.Compact && idx.MMapRecords == nil {
		if mmapIdx, mmapErr := loadIndexMMap(dbPath); mmapErr == nil {
			idx = mmapIdx
			vol = newServiceVolumeIndex(dbPath, idx)
		} else {
			serviceLog("lowmem mmap load fallback volume=%s db=%s err=%v", vol.volume, dbPath, mmapErr)
		}
		debug.FreeOSMemory()
	}
	vol.queryIndex = buildResidentQueryIndex(vol)
	vol.resetNameOrderBuild()
	vol.resetNameTrigrams()
	if vol.needsCompactChildrenBuild() {
		vol.buildCompactChildren()
	}
	return idx, vol, nil
}

func loadIndexForService(dbPath string) (*Index, error) {
	if !serviceLowMemoryMode() {
		idx, err := loadIndex(dbPath)
		if err != nil {
			return nil, err
		}
		ensureCompactIndexForService(idx)
		return idx, nil
	}
	idx, err := loadIndexMMap(dbPath)
	if err == nil {
		ensureCompactIndexForService(idx)
		return idx, nil
	}
	serviceLog("lowmem mmap initial load fallback db=%s err=%v", dbPath, err)
	idx, err = loadIndex(dbPath)
	if err != nil {
		return nil, err
	}
	ensureCompactIndexForService(idx)
	return idx, nil
}

func depthOfPath(path string) int {
	depth := 0
	for _, c := range path {
		if c == '\\' || c == '/' {
			depth++
		}
	}
	return depth
}

func ensureCompactIndexForService(idx *Index) {
	if idx == nil || idx.Compact || len(idx.Entries) == 0 {
		return
	}
	if len(idx.Roots) == 0 {
		shallow := idx.Entries[0]
		for _, entry := range idx.Entries {
			if depthOfPath(entry.Path) < depthOfPath(shallow.Path) {
				shallow = entry
			}
		}
		if p := filepath.Dir(shallow.Path); p != "" && p != "." {
			idx.Roots = []string{p}
		} else {
			idx.Roots = []string{shallow.Path}
		}
	}
	records := make([]CompactRecord, 0, len(idx.Entries))
	idByPath := make(map[string]int32, len(idx.Entries))
	if idx.Volume == "" {
		for _, entry := range idx.Entries {
			if vol := filepath.VolumeName(entry.Path); vol != "" {
				idx.Volume = vol
				break
			}
		}
	}
	for i, entry := range idx.Entries {
		path := filepath.Clean(entry.Path)
		name := entry.Name
		if name == "" {
			name = filepath.Base(path)
		}
		if name == "." || name == string(filepath.Separator) || name == "" {
			name = filepath.VolumeName(path)
			if name == "" {
				name = "."
			}
		}
		rec := CompactRecord{
			FRN:       uint64(i + 1),
			ParentFRN: uint64(i + 1),
			Parent:    -1,
			Name:      name,
			Mode:      entry.Mode,
			Size:      entry.Size,
			ModUnix:   entry.ModUnix,
		}
		records = append(records, rec)
		idByPath[strings.ToLower(path)] = int32(i)
	}
	for i, entry := range idx.Entries {
		path := filepath.Clean(entry.Path)
		parentPath := filepath.Dir(path)
		if parentPath == path || parentPath == "." {
			continue
		}
		parent, ok := idByPath[strings.ToLower(parentPath)]
		if !ok {
			continue
		}
		records[i].Parent = parent
		records[i].ParentFRN = records[parent].FRN
	}
	idx.Records = records
	idx.Entries = nil
	idx.NameOrder = nil
	idx.PathOrder = nil
	idx.Compact = true
	buildOrders(idx)
	if serviceLowMemoryMode() {
		idx.packCompactRecords(true)
	}
}

func newServiceVolumeIndex(dbPath string, idx *Index) *serviceVolumeIndex {
	vol := &serviceVolumeIndex{
		dbPath:         dbPath,
		index:          idx,
		volume:         idx.Volume,
		journalID:      idx.JournalID,
		checkpoint:     idx.Checkpoint,
		baseCheckpoint: idx.Checkpoint,
		state:          "ready",
		pathCache:      make(map[int]string),
		lastPersist:    time.Now(),
	}
	if contentSearchEnabled() {
		vol.content = newContentVolumeState(idx.Volume)
		vol.contentCoord = newContentCoordinator(vol.content)
	}
	if idx.Compact && idx.Source == "usn" {
		vol.ownedDirFRNs = ownedReplayDirFRNs(idx.Volume)
		recordCount := idx.compactRecordCount()
		largeResident := recordCount >= 1_000_000 || serviceLowMemoryMode()
		if !largeResident {
			ensureCompactNameOrderSorted(idx)
		} else if idx.MMapRecords == nil {
			idx.CompactNameOrder = nil
			idx.packCompactRecords(true)
		} else {
			idx.CompactNameOrder = nil
		}
		recordCount = idx.compactRecordCount()
		if !serviceLowMemoryMode() {
			vol.frns = make([]uint64, 0, recordCount)
			vol.frnRecordIDs = make([]uint32, 0, recordCount)
		}
		if !largeResident {
			vol.children = make(map[uint64]map[int]struct{}, recordCount)
			vol.exactNames = make(map[string][]int, recordCount/2)
		}
		if vol.frns != nil || vol.children != nil || vol.exactNames != nil {
			for i := 0; i < recordCount; i++ {
				rec := idx.compactRecord(i)
				if vol.frns != nil && rec.FRN != 0 {
					vol.frns = append(vol.frns, rec.FRN)
					vol.frnRecordIDs = append(vol.frnRecordIDs, uint32(i))
				}
				if vol.children != nil && rec.ParentFRN != 0 && rec.ParentFRN != rec.FRN {
					vol.addChild(rec.ParentFRN, i)
				}
				if name := idx.compactLowerNameAt(i); vol.exactNames != nil && !rec.Deleted && name != "" {
					vol.exactNames[name] = append(vol.exactNames[name], i)
				}
			}
		}
		if vol.frns != nil {
			sortFRNIndexEntries(vol.frns, vol.frnRecordIDs)
		}
		vol.queryIndex = buildResidentQueryIndex(vol)
		vol.applyDerivedSections()
		vol.resetNameOrderBuild()
		vol.resetNameTrigrams()
		if vol.needsCompactChildrenBuild() {
			vol.buildCompactChildren()
		}
	}
	vol.overlay = newOverlaySegment()
	vol.publishSnapshot()
	return vol
}

func newOverlaySegment() *overlaySegment {
	return &overlaySegment{
		byFRN: make(map[uint64]int32),
	}
}

func (set *overlayBaseIDSet) add(id int32) {
	if id < 0 {
		return
	}
	word := int(id) / 64
	bit := uint(id) % 64
	if word >= len(set.bits) {
		grown := make([]uint64, word+1)
		copy(grown, set.bits)
		set.bits = grown
	}
	mask := uint64(1) << bit
	if set.bits[word]&mask != 0 {
		return
	}
	set.bits[word] |= mask
	set.ids = append(set.ids, id)
	set.count++
}

func (set *overlayBaseIDSet) contains(id int32) bool {
	if id < 0 {
		return false
	}
	word := int(id) / 64
	if word >= len(set.bits) {
		return false
	}
	return set.bits[word]&(uint64(1)<<(uint(id)%64)) != 0
}

func (set *overlayBaseIDSet) len() int {
	if set == nil {
		return 0
	}
	return set.count
}

func (vol *serviceVolumeIndex) publishSnapshot() {
	if vol == nil {
		return
	}
	gen := vol.snapshotGen.Add(1)
	watermark := int32(0)
	var records []CompactRecord
	var tombstoneIDs []int32
	var shadowedIDs []int32
	if vol.overlay != nil {
		watermark = vol.overlay.watermark.Load()
		records = vol.overlay.records
		tombstoneIDs = sortedInt32Snapshot(vol.overlay.tombstone.ids)
		shadowedIDs = sortedInt32Snapshot(vol.overlay.shadowed.ids)
	}
	vol.snap.Store(&volumeSnapshot{base: vol.index, records: records, tombstoneIDs: tombstoneIDs, shadowedIDs: shadowedIDs, watermark: watermark, gen: gen})
}

func sortedInt32Snapshot(ids []int32) []int32 {
	if len(ids) == 0 {
		return nil
	}
	out := append([]int32(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (vol *serviceVolumeIndex) applyDerivedSections() {
	if vol == nil || vol.index == nil {
		return
	}
	derived := vol.index.Derived
	if len(derived.NameOrder) > 0 && len(derived.NameRank) > 0 {
		if vol.queryIndex == nil {
			vol.queryIndex = &residentQueryIndex{}
		}
		vol.queryIndex.nameOrder = derived.NameOrder
		vol.queryIndex.nameRank = derived.NameRank
	}
	if len(derived.SizeOrder) > 0 && len(derived.SizeRank) > 0 {
		if vol.queryIndex == nil {
			vol.queryIndex = &residentQueryIndex{}
		}
		vol.queryIndex.sizeOrder = derived.SizeOrder
		vol.queryIndex.sizeRank = derived.SizeRank
	}
	if len(derived.ModOrder) > 0 && len(derived.ModRank) > 0 {
		if vol.queryIndex == nil {
			vol.queryIndex = &residentQueryIndex{}
		}
		vol.queryIndex.modOrder = derived.ModOrder
		vol.queryIndex.modRank = derived.ModRank
	}
	if len(derived.ExtOrder) > 0 && len(derived.ExtRank) > 0 {
		if vol.queryIndex == nil {
			vol.queryIndex = &residentQueryIndex{}
		}
		vol.queryIndex.extOrder = derived.ExtOrder
		vol.queryIndex.extRank = derived.ExtRank
	}
	if len(derived.TypeOrder) > 0 && len(derived.TypeRank) > 0 {
		if vol.queryIndex == nil {
			vol.queryIndex = &residentQueryIndex{}
		}
		vol.queryIndex.typeOrder = derived.TypeOrder
		vol.queryIndex.typeRank = derived.TypeRank
	}
	if len(derived.PathOrder) > 0 && len(derived.PathRank) > 0 {
		if vol.queryIndex == nil {
			vol.queryIndex = &residentQueryIndex{}
		}
		vol.queryIndex.pathOrder = derived.PathOrder
		vol.queryIndex.pathRank = derived.PathRank
	}
	if len(derived.ChildOffsets) > 0 && len(derived.ChildIDs) > 0 {
		vol.childOffsets = derived.ChildOffsets
		vol.childIDs = derived.ChildIDs
		vol.rootIDs = derived.RootIDs
	}
	if len(derived.SubtreeStart) > 0 && len(derived.SubtreeEnd) > 0 && len(derived.SubtreeOrder) > 0 {
		vol.subtreeStart = derived.SubtreeStart
		vol.subtreeEnd = derived.SubtreeEnd
		vol.subtreeOrder = derived.SubtreeOrder
		vol.subtreeSizeRank = derived.SubtreeSizeRank
		vol.subtreeModRank = derived.SubtreeModRank
		vol.subtreeExtRank = derived.SubtreeExtRank
		vol.subtreeTypeRank = derived.SubtreeTypeRank
		vol.subtreePathRank = derived.SubtreePathRank
	}
	if len(derived.SubtreeBytes) > 0 {
		vol.subtreeBytes = derived.SubtreeBytes
	}
	if len(derived.FRNs) > 0 && len(derived.FRNRecordIDs) == len(derived.FRNs) {
		vol.frns = derived.FRNs
		vol.frnRecordIDs = derived.FRNRecordIDs
	}
}

func sortFRNIndexEntries(frns []uint64, ids []uint32) {
	if len(frns) <= 1 || len(frns) != len(ids) || sort.SliceIsSorted(frns, func(i, j int) bool {
		return frns[i] < frns[j]
	}) {
		return
	}
	sort.Sort(frnIndexPairs{frns: frns, ids: ids})
}

type frnIndexPairs struct {
	frns []uint64
	ids  []uint32
}

func (p frnIndexPairs) Len() int { return len(p.frns) }

func (p frnIndexPairs) Less(i, j int) bool {
	if p.frns[i] == p.frns[j] {
		return p.ids[i] < p.ids[j]
	}
	return p.frns[i] < p.frns[j]
}

func (p frnIndexPairs) Swap(i, j int) {
	p.frns[i], p.frns[j] = p.frns[j], p.frns[i]
	p.ids[i], p.ids[j] = p.ids[j], p.ids[i]
}

func catchUpServiceVolume(vol *serviceVolumeIndex) error {
	if vol.index == nil || !vol.index.Compact || vol.index.Source != "usn" || vol.volume == "" {
		return nil
	}
	handle, err := openVolume(vol.volume)
	if err != nil {
		vol.state = "stale"
		vol.staleReason = err.Error()
		return err
	}
	defer windows.CloseHandle(handle)

	journal, err := queryUSNJournal(handle)
	if err != nil {
		vol.state = "stale"
		vol.staleReason = err.Error()
		return err
	}
	if err := validateUSNCheckpoint(vol, journal); err != nil {
		vol.state = "stale"
		vol.staleReason = err.Error()
		return err
	}
	if vol.checkpoint >= journal.NextUsn {
		vol.state = "ready"
		return nil
	}
	vol.state = "replaying"
	buffer := make([]byte, 4*1024*1024)
	for vol.checkpoint < journal.NextUsn {
		nextUSN, changes, err := readUSNChanges(handle, journal.UsnJournalID, vol.checkpoint, buffer)
		if err != nil {
			vol.state = "stale"
			vol.staleReason = err.Error()
			return err
		}
		if nextUSN <= vol.checkpoint {
			break
		}
		// seekfs's own artifact churn is dropped before the WAL and the overlay
		// so the checkpoint can advance past it (nextUSN below) without either
		// growing from records that are never indexed.
		changes = vol.filterOwnedReplayChanges(changes)
		if err := appendWAL(vol.dbPath, nextUSN, changes); err != nil {
			vol.state = "stale"
			vol.staleReason = err.Error()
			return err
		}
		vol.applyUSNChanges(changes)
		vol.checkpoint = nextUSN
		vol.index.Checkpoint = nextUSN
	}
	if vol.dbPath != "" {
		vol.dirty = true
	}
	vol.state = "ready"
	vol.staleReason = ""
	return nil
}

func (vol *serviceVolumeIndex) idForFRN(frn uint64) (int, bool) {
	if frn == 0 {
		return 0, false
	}
	if vol.frnToID != nil {
		if id, ok := vol.frnToID[frn]; ok {
			return id, true
		}
	}
	i := sort.Search(len(vol.frns), func(i int) bool { return vol.frns[i] >= frn })
	if i < len(vol.frns) && i < len(vol.frnRecordIDs) && vol.frns[i] == frn {
		return int(vol.frnRecordIDs[i]), true
	}
	return 0, false
}

func (vol *serviceVolumeIndex) addFRNID(frn uint64, id int) {
	if frn == 0 {
		return
	}
	if _, ok := vol.idForFRN(frn); ok {
		return
	}
	if vol.frnToID == nil {
		vol.frnToID = make(map[uint64]int)
	}
	vol.frnToID[frn] = id
}

func (vol *serviceVolumeIndex) frnRecordCount() int {
	return len(vol.frns) + len(vol.frnToID)
}

// Loop pacing delays are package variables so tests can shorten them.
var (
	replayIdleDelay           = 500 * time.Millisecond
	replayErrorDelay          = 5 * time.Second
	staleRecoveryDelay        = 30 * time.Second
	staleRecoveryMax          = 5 * time.Minute
	replayStallCheck          = 15 * time.Second
	replayStallWindow         = 2 * time.Minute
	replayStallRebuildStrikes = 3
)

func (s *goSearchService) replayVolumeLoop(vol *serviceVolumeIndex) {
	buffer := make([]byte, 4*1024*1024)
	gen := vol.replayGen.Load()
	rebuildBlocked := false
	for {
		if cur := vol.replayGen.Load(); cur != gen {
			// A watchdog restart or rebuild took ownership; retire quietly.
			return
		}
		select {
		case <-s.stop:
			return
		default:
		}
		vol.mu.Lock()
		vol.lastReplayAt = time.Now()
		vol.mu.Unlock()
		applied, err := s.replayVolumeOnce(vol, buffer)
		if err != nil {
			if vol.replayGen.Load() != gen {
				// Retired by a watchdog restart or rebuild while the read was
				// in flight; its replacement owns the volume now, so do not
				// clobber the new state with this loop's error.
				return
			}
			serviceLog("background replay error volume=%s db=%s err=%v", vol.volume, vol.dbPath, err)
			vol.mu.Lock()
			vol.lastReplayErr = err.Error()
			vol.mu.Unlock()
			if shouldRebuildStaleIndex(err) {
				rebuildErr := s.rebuildVolumeInPlace(vol)
				if rebuildErr == nil {
					gen = vol.replayGen.Load()
					time.Sleep(replayIdleDelay)
					continue
				}
				var blocked staleRecoveryBlockedError
				if errors.As(rebuildErr, &blocked) {
					// The index file is locked by another process; retrying the
					// full rebuild every replayErrorDelay would re-run the
					// multi-gigabyte build each tick.  Stop attempting until
					// the volume recovers through the stale-recovery path.
					rebuildBlocked = true
					serviceLog("background stale rebuild blocked volume=%s db=%s reason=%s", vol.volume, vol.dbPath, blocked.reason)
				}
			}
			if rebuildBlocked {
				time.Sleep(staleRecoveryMax)
				continue
			}
			s.indexMu.Lock()
			if vol.replayGen.Load() == gen {
				vol.state = "stale"
				vol.staleReason = err.Error()
			}
			s.indexMu.Unlock()
			time.Sleep(replayErrorDelay)
			continue
		}
		if !applied {
			select {
			case <-s.stop:
				return
			default:
			}
			time.Sleep(replayIdleDelay)
		}
	}
}

// replayStallWatchdog is the trust-pack safety net for the live USN replay
// loop. A healthy volume advances its checkpoint whenever the journal has
// data ahead; a volume whose checkpoint stops moving for replayStallWindow
// while the journal still reports data past it is silently broken (the
// replay goroutine may be wedged in a blocked read, or the OS journal read
// is returning EOF early). The watchdog marks such a volume stale so the
// stale-recovery path rebuilds it instead of quietly missing every change.
func (s *goSearchService) replayStallWatchdog() {
	ticker := time.NewTicker(replayStallCheck)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.indexMu.RLock()
			volumes := append([]*serviceVolumeIndex(nil), s.volumes...)
			s.indexMu.RUnlock()
			for _, vol := range volumes {
				if vol == nil || vol.index == nil || vol.volume == "" || vol.dbPath == "" {
					continue
				}
				s.checkReplayStall(vol)
			}
		}
	}
}

func (s *goSearchService) checkReplayStall(vol *serviceVolumeIndex) {
	vol.mu.Lock()
	if vol.state != "ready" || vol.recovering.Load() {
		vol.mu.Unlock()
		return
	}
	lastReplay := vol.lastReplayAt
	cp := vol.checkpoint
	vol.mu.Unlock()
	if lastReplay.IsZero() {
		// Replay loop never stamped progress: give it a full window first.
		return
	}
	// Note: the loop heartbeat (lastReplayAt) is deliberately NOT used as a
	// gate here. A loop that spins without consuming (the observed C: failure
	// mode) keeps stamping it, so only checkpoint progress is ground truth.
	handle, err := openVolume(vol.volume)
	if err != nil {
		return
	}
	defer windows.CloseHandle(handle)
	journal, err := queryUSNJournal(handle)
	if err != nil {
		return
	}
	s.observeReplayStall(vol, cp, journal.NextUsn)
}

// observeReplayStall applies the graduated stall policy given the volume's
// checkpoint and the live journal position: observation first, replay-loop
// restart second, index rebuild last. Called with the journal confirmed to
// hold data past cp.
func (s *goSearchService) observeReplayStall(vol *serviceVolumeIndex, cp, journalNext int64) {
	vol.mu.Lock()
	// Require the checkpoint to be frozen across two consecutive
	// observations so a slow-but-moving replay (large backlog batches) is
	// not misclassified.
	if vol.stallObservedCp != cp || vol.stallObservedAt.IsZero() {
		vol.stallObservedCp = cp
		vol.stallObservedAt = time.Now()
		vol.mu.Unlock()
		return
	}
	if time.Since(vol.stallObservedAt) < replayStallWindow {
		vol.mu.Unlock()
		return
	}

	vol.stallObservedCp = 0
	vol.stallObservedAt = time.Time{}
	vol.replayStrikes++
	strikes := vol.replayStrikes
	vol.mu.Unlock()
	// First response is cheap: restart only the replay loop. The index is
	// healthy, so a full rebuild is avoided unless restarts fail repeatedly.
	if strikes < replayStallRebuildStrikes {
		reason := fmt.Sprintf("replay stall: checkpoint %d frozen while journal next is %d; restarting replay loop (strike %d/%d)", cp, journalNext, strikes, replayStallRebuildStrikes)
		serviceLog("replay stall detected volume=%s reason=%s", vol.volume, reason)
		vol.mu.Lock()
		vol.lastReplayErr = reason
		vol.mu.Unlock()
		vol.replayGen.Add(1) // retires the wedged loop at its next apply
		go s.replayVolumeLoop(vol)
		return
	}
	reason := fmt.Sprintf("replay stall: checkpoint %d frozen while journal next is %d; %d restarts did not help, rebuilding", cp, journalNext, strikes)
	serviceLog("replay stall detected volume=%s reason=%s", vol.volume, reason)
	s.indexMu.Lock()
	// Retire the current replay loop before the recovery rebuild starts: the
	// restarts of strikes 1-2 could not revive it, so it must not clobber the
	// stale reason while the (possibly multi-minute) rebuild is in flight.
	// Bumping the generation under the same lock keeps a retired loop's final
	// state write from racing this reason.
	vol.replayGen.Add(1)
	vol.state = "stale"
	vol.staleReason = reason
	vol.lastReplayErr = "replay stall watchdog"
	s.indexMu.Unlock()
	// Kick the stale recovery path so the volume is rebuilt rather than
	// waiting for the wedged replay goroutine.
	go s.staleRecoveryLoop(vol)
}

// staleRecoveryLoop retries a volume that started stale until its index is
// rebuilt, with exponential backoff between attempts.  On success it swaps
// the rebuilt volume in and starts the normal replay/persist loops.  If some
// other path recovers the volume first, the loop exits.
func (s *goSearchService) staleRecoveryLoop(vol *serviceVolumeIndex) {
	vol.recovering.Store(true)
	defer vol.recovering.Store(false)
	delay := staleRecoveryDelay
	for {
		select {
		case <-s.stop:
			return
		case <-time.After(delay):
		}
		s.indexMu.RLock()
		state := vol.state
		s.indexMu.RUnlock()
		if state != "stale" {
			return
		}
		if err := s.rebuildVolumeInPlace(vol); err != nil {
			var blocked staleRecoveryBlockedError
			if errors.As(err, &blocked) {
				// The index file is locked by another process; retrying is
				// futile and would rebuild multi-gigabyte indexes on every
				// backoff tick.  Leave the volume marked stale and stop.
				serviceLog("stale recovery blocked volume=%s db=%s reason=%s", vol.volume, vol.dbPath, blocked.reason)
				return
			}
			serviceLog("stale recovery rebuild failed volume=%s db=%s retry_after=%s err=%v", vol.volume, vol.dbPath, delay, err)
			delay *= 2
			if delay > staleRecoveryMax {
				delay = staleRecoveryMax
			}
			continue
		}
		go s.replayVolumeLoop(vol)
		go s.persistVolumeLoop(vol)
		vol.mu.Lock()
		vol.replayStrikes = 0
		vol.mu.Unlock()
		serviceLog("stale recovery complete volume=%s db=%s entries=%d", vol.volume, vol.dbPath, vol.index.entryCount())
		return
	}
}

// walCheckpointForApplied returns the checkpoint a replay batch may durably
// claim: only as far as the last change actually applied. A read can return
// more changes than fit one iteration (truncated to serviceUSNReplayBatchMax);
// callers must persist this value, not the unread journal head, so a restart
// replays the truncated tail instead of skipping it.
func walCheckpointForApplied(nextUSN int64, changes []usnChange) int64 {
	if len(changes) > 0 {
		return changes[len(changes)-1].USN
	}
	return nextUSN
}

// replayVolumeOnce reads and applies pending USN changes for one volume.  It
// holds the volume handle open across a bounded drain: the blocking wait read
// (BytesToWaitFor=1) returns as soon as a record is available, so re-opening
// the volume per batch would add a CreateFile + journal query to every tiny
// batch on a busy volume.  It returns applied=true when it advanced the
// checkpoint so the caller re-arms immediately instead of adding an idle delay
// to the visibility latency; applied=false means the journal was already
// caught up (the wait read timed out with no records).
func (s *goSearchService) replayVolumeOnce(vol *serviceVolumeIndex, buffer []byte) (bool, error) {
	gen := vol.replayGen.Load()
	queryHandle, err := openVolume(vol.volume)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(queryHandle)

	s.indexMu.RLock()
	fromUSN := vol.checkpoint
	journalID := vol.journalID
	s.indexMu.RUnlock()
	if journal, err := queryUSNJournal(queryHandle); err != nil {
		return false, err
	} else if err := validateUSNCheckpoint(vol, journal); err != nil {
		return false, err
	}

	reader, err := openUSNOverlappedReader(vol.volume)
	if err != nil {
		return false, err
	}
	defer reader.close()

	cancel := func() bool {
		if vol.replayGen.Load() != gen {
			return true
		}
		select {
		case <-s.stop:
			return true
		default:
			return false
		}
	}

	applied := false
	for batch := 0; batch < serviceUSNReplayDrainBatches; batch++ {
		if vol.replayGen.Load() != gen {
			return applied, nil
		}
		nextUSN, changes, canceled, err := reader.read(journalID, fromUSN, buffer, cancel)
		if err != nil {
			return applied, err
		}
		if canceled {
			return applied, nil
		}
		if nextUSN <= fromUSN {
			break
		}
		// Bound the per-iteration batch so a large USN backlog is applied
		// across several iterations, yielding to search requests between
		// batches instead of monopolizing the volume under one long apply.
		if len(changes) > serviceUSNReplayBatchMax {
			serviceLog("background replay large batch volume=%s changes=%d truncating to %d", vol.volume, len(changes), serviceUSNReplayBatchMax)
			changes = changes[:serviceUSNReplayBatchMax]
		}
		// The checkpoint must advance only as far as the last applied change
		// so the next iteration resumes from the truncated remainder. The WAL
		// must claim the same applied checkpoint: persisting the unread
		// journal head would skip the truncated tail after a restart,
		// permanently hiding newly created files that fell beyond the
		// truncated batch.
		appliedCheckpoint := walCheckpointForApplied(nextUSN, changes)
		vol.mu.Lock()
		if vol.checkpoint != fromUSN || vol.replayGen.Load() != gen {
			// Another path (stale rebuild) replaced the index while this read
			// was in flight; its replay owns the volume now. Drop this batch.
			vol.mu.Unlock()
			return applied, nil
		}
		// Filter after the applied checkpoint is derived from the full batch:
		// the checkpoint must still cover the truncated tail (see above), while
		// seekfs's own artifact churn must not reach the WAL or the overlay.
		changes = vol.filterOwnedReplayChanges(changes)
		if err := appendWAL(vol.dbPath, appliedCheckpoint, changes); err != nil {
			vol.state = "stale"
			vol.staleReason = err.Error()
			vol.mu.Unlock()
			return applied, err
		}
		vol.applyUSNChanges(changes)
		vol.checkpoint = appliedCheckpoint
		vol.index.Checkpoint = appliedCheckpoint
		vol.state = "ready"
		vol.staleReason = ""
		vol.lastReplayAt = time.Now()
		vol.lastReplayNext = nextUSN
		vol.lastReplayErr = ""
		vol.replayStrikes = 0
		vol.stallObservedCp = 0
		vol.stallObservedAt = time.Time{}
		vol.dirty = true
		vol.mu.Unlock()
		applied = true
		fromUSN = appliedCheckpoint
	}
	return applied, nil
}

func (s *goSearchService) persistVolumeLoop(vol *serviceVolumeIndex) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	gen := vol.persistGen.Load()
	for {
		if cur := vol.persistGen.Load(); cur != gen {
			// A replacement persist loop took ownership; retire quietly.
			return
		}
		select {
		case <-s.stop:
			s.persistVolumeIfDue(vol, true)
			return
		case <-ticker.C:
			s.sweepStaleIndexTempFilesIfDue(time.Now())
			s.persistVolumeIfDue(vol, false)
		}
	}
}

func (s *goSearchService) persistVolumeIfDue(vol *serviceVolumeIndex, force bool) {
	if vol.dbPath == "" {
		return
	}
	if !force && envBool("SEEKFS_DISABLE_BACKGROUND_PERSIST") {
		return
	}
	now := time.Now()
	s.indexMu.RLock()
	dirty := vol.dirty
	retryAfter := vol.persistRetryAfter
	due := force || vol.compactionDue(now)
	if !retryAfter.IsZero() && now.Before(retryAfter) {
		due = false
	}
	s.indexMu.RUnlock()
	if !dirty || !due {
		return
	}
	// Serialize the persist against the per-volume replay/write path.  We do
	// NOT hold the global service lock here: the overlay fold and the multi-GB
	// v9 tmp write can take minutes, and taking indexMu.Lock() during that
	// window would wedge every pipe request (info/search block at RLock).
	//
	// vol.mu is held only for the two fast sections: the overlay snapshot and
	// the final swap.  The fold and the multi-GB stage run WITHOUT vol.mu so
	// the replay loop keeps applying USN changes while persist works; new
	// files created during a persist stay searchable.  The swap carries the
	// live overlay's post-snapshot changes into the replacement (rewriting the
	// WAL down to those frames for crash recovery) so nothing regresses.
	watermark := 0
	if vol.overlay != nil {
		watermark = int(vol.overlay.watermark.Load())
	}
	var walBytes int64
	if vol.dbPath != "" {
		if info, err := os.Stat(walPath(vol.dbPath)); err == nil {
			walBytes = info.Size()
		}
	}
	persistStart := time.Now()
	serviceLog("background persist start volume=%s db=%s overlay_slots=%d wal_bytes=%d", vol.volume, vol.dbPath, watermark, walBytes)
	vol.mu.Lock()
	if !vol.dirty || vol.dbPath == "" {
		vol.mu.Unlock()
		return
	}
	now = time.Now()
	if !vol.persistRetryAfter.IsZero() && now.Before(vol.persistRetryAfter) {
		vol.mu.Unlock()
		return
	}
	// Snapshot the overlay instead of pausing replay: the fold and the
	// multi-GB stage below run WITHOUT vol.mu, so the replay loop keeps
	// applying USN changes (into the live overlay) and new files created
	// during a persist stay searchable.  Overlay records are append-only and
	// base records are never mutated in place, so a copy taken under vol.mu
	// is a stable point-in-time view for the fold; the swap later carries the
	// live overlay's post-snapshot delta into the replacement.
	foldCheckpoint := vol.checkpoint
	foldOverlay := snapshotOverlayForFold(vol.overlay)
	snapshotWatermark := 0
	if foldOverlay != nil {
		foldOverlay.checkpointAtRotate.Store(foldCheckpoint)
		snapshotWatermark = len(foldOverlay.records)
	}
	vol.mu.Unlock()
	compacted := compactOverlayIndexLocked(vol, foldOverlay)
	tmp, err := stageIndexFile(vol.dbPath, compacted)
	if err != nil {
		vol.mu.Lock()
		vol.notePersistFailureLocked(err, now)
		vol.mu.Unlock()
		serviceLog("background persist stage error volume=%s db=%s failures=%d retry_after=%s err=%v", vol.volume, vol.dbPath, vol.persistFailures, vol.persistRetryAfter.Format(time.RFC3339Nano), err)
		return
	}
	saved := false
	// The disk write is done and durable; only the fast swap-in remains.
	// Take the global lock plus vol.mu so no search is mid-flight against the
	// mmap view we are about to close and no replay batch is mid-apply while
	// the volume state is replaced; the critical section is fast.
	s.indexMu.Lock()
	vol.mu.Lock()
	if closeErr := closeIndexMMapRecords(vol.index); closeErr != nil {
		vol.notePersistFailureLocked(closeErr, time.Now())
		vol.mu.Unlock()
		s.indexMu.Unlock()
		_ = os.Remove(tmp)
		serviceLog("background persist unmap error volume=%s db=%s err=%v", vol.volume, vol.dbPath, closeErr)
		return
	}
	if commitErr := commitStageIndexFile(vol.dbPath, tmp); commitErr != nil {
		vol.notePersistFailureLocked(commitErr, time.Now())
		vol.mu.Unlock()
		s.indexMu.Unlock()
		serviceLog("background persist commit error volume=%s db=%s err=%v", vol.volume, vol.dbPath, commitErr)
		return
	}
	loaded, loadErr := loadIndexForService(vol.dbPath)
	if loadErr != nil {
		vol.notePersistFailureLocked(loadErr, time.Now())
		vol.mu.Unlock()
		s.indexMu.Unlock()
		serviceLog("background persist reload error volume=%s db=%s err=%v", vol.volume, vol.dbPath, loadErr)
		return
	}
	replacement := newServiceVolumeIndex(vol.dbPath, loaded)
	replacement.state = "ready"
	replacement.staleReason = ""
	replacement.dirty = false
	replacement.lastPersist = time.Now()
	replacement.persistFailures = 0
	replacement.persistRetryAfter = time.Time{}
	replacement.lastPersistErr = ""
	// The folded index claimed checkpoint foldCheckpoint, but replay kept
	// applying later changes into the live overlay while the multi-GB file was
	// being staged.  Carry ONLY that post-snapshot delta into the replacement
	// (the pre-snapshot slots are folded into the new base already), so the
	// carried overlay does not re-fold the same records on the next persist.
	// The WAL is rewritten down to the post-snapshot frames: pre-snapshot
	// frames are folded into the index file, and the kept frames are the only
	// durable record of the carried delta for crash recovery (replay is
	// idempotent per batch).
	seeded := 0
	if replacement.index.Compact && replacement.index.Source == "usn" && vol.overlay != nil {
		replacement.overlay = carryOverlayDelta(vol.overlay, snapshotWatermark, foldOverlay)
		seeded = int(replacement.overlay.watermark.Load())
		if replacement.checkpoint < vol.checkpoint {
			replacement.checkpoint = vol.checkpoint
			replacement.index.Checkpoint = vol.checkpoint
		}
	}
	replacement.publishSnapshot()
	replaceServiceVolumeContents(vol, replacement)
	for i, existing := range s.volumes {
		if existing == vol {
			s.indexes[i] = vol.index
			break
		}
	}
	if replacement.index.Compact && replacement.index.Source == "usn" {
		// Keep the readable prefix even when the read stopped early: a corrupt
		// or truncated tail (a persist killed mid-append) must not block the
		// rewrite.  Skipping it leaves the WAL above its size trigger, so the
		// next fold is due immediately and folds run back to back.  The dropped
		// tail is unrecoverable from the WAL either way and the journal still
		// covers that USN range on the next startup catch-up.
		frames, framesErr := readWALFramesAfter(vol.dbPath, foldCheckpoint)
		if framesErr != nil {
			serviceLog("background persist wal rewrite truncating corrupt tail volume=%s db=%s readable_frames=%d err=%v", vol.volume, vol.dbPath, len(frames), framesErr)
		}
		if rewriteErr := rewriteWAL(vol.dbPath, frames); rewriteErr != nil {
			serviceLog("background persist wal rewrite error volume=%s db=%s err=%v", vol.volume, vol.dbPath, rewriteErr)
		}
	}
	vol.dirty = seeded > 0
	vol.lastPersist = time.Now()
	vol.persistFailures = 0
	vol.persistRetryAfter = time.Time{}
	vol.lastPersistErr = ""
	saved = true
	vol.mu.Unlock()
	s.indexMu.Unlock()
	if saved {
		serviceLog("background persist complete volume=%s db=%s records=%d seeded=%d duration=%s", vol.volume, vol.dbPath, compacted.compactRecordCount(), seeded, time.Since(persistStart).Round(time.Millisecond))
		releaseServiceMemoryAfterSave()
		s.startBackgroundNameOrderBuilds([]*serviceVolumeIndex{vol})
		s.startBackgroundNameTrigramBuilds([]*serviceVolumeIndex{vol})
	}
}

// overlayCompactionSlotLimit returns the overlay watermark at which a persist
// becomes due.  The fixed floor of overlayCompactionMaxSlots keeps small
// volumes responsive, but a fixed slot count on a large volume means a single
// busy folder (a build tree, node_modules, or a spool) can make the pending
// delta a large fraction of the record set and trigger back-to-back multi-GB
// folds.  Scaling the limit to a fraction of the record count keeps the fold
// cost proportionate to the index: the pending overlay stays a few percent of
// the records, and the WAL/tombstone/age triggers still bound latency.
func (vol *serviceVolumeIndex) overlayCompactionSlotLimit() int {
	if vol == nil || vol.index == nil {
		return overlayCompactionMaxSlots
	}
	return overlayCompactionSlotLimitFor(vol.index.compactRecordCount())
}

func overlayCompactionSlotLimitFor(recordCount int) int {
	limit := overlayCompactionMaxSlots
	if scaled := recordCount / overlayCompactionSlotFraction; scaled > limit {
		limit = scaled
	}
	return limit
}

func (vol *serviceVolumeIndex) compactionDue(now time.Time) bool {
	if vol == nil {
		return false
	}
	if vol.overlay != nil {
		watermark := int(vol.overlay.watermark.Load())
		if watermark >= vol.overlayCompactionSlotLimit() {
			return true
		}
		baseCount := 0
		if vol.index != nil {
			baseCount = vol.index.compactRecordCount()
		}
		if baseCount > 0 && vol.overlay.tombstone.len()*100 >= baseCount*overlayCompactionTombstoneP {
			return true
		}
	}
	if vol.dbPath != "" {
		if info, err := os.Stat(walPath(vol.dbPath)); err == nil && info.Size() >= overlayCompactionMaxWAL {
			return true
		}
	}
	return !vol.lastPersist.IsZero() && now.Sub(vol.lastPersist) >= overlayCompactionDirtyAge
}

func (vol *serviceVolumeIndex) notePersistFailureLocked(err error, now time.Time) {
	if vol == nil || err == nil {
		return
	}
	vol.persistFailures++
	vol.persistRetryAfter = now.Add(persistFailureBackoff(vol.persistFailures))
	vol.lastPersistErr = err.Error()
}

func compactOverlayToDisk(vol *serviceVolumeIndex) error {
	if vol == nil || vol.index == nil || vol.dbPath == "" {
		return nil
	}
	compacted := compactOverlayIndex(vol)
	if err := closeIndexMMapRecords(vol.index); err != nil {
		return err
	}
	return saveIndex(vol.dbPath, compacted)
}

func closeIndexMMapRecords(idx *Index) error {
	if idx == nil || idx.MMapRecords == nil || idx.MMapRecords.file == nil {
		return nil
	}
	if err := idx.MMapRecords.file.close(); err != nil {
		return err
	}
	idx.MMapRecords = nil
	return nil
}

func compactOverlayIndex(vol *serviceVolumeIndex) *Index {
	return compactOverlayIndexLocked(vol, vol.overlay)
}

// compactOverlayIndexLocked folds the given point-in-time overlay snapshot
// into the base index.  The caller passes either the live segment (while
// holding vol.mu) or a snapshotOverlayForFold copy (without the lock).
func compactOverlayIndexLocked(vol *serviceVolumeIndex, foldOverlay *overlaySegment) *Index {
	base := vol.index
	out := &Index{
		Version:      indexVersion,
		Roots:        append([]string(nil), base.Roots...),
		BuiltAt:      time.Now(),
		Source:       base.Source,
		Volume:       base.Volume,
		JournalID:    base.JournalID,
		Checkpoint:   foldCheckpointForFold(vol, foldOverlay),
		ContentHash:  base.ContentHash,
		Compact:      true,
		CompactAttrs: base.CompactAttrs,
	}
	if out.Volume == "" {
		out.Volume = vol.volume
	}
	var records []CompactRecord
	if foldOverlay == nil {
		records = make([]CompactRecord, 0, base.compactRecordCount())
		for id := 0; id < base.compactRecordCount(); id++ {
			rec := base.compactRecord(id)
			if !rec.Deleted {
				rec.Parent = -1
				rec.Name = strings.Clone(rec.Name)
				records = append(records, rec)
			}
		}
	} else {
		records = make([]CompactRecord, 0, base.compactRecordCount()+len(foldOverlay.records))
		for id := 0; id < base.compactRecordCount(); id++ {
			if foldOverlay.tombstone.contains(int32(id)) {
				continue
			}
			if foldOverlay.shadowed.contains(int32(id)) {
				continue
			}
			rec := base.compactRecord(id)
			if rec.Deleted {
				continue
			}
			rec.Parent = -1
			rec.Name = strings.Clone(rec.Name)
			records = append(records, rec)
		}
		watermark := int(foldOverlay.watermark.Load())
		if watermark > len(foldOverlay.records) {
			watermark = len(foldOverlay.records)
		}
		for slot := 0; slot < watermark; slot++ {
			rec := foldOverlay.records[slot]
			if current, ok := foldOverlay.byFRN[rec.FRN]; ok && current != int32(slot) {
				continue
			}
			if rec.Deleted {
				continue
			}
			rec.Parent = -1
			rec.Name = strings.Clone(rec.Name)
			records = append(records, rec)
		}
	}
	idByFRN := make(map[uint64]int32, len(records))
	for i, rec := range records {
		idByFRN[rec.FRN] = int32(i)
	}
	for i := range records {
		parentFRN := records[i].ParentFRN
		if parentFRN == 0 || parentFRN == records[i].FRN {
			records[i].Parent = -1
			continue
		}
		if parent, ok := idByFRN[parentFRN]; ok {
			records[i].Parent = parent
		}
	}
	out.Records = records
	buildOrders(out)
	return out
}

// snapshotOverlayForFold returns a deep point-in-time copy of the overlay
// segment.  Caller must hold vol.mu.  The copy is safe to read without the
// lock afterwards because the live segment's records/byFRN are append-only
// (replay never mutates an existing slot) and the base-id sets only grow.
func snapshotOverlayForFold(live *overlaySegment) *overlaySegment {
	if live == nil {
		return nil
	}
	out := &overlaySegment{}
	watermark := int(live.watermark.Load())
	if watermark > len(live.records) {
		watermark = len(live.records)
	}
	out.records = append([]CompactRecord(nil), live.records[:watermark]...)
	out.byFRN = make(map[uint64]int32, len(live.byFRN))
	for frn, slot := range live.byFRN {
		if int(slot) < watermark {
			out.byFRN[frn] = slot
		}
	}
	out.tombstone = copyOverlayBaseIDSet(live.tombstone)
	out.shadowed = copyOverlayBaseIDSet(live.shadowed)
	out.watermark.Store(int32(len(out.records)))
	return out
}

func copyOverlayBaseIDSet(set overlayBaseIDSet) overlayBaseIDSet {
	out := overlayBaseIDSet{
		bits:  append([]uint64(nil), set.bits...),
		ids:   append([]int32(nil), set.ids...),
		count: set.count,
	}
	return out
}

// carryOverlayDelta builds the replacement overlay from the live segment's
// post-snapshot slots [from, watermark).  Caller must hold vol.mu (both at
// snapshot time and here).  The pre-snapshot slots are folded into the new
// base already, so only the post-snapshot tail is kept; slots are rebased to
// start at zero so the carried overlay stays compact across persists.
// Pre-snapshot tombstones/shadows are folded into the new base too, so only
// IDs added after the snapshot (absent from the fold copy) are carried.
// Slot identity is preserved consistently within the rebased segment: byFRN
// values shift by -from, and the per-call seen/path caches in overlayEntry
// are always built against the same records slice.
func carryOverlayDelta(live *overlaySegment, from int, foldOverlay *overlaySegment) *overlaySegment {
	if live == nil {
		return nil
	}
	out := newOverlaySegment()
	watermark := int(live.watermark.Load())
	if watermark > len(live.records) {
		watermark = len(live.records)
	}
	if watermark > from {
		out.records = append([]CompactRecord(nil), live.records[from:watermark]...)
		for frn, slot := range live.byFRN {
			if int(slot) >= from && int(slot) < watermark {
				out.byFRN[frn] = slot - int32(from)
			}
		}
		if foldOverlay != nil {
			out.tombstone = overlayBaseIDSetDelta(live.tombstone, foldOverlay.tombstone)
			out.shadowed = overlayBaseIDSetDelta(live.shadowed, foldOverlay.shadowed)
		} else {
			out.tombstone = copyOverlayBaseIDSet(live.tombstone)
			out.shadowed = copyOverlayBaseIDSet(live.shadowed)
		}
	}
	out.watermark.Store(int32(len(out.records)))
	return out
}

// overlayBaseIDSetDelta returns the entries of live that are absent from snap:
// the base IDs tombstoned/shadowed after the fold snapshot.  Those are the
// only set entries the replacement overlay must carry; the snapshot ones are
// already folded into the replacement base index.
func overlayBaseIDSetDelta(live, snap overlayBaseIDSet) overlayBaseIDSet {
	out := overlayBaseIDSet{}
	for _, id := range live.ids {
		if !snap.contains(id) {
			out.add(id)
		}
	}
	return out
}

// foldCheckpointForFold returns the checkpoint the fold may claim: the volume
// checkpoint captured when the overlay was snapshotted (persist passes that value
// via the snapshot's checkpointAtRotate), which by construction covers every
// change in the folded snapshot.
func foldCheckpointForFold(vol *serviceVolumeIndex, foldOverlay *overlaySegment) int64 {
	if foldOverlay != nil {
		if cp := foldOverlay.checkpointAtRotate.Load(); cp > 0 {
			return cp
		}
	}
	return vol.checkpoint
}

func persistFailureBackoff(failures int) time.Duration {
	if failures <= 0 {
		return time.Minute
	}
	if failures > 6 {
		failures = 6
	}
	return time.Duration(1<<(failures-1)) * time.Minute
}

func releaseServiceMemoryAfterSave() {
	debug.FreeOSMemory()
}

func queryUSNJournal(handle windows.Handle) (usnJournalDataV0, error) {
	var journal usnJournalDataV0
	var bytesReturned uint32
	err := windows.DeviceIoControl(
		handle,
		fsctlQueryUSNJournal,
		nil,
		0,
		(*byte)(unsafe.Pointer(&journal)),
		uint32(unsafe.Sizeof(journal)),
		&bytesReturned,
		nil,
	)
	return journal, err
}
