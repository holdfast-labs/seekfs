package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// overlaySlotEntry builds an Entry from a specific overlay slot without
// requiring the slot to be the FRN's latest state (unlike overlayEntry).
// Used by the watch-delta path to materialize states at a cursor boundary.
func (vol *serviceVolumeIndex) overlaySlotEntry(records []CompactRecord, latest map[uint64]int32, slot int, seen map[int32]struct{}, pathCache map[int]string) (Entry, bool) {
	if vol == nil || slot < 0 || slot >= len(records) {
		return Entry{}, false
	}
	if _, ok := seen[int32(slot)]; ok {
		return Entry{}, false
	}
	seen[int32(slot)] = struct{}{}
	rec := records[slot]
	if rec.Deleted {
		return Entry{}, false
	}
	path := vol.overlayRecordPath(records, latest, slot, seen, pathCache)
	if path == "" {
		return Entry{}, false
	}
	return Entry{
		Path:        path,
		Name:        rec.Name,
		LowerName:   strings.ToLower(rec.Name),
		LowerPath:   strings.ToLower(path),
		Mode:        rec.Mode,
		Size:        rec.Size,
		ModUnix:     rec.ModUnix,
		IndexSource: vol.index.Source,
		FRN:         rec.FRN,
	}, true
}

// overlayLiveMatchCount counts live (non-deleted, latest-slot-per-FRN) overlay
// records matching pq, reading only through the given snapshot's records
// slice up to watermark Ã¢â‚¬â€ the same walk mergeOverlayMatches performs for a
// full search, but without allocating/ranking Entry results since callers
// only need len(). It reuses latestOverlaySlotsByFRN/overlayEntry/
// entryMatches so overlay entry construction and match semantics never
// diverge from the search path (review G6: snapshot slices only, never
// vol.overlay maps, on this read path).
func (vol *serviceVolumeIndex) overlayLiveMatchCount(snap *volumeSnapshot, pq parsedQuery) int {
	count, _ := vol.overlayLiveMatchCountCancellable(snap, pq)
	return count
}

func (vol *serviceVolumeIndex) overlayLiveMatchCountCancellable(snap *volumeSnapshot, pq parsedQuery) (int, error) {
	if vol == nil || snap == nil || len(snap.records) == 0 {
		return 0, nil
	}
	watermark := int(snap.watermark)
	records := snap.records
	if watermark > len(records) {
		watermark = len(records)
	}
	records = records[:watermark]
	if len(records) == 0 {
		return 0, nil
	}
	latest := latestOverlaySlotsByFRN(records)
	pathCache := make(map[int]string)
	count := 0
	for slot := 0; slot < len(records); slot++ {
		if queryCanceled(pq) {
			return 0, errQueryCanceled
		}
		entry, ok := vol.overlayEntry(records, latest, slot, map[int32]struct{}{}, pathCache)
		if !ok {
			continue
		}
		if entryMatchesWithContent(vol, entry, pq, pq.MatchPath) {
			count++
		}
	}
	return count, nil
}

type rankedOverlayEntry struct {
	entry Entry
	rank  int
}

func (vol *serviceVolumeIndex) mergeRankedOverlayEntries(base []Entry, overlay []rankedOverlayEntry, limit int, countOnly bool, pq parsedQuery) []Entry {
	if len(overlay) == 0 {
		return base
	}
	out := make([]Entry, 0, len(base)+len(overlay))
	overlayPos := 0
	for _, entry := range base {
		baseRank := vol.baseEntryRank(entry, pq)
		for overlayPos < len(overlay) && overlay[overlayPos].rank <= baseRank {
			out = append(out, overlay[overlayPos].entry)
			overlayPos++
			if !countOnly && limit > 0 && len(out) >= limit {
				return out
			}
		}
		out = append(out, entry)
		if !countOnly && limit > 0 && len(out) >= limit {
			return out
		}
	}
	for overlayPos < len(overlay) {
		out = append(out, overlay[overlayPos].entry)
		overlayPos++
		if !countOnly && limit > 0 && len(out) >= limit {
			return out
		}
	}
	return out
}

func (vol *serviceVolumeIndex) overlayEntryRank(entry Entry, pq parsedQuery) int {
	if pq.SortColumn == "size" {
		return vol.entrySizeRank(entry)*2 + 1
	}
	if pq.SortColumn == "modified" {
		return vol.entryModifiedRank(entry)*2 + 1
	}
	if pq.SortColumn == "extension" {
		return vol.entryExtensionRank(entry)*2 + 1
	}
	if pq.SortColumn == "type" {
		return vol.entryTypeRank(entry)*2 + 1
	}
	if pq.SortColumn == "path" {
		return vol.overlayEntryPathRank(entry)
	}
	return vol.overlayEntryNameRank(entry)
}

