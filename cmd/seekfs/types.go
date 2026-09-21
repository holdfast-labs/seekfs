package main

import (
	"container/list"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const indexVersion = 9
const servicePathCacheLimit = 25_000
const serviceStartupDefaultWorkers = 2
const serviceResidentNameOrderMaxRecords = 2_000_000
const serviceBackgroundNameOrderMaxRecords = 30_000_000
const serviceExtTopPostingLimit = 512
const serviceResidentChildRangeMaxRecords = 2_000_000
const serviceStartupWALRebuildBytes = 512 * 1024 * 1024
const defaultQueryPostingPrefetchBytes = 32 * 1024 * 1024
const serviceNameTrigramCandidateMaxIDs = 25_000
const servicePathNameTrigramCandidateMaxIDs = 250_000

// serviceSingleTermPNGCDriverMaxIDs bounds the driver posting a single-term
// query may materialize when the selective trigram lane declines (every gram
// over cap or omitted-common) and the complete PNGC intersection lane rescues
// it instead of falling to a global bounded scan.
const serviceSingleTermPNGCDriverMaxIDs = 2_000_000

// serviceShortFoldMaxIDs bounds the posting intersection a 1-2 rune
// companion term may fold: the fold is a per-record substring check, so it
// must not run over an unbounded driver result.
const serviceShortFoldMaxIDs = 4_000_000
const serviceComponentTrigramCandidateMaxIDs = 10_000
const serviceComponentTrigramExpansionMaxIDs = 25_000
const serviceComponentMultiTermScanMaxIDs = 500_000
const serviceCompleteFilenameOrderWalkMaxRecords = 2_000_000
const serviceTrigramParallelVerifyMinIDs = 4_096
const serviceRankParallelMinIDs = 500_000
const serviceNameTrigramDefaultMaxRecords = 20_000_000
const filesystemFallbackMaxVisited = 100_000
const filesystemFallbackMaxDuration = 2 * time.Second
const defaultIdleMemoryRelease = 15 * time.Minute

const (
	nameTrigramStateDisabled int32 = iota
	nameTrigramStatePending
	nameTrigramStateBuilding
	nameTrigramStateReady
)

var indexMagic = [8]byte{'G', 'O', 'S', 'R', 'C', 'H', '0', '9'}
var walMagicV1 = []byte{'S', 'W', 'A', 'L', '1'}

const (
	indexSectionRANK uint32 = 'R'<<24 | 'A'<<16 | 'N'<<8 | 'K'
	indexSectionSRNK uint32 = 'S'<<24 | 'R'<<16 | 'N'<<8 | 'K'
	indexSectionMRNK uint32 = 'M'<<24 | 'R'<<16 | 'N'<<8 | 'K'
	indexSectionERNK uint32 = 'E'<<24 | 'R'<<16 | 'N'<<8 | 'K'
	indexSectionTRNK uint32 = 'T'<<24 | 'R'<<16 | 'N'<<8 | 'K'
	indexSectionPRNK uint32 = 'P'<<24 | 'R'<<16 | 'N'<<8 | 'K'
	indexSectionSUBT uint32 = 'S'<<24 | 'U'<<16 | 'B'<<8 | 'T'
	// SUBS is the per-record recursive directory size (uint64 per record, 0 for
	// files); written immediately after SUBT.  Older readers ignore it.
	indexSectionSUBS uint32 = 'S'<<24 | 'U'<<16 | 'B'<<8 | 'S'
	indexSectionCHLD uint32 = 'C'<<24 | 'H'<<16 | 'L'<<8 | 'D'
	indexSectionFRNS uint32 = 'F'<<24 | 'R'<<16 | 'N'<<8 | 'S'
	indexSectionLOWR uint32 = 'L'<<24 | 'O'<<16 | 'W'<<8 | 'R'
	indexSectionPATR uint32 = 'P'<<24 | 'A'<<16 | 'T'<<8 | 'R'
	indexSectionPEXT uint32 = 'P'<<24 | 'E'<<16 | 'X'<<8 | 'T'
	indexSectionPXRB uint32 = 'P'<<24 | 'X'<<16 | 'R'<<8 | 'B'
	indexSectionPXRC uint32 = 'P'<<24 | 'X'<<16 | 'R'<<8 | 'C'
	indexSectionPCMP uint32 = 'P'<<24 | 'C'<<16 | 'M'<<8 | 'P'
	indexSectionPNGR uint32 = 'P'<<24 | 'N'<<16 | 'G'<<8 | 'R'
	// PNGC is an optional companion to PNGR containing postings for grams
	// omitted from the selective name index.  Older readers ignore it.
	indexSectionPNGC uint32 = 'P'<<24 | 'N'<<16 | 'G'<<8 | 'C'
)

const gramPostingUnionMetadataMagic uint32 = 0x47524d32 // "GRM2"

func globalPlannerEnabled() bool {
	return envBool("SEEKFS_GLOBAL_PLANNER")
}

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

const packedLowerSameAsName = ^uint32(0)

const (
	fsctlEnumUSNData         = 0x000900b3
	fsctlQueryUSNJournal     = 0x000900f4
	fsctlReadUSNJournal      = 0x000900bb
	fileAttributeReadonly    = 0x01
	fileAttributeHidden      = 0x02
	fileAttributeSystem      = 0x04
	fileAttributeDir         = 0x10
	fileAttributeArchive     = 0x20
	usnReasonFileCreate      = 0x00000100
	usnReasonFileDelete      = 0x00000200
	usnReasonRenameOld       = 0x00001000
	usnReasonRenameNew       = 0x00002000
	usnReasonDataOverwrite   = 0x00000001
	usnReasonDataExtend      = 0x00000002
	usnReasonDataTruncation  = 0x00000004
	usnReasonBasicInfoChange = 0x00008000
	usnReasonClose           = 0x80000000
	// usnReasonNeedsInfoRefresh marks the journal reasons after which the file's
	// indexed size/modification time may have changed and must be re-read
	// (matching Everything, which re-reads file metadata on these events).
	usnReasonNeedsInfoRefresh = usnReasonDataOverwrite | usnReasonDataExtend |
		usnReasonDataTruncation | usnReasonBasicInfoChange | usnReasonClose |
		usnReasonFileCreate | usnReasonRenameNew | usnReasonRenameOld
	serviceName                 = "seekfs"
	defaultServicePipe          = `\\.\pipe\seekfs-service`
	defaultServiceSDDL          = `D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;IU)`
	serviceQueryTimeout         = 30 * time.Second
	persistDebounce             = 5 * time.Minute
	overlayCompactionDirtyAge   = 30 * time.Minute
	overlayCompactionMaxSlots   = 64 * 1024
	overlayCompactionMaxWAL     = 64 * 1024 * 1024
	overlayCompactionTombstoneP = 5
	// staleTempSweepInterval bounds how often a running service reaps abandoned
	// index stage temporaries; the sweep itself only removes files older than an
	// hour, so a live persist is never touched.
	staleTempSweepInterval = time.Hour
	// serviceUSNReplayBatchMax bounds the number of USN changes applied in a
	// single replay iteration.  A large backlog is drained across several
	// iterations so search requests are never starved by one long apply.
	serviceUSNReplayBatchMax = 10_000
	// serviceUSNReplayDrainBatches bounds how many batches one replay call
	// applies before yielding.  The volume handle is held open across the
	// drain so a catch-up does not pay a CreateFile + journal query per batch,
	// while still letting the watchdog retire the loop between calls.
	serviceUSNReplayDrainBatches = 8
	compactDiskRecordBytes       = 43
	compactWideDiskRecordBytes   = 45
	compactDiskFlag              = 1
	compactDiskWideRefsFlag      = 2
	compactDiskAttrsFlag         = 4
	compactNarrowParentSentinel  = 0xFFFFFF
	compactNarrowMaxRecordRef    = compactNarrowParentSentinel - 1
	compactWideParentSentinel    = ^uint32(0)
	packedSize64Sentinel         = ^uint32(0)
)

// overlayCompactionSlotFraction scales the persist watermark for large
// volumes: a fold becomes due once the pending overlay reaches this fraction
// of the record count (never below overlayCompactionMaxSlots), so a single
// busy folder cannot force back-to-back multi-GB folds.
const overlayCompactionSlotFraction = 32

type Entry struct {
	Path        string
	Name        string
	LowerPath   string
	LowerName   string
	Size        int64
	Mode        uint32
	ModUnix     int64
	IndexSource string
	// FRN is the NTFS file reference. Content search keys documents on it, so
	// entries built from a compact record or an overlay record carry it for
	// inline content verification. Zero for records that have no FRN.
	FRN uint64
	// Snippet is a short window of matched content text, populated only for
	// content-query results (and only on the local path). Empty otherwise.
	Snippet string
}

type jsonError struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

type jsonResult struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Volume      string `json:"volume,omitempty"`
	IsDir       bool   `json:"is_dir"`
	Size        *int64 `json:"size,omitempty"`
	Modified    string `json:"modified,omitempty"`
	IndexSource string `json:"index_source,omitempty"`
	Snippet     string `json:"snippet,omitempty"`
	Exists      *bool  `json:"exists,omitempty"`
}

