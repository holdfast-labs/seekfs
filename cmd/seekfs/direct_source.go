package main

// The direct builder is intentionally independent from Index and
// buildOrders.  It is the bounded ingestion seam for the direct filesystem
// to v9 path: sources write records into owned external-sort runs, final IDs
// are assigned once by FRN, and the existing v9 reader contract is emitted
// directly from the final spool.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	directDefaultRunRecords = 64 * 1024
	directDefaultRunBytes   = 64 * 1024 * 1024
	directSpoolHeaderBytes  = 8 + 8 + 4 + 8 + 8 + 4 + 4 + 2

	// directDefaultMaxInaccessible allows a handful of transient permission
	// or race failures during a large filesystem walk without discarding hours
	// of work.  The build reports SourceDegraded when the bound is hit so the
	// operator knows the published index skipped a few paths.
	directDefaultMaxInaccessible = 64
)

var errDirectDuplicateFRN = errors.New("direct source contains duplicate FRN")

// directRecord is the only source-to-spool representation.  It is fixed
// width apart from Name, so a source can be read incrementally and no Index
// object is needed between ingestion and output.
type directRecord struct {
	FRN       uint64
	ParentFRN uint64
	Mode      uint32
	Size      int64
	ModUnix   int64
	Name      string
	// Path is the canonical source path when the source can provide it.  A
	// missing path is deliberate for the elevation-free MFT/USN adapters; the
	// path rank then uses the folded name as its conservative fallback.
	Path string
	// Seq is the NTFS file-reference sequence number when the source knows it.
	// 0 means unknown (walk and synthetic sources).
	Seq uint16
}

type directRecordSource interface {
	Next(context.Context) (directRecord, error)
}

type directBuildOptions struct {
	OutputPath string
	SpoolDir   string
	Roots      []string
	Volume     string
	Source     string
	BuiltAt    time.Time
	JournalID  uint64
	Checkpoint int64
	Records    directRecordSource

	// RunRecords and RunBytes are hard bounds for each external-sort run.
	// The builder never allocates a source-sized slice.
	RunRecords  int
	RunBytes    int64
	RankWorkers int
	WalkWorkers int
	WalkQueue   int

	// WalkReport is populated by the read-only filesystem walk.  It is kept
	// outside the record spool so skipped/inaccessible paths remain bounded.
	WalkReport *directWalkReport

	// MaxInaccessible bounds how many inaccessible paths may be skipped before
	// the source-completeness gate refuses to publish an index.  A transient
	// permission/race must not turn a multi-hour build into a wasted run, but a
	// grossly incomplete walk must not be silently persisted.
	MaxInaccessible int

	// PhaseReporter receives per-phase build durations; it may be called from
	// worker goroutines during the rank-family phase, so implementations must be
	// safe for concurrent use.
	PhaseReporter func(name string, d time.Duration)
}

type directBuildStats struct {
	Records                  int                   `json:"records"`
	Runs                     int                   `json:"runs"`
	SpoolBytes               int64                 `json:"spool_bytes"`
	ScratchBytes             int64                 `json:"scratch_bytes"`
	MaxRunBytes              int64                 `json:"max_run_bytes"`
	NameBlobBytes            int64                 `json:"name_blob_bytes"`
	TokenBytes               int64                 `json:"token_table_bytes"`
	RecordBytes              int64                 `json:"record_table_bytes"`
	RankBytes                int64                 `json:"rank_section_bytes"`
	OutputBytes              int64                 `json:"output_bytes"`
	RuntimeHeap              uint64                `json:"runtime_heap_bytes"`
	Duration                 time.Duration         `json:"duration"`
	RankRecords              int                   `json:"rank_records"`
	ParentMisses             int                   `json:"parent_misses"`
	Sections                 []string              `json:"sections"`
	RankFamilies             []directRankReport    `json:"rank_families,omitempty"`
	SectionReports           []directSectionReport `json:"section_reports,omitempty"`
	FinalIDRule              string                `json:"final_id_rule"`
	SpoolSchema              string                `json:"spool_schema"`
	SourceComplete           bool                  `json:"source_complete,omitempty"`
	SourceSkipped            int                   `json:"source_skipped,omitempty"`
	SourceInaccessible       int                   `json:"source_inaccessible,omitempty"`
	SourceExcluded           int                   `json:"source_excluded,omitempty"`
	SourceReparseSkipped     int                   `json:"source_reparse_skipped,omitempty"`
	SourceReparseNotFollowed int                   `json:"source_reparse_not_followed,omitempty"`
	SourceExclusions         []string              `json:"source_exclusions,omitempty"`
	SourceSkipExamples       []string              `json:"source_skip_examples,omitempty"`
	SourceDegraded           bool                  `json:"source_degraded,omitempty"`
	SourceMaxInaccessible    int                   `json:"source_max_inaccessible,omitempty"`
	PhaseMillis              []string              `json:"phase_millis,omitempty"`
}

