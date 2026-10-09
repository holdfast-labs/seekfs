package main

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func (vol *serviceVolumeIndex) replayWALWithLimit(maxBytes int64) error {
	if vol == nil || vol.dbPath == "" {
		return nil
	}
	f, err := os.Open(walPath(vol.dbPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	if maxBytes > 0 {
		if info, err := f.Stat(); err == nil && info.Size() > maxBytes {
			return staleIndexError{
				reason:  fmt.Sprintf("wal %s is %d bytes; rebuilding instead of replaying", walPath(vol.dbPath), info.Size()),
				rebuild: true,
			}
		}
	}
	br := bufio.NewReaderSize(f, 1024*1024)
	prefix, err := br.Peek(len(walMagicV1))
	if err == nil && bytes.Equal(prefix, walMagicV1) {
		_, _ = br.Discard(len(walMagicV1))
		return vol.replayBinaryWAL(br)
	}
	dec := json.NewDecoder(br)
	applied := 0
	for {
		var batch walBatch
		if err := dec.Decode(&batch); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if batch.NextUSN <= vol.checkpoint {
			continue
		}
		vol.applyUSNChanges(batch.Changes)
		vol.checkpoint = batch.NextUSN
		vol.index.Checkpoint = batch.NextUSN
		vol.dirty = true
		applied++
	}
	if applied > 0 {
		serviceLog("replayed wal volume=%s db=%s batches=%d checkpoint=%d", vol.volume, vol.dbPath, applied, vol.checkpoint)
	}
	return nil
}

func (vol *serviceVolumeIndex) replayBinaryWAL(r io.Reader) error {
	applied := 0
	for {
		var header [8]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) && applied == 0 {
				return nil
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return err
			}
			return err
		}
		length := binary.LittleEndian.Uint32(header[0:4])
		wantCRC := binary.LittleEndian.Uint32(header[4:8])
		if length == 0 || length > 64*1024*1024 {
			return fmt.Errorf("invalid wal frame length %d", length)
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return err
		}
		if got := crc32.ChecksumIEEE(payload); got != wantCRC {
			return fmt.Errorf("wal frame crc mismatch got=%08x want=%08x", got, wantCRC)
		}
		batch, err := decodeBinaryWALBatch(payload)
		if err != nil {
			return err
		}
		if batch.NextUSN <= vol.checkpoint {
			continue
		}
		vol.applyUSNChanges(batch.Changes)
		vol.checkpoint = batch.NextUSN
		vol.index.Checkpoint = batch.NextUSN
		vol.dirty = true
		applied++
	}
}

func walPath(dbPath string) string {
	return dbPath + ".wal"
}

// readWALFramesAfter decodes every complete binary WAL frame whose NextUSN is
// past checkpoint, in file order.  Read-only: the WAL is left untouched.
func readWALFramesAfter(dbPath string, checkpoint int64) ([]walBatch, error) {
	f, err := os.Open(walPath(dbPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1024*1024)
	prefix, err := br.Peek(len(walMagicV1))
	if err != nil || !bytes.Equal(prefix, walMagicV1) {
		// Legacy JSON WAL (or empty/invalid magic): nothing to rewrite safely.
		return nil, nil
	}
	_, _ = br.Discard(len(walMagicV1))
	var frames []walBatch
	for {
		var header [8]byte
		if _, err := io.ReadFull(br, header[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return frames, err
		}
		length := binary.LittleEndian.Uint32(header[0:4])
		wantCRC := binary.LittleEndian.Uint32(header[4:8])
		if length == 0 || length > 64*1024*1024 {
			return frames, fmt.Errorf("invalid wal frame length %d", length)
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(br, payload); err != nil {
			return frames, err
		}
		if got := crc32.ChecksumIEEE(payload); got != wantCRC {
			return frames, fmt.Errorf("wal frame crc mismatch got=%08x want=%08x", got, wantCRC)
		}
		batch, err := decodeBinaryWALBatch(payload)
		if err != nil {
			return frames, err
		}
		if batch.NextUSN <= checkpoint {
			continue
		}
		frames = append(frames, batch)
	}
	return frames, nil
}

// rewriteWAL replaces the binary WAL with exactly the given frames, writing to
// a temp file and renaming so a crash mid-rewrite leaves either the old or the
// new WAL, never an empty one.  Called after persist to drop frames already
// folded into the index while keeping post-snapshot frames recoverable.
func rewriteWAL(dbPath string, frames []walBatch) error {
	if dbPath == "" {
		return errors.New("wal rewrite requires a db path")
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dbPath), filepath.Base(walPath(dbPath))+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	writeErr := func() error {
		if _, err := tmp.Write(walMagicV1); err != nil {
			return err
		}
		for _, frame := range frames {
			if err := appendBinaryWALFrame(tmp, frame.NextUSN, frame.Changes); err != nil {
				return err
			}
		}
		if err := tmp.Sync(); err != nil {
			return err
		}
		return tmp.Close()
	}()
	if writeErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return writeErr
	}
	if err := os.Rename(tmpName, walPath(dbPath)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func appendWAL(dbPath string, nextUSN int64, changes []usnChange) error {
	if dbPath == "" || len(changes) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(walPath(dbPath), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	err = appendBinaryWALFrame(f, nextUSN, changes)
	if syncErr := f.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func appendBinaryWALFrame(f *os.File, nextUSN int64, changes []usnChange) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		if _, err := f.Write(walMagicV1); err != nil {
			return err
		}
	}
	payload, err := encodeBinaryWALBatch(nextUSN, changes)
	if err != nil {
		return err
	}
	var header [8]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(len(payload)))
	binary.LittleEndian.PutUint32(header[4:8], crc32.ChecksumIEEE(payload))
	if _, err := f.Write(header[:]); err != nil {
		return err
	}
	_, err = f.Write(payload)
	return err
}

func encodeBinaryWALBatch(nextUSN int64, changes []usnChange) ([]byte, error) {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, nextUSN)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(changes)))
	for _, change := range changes {
		if len(change.Name) > int(^uint16(0)) {
			return nil, errors.New("wal change name too large")
		}
		_ = binary.Write(&buf, binary.LittleEndian, change.FRN)
		_ = binary.Write(&buf, binary.LittleEndian, change.ParentFRN)
		_ = binary.Write(&buf, binary.LittleEndian, change.USN)
		_ = binary.Write(&buf, binary.LittleEndian, change.Reason)
		_ = binary.Write(&buf, binary.LittleEndian, change.Attr)
		_ = binary.Write(&buf, binary.LittleEndian, uint16(len(change.Name)))
		_, _ = buf.WriteString(change.Name)
	}
	return buf.Bytes(), nil
}