func boolPtr(v bool) *bool {
	return &v
}

type jsonSearchResponse struct {
	OK                       bool           `json:"ok"`
	Query                    string         `json:"query"`
	Count                    int            `json:"count"`
	Limit                    int            `json:"limit,omitempty"`
	SearchMS                 float64        `json:"search_ms,omitempty"`
	Source                   string         `json:"source,omitempty"`
	Decline                  string         `json:"decline,omitempty"`
	Candidates               int            `json:"candidates,omitempty"`
	BlocksDecoded            int            `json:"blocks_decoded,omitempty"`
	BlocksSkipped            int            `json:"blocks_skipped,omitempty"`
	ScalarDriver             string         `json:"scalar_driver,omitempty"`
	ScalarInterval           int            `json:"scalar_interval,omitempty"`
	RecordsVerified          int            `json:"records_verified,omitempty"`
	ComponentDriver          string         `json:"component_driver,omitempty"`
	ComponentRoots           int            `json:"component_roots,omitempty"`
	ComponentIntervals       int            `json:"component_intervals,omitempty"`
	ComponentCardinality     int            `json:"component_cardinality,omitempty"`
	ComponentSelfHits        int            `json:"component_self_hits,omitempty"`
	ComponentBounds          string         `json:"component_bounds,omitempty"`
	ComponentRecordsVerified int            `json:"component_records_verified,omitempty"`
	FilenameDriver           string         `json:"filename_driver,omitempty"`
	FilenameRequiredGrams    int            `json:"filename_required_grams,omitempty"`
	FilenamePostingHint      int            `json:"filename_posting_hint,omitempty"`
	FilenameRecordsVerified  int            `json:"filename_records_verified,omitempty"`
	OverlayBaseWindow        int            `json:"overlay_base_window,omitempty"`
	PostingPrefetchBytes     int            `json:"posting_prefetch_bytes,omitempty"`
	PostingPrefetchRanges    int            `json:"posting_prefetch_ranges,omitempty"`
	PostingPrefetchPages     int            `json:"posting_prefetch_pages,omitempty"`
	PlannerMode              string         `json:"planner_mode,omitempty"`
	Fuzzy                    bool           `json:"fuzzy,omitempty"`
	EligibleVolumes          []string       `json:"eligible_volumes,omitempty"`
	Terms                    []traceTerm    `json:"terms,omitempty"`
	Declines                 []traceDecline `json:"declines,omitempty"`
	Fallback                 string         `json:"fallback,omitempty"`
	Complete                 *bool          `json:"complete,omitempty"`
	Content                  *contentHealth `json:"content,omitempty"`
	Results                  []jsonResult   `json:"results,omitempty"`
}