func (vol *serviceVolumeIndex) baseEntryRank(entry Entry, pq parsedQuery) int {
	if pq.SortColumn == "size" {
		return vol.entrySizeRank(entry) * 2
	}
	if pq.SortColumn == "modified" {
		return vol.entryModifiedRank(entry) * 2
	}
	if pq.SortColumn == "extension" {
		return vol.entryExtensionRank(entry) * 2
	}
	if pq.SortColumn == "type" {
		return vol.entryTypeRank(entry) * 2
	}
	if pq.SortColumn == "path" {
		return vol.entryPathRank(entry) * 2
	}
	return vol.baseEntryNameRank(entry)
}

func (vol *serviceVolumeIndex) entrySizeRank(entry Entry) int {
	if vol == nil || vol.index == nil {
		return 0
	}
	order := vol.sizeOrderForRank()
	pos := sort.Search(len(order), func(i int) bool {
		return vol.effectiveRecordSize(int(order[i])) >= entry.Size
	})
	return pos
}

// effectiveRecordSize is the size a record is ranked by: a directory's
// recursive subtree total (from the persisted SUBS column) or a file's own
// size.  The live overlay delta is intentionally excluded so the value stays
// monotonic in the persisted size order the binary search walks.
func (vol *serviceVolumeIndex) effectiveRecordSize(id int) int64 {
	if vol == nil || vol.index == nil || id < 0 || id >= vol.index.compactRecordCount() {
		return 0
	}
	rec := vol.index.compactRecord(id)
	if rec.Mode&uint32(os.ModeDir) != 0 && id < len(vol.subtreeBytes) {
		return int64(vol.subtreeBytes[id])
	}
	return rec.Size
}

func (vol *serviceVolumeIndex) entryModifiedRank(entry Entry) int {
	if vol == nil || vol.index == nil {
		return 0
	}
	order := vol.modifiedOrderForRank()
	recordCount := vol.index.compactRecordCount()
	pos := sort.Search(len(order), func(i int) bool {
		id := int(order[i])
		if id < 0 || id >= recordCount {
			return false
		}
		mod := vol.index.compactRecord(id).ModUnix
		if entry.ModUnix == 0 {
			return mod == 0
		}
		return mod <= entry.ModUnix
	})
	return pos
}

func (vol *serviceVolumeIndex) entryExtensionRank(entry Entry) int {
	if vol == nil || vol.index == nil {
		return 0
	}
	ext := strings.TrimPrefix(filepath.Ext(entry.Name), ".")
	ext = strings.ToLower(ext)
	name := entry.LowerName
	if name == "" {
		name = strings.ToLower(entry.Name)
	}
	order := vol.extensionOrderForRank()
	recordCount := vol.index.compactRecordCount()
	pos := sort.Search(len(order), func(i int) bool {
		id := int(order[i])
		if id < 0 || id >= recordCount {
			return false
		}
		recExt := compactRecordLowerExt(vol.index.compactRecord(id))
		if recExt != ext {
			return recExt >= ext
		}
		return vol.index.compactLowerNameAt(id) >= name
	})
	return pos
}

func (vol *serviceVolumeIndex) entryTypeRank(entry Entry) int {
	if vol == nil || vol.index == nil {
		return 0
	}
	class := 1
	if entry.Mode&uint32(os.ModeDir) != 0 {
		class = 0
	}
	name := entry.LowerName
	if name == "" {
		name = strings.ToLower(entry.Name)
	}
	order := vol.typeOrderForRank()
	recordCount := vol.index.compactRecordCount()
	pos := sort.Search(len(order), func(i int) bool {
		id := int(order[i])
		if id < 0 || id >= recordCount {
			return false
		}
		rec := vol.index.compactRecord(id)
		recClass := compactRecordTypeRank(rec)
		if recClass != class {
			return recClass >= class
		}
		return vol.index.compactLowerNameAt(id) >= name
	})
	return pos
}

