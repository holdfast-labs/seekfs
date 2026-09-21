package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type serviceResponse struct {
	OK                       bool                `json:"ok"`
	Message                  string              `json:"message,omitempty"`
	Fuzzy                    bool                `json:"fuzzy,omitempty"`
	PID                      int                 `json:"pid,omitempty"`
	Executable               string              `json:"executable,omitempty"`
	ExecutableHash           string              `json:"executable_hash,omitempty"`
	Version                  string              `json:"version,omitempty"`
	Commit                   string              `json:"commit,omitempty"`
	Date                     string              `json:"date,omitempty"`
	BuildFlavor              string              `json:"build_flavor,omitempty"`
	PipeName                 string              `json:"pipe_name,omitempty"`
	ProcessMode              string              `json:"process_mode,omitempty"`
	Entries                  int                 `json:"entries,omitempty"`
	Loading                  bool                `json:"loading,omitempty"`
	Count                    int                 `json:"count,omitempty"`
	SearchMS                 float64             `json:"search_ms,omitempty"`
	Source                   string              `json:"source,omitempty"`
	Decline                  string              `json:"decline,omitempty"`
	Candidates               int                 `json:"candidates,omitempty"`
	PlannerMode              string              `json:"planner_mode,omitempty"`
	EligibleVolumes          []string            `json:"eligible_volumes,omitempty"`
	BlocksDecoded            int                 `json:"blocks_decoded,omitempty"`
	BlocksSkipped            int                 `json:"blocks_skipped,omitempty"`
	ScalarDriver             string              `json:"scalar_driver,omitempty"`
	ScalarInterval           int                 `json:"scalar_interval,omitempty"`
	RecordsVerified          int                 `json:"records_verified,omitempty"`
	ComponentDriver          string              `json:"component_driver,omitempty"`
	ComponentRoots           int                 `json:"component_roots,omitempty"`
	ComponentIntervals       int                 `json:"component_intervals,omitempty"`
	ComponentCardinality     int                 `json:"component_cardinality,omitempty"`
	ComponentSelfHits        int                 `json:"component_self_hits,omitempty"`
	ComponentBounds          string              `json:"component_bounds,omitempty"`
	ComponentRecordsVerified int                 `json:"component_records_verified,omitempty"`
	FilenameDriver           string              `json:"filename_driver,omitempty"`
	FilenameRequiredGrams    int                 `json:"filename_required_grams,omitempty"`
	FilenamePostingHint      int                 `json:"filename_posting_hint,omitempty"`
	FilenameRecordsVerified  int                 `json:"filename_records_verified,omitempty"`
	OverlayBaseWindow        int                 `json:"overlay_base_window,omitempty"`
	PostingPrefetchBytes     int                 `json:"posting_prefetch_bytes,omitempty"`
	PostingPrefetchRanges    int                 `json:"posting_prefetch_ranges,omitempty"`
	PostingPrefetchPages     int                 `json:"posting_prefetch_pages,omitempty"`
	WatchVolumes             []watchVolumeCursor `json:"watch_volumes,omitempty"`
	WatchEvents              []watchDeltaEvent   `json:"watch_events,omitempty"`
	Terms                    []traceTerm         `json:"terms,omitempty"`
	Declines                 []traceDecline      `json:"declines,omitempty"`
	Fallback                 string              `json:"fallback,omitempty"`
	Complete                 *bool               `json:"complete,omitempty"`
	Results                  []string            `json:"results,omitempty"`
	Rows                     []jsonResult        `json:"rows,omitempty"`
	DBs                      []dbInfo            `json:"dbs,omitempty"`
	Runtime                  *runtimeMemoryInfo  `json:"runtime,omitempty"`
	Health                   string              `json:"health,omitempty"`
	HealthMessage            string              `json:"health_message,omitempty"`
	Content                  *contentHealth      `json:"content,omitempty"`
}

// redactContentText clears local-only matched content text (snippets) from the
// response rows. A non-local transport calls this at the command boundary so a
// future projection cannot leak document text even if it forgets to strip it.
func (r *serviceResponse) redactContentText() {
	if r == nil {
		return
	}
	for i := range r.Rows {
		r.Rows[i].Snippet = ""
	}
}