type jsonInfoResponse struct {
	OK          bool         `json:"ok"`
	Version     int          `json:"version"`
	Source      string       `json:"source"`
	BuiltAt     string       `json:"built_at"`
	Entries     int          `json:"entries"`
	Roots       []string     `json:"roots"`
	Volume      string       `json:"volume,omitempty"`
	JournalID   uint64       `json:"journal_id,omitempty"`
	Checkpoint  int64        `json:"checkpoint_usn,omitempty"`
	ContentHash string       `json:"content_hash,omitempty"`
	Layout      *indexLayout `json:"layout,omitempty"`
}

type indexLayout struct {
	FileBytes      int64   `json:"file_bytes,omitempty"`
	RecordBytes    int64   `json:"record_bytes,omitempty"`
	NameBlobBytes  int64   `json:"name_blob_bytes,omitempty"`
	NameTableBytes int64   `json:"name_table_bytes,omitempty"`
	OtherBytes     int64   `json:"other_bytes,omitempty"`
	RecordCount    int     `json:"record_count,omitempty"`
	UniqueNames    int     `json:"unique_names,omitempty"`
	BytesPerRecord float64 `json:"bytes_per_record,omitempty"`
}

type doctorResponse struct {
	OK            bool               `json:"ok"`
	ServiceName   string             `json:"service_name"`
	Installed     bool               `json:"installed"`
	Running       bool               `json:"running"`
	ServiceError  string             `json:"service_error,omitempty"`
	PipeReachable bool               `json:"pipe_reachable"`
	Entries       int                `json:"entries,omitempty"`
	Loading       bool               `json:"loading,omitempty"`
	QueryOK       bool               `json:"query_ok"`
	Message       string             `json:"message,omitempty"`
	DBs           []dbInfo           `json:"dbs,omitempty"`
	Runtime       *runtimeMemoryInfo `json:"runtime,omitempty"`
}

