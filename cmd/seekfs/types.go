package main

import (
	"container/list"
	"math"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const indexVersion = 10

// legacyIndexVersion predates the SEQN section. A v9 index loads without it;
// every record's NTFS sequence reads back as 0, which means "unknown".
const legacyIndexVersion = 9

func indexVersionSupported(v int) bool {
	return v == legacyIndexVersion || v == indexVersion
}

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
var walMagicV2 = []byte{'S', 'W', 'A', 'L', '2'}

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
	// SEQN is the per-record NTFS file-reference sequence number (uint16 per
	// record id). It disambiguates a reused MFT record from its previous
	// occupant. Absent on v9 indexes, where every sequence reads back as 0.
	indexSectionSEQN uint32 = 'S'<<24 | 'E'<<16 | 'Q'<<8 | 'N'
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
	// GitStatus is the git worktree status of the hit (modified, staged,
	// untracked, ignored, conflict, clean, tracked). Empty when git was not
	// resolved for the query (no git: filter and no usage signals) or the
	// file sits outside any git worktree.
	GitStatus string `json:"git_status,omitempty"`
	// Lines is the `search --lines` output for a content hit (matching and
	// context lines, re-read client-side); empty otherwise.
	Lines []contentOutputLine `json:"lines,omitempty"`
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
	FuzzyCorrection          string         `json:"fuzzy_correction,omitempty"`
	EligibleVolumes          []string       `json:"eligible_volumes,omitempty"`
	Terms                    []traceTerm    `json:"terms,omitempty"`
	Declines                 []traceDecline `json:"declines,omitempty"`
	Fallback                 string         `json:"fallback,omitempty"`
	Complete                 *bool          `json:"complete,omitempty"`
	Content                  *contentHealth `json:"content,omitempty"`
	Results                  []jsonResult   `json:"results,omitempty"`
	BudgetExceeded           bool           `json:"budget_exceeded,omitempty"`
	NextCursor               string         `json:"next_cursor,omitempty"`
	CursorRestarted          bool           `json:"cursor_restarted,omitempty"`
	CursorRestartReason      string         `json:"cursor_restart_reason,omitempty"`
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
	// nameIdent holds the lazily built distinct-name identity (name ids,
	// char masks, exemplars) backing the name-memo query lane. It is built
	// once in the background after load; queries use it only when present.
	// Base mutations that can change names (set/appendCompactRecord) reset
	// it; order-preserving repacks keep it.
	nameIdent atomic.Pointer[nameIdentity]
	// nameIdentMu serializes the lazy identity build.
	nameIdentMu sync.Mutex
	// memoTopo caches the parents-first order of child-bearing records
	// for the per-query dir fold. Built with the identity's lock; reset
	// together with it.
	memoTopo atomic.Pointer[dirTopo]
	// memoGen is bumped on every identity reset so a memo built from a
	// previous identity can never be stored or served afterwards.
	memoGen atomic.Uint64
	// accentIDs caches the record IDs whose lowercased name changes under
	// accent folding (see fold.go), with the record count at build.
	// accentBuilt reports whether the cache is valid for accentCount.
	// Reset together with the name identity; nil on ASCII corpora.
	accentIDs   []int32
	accentCount int
	accentBuilt bool
	// nameMemo is the single-entry per-query memo cache (name bits plus
	// folded dir bits with the identity it was built from). It carries its
	// own lock; entries are validated by query signature, record count and
	// generation before use.
	nameMemo nameMemoCache
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
	// FRNSeq is the NTFS sequence number per record id (parallel to the record
	// table), persisted in the SEQN section. Nil on legacy v9 indexes.
	FRNSeq []uint16
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
	// limit caches postingBlockCacheMaxBytes for this cache: get/add run per
	// posting block, and reading the environment there was a syscall each.
	limitOnce sync.Once
	limit     int64
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
	// Content scope. Content holds global [content] / top-level content_* keys;
	// ContentByVolume holds [content."<volume>"] overrides keyed by volume name.
	Content         contentScopeConfig
	ContentByVolume map[string]contentScopeConfig
	Features        map[string]featureConfig
	PluginPath      string
	Plugins         map[string]pluginDefinition
}