// servicePrincipal describes the caller of a service command and the
// capabilities derived from its Windows identity.  A principal is produced by
// impersonating the pipe client token (local callers) or by authenticating a
// remote session (later phases); it is never constructed from client-supplied
// fields.
type servicePrincipal struct {
	// Elevated is true for SYSTEM and for elevated (UAC) administrators.  Only
	// elevated principals may issue mutation commands (index-usn and future
	// mutations).
	Elevated bool
	// SID is the caller's user SID when it can be resolved, else "".
	SID string
}

// serviceCapabilities is the per-caller command allowlist.  Phase 1 derives it
// from the local principal: read-only commands for ordinary users, plus
// mutation for elevated/SYSTEM.  Remote callers (Phase 3+) get a strict
// read-only allowlist and sanitized projections.
type serviceCapabilities struct {
	// ReadOnly permits search, count, info, status, and (locally) watch-delta.
	ReadOnly bool
	// Mutate permits index-usn / service-index-usn and future mutation commands.
	Mutate bool
	// Remote marks a non-local caller; remote callers get sanitized projections
	// and no watch-delta until later phases.
	Remote bool
}

// impersonateNamedPipeClient exposes advapi32.ImpersonateNamedPipeClient, which
// is not declared in x/sys/windows v0.38.0.
var procImpersonateNamedPipeClient = syscall.NewLazyDLL("advapi32.dll").NewProc("ImpersonateNamedPipeClient")

func impersonateNamedPipeClient(pipe syscall.Handle) error {
	r1, _, e1 := procImpersonateNamedPipeClient.Call(uintptr(pipe))
	if r1 == 0 {
		if e1 != syscall.Errno(0) {
			return e1
		}
		return syscall.EINVAL
	}
	return nil
}

// servicePrincipalForPipeConn builds the caller principal for a local named-pipe
// connection by impersonating the client token.  It follows the documented
// fail-safe sequence:
//
//  1. Lock the goroutine to its OS thread (impersonation is thread-local).
//  2. Call ImpersonateNamedPipeClient and verify success; on failure, fail
//     closed to a non-elevated (read-only) principal rather than falling through
//     to the privileged service context.
//  3. Inspect the client token while impersonating and copy the result.
//  4. RevertToSelf before returning.  A failed reversion leaves this OS thread
//     in the client's security context; the process is terminated immediately
//     (serviceFatalExit) rather than returning that thread to the scheduler.
//
// The returned principal is a plain value; no impersonation remains in effect
// on the normal path.  RevertToSelf failure does not return.
func servicePrincipalForPipeConn(conn *os.File) servicePrincipal {
	principal := servicePrincipal{}
	if conn == nil {
		return principal
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := impersonateNamedPipeClient(syscall.Handle(conn.Fd())); err != nil {
		// Failed impersonation: do not fall through to the privileged service
		// context.  Fail closed to read-only.
		serviceLog("pipe impersonation failed: %v; failing closed to read-only", err)
		return principal
	}

	// serviceRevertToSelfOrDie reverts the client impersonation and, on
	// failure, terminates the process immediately.  os.Exit does not run
	// deferred functions, so the still-impersonating OS thread is never
	// returned to Go's scheduler.  Must be called on the locked OS thread and
	// only after all token inspection is complete.
	serviceRevertToSelfOrDie := func() {
		if err := windows.RevertToSelf(); err != nil {
			serviceLog("FATAL: RevertToSelf failed while handling a pipe request: %v; the thread remains in the client's security context, exiting", err)
			serviceFatalExit(1)
		}
	}

	th := windows.CurrentThread()
	var token windows.Token
	// openAsSelf=true: access the thread (impersonation) token using the
	// process token's security context, which is what we want here.
	if err := windows.OpenThreadToken(th, windows.TOKEN_QUERY, true, &token); err == nil {
		principal = servicePrincipalFromToken(token)
		token.Close()
		serviceRevertToSelfOrDie()
		return principal
	}
	// Could not open/read the client token: fail closed to read-only.
	serviceRevertToSelfOrDie()
	return servicePrincipal{}
}

// servicePrincipalFromToken derives a principal from an access token using the
// documented policy: mutation is granted to LocalSystem, or to a UAC-elevated
// token that is an enabled member of Builtin Administrators.  Any token or
// group-query error leaves the principal non-elevated (read-only).
func servicePrincipalFromToken(token windows.Token) servicePrincipal {
	principal := servicePrincipal{}

	user, err := token.GetTokenUser()
	if err != nil || user.User.Sid == nil {
		// The caller's identity could not be resolved.  Fail closed to a
		// read-only principal: do not proceed to the elevation/membership
		// checks, which would otherwise be able to grant mutation without a
		// verified user SID.
		return principal
	}
	principal.SID = user.User.Sid.String()

	// LocalSystem always gets mutation.
	if sysSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid); err == nil && user.User.Sid.Equals(sysSID) {
		principal.Elevated = true
		return principal
	}

	// Elevated (UAC) and an enabled member of Builtin Administrators.
	if token.IsElevated() {
		if adminsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid); err == nil {
			if member, err := token.IsMember(adminsSID); err == nil && member {
				principal.Elevated = true
			}
		}
	}
	return principal
}