type benchSummary struct {
	OK         bool                `json:"ok"`
	Mode       string              `json:"mode"`
	Iterations int                 `json:"iterations"`
	Failures   int                 `json:"failures"`
	Queries    int                 `json:"queries"`
	Stats      map[string]float64  `json:"stats_ms"`
	Backend    map[string]float64  `json:"backend_stats_ms,omitempty"`
	Sources    map[string]int      `json:"sources,omitempty"`
	Declines   map[string]int      `json:"declines,omitempty"`
	Candidates map[string]float64  `json:"candidate_stats,omitempty"`
	PerQuery   []benchQuerySummary `json:"per_query,omitempty"`
}

type benchQuerySummary struct {
	Query                 string             `json:"query"`
	Iterations            int                `json:"iterations"`
	Failures              int                `json:"failures"`
	Stats                 map[string]float64 `json:"stats_ms"`
	Backend               map[string]float64 `json:"backend_stats_ms,omitempty"`
	Sources               map[string]int     `json:"sources,omitempty"`
	Declines              map[string]int     `json:"declines,omitempty"`
	Candidates            map[string]float64 `json:"candidate_stats,omitempty"`
	ResultHash            string             `json:"result_hash,omitempty"`
	ResultCount           int                `json:"result_count,omitempty"`
	ResultConsistent      bool               `json:"result_consistent"`
	DiagnosticsConsistent bool               `json:"diagnostics_consistent"`
	Diagnostics           *benchDiagnostics  `json:"diagnostics,omitempty"`
}

type benchDiagnostics struct {
	Source          string `json:"source,omitempty"`
	Driver          string `json:"driver,omitempty"`
	Candidates      int    `json:"candidates,omitempty"`
	RecordsVerified int    `json:"records_verified,omitempty"`
	BlocksDecoded   int    `json:"blocks_decoded,omitempty"`
	BlocksSkipped   int    `json:"blocks_skipped,omitempty"`
	Complete        string `json:"complete,omitempty"`
}

type Index struct {
	Version                int
	Roots                  []string
	BuiltAt                time.Time
	Source                 string
	Volume                 string
	JournalID              uint64
	Checkpoint             int64
	ContentHash            string
	Entries                []Entry
	NameOrder              []int
	PathOrder              []int
	Compact                bool
	Records                []CompactRecord
	PackedRecords          *PackedRecords
	MMapRecords            *MMapRecords
	CompactAttrs           bool
	CompactNameOrder       []int
	NameBlob               []byte
	Derived                indexDerivedSections
	DBPath                 string
	baseDeletedState       atomic.Uint32 // 0 unknown, 1 no deleted records, 2 has deleted records
	componentCoverageMu    sync.Mutex
	componentCoverageCache map[string]mappedComponentCoverage
	// dirSizeDelta holds per-base-directory size adjustments from the live
	// overlay, published as an immutable map so lock-free readers see a stable
	// view.  It is reset when the volume is persisted (the new base already
	// includes the overlay).
	dirSizeDelta atomic.Pointer[map[int]int64]
}