func decodeBinaryWALBatch(payload []byte) (walBatch, error) {
	var batch walBatch
	if len(payload) < 12 {
		return batch, errors.New("wal frame too small")
	}
	off := 0
	batch.NextUSN = int64(binary.LittleEndian.Uint64(payload[off:]))
	off += 8
	count := int(binary.LittleEndian.Uint32(payload[off:]))
	off += 4
	if count < 0 {
		return batch, errors.New("invalid wal change count")
	}
	batch.Changes = make([]usnChange, 0, count)
	for i := 0; i < count; i++ {
		if off+34 > len(payload) {
			return batch, errors.New("truncated wal change")
		}
		change := usnChange{}
		change.FRN = binary.LittleEndian.Uint64(payload[off:])
		off += 8
		change.ParentFRN = binary.LittleEndian.Uint64(payload[off:])
		off += 8
		change.USN = int64(binary.LittleEndian.Uint64(payload[off:]))
		off += 8
		change.Reason = binary.LittleEndian.Uint32(payload[off:])
		off += 4
		change.Attr = binary.LittleEndian.Uint32(payload[off:])
		off += 4
		nameLen := int(binary.LittleEndian.Uint16(payload[off:]))
		off += 2
		if off+nameLen < off || off+nameLen > len(payload) {
			return batch, errors.New("truncated wal change name")
		}
		change.Name = string(payload[off : off+nameLen])
		off += nameLen
		batch.Changes = append(batch.Changes, change)
	}
	if off != len(payload) {
		return batch, errors.New("wal frame has trailing bytes")
	}
	return batch, nil
}

func removeWAL(dbPath string) error {
	if dbPath == "" {
		return nil
	}
	err := os.Remove(walPath(dbPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (vol *serviceVolumeIndex) rebuildChildren() {
	recordCount := vol.index.compactRecordCount()
	vol.children = make(map[uint64]map[int]struct{}, recordCount)
	for i := 0; i < recordCount; i++ {
		rec := vol.index.compactRecord(i)
		if rec.ParentFRN != 0 && rec.ParentFRN != rec.FRN {
			vol.addChild(rec.ParentFRN, i)
		}
	}
}

func (vol *serviceVolumeIndex) addExactName(id int) {
	if id < 0 || id >= vol.index.compactRecordCount() {
		return
	}
	rec := vol.index.compactRecord(id)
	name := vol.index.compactLowerNameAt(id)
	if rec.Deleted || name == "" {
		return
	}
	if vol.exactNames == nil {
		vol.exactNames = make(map[string][]int)
	}
	vol.exactNames[name] = append(vol.exactNames[name], id)
}

func (vol *serviceVolumeIndex) removeExactName(id int) {
	if id < 0 || id >= vol.index.compactRecordCount() || vol.exactNames == nil {
		return
	}
	name := vol.index.compactLowerNameAt(id)
	if name == "" {
		return
	}
	list := vol.exactNames[name]
	for i, value := range list {
		if value == id {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(vol.exactNames, name)
	} else {
		vol.exactNames[name] = list
	}
}

func (vol *serviceVolumeIndex) addChild(parentFRN uint64, id int) {
	if parentFRN == 0 {
		return
	}
	vol.childOffsets = nil
	vol.childIDs = nil
	if vol.children == nil {
		return
	}
	kids := vol.children[parentFRN]
	if kids == nil {
		kids = make(map[int]struct{})
		vol.children[parentFRN] = kids
	}
	kids[id] = struct{}{}
}

func (vol *serviceVolumeIndex) removeChild(parentFRN uint64, id int) {
	if parentFRN == 0 {
		return
	}
	vol.childOffsets = nil
	vol.childIDs = nil
	if vol.children == nil {
		return
	}
	kids := vol.children[parentFRN]
	if kids == nil {
		return
	}
	delete(kids, id)
	if len(kids) == 0 {
		delete(vol.children, parentFRN)
	}
}

func (vol *serviceVolumeIndex) repairChildren(parentFRN uint64, parentID int) {
	if vol.children == nil {
		return
	}
	for childID := range vol.children[parentFRN] {
		if childID == parentID || childID < 0 || childID >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(childID)
		rec.Parent = int32(parentID)
		vol.index.setCompactRecord(childID, rec)
	}
}

func (vol *serviceVolumeIndex) buildCompactChildren() {
	if vol == nil || vol.index == nil {
		return
	}
	recordCount := vol.index.compactRecordCount()
	counts := make([]uint32, recordCount+1)
	roots := make([]uint32, 0, 16)
	total := 0
	for id := 0; id < recordCount; id++ {
		rec := vol.index.compactRecord(id)
		parent := int(rec.Parent)
		if parent < 0 || parent >= recordCount || parent == id {
			if !rec.Deleted {
				roots = append(roots, uint32(id))
			}
			continue
		}
		counts[parent+1]++
		total++
	}
	vol.rootIDs = roots
	if total == 0 {
		return
	}
	for i := 1; i < len(counts); i++ {
		counts[i] += counts[i-1]
	}
	childIDs := make([]uint32, total)
	next := append([]uint32(nil), counts[:recordCount]...)
	for id := 0; id < recordCount; id++ {
		rec := vol.index.compactRecord(id)
		parent := int(rec.Parent)
		if parent < 0 || parent >= recordCount || parent == id {
			continue
		}
		pos := next[parent]
		childIDs[pos] = uint32(id)
		next[parent]++
	}
	vol.childOffsets = counts
	vol.childIDs = childIDs
	if serviceSubtreeIntervalsEnabled() {
		vol.buildSubtreeRanges()
	} else {
		vol.subtreeOrder = nil
		vol.subtreeStart = nil
		vol.subtreeEnd = nil
	}
}

func (vol *serviceVolumeIndex) needsCompactChildrenBuild() bool {
	return vol != nil &&
		vol.index != nil &&
		vol.index.Compact &&
		vol.index.Source == "usn" &&
		len(vol.childOffsets) == 0 &&
		len(vol.childIDs) == 0 &&
		len(vol.index.Derived.ChildOffsets) == 0
}

func serviceSubtreeIntervalsEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SEEKFS_SUBTREE_INTERVALS")))
	if serviceLowMemoryMode() && v == "" {
		return false
	}
	return v != "0" && v != "false" && v != "no" && v != "off"
}

func servicePathGramsEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SEEKFS_PATH_GRAMS")))
	if serviceLowMemoryMode() && v == "" {
		return true
	}
	return v != "0" && v != "false" && v != "no" && v != "off"
}

func serviceNameOrderEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SEEKFS_NAME_ORDER")))
	if serviceLowMemoryMode() && v == "" {
		return false
	}
	return v != "0" && v != "false" && v != "no" && v != "off"
}

func serviceNameOrderEnabledForIndex(idx *Index) bool {
	if idx == nil || !idx.Compact || !serviceNameOrderEnabled() {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SEEKFS_NAME_ORDER")))
	if v == "1" || v == "true" || v == "yes" || v == "on" {
		return true
	}
	return idx.compactRecordCount() <= serviceBackgroundNameOrderMaxRecords
}

func serviceNameTrigramsEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SEEKFS_NAME_TRIGRAMS")))
	if serviceLowMemoryMode() && v == "" {
		return true
	}
	return v != "0" && v != "false" && v != "no" && v != "off"
}

func serviceLowMemoryMode() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SEEKFS_MEMORY_MODE")))
	return v == "lowmem" || v == "mmap" || v == "low-memory"
}