// serviceFatalExit is the production process-fail-fast hook.  It is a variable
// so tests can intercept the fatal impersonation-state path.
var serviceFatalExit = func(code int) { os.Exit(code) }

// tokenUserSID returns the user SID string for a token, or "" on failure.
func tokenUserSID(token windows.Token) string {
	user, err := token.GetTokenUser()
	if err != nil || user.User.Sid == nil {
		return ""
	}
	return user.User.Sid.String()
}

// localServiceCapabilities maps a local pipe principal to capabilities.  SYSTEM
// and elevated administrators may mutate; everyone else is read-only.  Remote
// callers are never classified through this path.
func localServiceCapabilities(p servicePrincipal) serviceCapabilities {
	return serviceCapabilities{
		ReadOnly: true,
		Mutate:   p.Elevated,
	}
}

// Service health values reported via the info response and surfaced in the UI
// as the bottom-right status dot.
const (
	serviceHealthOK       = "ok"
	serviceHealthDegraded = "degraded"
	serviceHealthError    = "error"
)

// classifyServiceHealth derives the overall service health from the per-db
// snapshots.  Precedence: error (a volume is stale or errored) > degraded
// (catching up, loading, or persist trouble) > ok.  The returned message
// names the first problem found so the UI can show why the dot is not green.
func classifyServiceHealth(loading bool, loadErr string, infos []dbInfo) (string, string) {
	for i := range infos {
		info := infos[i]
		switch info.State {
		case "stale", "error":
			reason := firstNonEmpty(info.StaleReason, "volume "+info.Volume+" is "+info.State)
			return serviceHealthError, reason
		}
	}
	if loadErr != "" {
		return serviceHealthError, loadErr
	}
	for i := range infos {
		info := infos[i]
		if info.PersistFailures > 0 || info.LastPersistError != "" {
			return serviceHealthDegraded, firstNonEmpty(info.LastPersistError, "persist failing on volume "+info.Volume)
		}
		if info.State == "replaying" {
			return serviceHealthDegraded, "catching up volume " + info.Volume
		}
	}
	if loading {
		return serviceHealthDegraded, "loading indexes"
	}
	if len(infos) == 0 {
		return serviceHealthDegraded, "no indexes loaded"
	}
	return serviceHealthOK, ""
}

func serviceInfoResponse(resp serviceResponse) serviceResponse {
	return serviceInfoResponseFor(resp, "", "")
}

func serviceInfoResponseFor(resp serviceResponse, pipeName, processMode string) serviceResponse {
	resp.PID = os.Getpid()
	resp.Version = version
	resp.Commit = commit
	resp.Date = date
	resp.BuildFlavor = serviceBuildFlavor()
	resp.PipeName = pipeName
	resp.ProcessMode = processMode
	if exe, err := os.Executable(); err == nil {
		resp.Executable = exe
		resp.ExecutableHash = executableContentHash(exe)
	}
	return resp
}