type directRunFile struct {
	path  string
	bytes int64
}

type directRankItem struct {
	Key string
	ID  uint32
}

type directRankSpec struct {
	Tag  uint32
	Name string
	Key  func(directRecord) string
	// KeyWithID, when set, overrides Key and receives the record id so the
	// size rank can order directories by their recursive subtree total.
	KeyWithID func(uint32, directRecord) string
}

type directRankReport struct {
	Name        string `json:"name"`
	Tag         uint32 `json:"tag"`
	Runs        int    `json:"runs"`
	Bytes       int64  `json:"section_bytes"`
	RunBytes    int64  `json:"run_bytes"`
	MaxRunBytes int64  `json:"max_run_bytes"`
}

type directSectionReport struct {
	Name         string `json:"name"`
	Tag          uint32 `json:"tag"`
	Runs         int    `json:"runs"`
	Bytes        int64  `json:"section_bytes"`
	ScratchBytes int64  `json:"scratch_bytes"`
}

type directRankHeap struct {
	items []itemWithRun
}

func (h directRankHeap) Len() int           { return len(h.items) }
func (h directRankHeap) Less(i, j int) bool { return h.items[i].less(h.items[j]) }
func (h directRankHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *directRankHeap) Push(x any)        { h.items = append(h.items, x.(itemWithRun)) }
func (h *directRankHeap) Pop() any {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}

type directRunHead struct {
	rec directRecord
	run int
}

type directRunHeap struct {
	items []directRunHead
}