// envFirst returns the first set, non-empty value among names.  The trailing
// names let a renamed environment knob keep accepting its previous spelling.
func envFirst(names ...string) (string, bool) {
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v, true
		}
	}
	return "", false
}

func envTruthy(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func envBool(names ...string) bool {
	v, ok := envFirst(names...)
	return ok && envTruthy(v)
}

func serviceNameTrigramsEnabledForIndex(idx *Index) bool {
	if idx == nil || !idx.Compact || !serviceNameTrigramsEnabled() {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SEEKFS_NAME_TRIGRAMS")))
	if v == "1" || v == "true" || v == "yes" || v == "on" {
		return true
	}
	if serviceLowMemoryMode() {
		return true
	}
	return idx.compactRecordCount() <= serviceNameTrigramMaxRecords()
}

func serviceNameTrigramMaxRecords() int {
	raw := strings.TrimSpace(os.Getenv("SEEKFS_NAME_TRIGRAM_MAX_RECORDS"))
	if raw == "" {
		return serviceNameTrigramDefaultMaxRecords
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return serviceNameTrigramDefaultMaxRecords
	}
	return n
}

func serviceLowMemoryTrigramStoredPostingMax() int {
	raw := strings.TrimSpace(os.Getenv("SEEKFS_LOW_MEMORY_TRIGRAM_MAX_POSTING"))
	if raw == "" {
		return trigramLowMemoryStoredPostingMaxCount
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return trigramLowMemoryStoredPostingMaxCount
	}
	return n
}

func (s *goSearchService) startBackgroundNameTrigramBuilds(volumes []*serviceVolumeIndex) {
	if !serviceNameTrigramsEnabled() || len(volumes) == 0 {
		return
	}
	go func() {
		for _, vol := range volumes {
			if vol == nil || !serviceNameTrigramsEnabledForIndex(vol.index) {
				continue
			}
			s.rebuildNameTrigramsInBackground(vol)
		}
		debug.FreeOSMemory()
	}()
}

func (s *goSearchService) startBackgroundNameOrderBuilds(volumes []*serviceVolumeIndex) {
	if !serviceNameOrderEnabled() || len(volumes) == 0 {
		return
	}
	go func() {
		for _, vol := range volumes {
			if vol == nil || !serviceNameOrderEnabledForIndex(vol.index) {
				continue
			}
			s.rebuildNameOrderInBackground(vol)
		}
		debug.FreeOSMemory()
	}()
}

func (s *goSearchService) rebuildNameOrderInBackground(vol *serviceVolumeIndex) {
	if vol == nil || !vol.nameOrderState.CompareAndSwap(nameTrigramStatePending, nameTrigramStateBuilding) {
		return
	}
	vol.rebuildNameOrderLocked()
}

func (s *goSearchService) rebuildNameTrigramsInBackground(vol *serviceVolumeIndex) {
	if vol == nil || !vol.nameTrigramState.CompareAndSwap(nameTrigramStatePending, nameTrigramStateBuilding) {
		return
	}
	vol.rebuildNameTrigramsLocked()
}

func (vol *serviceVolumeIndex) resetNameOrderBuild() {
	if vol == nil {
		return
	}
	vol.nameOrderMillis.Store(0)
	if vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0 {
		vol.nameOrderState.Store(nameTrigramStateReady)
		return
	}
	if serviceNameOrderEnabledForIndex(vol.index) {
		vol.nameOrderState.Store(nameTrigramStatePending)
	} else {
		vol.nameOrderState.Store(nameTrigramStateDisabled)
	}
}

func (vol *serviceVolumeIndex) rebuildNameOrderLocked() {
	if vol == nil || vol.index == nil || !serviceNameOrderEnabledForIndex(vol.index) {
		vol.resetNameOrderBuild()
		return
	}
	start := time.Now()
	order, ranks := buildCompactNameOrderRank(vol.index)
	vol.searchMu.Lock()
	if vol.queryIndex == nil {
		vol.queryIndex = &residentQueryIndex{}
	}
	vol.queryIndex.nameOrder = order
	vol.queryIndex.nameRank = ranks
	vol.queryIndex.extTop = buildExtTopPostings(vol.queryIndex.ext, ranks, serviceExtTopPostingLimit)
	vol.nameOrderMillis.Store(time.Since(start).Milliseconds())
	vol.nameOrderState.Store(nameTrigramStateReady)
	vol.searchMu.Unlock()
	serviceLog("built resident name order volume=%s records=%d bytes=%d elapsed=%s",
		vol.volume, vol.index.compactRecordCount(), (len(order)+len(ranks))*4, time.Since(start).Round(time.Millisecond))
}

// liveCompactIDs returns the ids of non-deleted compact records in ascending
// order.  Every rank builder ranks exactly this set; sharing it keeps the ranks
// consistent and moves the boilerplate here.
func liveCompactIDs(idx *Index, recordCount int) []uint32 {
	order := make([]uint32, 0, recordCount)
	for id := 0; id < recordCount; id++ {
		if !idx.compactDeletedAt(id) {
			order = append(order, uint32(id))
		}
	}
	return order
}

// compactRanksFromOrder derives rank[id] = position of id in order.
func compactRanksFromOrder(order []uint32, recordCount int) []uint32 {
	ranks := make([]uint32, recordCount)
	for i := range ranks {
		ranks[i] = uint32(i)
	}
	for pos, id32 := range order {
		if id := int(id32); id >= 0 && id < recordCount {
			ranks[id] = uint32(pos)
		}
	}
	return ranks
}

func buildCompactNameOrderRank(idx *Index) ([]uint32, []uint32) {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	if recordCount == 0 {
		return nil, nil
	}
	order := make([]uint32, 0, recordCount)
	lower := make([]string, recordCount)
	for id := 0; id < recordCount; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		order = append(order, uint32(id))
		lower[id] = idx.compactLowerNameOf(id, rec)
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if lower[a] != lower[b] {
			return lower[a] < lower[b]
		}
		return a < b
	})
	return order, compactRanksFromOrder(order, recordCount)
}