func executableContentHash(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func serviceBuildFlavor() string {
	return serviceBuildFlavorForMemoryMode(os.Getenv("SEEKFS_MEMORY_MODE"))
}

func serviceBuildFlavorForMemoryMode(memoryMode string) string {
	parts := []string{"cli", "service"}
	switch strings.ToLower(strings.TrimSpace(memoryMode)) {
	case "lowmem", "mmap", "low-memory":
		parts = append(parts, "lowmem")
	}
	return strings.Join(parts, ",")
}

type dbInfo struct {
	Path              string              `json:"path"`
	Entries           int                 `json:"entries"`
	Source            string              `json:"source"`
	BuiltAt           string              `json:"built_at"`
	Volume            string              `json:"volume,omitempty"`
	JournalID         uint64              `json:"journal_id,omitempty"`
	Checkpoint        int64               `json:"checkpoint_usn,omitempty"`
	State             string              `json:"state,omitempty"`
	StaleReason       string              `json:"stale_reason,omitempty"`
	FRNRecords        int                 `json:"frn_records,omitempty"`
	Recent            int                 `json:"recent,omitempty"`
	PathCache         int                 `json:"path_cache,omitempty"`
	TermCache         int                 `json:"term_cache,omitempty"`
	PathTerms         int                 `json:"path_term_cache,omitempty"`
	ExtCache          int                 `json:"ext_cache,omitempty"`
	RecentSeq         uint64              `json:"recent_seq,omitempty"`
	Dirty             bool                `json:"dirty,omitempty"`
	LastPersist       string              `json:"last_persist,omitempty"`
	PersistFailures   int                 `json:"persist_failures,omitempty"`
	PersistRetryAfter string              `json:"persist_retry_after,omitempty"`
	LastPersistError  string              `json:"last_persist_error,omitempty"`
	LastReplayAt      string              `json:"last_replay_at,omitempty"`
	LastReplayError   string              `json:"last_replay_error,omitempty"`
	LastReplayNext    int64               `json:"last_replay_next,omitempty"`
	QueryExtKeys      int                 `json:"query_ext_keys,omitempty"`
	QueryDirs         int                 `json:"query_dirs,omitempty"`
	NameOrderState    string              `json:"name_order_state,omitempty"`
	NameOrderMillis   int64               `json:"name_order_build_ms,omitempty"`
	NameTrigramState  string              `json:"name_trigram_state,omitempty"`
	NameTrigramMillis int64               `json:"name_trigram_build_ms,omitempty"`
	DerivedSections   []string            `json:"derived_sections,omitempty"`
	DerivedBytes      int                 `json:"derived_bytes,omitempty"`
	Memory            *residentMemoryInfo `json:"memory,omitempty"`
}

type residentMemoryInfo struct {
	Records           int   `json:"records"`
	MMapRecordBytes   int64 `json:"mmap_record_bytes,omitempty"`
	NameBlobBytes     int   `json:"name_blob_bytes,omitempty"`
	LowerBlobBytes    int   `json:"lower_blob_bytes,omitempty"`
	RecordBytes       int64 `json:"record_bytes,omitempty"`
	NameOrderBytes    int   `json:"name_order_bytes,omitempty"`
	ExtPostBytes      int   `json:"ext_posting_bytes,omitempty"`
	NameTrigramBytes  int   `json:"name_trigram_bytes,omitempty"`
	NameTrigramKeys   int   `json:"name_trigram_keys,omitempty"`
	TypePostBytes     int   `json:"type_posting_bytes,omitempty"`
	ChildBytes        int   `json:"child_bytes,omitempty"`
	FRNIndexBytes     int   `json:"frn_index_bytes,omitempty"`
	FRNOverlayEntries int   `json:"frn_overlay_entries,omitempty"`
	KnownBytes        int64 `json:"known_bytes,omitempty"`
}

type runtimeMemoryInfo struct {
	HeapAllocBytes    uint64  `json:"heap_alloc_bytes"`
	HeapInuseBytes    uint64  `json:"heap_inuse_bytes"`
	HeapIdleBytes     uint64  `json:"heap_idle_bytes"`
	HeapReleasedBytes uint64  `json:"heap_released_bytes"`
	HeapSysBytes      uint64  `json:"heap_sys_bytes"`
	StackInuseBytes   uint64  `json:"stack_inuse_bytes"`
	SysBytes          uint64  `json:"sys_bytes"`
	NumGC             uint32  `json:"num_gc"`
	GCCPUFraction     float64 `json:"gc_cpu_fraction,omitempty"`
	GoMemLimitBytes   int64   `json:"go_mem_limit_bytes,omitempty"`
}

type goSearchService struct {
	pipeName    string
	sddl        string
	processMode string
	stop        chan struct{}
	stopOnce    sync.Once
	dbs         []string
	indexes     []*Index
	volumes     []*serviceVolumeIndex
	loading     bool
	loadErr     string
	indexMu     sync.RWMutex
	requestSeq  atomic.Int64
	// lastTempSweep holds the unix-nano time of the last stale index temp sweep,
	// so the periodic sweep runs once per interval no matter how many volume
	// loops call it.
	lastTempSweep atomic.Int64
	// remoteAddr, when non-empty, is a loopback address (Mode L) that this
	// service also listens on with the versioned JSON frame transport.  It is
	// empty (disabled) by default.
	remoteAddr string
	remoteSrv  *remoteLoopbackServer
	// contentLocks holds the advisory `.gsx.lock` locks this service owns for
	// the volumes it has attached or built, keyed by the `.gsx` path. They are
	// held for the volume's lifetime so a concurrent `content-index -db` fails
	// fast rather than clobbering the service's sidecar (M10). Bounded by the
	// distinct sidecar paths; released at process exit.
	contentLocksMu sync.Mutex
	contentLocks   map[string]*contentVolumeLock
}

// signalServiceStop closes the stop channel exactly once.  It is safe to call
// from the service control loop and from a pipe handler that hits an
// unrecoverable impersonation state.
func (s *goSearchService) signalServiceStop() {
	if s == nil || s.stop == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stop) })
}