func (vol *serviceVolumeIndex) entryPathRank(entry Entry) int {
	if vol == nil || vol.index == nil {
		return 0
	}
	path := entry.LowerPath
	if path == "" {
		path = strings.ToLower(entry.Path)
	}
	order := vol.pathOrderForRank()
	recordCount := vol.index.compactRecordCount()
	cache := make(map[int]string)
	pos := sort.Search(len(order), func(i int) bool {
		id := int(order[i])
		if id < 0 || id >= recordCount {
			return false
		}
		return strings.ToLower(vol.index.reconstructCompactPathCached(id, cache)) >= path
	})
	return pos
}

func (vol *serviceVolumeIndex) overlayEntryPathRank(entry Entry) int {
	if vol == nil || vol.index == nil {
		return 0
	}
	path := entry.LowerPath
	if path == "" {
		path = strings.ToLower(entry.Path)
	}
	order := vol.pathOrderForRank()
	recordCount := vol.index.compactRecordCount()
	cache := make(map[int]string)
	pos := sort.Search(len(order), func(i int) bool {
		id := int(order[i])
		if id < 0 || id >= recordCount {
			return false
		}
		return strings.ToLower(vol.index.reconstructCompactPathCached(id, cache)) >= path
	})
	if pos < len(order) {
		id := int(order[pos])
		if id >= 0 && id < recordCount && strings.ToLower(vol.index.reconstructCompactPathCached(id, cache)) == path {
			return pos*2 + 1
		}
	}
	return pos*2 - 1
}

func (vol *serviceVolumeIndex) overlayEntryNameRank(entry Entry) int {
	name := entry.LowerName
	if name == "" {
		name = strings.ToLower(entry.Name)
	}
	if vol == nil || vol.index == nil {
		return 0
	}
	order := vol.mappedOrCompactNameOrder()
	recordCount := vol.index.compactRecordCount()
	pos := sort.Search(len(order), func(i int) bool {
		id := int(order[i])
		if id < 0 || id >= recordCount {
			return false
		}
		return vol.index.compactLowerNameAt(id) >= name
	})
	if pos < len(order) {
		id := int(order[pos])
		if id >= 0 && id < recordCount && vol.index.compactLowerNameAt(id) == name {
			return pos*2 + 1
		}
	}
	return pos*2 - 1
}

func (vol *serviceVolumeIndex) baseEntryNameRank(entry Entry) int {
	name := entry.LowerName
	if name == "" {
		name = strings.ToLower(entry.Name)
	}
	if vol == nil || vol.index == nil {
		return 0
	}
	order := vol.mappedOrCompactNameOrder()
	recordCount := vol.index.compactRecordCount()
	pos := sort.Search(len(order), func(i int) bool {
		id := int(order[i])
		if id < 0 || id >= recordCount {
			return false
		}
		return vol.index.compactLowerNameAt(id) >= name
	})
	return pos * 2
}

func (vol *serviceVolumeIndex) mappedOrCompactNameOrder() []uint32 {
	if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.nameOrder) > 0 {
		return vol.queryIndex.nameOrder
	}
	if vol == nil || vol.index == nil {
		return nil
	}
	if len(vol.index.CompactNameOrder) == 0 {
		return nil
	}
	out := make([]uint32, len(vol.index.CompactNameOrder))
	for i, id := range vol.index.CompactNameOrder {
		out[i] = uint32(id)
	}
	return out
}

func (vol *serviceVolumeIndex) sizeOrderForRank() []uint32 {
	if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.sizeOrder) > 0 {
		return vol.queryIndex.sizeOrder
	}
	if vol != nil && vol.index != nil && len(vol.index.Derived.SizeOrder) > 0 {
		return vol.index.Derived.SizeOrder
	}
	if vol == nil || vol.index == nil || !vol.index.compactHasSize() {
		return nil
	}
	order, _ := buildCompactSizeOrderRank(vol.index)
	return order
}

func (vol *serviceVolumeIndex) modifiedOrderForRank() []uint32 {
	if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.modOrder) > 0 {
		return vol.queryIndex.modOrder
	}
	if vol != nil && vol.index != nil && len(vol.index.Derived.ModOrder) > 0 {
		return vol.index.Derived.ModOrder
	}
	if vol == nil || vol.index == nil || !vol.index.compactHasModTime() {
		return nil
	}
	order, _ := buildCompactModifiedOrderRank(vol.index)
	return order
}

func (vol *serviceVolumeIndex) extensionOrderForRank() []uint32 {
	if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.extOrder) > 0 {
		return vol.queryIndex.extOrder
	}
	if vol != nil && vol.index != nil && len(vol.index.Derived.ExtOrder) > 0 {
		return vol.index.Derived.ExtOrder
	}
	if vol == nil || vol.index == nil {
		return nil
	}
	order, _ := buildCompactExtensionOrderRank(vol.index)
	return order
}