func buildCompactSizeOrderRank(idx *Index) ([]uint32, []uint32) {
	if idx == nil {
		return nil, nil
	}
	return buildCompactSizeOrderRankWithDirBytes(idx, idx.Derived.SubtreeBytes)
}

// buildCompactSizeOrderRankWithDirBytes orders records by their effective size:
// a directory uses its recursive subtree total from dirBytes, a file its own
// size.  This keeps sort:size and the size order scan correct for directories.
func buildCompactSizeOrderRankWithDirBytes(idx *Index, dirBytes []uint64) ([]uint32, []uint32) {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	if recordCount == 0 {
		return nil, nil
	}
	order := make([]uint32, 0, recordCount)
	lower := make([]string, recordCount)
	size := make([]int64, recordCount)
	for id := 0; id < recordCount; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		order = append(order, uint32(id))
		lower[id] = idx.compactLowerNameOf(id, rec)
		// A directory sorts by its recursive subtree total when known, a file by
		// its own size (matches buildCompactSizeOrderRankWithDirBytes' sizeAt).
		if rec.Mode&uint32(os.ModeDir) != 0 && id >= 0 && id < len(dirBytes) {
			size[id] = int64(dirBytes[id])
		} else {
			size[id] = rec.Size
		}
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if size[a] != size[b] {
			return size[a] < size[b]
		}
		if lower[a] != lower[b] {
			return lower[a] < lower[b]
		}
		return a < b
	})
	return order, compactRanksFromOrder(order, recordCount)
}

func buildCompactModifiedOrderRank(idx *Index) ([]uint32, []uint32) {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	if recordCount == 0 {
		return nil, nil
	}
	order := make([]uint32, 0, recordCount)
	lower := make([]string, recordCount)
	mod := make([]int64, recordCount)
	for id := 0; id < recordCount; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		order = append(order, uint32(id))
		lower[id] = idx.compactLowerNameOf(id, rec)
		mod[id] = rec.ModUnix
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if mod[a] != mod[b] {
			if mod[a] == 0 {
				return false
			}
			if mod[b] == 0 {
				return true
			}
			return mod[a] > mod[b]
		}
		if lower[a] != lower[b] {
			return lower[a] < lower[b]
		}
		return a < b
	})
	return order, compactRanksFromOrder(order, recordCount)
}

func buildCompactExtensionOrderRank(idx *Index) ([]uint32, []uint32) {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	if recordCount == 0 {
		return nil, nil
	}
	order := make([]uint32, 0, recordCount)
	lower := make([]string, recordCount)
	ext := make([]string, recordCount)
	for id := 0; id < recordCount; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		order = append(order, uint32(id))
		lower[id] = idx.compactLowerNameOf(id, rec)
		ext[id] = compactRecordLowerExt(rec)
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if ext[a] != ext[b] {
			return ext[a] < ext[b]
		}
		if lower[a] != lower[b] {
			return lower[a] < lower[b]
		}
		return a < b
	})
	return order, compactRanksFromOrder(order, recordCount)
}

func buildCompactTypeOrderRank(idx *Index) ([]uint32, []uint32) {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	if recordCount == 0 {
		return nil, nil
	}
	order := make([]uint32, 0, recordCount)
	lower := make([]string, recordCount)
	ty := make([]uint8, recordCount)
	for id := 0; id < recordCount; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted {
			continue
		}
		order = append(order, uint32(id))
		lower[id] = idx.compactLowerNameOf(id, rec)
		ty[id] = uint8(compactRecordTypeRank(rec))
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if ty[a] != ty[b] {
			return ty[a] < ty[b]
		}
		if lower[a] != lower[b] {
			return lower[a] < lower[b]
		}
		return a < b
	})
	return order, compactRanksFromOrder(order, recordCount)
}