type indexDerivedSections struct {
	NameOrder        []uint32
	NameRank         []uint32
	SizeOrder        []uint32
	SizeRank         []uint32
	ModOrder         []uint32
	ModRank          []uint32
	ExtOrder         []uint32
	ExtRank          []uint32
	TypeOrder        []uint32
	TypeRank         []uint32
	PathOrder        []uint32
	PathRank         []uint32
	ChildOffsets     []uint32
	ChildIDs         []uint32
	RootIDs          []uint32
	SubtreeStart     []uint32
	SubtreeEnd       []uint32
	SubtreeOrder     []uint32
	SubtreeSizeRank  []uint32
	SubtreeModRank   []uint32
	SubtreeExtRank   []uint32
	SubtreeTypeRank  []uint32
	SubtreePathRank  []uint32
	SubtreeBytes     []uint64
	FRNs             []uint64
	FRNRecordIDs     []uint32
	LowerBlob        []byte
	LowerOffs        []uint32
	LowerLens        []uint16
	AttrBits         map[uint32][]uint32
	Postings         map[uint32]mappedPostingSection
	PostingBounds    map[uint32]postingRankBounds
	NameTrigrams     *compressedTrigramIndex
	SelfNameTrigrams *compressedTrigramIndex
}

type mappedPostingSection struct {
	EntryCount int
	BlockCount int
	Bytes      int
	Data       []byte
	RankBounds postingRankBounds
}

type postingRankBounds struct {
	BlockCount int
	Name       []uint32
	Size       []uint32
	Modified   []uint32
	Extension  []uint32
	Type       []uint32
	Path       []uint32
}

type postingBlockCacheKey struct {
	base  uintptr
	bytes int
	block int
}

type postingBlockCacheEntry struct {
	key   postingBlockCacheKey
	ids   []uint32
	bytes int64
}

type postingBlockLRU struct {
	mu    sync.Mutex
	ll    list.List
	items map[postingBlockCacheKey]*list.Element
	bytes int64
}

var servicePostingBlockCache postingBlockLRU

type appConfig struct {
	DBs          []string
	Volumes      []string
	ServicePipe  string
	DefaultLimit int
	OutputFormat string
	SeekFSDir    string
	RemoteAddr   string
}

type queryOptions struct {
	Query         string       `json:"query"`
	MatchPath     bool         `json:"match_path"`
	Limit         int          `json:"limit"`
	Under         string       `json:"under,omitempty"`
	Exists        bool         `json:"exists,omitempty"`
	CWDBias       string       `json:"cwd_bias,omitempty"`
	RootBias      string       `json:"root_bias,omitempty"`
	Recent        string       `json:"recent,omitempty"`
	ModifiedAfter string       `json:"modified_after,omitempty"`
	CaseSensitive bool         `json:"case_sensitive,omitempty"`
	Fuzzy         bool         `json:"fuzzy,omitempty"`
	DeadlineUnix  int64        `json:"deadline_unix,omitempty"`
	RequestSeq    int64        `json:"request_seq,omitempty"`
	Cancel        func() bool  `json:"-"`
	Trace         *searchTrace `json:"-"`
	// Per-query content budgets. Zero means "use the default"; they are never
	// globals, so concurrent queries cannot race on each other's limits.
	ContentCandidateBudget int `json:"content_candidate_budget,omitempty"`
	ContentScanVisitBudget int `json:"content_scan_visit_budget,omitempty"`
	// contentCount, when non-nil, makes the content compact scan count matches
	// in place instead of materializing and retaining []Entry. Local-only.
	contentCount *int `json:"-"`
	// parsedOverride, when non-nil, makes parseQuery return this tree instead of
	// re-parsing Query. PF-7b: the filename-only evaluation of a volume whose
	// content index is unusable needs a stripped query that cannot be expressed
	// as a Query string. Local-only; nil on every normal request.
	parsedOverride *parsedQuery `json:"-"`
	// contentFullCandidates disables the bounded content-candidate fast path
	// (bounded posting materialization + broad rank-ordered scan) so a
	// differential test can compare it against the historical full-budget
	// candidate logic. Local-only; false on every normal request.
	contentFullCandidates bool `json:"-"`
	// contentWindow is the enlarged content window the completeness signal is
	// measured against when the verify limit itself is smaller (a single-volume
	// default-order query verifies only its page limit but must still report
	// incompleteness when the candidate superset exceeds the window).
	// Local-only; 0 on every normal request.
	contentWindow int `json:"-"`
	// contentProbe, when non-nil, is the per-volume channel the reduced content
	// path uses to report whether it truncated the posting superset at the
	// completeness window or proved the superset below it. The driver creates a
	// fresh probe per volume search so the signal is never shared across
	// queries. Local-only; nil on every normal request.
	contentProbe *contentCompletenessProbe `json:"-"`
}