func (vol *serviceVolumeIndex) typeOrderForRank() []uint32 {
	if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.typeOrder) > 0 {
		return vol.queryIndex.typeOrder
	}
	if vol != nil && vol.index != nil && len(vol.index.Derived.TypeOrder) > 0 {
		return vol.index.Derived.TypeOrder
	}
	if vol == nil || vol.index == nil {
		return nil
	}
	order, _ := buildCompactTypeOrderRank(vol.index)
	return order
}

func (vol *serviceVolumeIndex) pathOrderForRank() []uint32 {
	if vol != nil && vol.queryIndex != nil && len(vol.queryIndex.pathOrder) > 0 {
		return vol.queryIndex.pathOrder
	}
	if vol != nil && vol.index != nil && len(vol.index.Derived.PathOrder) > 0 {
		return vol.index.Derived.PathOrder
	}
	if vol == nil || vol.index == nil {
		return nil
	}
	order, _ := buildCompactPathOrderRank(vol.index)
	return order
}

func latestOverlaySlotsByFRN(records []CompactRecord) map[uint64]int32 {
	latest := make(map[uint64]int32, len(records))
	for i := len(records) - 1; i >= 0; i-- {
		frn := records[i].FRN
		if frn == 0 {
			continue
		}
		if _, exists := latest[frn]; !exists {
			latest[frn] = int32(i)
		}
	}
	return latest
}

func (vol *serviceVolumeIndex) overlayEntry(records []CompactRecord, latest map[uint64]int32, slot int, seen map[int32]struct{}, pathCache map[int]string) (Entry, bool) {
	if vol == nil || slot < 0 || slot >= len(records) {
		return Entry{}, false
	}
	if _, ok := seen[int32(slot)]; ok {
		return Entry{}, false
	}
	seen[int32(slot)] = struct{}{}
	rec := records[slot]
	if latestSlot := latest[rec.FRN]; latestSlot != int32(slot) {
		return Entry{}, false
	}
	if rec.Deleted {
		return Entry{}, false
	}
	path := vol.overlayRecordPath(records, latest, slot, seen, pathCache)
	if path == "" {
		return Entry{}, false
	}
	return Entry{
		Path:        path,
		Name:        rec.Name,
		LowerName:   strings.ToLower(rec.Name),
		LowerPath:   strings.ToLower(path),
		Mode:        rec.Mode,
		Size:        rec.Size,
		ModUnix:     rec.ModUnix,
		IndexSource: vol.index.Source,
		FRN:         rec.FRN,
	}, true
}