func buildCompactPathOrderRank(idx *Index) ([]uint32, []uint32) {
	recordCount := 0
	if idx != nil {
		recordCount = idx.compactRecordCount()
	}
	if recordCount == 0 {
		return nil, nil
	}
	order := liveCompactIDs(idx, recordCount)
	keys := make([]string, recordCount)
	computed := make([]bool, recordCount)
	seen := make([]int32, recordCount)
	var gen int32
	var parts []string
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		// Resolve keys lazily in comparator order with a shared memo, exactly as
		// the original map-cache path did.  The root-name skip depends on the
		// cached ancestor's chain length, so eager id-order precomputation could
		// disagree with the original in that edge case.
		if !computed[a] {
			gen++
			parts = idx.reconstructLowerPathInto(int(a), keys, computed, seen, gen, parts[:0])
		}
		if !computed[b] {
			gen++
			parts = idx.reconstructLowerPathInto(int(b), keys, computed, seen, gen, parts[:0])
		}
		if keys[a] != keys[b] {
			return keys[a] < keys[b]
		}
		return a < b
	})
	return order, compactRanksFromOrder(order, recordCount)
}

func compactRecordTypeRank(rec CompactRecord) int {
	if rec.Mode&uint32(os.ModeDir) != 0 {
		return 0
	}
	return 1
}

func compactRecordLowerExt(rec CompactRecord) string {
	ext := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
	if ext == "" {
		return ""
	}
	return strings.ToLower(ext)
}

func buildExtTopPostings(ext map[string][]uint32, ranks []uint32, limit int) map[string][]uint32 {
	return buildExtTopPostingsMin(ext, ranks, limit, 1)
}

func buildExtTopPostingsMin(ext map[string][]uint32, ranks []uint32, limit int, minIDs int) map[string][]uint32 {
	if len(ext) == 0 || limit <= 0 {
		return nil
	}
	if minIDs <= 0 {
		minIDs = 1
	}
	out := make(map[string][]uint32, len(ext))
	for key, ids := range ext {
		if len(ids) < minIDs {
			continue
		}
		if len(ids) <= limit {
			top := append([]uint32(nil), ids...)
			sortExtTopByRank(top, ranks)
			out[key] = top
			continue
		}
		h := make(extRankMaxHeap, 0, limit)
		for _, id := range ids {
			item := extRankItem{id: id, rank: extRankOf(id, ranks)}
			if len(h) < limit {
				heap.Push(&h, item)
				continue
			}
			if extRankLess(item, h[0]) {
				h[0] = item
				heap.Fix(&h, 0)
			}
		}
		top := make([]uint32, len(h))
		for i := range h {
			top[i] = h[i].id
		}
		sortExtTopByRank(top, ranks)
		out[key] = top
	}
	return out
}

func sortExtTopByRank(ids []uint32, ranks []uint32) {
	sort.Slice(ids, func(i, j int) bool {
		a, b := ids[i], ids[j]
		ra, rb := extRankOf(a, ranks), extRankOf(b, ranks)
		if ra == rb {
			return a < b
		}
		return ra < rb
	})
}

func extRankOf(id uint32, ranks []uint32) uint32 {
	if int(id) >= len(ranks) {
		return id
	}
	return ranks[id]
}

func extRankLess(a, b extRankItem) bool {
	if a.rank == b.rank {
		return a.id < b.id
	}
	return a.rank < b.rank
}

type extRankItem struct {
	id   uint32
	rank uint32
}

type extRankMaxHeap []extRankItem

func (h extRankMaxHeap) Len() int { return len(h) }

func (h extRankMaxHeap) Less(i, j int) bool {
	if h[i].rank == h[j].rank {
		return h[i].id > h[j].id
	}
	return h[i].rank > h[j].rank
}

func (h extRankMaxHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *extRankMaxHeap) Push(x any) {
	*h = append(*h, x.(extRankItem))
}