type queryOptions struct {
	Query     string `json:"query"`
	MatchPath bool   `json:"match_path"`
	Limit     int    `json:"limit"`
	Under     string `json:"under,omitempty"`
	Exists    bool   `json:"exists,omitempty"`
	CWDBias   string `json:"cwd_bias,omitempty"`
	RootBias  string `json:"root_bias,omitempty"`
	// Near is a proximity anchor path (CLI --near / near: token): within a
	// match-quality band, files fewer path hops away rank first.
	Near string `json:"near,omitempty"`
	// Current is the CLI --current demotion path: the file the user already
	// has open ranks last within its match-quality band.
	Current       string       `json:"current,omitempty"`
	Recent        string       `json:"recent,omitempty"`
	ModifiedAfter string       `json:"modified_after,omitempty"`
	CaseSensitive bool         `json:"case_sensitive,omitempty"`
	Fuzzy         bool         `json:"fuzzy,omitempty"`
	DeadlineUnix  int64        `json:"deadline_unix,omitempty"`
	RequestSeq    int64        `json:"request_seq,omitempty"`
	Cancel        func() bool  `json:"-"`
	Trace         *searchTrace `json:"-"`
	// SignalStore is the per-user frecency/query-history store for this
	// request. The service sets it from the caller's SID; direct (local
	// index) searches leave it nil and fall back to the current process
	// user's store. Local-only; never crosses the wire.
	SignalStore *signalStore `json:"-"`
	// BudgetMS is the caller's explicit wall-clock content budget in ms
	// (--budget). It is 0 when omitted or explicitly zero; BudgetDisabled retains
	// that CLI distinction across the wire. The restored default leaves time
	// unbounded unless BudgetMS is positive.
	BudgetMS int `json:"budget_ms,omitempty"`
	// BudgetDisabled marks an explicit "run with no wall-clock content bound"
	// (--budget 0). Omitted also leaves time unbounded under the historical
	// default; the marker preserves the explicit selection for compatible wire
	// behavior. The finite disk safety valve remains independent.
	BudgetDisabled bool `json:"budget_disabled,omitempty"`
	// Per-query content budgets. Zero means "use the default"; they are never
	// globals, so concurrent queries cannot race on each other's limits.
	ContentCandidateBudget int `json:"content_candidate_budget,omitempty"`
	ContentScanVisitBudget int `json:"content_scan_visit_budget,omitempty"`
	// ContentDiskVerifyBudget overrides the per-query disk verification budget
	// (contentDiskVerifyBudget, default 512) for this
	// request; values <= 0 keep the default and larger values are clamped to
	// contentDiskVerifyBudgetMax.
	// Wire-visible so the resident service honors an opted-in read bound
	// (--content-verify-budget). Never a global, so concurrent queries cannot
	// race on each other's bound.
	ContentDiskVerifyBudget int `json:"content_disk_verify_budget,omitempty"`
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
	// lineOutput is the CLI's --lines request; client-side only (never sent
	// to the service, never serialized).
	lineOutput *contentLineOutput `json:"-"`
	// lineBudget metadata is returned by the service so client-side --lines
	// enrichment shares the same absolute deadline and remaining disk-read cap.
	// Local-only; it is never sent in service requests.
	lineBudgetMetadata      bool
	lineBudgetDeadline      time.Time
	lineDiskVerifyRemaining *int
	Cursor                  string `json:"cursor,omitempty"`
	// contentAfter is the v2 content cursor resume state. Local-only; nil on
	// every normal request.
	contentAfter *contentCursorResume
}