// contentCompletenessProbe carries the reduced default-order path's
// completeness evidence from the candidate source back to the volume driver.
// windowCapped means the posting superset exceeded the window and was truncated
// there (so the driver must decide from the page); supersetBelowWindow means the
// whole superset was materialized and is strictly smaller than the window (so
// every match fits below it and the result is complete).
type contentCompletenessProbe struct {
	windowCapped        bool
	supersetBelowWindow bool
}

type parsedQuery struct {
	Raw               string
	Terms             []string
	Fuzzy             bool
	ImplicitPathTerms []string
	Impossible        bool
	MatchPath         bool
	CaseSensitive     bool
	Exts              []string
	Dirs              []string
	Globs             []string
	Regexps           []*regexp.Regexp
	RegexTerms        []string
	Type              string
	Parents           []string
	Under             string
	Exists            bool
	ModifiedAfter     time.Time
	HasModAfter       bool
	SizeFilters       []sizeFilter
	DateFilters       []dateFilter
	AttrFilters       []uint32
	Content           []contentLeaf
	SortColumn        string
	OrGroups          [][]parsedQuery
	NotGroups         []parsedQuery
	CWDBias           string
	RootBias          string
	Limit             int
	CountOnly         bool
	DeadlineUnix      int64
	Cancel            func() bool
	Trace             *searchTrace
	// Per-query content budgets copied from queryOptions; 0 means use default.
	ContentCandidateBudget int
	ContentScanVisitBudget int
	// contentFullCandidates mirrors queryOptions.contentFullCandidates so
	// nameTermCandidates can bypass the bounded content-candidate fast path.
	contentFullCandidates bool
	// contentWindow mirrors queryOptions.contentWindow: the completeness window
	// when the verify limit is smaller than it.
	contentWindow int
	// contentProbe mirrors queryOptions.contentProbe: the per-volume channel a
	// reduced content search reports its completeness evidence through.
	contentProbe *contentCompletenessProbe
	// contentCandidateMeta carries a candidate source's self-report back to
	// searchCompact for the current search call (never shared across queries).
	contentCandidateMeta *contentCandidateMeta
	// hidden is the overlay hidden-base-ID set captured once for this search
	// call and threaded into the bounded candidate scan, so the scan's
	// early-stop guard drops exactly the records the caller's verify loop drops
	// instead of re-reading the volume snapshot and racing a republish.
	hidden hiddenBaseIDs
}

// contentCandidateMeta is the per-search-call channel a candidate source uses to
// tell searchCompact that it already ran the full inline content predicate.
type contentCandidateMeta struct {
	verified bool
}