func (h *extRankMaxHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func (vol *serviceVolumeIndex) nameOrderStateString() string {
	if vol == nil {
		return ""
	}
	switch vol.nameOrderState.Load() {
	case nameTrigramStatePending:
		return "pending"
	case nameTrigramStateBuilding:
		return "building"
	case nameTrigramStateReady:
		return "ready"
	default:
		if serviceNameOrderEnabled() {
			return "disabled"
		}
		return ""
	}
}

func (vol *serviceVolumeIndex) nameTrigramIndex() *compressedTrigramIndex {
	if vol == nil {
		return nil
	}
	return vol.nameTrigrams.Load()
}

func (vol *serviceVolumeIndex) nameQuadgramIndex() *compressedTrigramIndex {
	if vol == nil {
		return nil
	}
	return vol.nameQuadgrams.Load()
}

func (vol *serviceVolumeIndex) markNameTrigramRecent(id int) {
	if vol == nil || id < 0 || !serviceNameTrigramsEnabledForIndex(vol.index) {
		return
	}
	if vol.nameTrigramRecent == nil {
		vol.nameTrigramRecent = make(map[int]struct{})
	}
	vol.nameTrigramRecent[id] = struct{}{}
}

func (vol *serviceVolumeIndex) resetNameTrigrams() {
	if vol == nil {
		return
	}
	if vol.index != nil && vol.index.Derived.NameTrigrams != nil {
		vol.nameTrigrams.Store(vol.index.Derived.NameTrigrams)
		vol.nameQuadgrams.Store(nil)
		vol.nameTrigramMillis.Store(0)
		vol.nameTrigramRecent = nil
		vol.nameTrigramState.Store(nameTrigramStateReady)
		return
	}
	vol.nameTrigrams.Store(nil)
	vol.nameQuadgrams.Store(nil)
	vol.nameTrigramMillis.Store(0)
	vol.nameTrigramRecent = nil
	if serviceNameTrigramsEnabledForIndex(vol.index) {
		vol.nameTrigramState.Store(nameTrigramStatePending)
	} else {
		vol.nameTrigramState.Store(nameTrigramStateDisabled)
	}
}

func (vol *serviceVolumeIndex) rebuildNameTrigramsLocked() {
	if vol == nil || vol.index == nil || !serviceNameTrigramsEnabledForIndex(vol.index) {
		vol.resetNameTrigrams()
		return
	}
	start := time.Now()
	var ti *compressedTrigramIndex
	if shouldUseExternalNameGram(vol.index.compactRecordCount()) {
		maxPosting := serviceLowMemoryTrigramStoredPostingMax()
		if !serviceLowMemoryMode() {
			// Match buildNameTrigramIndex + dropCommonPostings: store every
			// gram, then drop the common postings, leaving no omitted marker.
			maxPosting = 1 << 30
		}
		pngr, _, err := buildNameGramIndexExternal(context.Background(), vol.index, 3, maxPosting, nameGramSpoolDir())
		if err == nil {
			if !serviceLowMemoryMode() {
				pngr.dropCommonPostings(trigramStoredPostingMaxCount)
			}
			ti = pngr
		} else {
			serviceLog("external name trigram build failed volume=%s records=%d err=%v; using in-memory builder",
				vol.volume, vol.index.compactRecordCount(), err)
		}
	}
	if ti == nil {
		if serviceLowMemoryMode() {
			ti = buildSelectiveNameTrigramIndex(vol.index, serviceLowMemoryTrigramStoredPostingMax())
		} else {
			ti = buildNameTrigramIndex(vol.index)
			ti.dropCommonPostings(trigramStoredPostingMaxCount)
		}
	}
	vol.searchMu.Lock()
	vol.nameTrigrams.Store(ti)
	vol.nameQuadgrams.Store(nil)
	vol.nameTrigramRecent = nil
	vol.nameTrigramMillis.Store(time.Since(start).Milliseconds())
	vol.nameTrigramState.Store(nameTrigramStateReady)
	vol.searchMu.Unlock()
	serviceLog("built name trigram index volume=%s records=%d keys=%d bytes=%d elapsed=%s",
		vol.volume, vol.index.compactRecordCount(), ti.keyCount(), ti.postingBytes, time.Since(start).Round(time.Millisecond))
}

func (vol *serviceVolumeIndex) nameTrigramStateString() string {
	if vol == nil {
		return ""
	}
	switch vol.nameTrigramState.Load() {
	case nameTrigramStatePending:
		return "pending"
	case nameTrigramStateBuilding:
		return "building"
	case nameTrigramStateReady:
		return "ready"
	default:
		if serviceNameTrigramsEnabled() {
			return "disabled"
		}
		return ""
	}
}

func (vol *serviceVolumeIndex) childIDsForRecord(id int) []uint32 {
	if vol == nil || id < 0 {
		return nil
	}
	if len(vol.childOffsets) > id+1 {
		start, end := vol.childOffsets[id], vol.childOffsets[id+1]
		if start <= end && int(end) <= len(vol.childIDs) {
			return vol.childIDs[start:end]
		}
	}
	if vol.children == nil || vol.index == nil || id >= vol.index.compactRecordCount() {
		return nil
	}
	rec := vol.index.compactRecord(id)
	kids := vol.children[rec.FRN]
	if len(kids) == 0 {
		return nil
	}
	out := make([]uint32, 0, len(kids))
	for childID := range kids {
		if childID >= 0 {
			out = append(out, uint32(childID))
		}
	}
	return out
}

func buildResidentQueryIndex(vol *serviceVolumeIndex) *residentQueryIndex {
	return buildResidentQueryIndexMode(vol, false)
}

func buildResidentQueryIndexForPersistence(vol *serviceVolumeIndex) *residentQueryIndex {
	return buildResidentQueryIndexMode(vol, true)
}

func buildResidentQueryIndexMode(vol *serviceVolumeIndex, persistence bool) *residentQueryIndex {
	recordCount := vol.index.compactRecordCount()
	hasMappedExt := false
	hasMappedComponents := false
	if postings := vol.index.Derived.Postings; postings != nil {
		if section := postings[indexSectionPEXT]; len(section.Data) > 0 {
			hasMappedExt = true
		}
		if section := postings[indexSectionPCMP]; len(section.Data) > 0 {
			hasMappedComponents = true
		}
	}
	mappedLowmem := serviceLowMemoryMode() && vol.index.MMapRecords != nil &&
		len(vol.index.Derived.NameOrder) > 0 && len(vol.index.Derived.NameRank) > 0 &&
		hasMappedExt && hasMappedComponents
	qi := &residentQueryIndex{}
	if !hasMappedExt {
		qi.ext = make(map[string][]uint32)
	}
	if !hasMappedComponents {
		qi.components = make(map[string][]uint32)
	}
	if !mappedLowmem && !persistence {
		qi.dirs = make([]uint32, 0, recordCount/8)
		qi.dirsReady = true
	}
	sortAttrBits := false
	if vol.index.compactHasAttrs() && !mappedLowmem {
		qi.attrBits = make(map[uint32][]uint32, 5)
		sortAttrBits = true
	}
	if vol.index.compactHasAttrs() && mappedLowmem && len(vol.index.Derived.AttrBits) > 0 {
		qi.attrBits = vol.index.Derived.AttrBits
	}
	if servicePathGramsEnabled() && !mappedLowmem && !persistence {
		qi.pathGrams = make(map[string][]uint32)
	}
	if !mappedLowmem {
		for id := 0; id < recordCount; id++ {
			rec := vol.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			name := vol.index.compactLowerNameAt(id)
			if rec.Mode&uint32(os.ModeDir) != 0 {
				qi.dirs = append(qi.dirs, uint32(id))
				if !hasMappedComponents && name != "" && name != "." {
					qi.components[name] = append(qi.components[name], uint32(id))
				}
				if qi.pathGrams != nil && name != "" && name != "." {
					for _, gram := range componentGrams(name) {
						qi.pathGrams[gram] = append(qi.pathGrams[gram], uint32(id))
					}
				}
			}
			if !hasMappedExt {
				actualExt := strings.TrimPrefix(filepath.Ext(rec.Name), ".")
				if actualExt != "" {
					ext := strings.ToLower(actualExt)
					qi.ext[ext] = append(qi.ext[ext], uint32(id))
				}
			}
			if qi.attrBits != nil {
				for _, bit := range queryAttrBits() {
					if rec.Mode&bit == bit {
						qi.attrBits[bit] = append(qi.attrBits[bit], uint32(id))
					}
				}
			}
		}
	}
	sortResidentPostings(qi.ext)
	if qi.pathGrams != nil {
		sortResidentPostings(qi.pathGrams)
	}
	sortResidentPostings(qi.components)
	if sortAttrBits {
		sortResidentAttrPostings(qi.attrBits)
	}
	sortUint32s(qi.dirs)
	if len(vol.index.Derived.NameOrder) > 0 && len(vol.index.Derived.NameRank) > 0 {
		qi.nameOrder, qi.nameRank = vol.index.Derived.NameOrder, vol.index.Derived.NameRank
		qi.sizeOrder, qi.sizeRank = vol.index.Derived.SizeOrder, vol.index.Derived.SizeRank
		qi.modOrder, qi.modRank = vol.index.Derived.ModOrder, vol.index.Derived.ModRank
		qi.extOrder, qi.extRank = vol.index.Derived.ExtOrder, vol.index.Derived.ExtRank
		qi.typeOrder, qi.typeRank = vol.index.Derived.TypeOrder, vol.index.Derived.TypeRank
		qi.pathOrder, qi.pathRank = vol.index.Derived.PathOrder, vol.index.Derived.PathRank
		if !persistence {
			qi.extTop = buildExtTopPostings(qi.ext, qi.nameRank, serviceExtTopPostingLimit)
		}
	} else if !persistence && serviceNameOrderEnabled() && recordCount <= serviceResidentNameOrderMaxRecords {
		qi.nameOrder, qi.nameRank = buildCompactNameOrderRank(vol.index)
		if vol.index.compactHasSize() {
			qi.sizeOrder, qi.sizeRank = buildCompactSizeOrderRank(vol.index)
		}
		if vol.index.compactHasModTime() {
			qi.modOrder, qi.modRank = buildCompactModifiedOrderRank(vol.index)
		}
		qi.extOrder, qi.extRank = buildCompactExtensionOrderRank(vol.index)
		qi.typeOrder, qi.typeRank = buildCompactTypeOrderRank(vol.index)
		qi.pathOrder, qi.pathRank = buildCompactPathOrderRank(vol.index)
		if !persistence {
			qi.extTop = buildExtTopPostings(qi.ext, qi.nameRank, serviceExtTopPostingLimit)
		}
	} else if !persistence {
		qi.extTop = buildExtTopPostingsMin(qi.ext, nil, serviceExtTopPostingLimit, serviceExtTopPostingLimit)
	}
	return qi
}

func (vol *serviceVolumeIndex) clearSearchCachesLocked() {
	vol.pathCache = make(map[int]string)
	vol.termCache = nil
	vol.pathTermCache = nil
	vol.extCache = nil
	vol.underCache = nil
	vol.underRootCache = nil
}

func (vol *serviceVolumeIndex) trimSearchCachesLocked() {
	if len(vol.pathCache) > servicePathCacheLimit {
		vol.pathCache = make(map[int]string)
	}
	vol.termMu.Lock()
	defer vol.termMu.Unlock()
	if vol.postingListCacheBytesLocked() > postingListCacheMaxBytes() {
		vol.termCache = nil
		vol.pathTermCache = nil
		vol.extCache = nil
		vol.underCache = nil
		vol.underRootCache = nil
	}
	vol.searchCount++
}

func (vol *serviceVolumeIndex) residentMemoryInfo() *residentMemoryInfo {
	if vol == nil || vol.index == nil {
		return nil
	}
	recordCount := vol.index.compactRecordCount()
	info := &residentMemoryInfo{Records: recordCount}
	if p := vol.index.PackedRecords; p != nil {
		info.NameBlobBytes = len(p.NameBlob)
		info.LowerBlobBytes = len(p.LowerBlob)
		info.RecordBytes = int64(len(p.FRNs))*8 +
			int64(len(p.ParentFRNExtras))*16 +
			int64(len(p.Parents))*4 +
			int64(len(p.NameOffs))*4 +
			int64(len(p.NameLens))*2 +
			int64(len(p.LowerOffs))*4 +
			int64(len(p.Modes))*4 +
			int64(len(p.Size32))*4 +
			int64(len(p.Size64IDs))*4 +
			int64(len(p.Size64Values))*8 +
			int64(len(p.ModUnix))*8 +
			int64(len(p.DeletedBits))*8
	}
	if m := vol.index.MMapRecords; m != nil {
		info.MMapRecordBytes = int64(len(m.nameBlob)) + int64(len(m.tokenTable)) + int64(len(m.recordData))
	}
	if vol.queryIndex != nil {
		info.NameOrderBytes = (len(vol.queryIndex.nameOrder) + len(vol.queryIndex.nameRank)) * 4
		info.TypePostBytes = len(vol.queryIndex.dirs) * 4
		for _, list := range vol.queryIndex.ext {
			info.ExtPostBytes += len(list) * 4
		}
		for _, list := range vol.queryIndex.extTop {
			info.ExtPostBytes += len(list) * 4
		}
		for _, list := range vol.queryIndex.pathGrams {
			info.ExtPostBytes += len(list) * 4
		}
		for _, list := range vol.queryIndex.components {
			info.TypePostBytes += len(list) * 4
		}
	}
	if trigrams := vol.nameTrigramIndex(); trigrams != nil {
		info.NameTrigramBytes = trigrams.postingBytes
		info.NameTrigramKeys = trigrams.keyCount()
	}
	info.ChildBytes = (len(vol.childOffsets) + len(vol.childIDs) + len(vol.rootIDs) + len(vol.subtreeOrder) + len(vol.subtreeStart) + len(vol.subtreeEnd)) * 4
	info.FRNIndexBytes = len(vol.frns)*8 + len(vol.frnRecordIDs)*4
	info.FRNOverlayEntries = len(vol.frnToID)
	if ident := vol.index.nameIdent.Load(); ident != nil {
		info.NameIdentityBytes = int64(len(ident.masks))*8 + int64(len(ident.exemplar)+len(ident.recName)+len(ident.roots))*4
	}
	if topo := vol.index.memoTopo.Load(); topo != nil {
		info.NameIdentityBytes += int64(len(topo.order)) * 4
	}
	vol.index.nameMemo.mu.Lock()
	if memo := vol.index.nameMemo.memo; memo != nil {
		info.NameMemoBytes = int64(len(memo.nameBits)+len(memo.nameNeg)+len(memo.dirBits)+len(memo.dirNeg))*2 + int64(len(memo.visited))*8
	}
	vol.index.nameMemo.mu.Unlock()
	info.KnownBytes = int64(info.NameBlobBytes) +
		int64(info.LowerBlobBytes) +
		info.RecordBytes +
		info.MMapRecordBytes +
		int64(info.NameOrderBytes) +
		int64(info.ExtPostBytes) +
		int64(info.NameTrigramBytes) +
		int64(info.TypePostBytes) +
		int64(info.ChildBytes) +
		int64(info.FRNIndexBytes) + info.NameIdentityBytes + info.NameMemoBytes
	return info
}

func runtimeMemorySnapshot() *runtimeMemoryInfo {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	limit := debug.SetMemoryLimit(-1)
	return &runtimeMemoryInfo{
		HeapAllocBytes:    m.HeapAlloc,
		HeapInuseBytes:    m.HeapInuse,
		HeapIdleBytes:     m.HeapIdle,
		HeapReleasedBytes: m.HeapReleased,
		HeapSysBytes:      m.HeapSys,
		StackInuseBytes:   m.StackInuse,
		SysBytes:          m.Sys,
		NumGC:             m.NumGC,
		GCCPUFraction:     m.GCCPUFraction,
		GoMemLimitBytes:   limit,
	}
}

var serviceLastQueryUnixNano atomic.Int64
var serviceRuntimeTuningOnce sync.Once

func serviceNoteQueryActivity() {
	serviceLastQueryUnixNano.Store(time.Now().UnixNano())
}

func applyServiceRuntimeMemoryTuning() {
	serviceRuntimeTuningOnce.Do(func() {
		if raw := strings.TrimSpace(os.Getenv("SEEKFS_GO_MEM_LIMIT_MB")); raw != "" {
			if mb, err := strconv.ParseInt(raw, 10, 64); err == nil && mb > 0 {
				prev := debug.SetMemoryLimit(int64(mb) << 20)
				serviceLog("go memory limit set to %d MiB (previous %d bytes)", mb, prev)
			}
		}
		idle := defaultIdleMemoryRelease
		if raw := strings.TrimSpace(os.Getenv("SEEKFS_IDLE_RELEASE_SECONDS")); raw != "" {
			if secs, err := strconv.Atoi(raw); err == nil {
				idle = time.Duration(secs) * time.Second
			}
		}
		if idle > 0 {
			go serviceIdleMemoryReleaseLoop(idle)
		}
	})
}

func serviceIdleMemoryReleaseLoop(idle time.Duration) {
	var lastRelease time.Time
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		last := serviceLastQueryUnixNano.Load()
		if last != 0 && time.Since(time.Unix(0, last)) < idle {
			continue
		}
		if !lastRelease.IsZero() && time.Since(lastRelease) < idle {
			continue
		}
		debug.FreeOSMemory()
		lastRelease = time.Now()
		serviceLog("idle memory release ran (threshold %s)", idle)
	}
}