type serviceVolumeIndex struct {
	mu          sync.Mutex
	dbPath      string
	index       *Index
	volume      string
	journalID   uint64
	checkpoint  int64
	state       string
	staleReason string
	// baseCheckpoint is the USN watermark of the persisted record index:
	// the checkpoint the base record set is complete through. It trails
	// `checkpoint` while an unfolded overlay is pending, so a service-owned
	// content build (PF-3) can stamp a sidecar that matches exactly the
	// records it walked; restart catch-up then replays the overlay changes.
	baseCheckpoint int64
	// content is the per-volume content-search state and coordinator. Both are
	// nil unless content search is enabled (SEEKFS_CONTENT_SEARCH=1).
	content      *contentVolumeState
	contentCoord *contentCoordinator
	// contentBuildBusy serializes service-owned content builds: at most one
	// runs per volume at a time, and it is left set for the whole build.
	contentBuildBusy atomic.Bool
	// contentBuildAttempts counts consecutive generation-change retries of a
	// service-owned content build. An external ensureContentBuild resets it; the
	// retry path increments it so a volume whose generation never settles gives
	// up (degraded + BuildError) instead of spinning builds forever.
	contentBuildAttempts atomic.Int32
	// ownedDirFRNs holds the NTFS file references of directories that hold
	// seekfs's own artifacts (the seekfs dir and the name-gram spool dir) for
	// this volume.  USN changes whose ParentFRN is in this set are consumed
	// without entering the overlay, so the service does not index its own
	// multi-GB staged writes, WAL, logs, and spill files.  Built once at
	// volume load and read-only afterwards; a nil map disables the filter.
	ownedDirFRNs map[uint64]struct{}
	// ownedDynamicDirFRNs extends ownedDirFRNs with directories created under
	// owned directories at runtime (the external builder's MkdirTemp scratch
	// dirs), whose own children must be filtered too.  Guarded by ownedDirMu.
	ownedDynamicDirFRNs map[uint64]struct{}
	ownedDirMu          sync.RWMutex

	frnToID           map[uint64]int
	frns              []uint64
	frnRecordIDs      []uint32
	children          map[uint64]map[int]struct{}
	childOffsets      []uint32
	childIDs          []uint32
	rootIDs           []uint32
	subtreeOrder      []uint32
	subtreeStart      []uint32
	subtreeEnd        []uint32
	subtreeBytes      []uint64
	dirSizeDelta      map[int]int64
	subtreeSizeRank   []uint32
	subtreeModRank    []uint32
	subtreeExtRank    []uint32
	subtreeTypeRank   []uint32
	subtreePathRank   []uint32
	exactNames        map[string][]int
	pathCache         map[int]string
	queryIndex        *residentQueryIndex
	nameOrderState    atomic.Int32
	nameOrderMillis   atomic.Int64
	nameTrigrams      atomic.Pointer[compressedTrigramIndex]
	nameQuadgrams     atomic.Pointer[compressedTrigramIndex]
	nameTrigramState  atomic.Int32
	nameTrigramMillis atomic.Int64
	searchMu          sync.Mutex
	termMu            sync.Mutex
	walkMu            sync.Mutex
	termCache         map[string]postingCacheEntry
	pathTermCache     map[string]postingCacheEntry
	extCache          map[string]postingCacheEntry
	recentIDs         map[int]struct{}
	nameTrigramRecent map[int]struct{}
	recentSeq         uint64
	replayGen         atomic.Uint64
	persistGen        atomic.Uint64
	underCache        map[int]postingCacheEntry
	underRootCache    map[string]postingCacheEntry
	dirty             bool
	lastPersist       time.Time
	persistFailures   int
	persistRetryAfter time.Time
	lastPersistErr    string
	lastReplayAt      time.Time
	lastReplayErr     string
	lastReplayNext    int64
	replayStrikes     int
	stallObservedCp   int64
	stallObservedAt   time.Time
	recovering        atomic.Bool
	searchCount       uint64
	overlay           *overlaySegment
	snap              atomic.Pointer[volumeSnapshot]
	snapshotGen       atomic.Uint64
}