func (h directRunHeap) Len() int { return len(h.items) }
func (h directRunHeap) Less(i, j int) bool {
	a, b := h.items[i].rec, h.items[j].rec
	if a.FRN != b.FRN {
		return a.FRN < b.FRN
	}
	if a.ParentFRN != b.ParentFRN {
		return a.ParentFRN < b.ParentFRN
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.Mode != b.Mode {
		return a.Mode < b.Mode
	}
	if a.Size != b.Size {
		return a.Size < b.Size
	}
	return a.ModUnix < b.ModUnix
}
func (h directRunHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *directRunHeap) Push(x any)   { h.items = append(h.items, x.(directRunHead)) }
func (h *directRunHeap) Pop() any {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}

type directSliceSource struct {
	records []directRecord
	pos     int
}

type directSyntheticSource struct {
	next, remaining int
}

type directMFTSource struct {
	entries *mftTable
	pos     int
}

func newDirectMFTSource(entries *mftTable) directRecordSource {
	return &directMFTSource{entries: entries}
}

func (s *directMFTSource) Next(ctx context.Context) (directRecord, error) {
	select {
	case <-ctx.Done():
		return directRecord{}, ctx.Err()
	default:
	}
	if s.entries == nil {
		return directRecord{}, io.EOF
	}
	for s.pos < len(s.entries.recs) {
		frn := uint64(s.pos)
		s.pos++
		rec := &s.entries.recs[frn]
		if rec.flags&mftRecordPresent == 0 {
			continue
		}
		e := mftEntry{
			frn:       frn,
			parentFRN: rec.parentFRN,
			name:      s.entries.nameAt(rec),
			attr:      rec.attr,
			size:      rec.size,
			modUnix:   rec.modUnix,
			isDir:     rec.flags&mftRecordDir != 0,
			inUse:     rec.flags&mftRecordInUse != 0,
			seq:       rec.seq,
			parentSeq: rec.parentSeq,
		}
		parentFRN := e.parentFRN
		if parentFRN == e.frn {
			parentFRN = 0
		}
		return directRecord{
			FRN:       e.frn,
			ParentFRN: parentFRN,
			Mode:      modeFromAttrs(e.attr),
			Size:      e.size,
			ModUnix:   e.modUnix,
			Name:      e.name,
			Seq:       e.seq,
		}, nil
	}
	return directRecord{}, io.EOF
}

type directUSNSource struct {
	nodes map[uint64]usnNode
	frns  []uint64
	pos   int
}

func newDirectUSNSource(nodes map[uint64]usnNode) directRecordSource {
	frns := make([]uint64, 0, len(nodes))
	for frn := range nodes {
		frns = append(frns, frn)
	}
	sort.Slice(frns, func(i, j int) bool { return frns[i] < frns[j] })
	return &directUSNSource{nodes: nodes, frns: frns}
}

func (s *directUSNSource) Next(ctx context.Context) (directRecord, error) {
	select {
	case <-ctx.Done():
		return directRecord{}, ctx.Err()
	default:
	}
	if s.pos >= len(s.frns) {
		return directRecord{}, io.EOF
	}
	n := s.nodes[s.frns[s.pos]]
	s.pos++
	return directRecord{FRN: n.frn, ParentFRN: n.parentFRN, Mode: modeFromAttrs(n.attr), Name: n.name, Seq: n.seq}, nil
}

func (s *directSyntheticSource) Next(ctx context.Context) (directRecord, error) {
	select {
	case <-ctx.Done():
		return directRecord{}, ctx.Err()
	default:
	}
	if s.remaining == 0 {
		return directRecord{}, io.EOF
	}
	id := s.next
	s.next++
	s.remaining--
	return directRecord{
		FRN:       uint64(id + 1),
		ParentFRN: uint64(id),
		Size:      int64(id % 100000),
		ModUnix:   int64(1_700_000_000_000_000_000 + id),
		Name:      fmt.Sprintf("prototype-%05d.txt", id%100000),
		Path:      fmt.Sprintf(`synthetic:\prototype-%05d.txt`, id%100000),
	}, nil
}

func (s *directSliceSource) Next(ctx context.Context) (directRecord, error) {
	select {
	case <-ctx.Done():
		return directRecord{}, ctx.Err()
	default:
	}
	if s.pos >= len(s.records) {
		return directRecord{}, io.EOF
	}
	rec := s.records[s.pos]
	s.pos++
	return rec, nil
}

// newDirectSliceSource is used by deterministic fixtures and by the
// source-order invariance tests.  Production sources implement the same
// small interface and may stream from USN/MFT or a walk.
func newDirectSliceSource(records []directRecord) directRecordSource {
	return &directSliceSource{records: records}
}

type directWalkSource struct {
	records chan directRecord
	errs    chan error
	done    chan struct{}
	once    sync.Once
	report  *directWalkReport
}

func newDirectWalkSource(root string) (directRecordSource, error) {
	return newDirectWalkSourceWithExclusions(root, nil, nil, nil)
}

// directWalkReport is intentionally counter-based.  A full list of skipped
// paths can be larger than the builder's memory budget, so only a small sample
// is retained for diagnostics while the complete counts are emitted in the
// build report.
type directWalkReport struct {
	Root               string
	Exclusions         []string
	Skipped            int
	Inaccessible       int
	Excluded           int
	ReparseSkipped     int
	ReparseNotFollowed int
	SkipExamples       []string
	SourceComplete     bool
}

func (r *directWalkReport) note(kind string, path string) {
	if r == nil {
		return
	}
	switch kind {
	case "skipped":
		r.Skipped++
	case "inaccessible":
		r.Inaccessible++
	case "excluded":
		r.Excluded++
	case "reparse":
		r.ReparseSkipped++
	case "reparse-not-followed":
		r.ReparseNotFollowed++
	}
	if len(r.SkipExamples) < 64 && path != "" {
		r.SkipExamples = append(r.SkipExamples, kind+":"+path)
	}
}

func newDirectWalkSourceWithExclusions(root string, exclusionRoots, exclusionSuffixes []string, report *directWalkReport) (directRecordSource, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	if directPathIsReparse(abs) {
		return nil, fmt.Errorf("direct walk root is a reparse point: %s", abs)
	}
	canonicalExclusions := make([]string, 0, len(exclusionRoots))
	for _, exclusion := range exclusionRoots {
		if exclusion == "" {
			continue
		}
		canonical, canonicalErr := filepath.Abs(exclusion)
		if canonicalErr != nil {
			return nil, canonicalErr
		}
		canonical, canonicalErr = filepath.EvalSymlinks(canonical)
		if canonicalErr != nil {
			// A not-yet-created builder directory is still a valid exclusion;
			// canonicalize its existing parent and append the requested leaf.
			parent := filepath.Dir(canonical)
			parent, canonicalErr = filepath.EvalSymlinks(parent)
			if canonicalErr != nil {
				return nil, canonicalErr
			}
			canonical = filepath.Join(parent, filepath.Base(canonical))
		}
		canonicalExclusions = append(canonicalExclusions, filepath.Clean(canonical))
	}
	if report != nil {
		report.Root = abs
		report.Exclusions = append([]string(nil), canonicalExclusions...)
		report.SourceComplete = true
	}
	s := &directWalkSource{
		records: make(chan directRecord, 256),
		errs:    make(chan error, 1),
		done:    make(chan struct{}),
		report:  report,
	}
	go func() {
		walkErr := filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if report != nil {
					report.SourceComplete = false
					report.note("inaccessible", path)
				}
				return nil
			}
			if directPathUnderAny(path, canonicalExclusions) {
				if report != nil {
					report.note("excluded", path)
				}
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			select {
			case <-s.done:
				return context.Canceled
			default:
			}
			if directPathIsReparse(path) {
				if report != nil {
					report.SourceComplete = false
					report.note("reparse", path)
				}
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			lowerPath := strings.ToLower(path)
			for _, suffix := range exclusionSuffixes {
				if strings.HasSuffix(lowerPath, strings.ToLower(suffix)) {
					if report != nil {
						report.note("excluded", path)
					}
					return nil
				}
			}
			info, infoErr := d.Info()
			if infoErr != nil {
				if report != nil {
					report.SourceComplete = false
					report.note("inaccessible", path)
				}
				return nil
			}
			frn := directStablePathID(path)
			parent := uint64(0)
			clean := filepath.Clean(path)
			parentPath := filepath.Dir(clean)
			if filepath.Clean(abs) != clean {
				parent = directStablePathID(parentPath)
			}
			rec := directRecord{
				FRN:       frn,
				ParentFRN: parent,
				Mode:      uint32(info.Mode()),
				Size:      info.Size(),
				ModUnix:   directWalkModUnix(info),
				Name:      d.Name(),
				Path:      clean,
			}
			select {
			case s.records <- rec:
				return nil
			case <-s.done:
				return context.Canceled
			}
		})
		close(s.records)
		s.errs <- walkErr
		close(s.errs)
	}()
	return s, nil
}