type searchTrace struct {
	Source                   string
	Decline                  string
	Candidates               int
	PlannerMode              string
	EligibleVolumes          []string
	Terms                    []traceTerm
	Declines                 []traceDecline
	Fallback                 string
	BlocksDecoded            int
	BlocksSkipped            int
	ScalarDriver             string
	ScalarInterval           int
	ScalarRecordsVerified    int
	ComponentDriver          string
	ComponentRoots           int
	ComponentIntervals       int
	ComponentCardinality     int
	ComponentSelfHits        int
	ComponentBounds          string
	ComponentRecordsVerified int
	FilenameDriver           string `json:"filename_driver,omitempty"`
	FilenameRequiredGrams    int    `json:"filename_required_grams,omitempty"`
	FilenamePostingHint      int    `json:"filename_posting_hint,omitempty"`
	FilenameRecordsVerified  int    `json:"filename_records_verified,omitempty"`
	OverlayBaseWindow        int    `json:"overlay_base_window,omitempty"`
	PostingPrefetchBytes     int    `json:"posting_prefetch_bytes,omitempty"`
	PostingPrefetchRanges    int    `json:"posting_prefetch_ranges,omitempty"`
	PostingPrefetchPages     int    `json:"posting_prefetch_pages,omitempty"`
	Complete                 *bool
	// ContentPartial is set when a content query answered from only a subset of
	// the eligible volumes; ContentSkippedVolumes names the volumes left out.
	ContentPartial        bool
	ContentSkippedVolumes []string
	// ContentIncomplete is set when a content query could not evaluate the full
	// candidate superset within the memory budget. The result is a visible
	// degradation, never a silent truncation; a count refuses outright.
	ContentIncomplete bool
	// ContentCountDivergent is set on a content count for the `under:`/Exists
	// shapes: a count never stats, so it can exceed the search result set. The
	// count is still returned; this only makes the divergence visible.
	ContentCountDivergent bool
	// ContentCandidatesVerified records that the content candidate source
	// already ran the full inline content predicate (the rank-ordered bounded
	// content scan), so the downstream verify loop can skip re-evaluating it.
	// Local-only plumbing; never serialized.
	ContentCandidatesVerified bool `json:"-"`
}