// contentCursorResume carries a v2 cursor's last emitted record and the
// per-search flag that reports whether that record was found in the rebuilt
// candidate order. skip is the scan position the bounded candidate path starts
// from (set while rebuilding).
type contentCursorResume struct {
	FRN   uint64
	Found bool
	skip  int
	// Key is the page key of the last emitted row. Overlay rows at or before
	// it are already emitted and stay out of the resumed page.
	Key *contentCursorKey
	// pageLimit is the user page size for a v2 resume whose base still has
	// hidden rows. overlayKeys are the matching overlay page keys after the
	// cursor, in ascending order. Together they let the base verify walk stop
	// once the merged page is provably full. Zero/nil on every other path.
	pageLimit   int
	overlayKeys []contentCursorKey
	// overlay caches the ranked overlay matches computed for the page stop so
	// the post-search merge reuses them instead of evaluating the overlay
	// twice. Local-only; nil on every other path.
	overlay []rankedOverlayEntry
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
	CaseExplicit      bool
	// ContentSmartCase is ripgrep-style smart case for content leaves only
	// (no explicit case:, every content leaf with letters has an uppercase
	// literal). Name and path terms keep CaseSensitive; content matching
	// reads contentCaseSensitive().
	ContentSmartCase bool
	Exts             []string
	Dirs             []string
	Globs            []string
	// GlobAlts parallels Globs: precomputed brace expansions per pattern
	// (exactly [pattern] when braceless), so verification never expands
	// per candidate. Falls back to live expansion when short (hand-built
	// queries in tests).
	GlobAlts [][]string
	// PathGlobs holds glob patterns that match against the full path (they
	// contain a separator or `**`, possibly with brace sets). A hit matches
	// when ANY brace expansion matches the normalized full path.
	PathGlobs []string
	// PathGlobAlts parallels PathGlobs with precomputed brace expansions.
	PathGlobAlts [][]string
	// RegexMasks holds per-regex letter-mask prefilters, parallel to
	// Regexps: each entry lists one combined mask per provably-required
	// literal disjunct (nil when unprovable). The entry gate skips
	// MatchString when no disjunct mask is a subset of the path mask.
	RegexMasks [][]uint64
	// ExactTerms holds `'exact` name constraints: the folded file name must
	// equal the term. Prefixes (`^prefix`) and Suffixes (`suffix$`) anchor
	// the file name the same way. The stripped text is also appended to
	// Terms so posting lanes engage; these fields only narrow.
	ExactTerms []string
	Prefixes   []string
	Suffixes   []string
	// InUnders holds `in:<dir>` roots (normalized absolute paths), merged
	// into Under at the end of parseQuery. NotUnders holds negated
	// `!in:<dir>` roots, which no single Under can express.
	InUnders  []string
	NotUnders []string
	// QuotedTerms holds normalized terms from double-quoted phrases. They
	// match literally (no accent folding), preserving exact-match
	// semantics for quoted input like `case:` does.
	QuotedTerms   map[string]struct{}
	Regexps       []*regexp.Regexp
	RegexTerms    []string
	Type          string
	Parents       []string
	Under         string
	Exists        bool
	ModifiedAfter time.Time
	HasModAfter   bool
	SizeFilters   []sizeFilter
	DateFilters   []dateFilter
	AttrFilters   []uint32
	Content       []contentLeaf
	Features      []featureLeaf
	SortColumn    string
	OrGroups      [][]parsedQuery
	NotGroups     []parsedQuery
	CWDBias       string
	RootBias      string
	// Near is the proximity anchor (normalized absolute path, "" when none).
	Near string
	// Current is the `--current` demotion path (normalized absolute path,
	// "" when none): the file the user already has open. It only reorders
	// within a match-quality band, never across one, and never changes
	// result sets or counts.
	Current string
	// GitFilters holds git: filter alternatives (modified, staged,
	// untracked, ignored, conflict, clean, tracked); a hit must satisfy at
	// least one. Empty means no git filtering (result sets unchanged).
	GitFilters []string
	// gitCtx is the per-query git resolution context (stat-generation gate
	// so each repo stats HEAD/index at most once per query). Nil for
	// hand-built trees; lookups stay correct, only less cached.
	gitCtx       *queryGitContext
	Limit        int
	CountOnly    bool
	DeadlineUnix int64
	Cancel       func() bool
	Trace        *searchTrace
	// Per-query content budgets copied from queryOptions; 0 means use default.
	ContentCandidateBudget  int
	ContentScanVisitBudget  int
	ContentDiskVerifyBudget int
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
	// contentVerifyState is a per-candidate status shared with the content text
	// reader. A failed budget reservation marks this candidate unverified even
	// when another worker later completes out of order.
	contentVerifyState *atomic.Bool
	// hidden is the overlay hidden-base-ID set captured once for this search
	// call and threaded into the bounded candidate scan, so the scan's
	// early-stop guard drops exactly the records the caller's verify loop drops
	// instead of re-reading the volume snapshot and racing a republish.
	hidden hiddenBaseIDs
	// contentAfter mirrors queryOptions.contentAfter: the v2 cursor resume
	// state. Local-only; nil on every normal request.
	contentAfter *contentCursorResume
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
	// CountDivergent is the non-content twin of ContentCountDivergent: search
	// stat-filters stale entries under --under/Exists while counts do not, so
	// a count may exceed a matching search after deletions. Set on potential,
	// like its twin, not on proven divergence.
	CountDivergent bool
	// ContentCandidatesVerified records that the content candidate source
	// already ran the full inline content predicate (the rank-ordered bounded
	// content scan), so the downstream verify loop can skip re-evaluating it.
	// Local-only plumbing; never serialized.
	ContentCandidatesVerified bool `json:"-"`
	// FuzzyCorrection names the corrected content word(s) when a zero-result
	// content count was retried with typo-tolerant corrections, so the count
	// response can carry the same "Showing results for X" signal as search.
	FuzzyCorrection string `json:"-"`
	// DiskVerified counts a query's disk content verifications (open,
	// extract, match) across every volume and lane. Past
	// contentDiskVerifyBudget, verification short-circuits to a miss and
	// DiskCapped marks the trace incomplete: the result is a visible
	// subset, never a silent truncation; a count refuses outright. Delta
	// hits never count (memory, not disk). Local-only plumbing.
	//
	// During a query the live accounting lives in disk (an atomic, pointer-
	// shared reservation): every verify worker reserves a slot there, so the
	// cap is exact and race-free even though a searchTrace is copied by value.
	// DiskVerified / DiskCapped / BudgetExceeded are published copies filled by
	// publishDiskBudget once the query settles, for readers and serialization.
	DiskVerified   int       `json:"-"`
	DiskCapped     bool      `json:"-"`
	BudgetExceeded bool      `json:"budget_exceeded,omitempty"`
	BudgetDeadline time.Time `json:"-"`
	// DiskVerifyBudget is the resolved per-query disk verification cap
	// (contentDiskVerifyBudget, or the caller's clampContentDiskVerifyBudget
	// override). 0 keeps the built-in default. Local-only plumbing.
	DiskVerifyBudget int `json:"-"`
	// disk is the per-query shared reservation behind DiskVerifyBudget. It is a
	// pointer so a searchTrace stays copyable (go vet copylocks); every copy
	// shares one accounting. nil means uncapped (bounded callers).
	disk *contentDiskBudget `json:"-"`
	// contentFrontier is the first (lowest-ranked) unverified base candidate an
	// incomplete content page stopped at, recorded by the verify paths'
	// single-threaded mergers. mergeOverlayMatches drops any merged entry that
	// sorts at or after it, so a partial overlay-merged page stays a rank
	// prefix of the complete result. Local-only plumbing.
	contentFrontier    Entry
	contentFrontierSet bool
}