type overlaySegment struct {
	records   []CompactRecord
	byFRN     map[uint64]int32
	tombstone overlayBaseIDSet
	shadowed  overlayBaseIDSet
	watermark atomic.Int32
	// checkpointAtRotate is set when persist snapshots the segment: the volume
	// checkpoint at snapshot time, which covers every change in this segment.
	// It is the checkpoint the folded index may durably claim.
	checkpointAtRotate atomic.Int64
}

type overlayBaseIDSet struct {
	bits  []uint64
	ids   []int32
	count int
}

type volumeSnapshot struct {
	base         *Index
	records      []CompactRecord
	tombstoneIDs []int32
	shadowedIDs  []int32
	watermark    int32
	gen          uint64
}

type residentQueryIndex struct {
	ext        map[string][]uint32
	extTop     map[string][]uint32
	pathGrams  map[string][]uint32
	components map[string][]uint32
	attrBits   map[uint32][]uint32
	nameOrder  []uint32
	nameRank   []uint32
	sizeOrder  []uint32
	sizeRank   []uint32
	modOrder   []uint32
	modRank    []uint32
	extOrder   []uint32
	extRank    []uint32
	typeOrder  []uint32
	typeRank   []uint32
	pathOrder  []uint32
	pathRank   []uint32
	dirs       []uint32
	dirsReady  bool
}

type postingCacheEntry struct {
	ids []int
	gen uint64
}

func serviceLog(format string, args ...any) {
	dir := filepath.Join(os.Getenv("ProgramData"), "seekfs")
	if dir == "" || dir == "seekfs" {
		dir = filepath.Join(os.TempDir(), "seekfs")
	}
	_ = os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(filepath.Join(dir, "service.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, time.Now().Format(time.RFC3339Nano)+" "+format+"\n", args...)
}