func (vol *serviceVolumeIndex) overlayRecordPath(records []CompactRecord, latest map[uint64]int32, slot int, seen map[int32]struct{}, pathCache map[int]string) string {
	if pathCache == nil {
		pathCache = make(map[int]string)
	}
	if slot < 0 || slot >= len(records) {
		return ""
	}
	rec := records[slot]
	if rec.Deleted {
		return ""
	}
	name := rec.Name
	if name == "" {
		name = "."
	}
	if rec.ParentFRN == 0 || rec.ParentFRN == rec.FRN {
		if vol.volume != "" && name != "." {
			return vol.volume + `\` + name
		}
		if vol.volume != "" {
			return vol.volume + `\`
		}
		return name
	}
	if parentSlot, ok := latest[rec.ParentFRN]; ok && parentSlot >= 0 {
		if _, ok := seen[parentSlot]; ok {
			return ""
		}
		parentPath := vol.overlayRecordPath(records, latest, int(parentSlot), seen, pathCache)
		if parentPath != "" {
			return joinOverlayPath(parentPath, name)
		}
		return ""
	}
	if parentID, ok := vol.idForFRN(rec.ParentFRN); ok {
		parentPath := vol.index.reconstructCompactPathCached(parentID, pathCache)
		if parentPath != "" {
			return joinOverlayPath(parentPath, name)
		}
	}
	if vol.volume != "" {
		return vol.volume + `\` + name
	}
	return name
}

func joinOverlayPath(parentPath, name string) string {
	if parentPath == "" {
		return name
	}
	if strings.HasSuffix(parentPath, `\`) || strings.HasSuffix(parentPath, `/`) {
		return parentPath + name
	}
	if vol := filepath.VolumeName(parentPath); vol != "" && strings.EqualFold(parentPath, vol) {
		return parentPath + `\` + name
	}
	return filepath.Join(parentPath, name)
}

func prioritizeServiceVolumesForPathTerms(volumes []*serviceVolumeIndex, opts queryOptions) []*serviceVolumeIndex {
	if len(volumes) < 2 || opts.Under != "" {
		return volumes
	}
	pq, err := parseQuery(opts)
	if err != nil || !pq.MatchPath {
		return volumes
	}
	type scoredVolume struct {
		vol          *serviceVolumeIndex
		matchedTerms int
		score        int
		pos          int
	}
	scored := make([]scoredVolume, 0, len(volumes))
	hasScore := false
	for i, vol := range volumes {
		score := 0
		matchedTerms := 0
		if vol != nil && vol.queryIndex != nil {
			for _, term := range pq.Terms {
				if len(term) < 3 || isVolumeQueryTerm(term) || strings.ContainsAny(term, `\/*?[]:`) {
					continue
				}
				if hits := vol.componentPostingCount(term); hits > 0 {
					matchedTerms++
					score += 1_000_000 / max(1, hits)
				}
				if ext := strings.TrimPrefix(term, "."); ext != "" && bareExtensionCandidateTerm(ext) {
					if hits := vol.extPostingCount(ext); hits > 0 {
						score += 10_000_000 + 2_000_000/max(1, hits)
					}
				}
			}
		}
		if score > 0 {
			hasScore = true
		}
		scored = append(scored, scoredVolume{vol: vol, matchedTerms: matchedTerms, score: score, pos: i})
	}
	if !hasScore {
		return volumes
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].matchedTerms != scored[j].matchedTerms {
			return scored[i].matchedTerms > scored[j].matchedTerms
		}
		if scored[i].score == scored[j].score {
			return scored[i].pos < scored[j].pos
		}
		return scored[i].score > scored[j].score
	})
	out := make([]*serviceVolumeIndex, len(scored))
	for i, item := range scored {
		out[i] = item.vol
	}
	return out
}

func lockVolumeSearch(vol *serviceVolumeIndex, opts queryOptions) (bool, bool) {
	if vol == nil {
		return false, false
	}
	pq := parsedQuery{DeadlineUnix: opts.DeadlineUnix, Cancel: opts.Cancel}
	return false, !queryCanceled(pq)
}

func filterImplicitUnderExisting(matches []Entry, opts queryOptions, countOnly bool) []Entry {
	if countOnly || opts.Exists || opts.Under == "" || len(matches) == 0 {
		return matches
	}
	info, err := os.Stat(opts.Under)
	if err != nil || !info.IsDir() {
		return matches
	}
	out := matches[:0]
	for _, entry := range matches {
		if _, err := os.Stat(entry.Path); err == nil {
			out = append(out, entry)
		}
	}
	return out
}

func filesystemUnderFallbackSearch(opts queryOptions, countOnly bool) ([]Entry, bool, bool) {
	return filesystemUnderFallbackSearchLimited(opts, countOnly, filesystemFallbackMaxVisited, filesystemFallbackMaxDuration)
}

func filesystemUnderFallbackSearchLimited(opts queryOptions, countOnly bool, maxVisited int, maxDuration time.Duration) ([]Entry, bool, bool) {
	if countOnly || opts.Under == "" || isVolumeRoot(opts.Under) {
		return nil, false, true
	}
	root := normalizeFilterPath(opts.Under)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, false, true
	}
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, false, true
	}
	limit := normalizedLimit(opts.Limit, false)
	pq.Limit = limit
	matches := make([]Entry, 0, min(limit, 128))
	deadline := time.Time{}
	if maxDuration > 0 {
		deadline = time.Now().Add(maxDuration)
	}
	visited := 0
	complete := true
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		visited++
		if maxVisited > 0 && visited > maxVisited {
			complete = false
			return filepath.SkipAll
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			complete = false
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		entry := Entry{
			Path:      path,
			Name:      d.Name(),
			LowerPath: strings.ToLower(path),
			LowerName: strings.ToLower(d.Name()),
			Size:      info.Size(),
			Mode:      uint32(info.Mode()),
			ModUnix:   info.ModTime().UnixNano(),
		}
		if entryMatches(entry, pq, pq.MatchPath) {
			matches = append(matches, entry)
			if limit > 0 && len(matches) >= limit {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if len(matches) == 0 {
		return nil, false, complete
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].LowerName == matches[j].LowerName {
			return matches[i].LowerPath < matches[j].LowerPath
		}
		return matches[i].LowerName < matches[j].LowerName
	})
	return matches, true, complete
}