type traceTerm struct {
	Term      string `json:"term,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Source    string `json:"source,omitempty"`
	CountHint int    `json:"count_hint,omitempty"`
	Exact     bool   `json:"exact"`
	Volume    string `json:"volume,omitempty"`
}

type traceDecline struct {
	Source string `json:"source,omitempty"`
	Reason string `json:"reason,omitempty"`
	Volume string `json:"volume,omitempty"`
}

func (t *searchTrace) setSource(source string, candidates int) {
	if t == nil {
		return
	}
	if t.Source != "" && source != "exact-empty" {
		return
	}
	t.Source = source
	t.Candidates = candidates
}

func (t *searchTrace) setDecline(reason string) {
	if t == nil || t.Decline != "" {
		return
	}
	t.Decline = reason
	t.addDecline(reason)
}

func (t *searchTrace) setPlannerMode(mode string) {
	if t == nil || t.PlannerMode != "" {
		return
	}
	t.PlannerMode = mode
}

func (t *searchTrace) setEligibleVolumes(volumes []*serviceVolumeIndex) {
	if t == nil || t.EligibleVolumes != nil {
		return
	}
	t.EligibleVolumes = make([]string, 0, len(volumes))
	for _, vol := range volumes {
		if vol == nil || vol.volume == "" {
			continue
		}
		t.EligibleVolumes = append(t.EligibleVolumes, vol.volume)
	}
}

func (t *searchTrace) addPostingBlocks(decoded, skipped int) {
	if t == nil {
		return
	}
	t.BlocksDecoded += decoded
	t.BlocksSkipped += skipped
}

func (t *searchTrace) addComponentStats(driver string, roots, intervals, cardinality, selfHits, recordsVerified int, bounds bool) {
	if t == nil {
		return
	}
	if t.ComponentDriver == "" {
		t.ComponentDriver = driver
	} else if t.ComponentDriver != driver {
		t.ComponentDriver = "mixed"
	}
	t.ComponentRoots += roots
	t.ComponentIntervals += intervals
	t.ComponentCardinality += cardinality
	t.ComponentSelfHits += selfHits
	t.ComponentRecordsVerified += recordsVerified
	if t.ComponentBounds == "" {
		if bounds {
			t.ComponentBounds = "available"
		} else {
			t.ComponentBounds = "unavailable"
		}
	} else if !bounds {
		t.ComponentBounds = "mixed"
	}
}

func (t *searchTrace) replaceDecline(reason string) {
	if t == nil {
		return
	}
	t.Decline = reason
	t.addDecline(reason)
}

func (t *searchTrace) setFallback(route string) {
	if t == nil || t.Fallback != "" {
		return
	}
	t.Fallback = route
}

func (t *searchTrace) setContentIncomplete() {
	if t != nil {
		t.ContentIncomplete = true
	}
}

func (t *searchTrace) setComplete(complete bool) {
	if t == nil {
		return
	}
	t.Complete = boolPtr(complete)
}

func (t *searchTrace) completePtr() *bool {
	if t == nil || t.Complete == nil {
		return boolPtr(true)
	}
	return t.Complete
}

func (t *searchTrace) addTerm(term traceTerm) {
	if t == nil || term.Source == "" {
		return
	}
	t.Terms = append(t.Terms, term)
}

func (t *searchTrace) addTerms(terms []traceTerm) {
	if t == nil {
		return
	}
	for _, term := range terms {
		t.addTerm(term)
	}
}

func (t *searchTrace) addDecline(reason string) {
	if t == nil || reason == "" {
		return
	}
	decline := traceDecline{Reason: reason}
	if before, after, ok := strings.Cut(reason, ":"); ok {
		decline.Source = before
		decline.Reason = after
	}
	t.Declines = append(t.Declines, decline)
}

func (t *searchTrace) addDeclineForVolume(reason, volume string) {
	if t == nil || reason == "" {
		return
	}
	t.Decline = reason
	decline := traceDecline{Reason: reason, Volume: volume}
	if before, after, ok := strings.Cut(reason, ":"); ok {
		decline.Source = before
		decline.Reason = after
	}
	t.Declines = append(t.Declines, decline)
}

// sizeFilter expresses a size:<op><bytes> constraint, e.g. size:>100mb.
type sizeFilter struct {
	op    string // ">", ">=", "<", "<=", "="
	bytes int64
}

// dateFilter expresses a dm:<spec> constraint over modification time. The
// constraint is satisfied when ModUnix falls within [after, before).
type dateFilter struct {
	after  time.Time
	before time.Time
}

type CompactRecord struct {
	FRN       uint64
	ParentFRN uint64
	Parent    int32
	Name      string
	NameOff   uint32
	NameLen   uint16
	Mode      uint32
	Size      int64
	ModUnix   int64
	Deleted   bool
}

type PackedRecords struct {
	FRNs            []uint64
	ParentFRNExtras map[int]uint64
	Parents         []int32
	NameOffs        []uint32
	NameLens        []uint16
	LowerOffs       []uint32
	DirBits         []uint64
	ModeExtraIDs    []uint32
	ModeExtraValues []uint32
	Size32          []uint32
	Size64IDs       []uint32
	Size64Values    []int64
	ModUnix         []int64
	DeletedBits     []uint64
	NameBlob        []byte
	LowerBlob       []byte
}

type MMapRecords struct {
	file       *mappedIndexFile
	wideRefs   bool
	count      int
	nameBlob   []byte
	tokenTable []byte
	recordData []byte
	hasSize    bool
	hasModUnix bool
}

type usnJournalDataV0 struct {
	UsnJournalID    uint64
	FirstUsn        int64
	NextUsn         int64
	LowestValidUsn  int64
	MaxUsn          int64
	MaximumSize     uint64
	AllocationDelta uint64
}

type mftEnumDataV0 struct {
	StartFileReferenceNumber uint64
	LowUsn                   int64
	HighUsn                  int64
}

type readUSNJournalDataV0 struct {
	StartUsn          int64
	ReasonMask        uint32
	ReturnOnlyOnClose uint32
	Timeout           uint64
	BytesToWaitFor    uint64
	UsnJournalID      uint64
}

type usnNode struct {
	frn       uint64
	parentFRN uint64
	name      string
	attr      uint32
}

type usnChange struct {
	FRN       uint64
	ParentFRN uint64
	USN       int64
	Reason    uint32
	Attr      uint32
	Name      string
}

type walBatch struct {
	NextUSN int64       `json:"next_usn"`
	Changes []usnChange `json:"changes"`
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}