// contentDiskBudget is the per-query disk-verification reservation shared by
// every content verify worker. The atomic used-counter makes the cap exact and
// race-free: the limit-th take() reserves a read, and the (limit+1)-th fails
// and latches capped, so no worker ever reads past the bound under contention.
// A past wall-clock deadline (--budget) latches exceeded instead; the two are
// tracked separately so a pre-verified page (no disk reads) is truncated by the
// deadline but never by an earlier disk cap.
type contentDiskBudget struct {
	limit             int64
	used              atomic.Int64
	capped            atomic.Bool
	exceeded          atomic.Bool
	incomplete        atomic.Bool
	enrichmentSkipped atomic.Bool
}

// take reserves one disk read against the cap. It returns false (and latches
// capped) once limit reads are already reserved.
func (b *contentDiskBudget) take() bool {
	if b == nil {
		return true
	}
	for {
		cur := b.used.Load()
		if cur >= b.limit {
			b.capped.Store(true)
			return false
		}
		if b.used.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// remaining reports how many disk reads are still reservable. It is used to
// keep the cap boundary deterministic (see verifyContentSupersetPage).
func (b *contentDiskBudget) remaining() int64 {
	if b == nil {
		return math.MaxInt64
	}
	r := b.limit - b.used.Load()
	if r < 0 {
		return 0
	}
	return r
}

// capDiskBudgetForProbe lowers the query's disk reservation to at most limit
// reads, never raising it. It bounds a completeness-probe re-search: the probe
// runs after the reduced first pass has stopped and all its verify workers have
// joined, so the single-threaded swap is race-free. The lowered reservation
// stays in force for the rest of the query (including snippets and later
// volumes), so once an unknown-probe widen is needed the whole query reads no
// more than a default-budget query would; a probe that exhausts it latches
// capped/incomplete on the shared reservation exactly as the default valve
// would. A limit at or above the current one is left untouched, and a used count
// already past the limit caps the probe at the used count (no reads remain).
func (t *searchTrace) capDiskBudgetForProbe(limit int) {
	if t == nil || t.disk == nil || limit <= 0 || int64(limit) >= t.disk.limit {
		return
	}
	if used := t.disk.used.Load(); int64(limit) < used {
		limit = int(used)
	}
	t.disk.limit = int64(limit)
}

// capFiresWithin reports whether a batch of n more reads could exhaust the
// exact disk cap. Callers verify such a boundary batch serially, in rank order,
// so the capped page is a deterministic rank-prefix rather than whichever
// workers happened to win the last slots.
func (t *searchTrace) capFiresWithin(n int) bool {
	if t == nil || t.disk == nil {
		return false
	}
	return t.disk.remaining() < int64(n)
}

// ensureDiskBudget resolves the query's shared disk reservation if the caller
// has not already done so. Idempotent and single-threaded: call it at a content
// search/count entry point before any verify worker starts.
func (pq parsedQuery) ensureDiskBudget() {
	if pq.Trace != nil {
		pq.Trace.initDiskBudget()
	}
}

// initDiskBudget resolves the query's disk cap into a shared reservation. Call
// it once per query, single-threaded, before any verify worker starts.
func (t *searchTrace) initDiskBudget() {
	if t == nil || t.disk != nil {
		return
	}
	limit := t.DiskVerifyBudget
	if limit <= 0 {
		limit = contentDiskVerifyBudget
	}
	t.disk = &contentDiskBudget{limit: int64(limit)}
}

// acquireDiskRead reports whether the caller may perform one disk content
// verification. It enforces the wall-clock deadline (latching exceeded) and
// then reserves a slot against the exact disk cap. A nil trace (bounded
// callers) or an uninitialized trace is uncapped.
func (t *searchTrace) acquireDiskRead() bool {
	if t == nil {
		return true
	}
	b := t.disk
	if b == nil {
		return true
	}
	if !t.BudgetDeadline.IsZero() && !time.Now().Before(t.BudgetDeadline) {
		b.exceeded.Store(true)
		b.incomplete.Store(true)
		return false
	}
	if !b.take() {
		b.incomplete.Store(true)
		return false
	}
	return true
}

// acquireOptionalDiskRead shares the per-query deadline and exact read cap for
// presentation-only work (snippets, line/context output) without changing the
// completeness of already verified matches when that enrichment is skipped.
func (t *searchTrace) acquireOptionalDiskRead() bool {
	if t == nil || t.disk == nil {
		return true
	}
	b := t.disk
	if !t.BudgetDeadline.IsZero() && !time.Now().Before(t.BudgetDeadline) {
		b.enrichmentSkipped.Store(true)
		return false
	}
	if !b.take() {
		b.enrichmentSkipped.Store(true)
		return false
	}
	return true
}

func (t *searchTrace) optionalEnrichmentSkipped() bool {
	return t != nil && t.disk != nil && t.disk.enrichmentSkipped.Load()
}

func (t *searchTrace) markOptionalEnrichmentSkipped() {
	if t != nil && t.disk != nil {
		t.disk.enrichmentSkipped.Store(true)
	}
}

// armContentDeadline arms the query's wall-clock content deadline from the
// caller's selection: BudgetDisabled (explicit --budget 0) leaves it unarmed;
// an explicit budgetMS>0 uses that; omitted or zero budget has no implicit
// deadline under the restored default (contentDefaultBudgetMS is 0).
// It is a no-op when a deadline is already armed or when the resolved value is
// 0. Call it only on content paths (queryHasAnyContentLeaf): the deadline is
// consulted exclusively by disk verification (acquireDiskRead/deadlineReached),
// so name-only queries must not pay for it.
func (t *searchTrace) armContentDeadline(budgetMS int, disabled bool) {
	if t == nil || !t.BudgetDeadline.IsZero() {
		return
	}
	ms := contentBudgetDeadlineMS(budgetMS, disabled)
	if ms <= 0 {
		return
	}
	t.BudgetDeadline = time.Now().Add(time.Duration(ms) * time.Millisecond)
}

// contentBudgetDeadlineMS resolves the effective wall-clock content budget in
// milliseconds. It returns 0 when no deadline should be armed: explicit zero,
// omission under the restored default, or a test-selected disabled default.
func contentBudgetDeadlineMS(budgetMS int, disabled bool) int {
	if disabled {
		return 0
	}
	if budgetMS > 0 {
		return budgetMS
	}
	if contentDefaultBudgetMS > 0 {
		return contentDefaultBudgetMS
	}
	return 0
}

// stopDiskVerify reports whether further disk verifications would only miss:
// the exact cap or the wall-clock deadline has latched.
func (t *searchTrace) stopDiskVerify() bool {
	if t == nil || t.disk == nil {
		return false
	}
	return t.disk.capped.Load() || t.disk.exceeded.Load()
}

// deadlineReached reports whether the wall-clock deadline has passed, without
// latching. Used to cancel work that reads no disk (e.g. reconstructing an
// already-verified page).
func (t *searchTrace) deadlineReached() bool {
	return t != nil && !t.BudgetDeadline.IsZero() && !time.Now().Before(t.BudgetDeadline)
}

func (t *searchTrace) markBudgetExceeded() {
	if t == nil || t.disk == nil {
		return
	}
	t.disk.exceeded.Store(true)
	t.disk.incomplete.Store(true)
}

// contentIncomplete reports the current incompleteness, reading the live
// shared reservation while a query runs (before publishDiskBudget copies it to
// the plain field).
func (t *searchTrace) contentIncomplete() bool {
	if t == nil {
		return false
	}
	if t.disk != nil {
		return t.disk.incomplete.Load()
	}
	return t.ContentIncomplete
}

// publishDiskBudget copies the settled shared reservation onto the trace's
// plain fields for readers and JSON serialization. Call once, after the query
// returns and all workers have stopped.
func (t *searchTrace) publishDiskBudget() {
	if t == nil || t.disk == nil {
		return
	}
	t.DiskVerified = int(t.disk.used.Load())
	t.DiskCapped = t.disk.capped.Load()
	t.BudgetExceeded = t.disk.exceeded.Load()
	if t.disk.incomplete.Load() {
		t.ContentIncomplete = true
	}
}

// recordContentFrontier remembers the lowest-ranked unverified base candidate
// that stopped an incomplete content page. Each verify path calls it from its
// single-threaded merger; a later, higher-ranked frontier never replaces an
// earlier one, so a multi-volume query keeps the earliest cutoff.
func (t *searchTrace) recordContentFrontier(entry Entry, pq parsedQuery) {
	if t == nil {
		return
	}
	if !t.contentFrontierSet || compareSearchAllEntries(entry, t.contentFrontier, pq) < 0 {
		t.contentFrontier = entry
		t.contentFrontierSet = true
	}
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

// markCountDivergent flags Under/Exists counts whose totals may exceed a
// matching search (search stat-filters stale entries while counts do not).
// Content queries set their own twin flag instead, so they are excluded.
func markCountDivergent(opts queryOptions, pq parsedQuery) {
	if opts.Trace == nil || queryHasAnyContentLeaf(pq) {
		return
	}
	if opts.Under != "" || opts.Exists {
		opts.Trace.CountDivergent = true
	}
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
	if t == nil {
		return
	}
	// With a live reservation the write goes through shared atomic state (many
	// verify workers may call this concurrently) and is published later.
	if t.disk != nil {
		t.disk.incomplete.Store(true)
		return
	}
	t.ContentIncomplete = true
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
	// Seq is the NTFS file-reference sequence number of this record (the high
	// 16 bits of the 64-bit reference). 0 means unknown (legacy or walk). It
	// lets replay tell a reused record from its previous occupant.
	Seq uint16
}

type PackedRecords struct {
	FRNs            []uint64
	ParentFRNExtras map[int]uint64
	Parents         []int32
	NameOffs        []uint32
	NameLens        []uint16
	LowerOffs       []uint32
	Modes           []uint32
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
	seq       uint16
	parentSeq uint16
}

type usnChange struct {
	FRN       uint64
	ParentFRN uint64
	USN       int64
	Reason    uint32
	Attr      uint32
	Name      string
	// Seq is the file reference's NTFS sequence number. It is omitted from
	// legacy (v1) WAL/JSON frames and reads back as 0 (unknown).
	Seq uint16 `json:"seq,omitempty"`
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