func directPathUnderAny(path string, roots []string) bool {
	path = filepath.Clean(path)
	for _, root := range roots {
		root = filepath.Clean(root)
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func directPathIsReparse(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func (s *directWalkSource) Next(ctx context.Context) (directRecord, error) {
	select {
	case <-ctx.Done():
		return directRecord{}, ctx.Err()
	case rec, ok := <-s.records:
		if ok {
			return rec, nil
		}
		if err := <-s.errs; err != nil && !errors.Is(err, context.Canceled) {
			return directRecord{}, err
		}
		return directRecord{}, io.EOF
	}
}

func (s *directWalkSource) Close() { s.once.Do(func() { close(s.done) }) }

func directStablePathID(path string) uint64 {
	// FNV-1a is stable across processes and adequate for the walk adapter.
	// A collision is rejected by the final-ID merge rather than silently
	// producing ambiguous parent links.
	normal := strings.ToLower(filepath.Clean(path))
	var hash uint64 = 14695981039346656037
	for i := 0; i < len(normal); i++ {
		hash ^= uint64(normal[i])
		hash *= 1099511628211
	}
	if hash == 0 {
		return 1
	}
	return hash
}

func directWalkModUnix(info os.FileInfo) int64 {
	// Windows directory timestamps can change as directory metadata is read.
	// Keep walk output deterministic; authoritative USN/MFT sources retain
	// directory timestamps when they are available.
	if info.IsDir() {
		return 0
	}
	return info.ModTime().UnixNano()
}