func sortResidentPostings(postings map[string][]uint32) {
	for key, list := range postings {
		sortUint32s(list)
		postings[key] = uniqueSortedUint32s(list)
	}
}

func sortResidentAttrPostings(postings map[uint32][]uint32) {
	for key, list := range postings {
		sortUint32s(list)
		postings[key] = uniqueSortedUint32s(list)
	}
}

func queryAttrBits() []uint32 {
	return []uint32{
		fileAttributeReadonly,
		fileAttributeHidden,
		fileAttributeSystem,
		fileAttributeDir,
		fileAttributeArchive,
	}
}

func componentGrams(s string) []string {
	return fixedGrams(s, 3)
}

func fixedGrams(s string, n int) []string {
	if n <= 0 {
		return nil
	}
	if len(s) < n {
		return nil
	}
	seen := make(map[string]struct{}, len(s))
	out := make([]string, 0, len(s)-n+1)
	for i := 0; i+n <= len(s); i++ {
		gram := s[i : i+n]
		if _, ok := seen[gram]; ok {
			continue
		}
		seen[gram] = struct{}{}
		out = append(out, gram)
	}
	return out
}

func compactLowerName(rec CompactRecord) string {
	return strings.ToLower(rec.Name)
}

func newPackedRecords(records []CompactRecord) *PackedRecords {
	if len(records) == 0 {
		return nil
	}
	hasSize := false
	hasModUnix := false
	for _, rec := range records {
		if rec.Size != 0 {
			hasSize = true
		}
		if rec.ModUnix != 0 {
			hasModUnix = true
		}
	}
	p := &PackedRecords{
		FRNs:        make([]uint64, len(records)),
		Parents:     make([]int32, len(records)),
		NameOffs:    make([]uint32, len(records)),
		NameLens:    make([]uint16, len(records)),
		LowerOffs:   make([]uint32, len(records)),
		Modes:       make([]uint32, len(records)),
		DeletedBits: make([]uint64, (len(records)+63)/64),
		NameBlob:    make([]byte, 0, len(records)*16),
		LowerBlob:   make([]byte, 0, len(records)*16),
	}
	if hasSize {
		p.Size32 = make([]uint32, len(records))
	}
	if hasModUnix {
		p.ModUnix = make([]int64, len(records))
	}
	nameRefs := make(map[string]struct {
		off uint32
		len uint16
	}, len(records)/2)
	lowerRefs := make(map[string]struct {
		off uint32
		len uint16
	}, len(records)/2)
	for i, rec := range records {
		p.FRNs[i] = rec.FRN
		p.Parents[i] = rec.Parent
		p.setNameDedup(i, rec.Name, nameRefs)
		p.setLowerNameDedup(i, rec.Name, lowerRefs)
		p.setMode(i, rec.Mode)
		if p.Size32 != nil {
			p.setSize(i, rec.Size)
		}
		if p.ModUnix != nil {
			p.ModUnix[i] = rec.ModUnix
		}
		p.setDeleted(i, rec.Deleted)
	}
	for i, rec := range records {
		p.setParentFRN(i, rec.ParentFRN)
	}
	return p
}
