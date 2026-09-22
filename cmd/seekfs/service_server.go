package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"time"
	"unsafe"
)

func (s *goSearchService) servePipeListener() {
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		file, err := s.createPipeInstance()
		if err != nil {
			return
		}
		go handleServiceConn(file, s)
	}
}

func (s *goSearchService) createPipeInstance() (*os.File, error) {
	ptr, err := windows.UTF16PtrFromString(s.pipeName)
	if err != nil {
		return nil, err
	}
	sa, err := securityAttributesFromSDDL(s.sddl)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateNamedPipe(
		ptr,
		windows.PIPE_ACCESS_DUPLEX,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		windows.PIPE_UNLIMITED_INSTANCES,
		64*1024,
		64*1024,
		0,
		sa,
	)
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-s.stop:
			_ = windows.CloseHandle(handle)
		case <-done:
		}
	}()
	err = windows.ConnectNamedPipe(handle, nil)
	close(done)
	if err == nil || err == windows.ERROR_PIPE_CONNECTED {
		return os.NewFile(uintptr(handle), s.pipeName), nil
	}
	windows.CloseHandle(handle)
	return nil, err
}

func securityAttributesFromSDDL(sddl string) (*windows.SecurityAttributes, error) {
	if strings.TrimSpace(sddl) == "" {
		return nil, nil
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
		InheritHandle:      0,
	}, nil
}

func handleServiceConn(conn *os.File, s *goSearchService) {
	defer conn.Close()
	defer func() {
		if r := recover(); r != nil {
			serviceLog("panic: %v\n%s", r, string(debug.Stack()))
			_ = json.NewEncoder(conn).Encode(serviceResponse{OK: false, Message: fmt.Sprintf("service panic: %v", r)})
		}
	}()
	var req serviceRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(serviceResponse{OK: false, Message: err.Error()})
		return
	}
	// Phase 1: derive the caller's capabilities from its Windows identity by
	// impersonating the pipe client.  Failures fail closed (read-only).  A
	// RevertToSelf failure is process-fatal and does not return.
	principal := servicePrincipalForPipeConn(conn)
	caps := localServiceCapabilities(principal)
	s.handleServiceCommand(conn, principal, caps, &req)
}

// serviceCommandClass classifies a service command for capability gating.
// The allowlist is deny-by-default: an unrecognized command is denied.
type serviceCommandClass int

const (
	serviceCommandReadOnly serviceCommandClass = iota
	serviceCommandMutate
	serviceCommandLocalOnly
	serviceCommandUnknown
)

// classifyServiceCommand maps a service command name to the capability class it
// requires.  This is the single authority for which commands a caller may issue;
// future mutation commands must be listed here to be granted to elevated
// callers, and are otherwise denied by default.
func classifyServiceCommand(command string) serviceCommandClass {
	switch command {
	case "search", "info", "status":
		return serviceCommandReadOnly
	case "watch-delta":
		// watch-delta is a read-only local operation but is deferred remotely
		// until Phase 7: it exposes cursor semantics that need scoped identity.
		return serviceCommandLocalOnly
	case "index-usn":
		return serviceCommandMutate
	default:
		return serviceCommandUnknown
	}
}

// remoteServiceCommandAllowed is the canonical remote-operation allowlist.
// Rev 5 exposes search, count (a search option), and sanitized info to remote
// callers; status is folded into sanitized info rather than being a separate
// wire command, and watch-delta is deferred until Phase 7.  Everything else is
// denied for remote callers regardless of capability.
func remoteServiceCommandAllowed(command string) bool {
	switch command {
	case "search", "info":
		return true
	default:
		return false
	}
}

// serviceCommandAllowed reports whether a caller with the given capabilities may
// issue a command.  Deny-by-default: unknown commands and any command exceeding
// the caller's capabilities are rejected.  Remote callers are held to the
// canonical search/info-only wire allowlist FIRST, before capability-class
// dispatch, so a remote principal can never reach a mutation branch even with
// Mutate enabled.
func serviceCommandAllowed(command string, caps serviceCapabilities) bool {
	if caps.Remote && !remoteServiceCommandAllowed(command) {
		return false
	}
	switch classifyServiceCommand(command) {
	case serviceCommandReadOnly:
		return caps.ReadOnly
	case serviceCommandMutate:
		return caps.Mutate
	case serviceCommandLocalOnly:
		return caps.ReadOnly && !caps.Remote
	default:
		return false
	}
}

// handleServiceCommand dispatches a decoded service request to the engine.
// Phase 1 keeps the local pipe as the only transport; the principal and
// capabilities gate commands.  Later phases reuse this dispatch from
// the broker/remote transport with a remote principal and a strict read-only
// capability set.
func (s *goSearchService) handleServiceCommand(w io.Writer, principal servicePrincipal, caps serviceCapabilities, req *serviceRequest) {
	if !serviceCommandAllowed(req.Command, caps) {
		message := "command not permitted for this caller"
		if classifyServiceCommand(req.Command) == serviceCommandMutate {
			message = "this command requires elevation; rerun it from an elevated prompt"
		}
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: message})
		return
	}
	switch req.Command {
	case "info":
		s.serviceCommandInfo(w, req)
	case "search":
		s.serviceCommandSearch(w, caps, req)
	case "watch-delta":
		s.serviceCommandWatchDelta(w, req)
	case "index-usn":
		s.serviceCommandIndexUSN(w, req)
	case "status":
		s.serviceCommandStatus(w)
	default:
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: "unknown command"})
	}
}

func (s *goSearchService) serviceCommandInfo(w io.Writer, req *serviceRequest) {
	s.indexMu.RLock()
	volumes := append([]*serviceVolumeIndex(nil), s.volumes...)
	loading := s.loading
	loadErr := s.loadErr
	s.indexMu.RUnlock()
	infos := make([]dbInfo, 0, len(volumes))
	total := 0
	for _, vol := range volumes {
		idx := vol.index
		total += idx.entryCount()
		info := dbInfo{
			Path:             vol.dbPath,
			Entries:          idx.entryCount(),
			Source:           idx.Source,
			BuiltAt:          idx.BuiltAt.Format(time.RFC3339Nano),
			Volume:           vol.volume,
			JournalID:        vol.journalID,
			Checkpoint:       vol.checkpoint,
			State:            vol.state,
			StaleReason:      vol.staleReason,
			FRNRecords:       vol.frnRecordCount(),
			Recent:           len(vol.recentIDs),
			PathCache:        len(vol.pathCache),
			TermCache:        len(vol.termCache),
			PathTerms:        len(vol.pathTermCache),
			ExtCache:         len(vol.extCache),
			RecentSeq:        vol.recentSeq,
			Dirty:            vol.dirty,
			PersistFailures:  vol.persistFailures,
			LastPersistError: vol.lastPersistErr,
			LastReplayError:  vol.lastReplayErr,
			LastReplayNext:   vol.lastReplayNext,
		}
		if !vol.lastPersist.IsZero() {
			info.LastPersist = vol.lastPersist.Format(time.RFC3339Nano)
		}
		if !vol.lastReplayAt.IsZero() {
			info.LastReplayAt = vol.lastReplayAt.Format(time.RFC3339Nano)
		}
		if !vol.persistRetryAfter.IsZero() {
			info.PersistRetryAfter = vol.persistRetryAfter.Format(time.RFC3339Nano)
		}
		if vol.queryIndex != nil {
			info.QueryExtKeys = len(vol.queryIndex.ext)
			info.QueryDirs = len(vol.queryIndex.dirs)
		}
		info.NameOrderState = vol.nameOrderStateString()
		info.NameOrderMillis = vol.nameOrderMillis.Load()
		info.NameTrigramState = vol.nameTrigramStateString()
		info.NameTrigramMillis = vol.nameTrigramMillis.Load()
		info.DerivedSections, info.DerivedBytes = derivedSectionInfo(idx.Derived)
		info.Memory = vol.residentMemoryInfo()
		infos = append(infos, info)
	}
	if !loading && serviceResidentBackgroundLoading(volumes) {
		loading = true
	}
	message := ""
	if loading {
		message = "loading indexes"
	} else if loadErr != "" {
		message = loadErr
	}
	health, healthMessage := classifyServiceHealth(loading, loadErr, infos)
	_ = json.NewEncoder(w).Encode(serviceInfoResponseFor(serviceResponse{OK: loadErr == "", Message: message, Entries: total, Loading: loading, DBs: infos, Runtime: runtimeMemorySnapshot(), Health: health, HealthMessage: healthMessage, Content: s.contentHealthSnapshot()}, s.pipeName, s.processMode))
}

func (s *goSearchService) serviceCommandSearch(w io.Writer, caps serviceCapabilities, req *serviceRequest) {
	serviceNoteQueryActivity()
	s.indexMu.RLock()
	if len(s.indexes) == 0 {
		loading := s.loading
		loadErr := s.loadErr
		s.indexMu.RUnlock()
		message := "service has no search indexes loaded"
		if loading {
			message = "loading indexes"
		} else if loadErr != "" {
			message = loadErr
		}
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: message, Loading: loading})
		return
	}
	opts := requestToOptionsFromService(*req)
	if opts.DeadlineUnix == 0 {
		opts.DeadlineUnix = time.Now().Add(serviceQueryTimeout - 250*time.Millisecond).UnixNano()
	}
	trace := &searchTrace{}
	opts.Trace = trace
	if req.RequestSeq > 0 {
		for {
			current := s.requestSeq.Load()
			if req.RequestSeq <= current || s.requestSeq.CompareAndSwap(current, req.RequestSeq) {
				break
			}
		}
		opts.Cancel = func() bool {
			return req.RequestSeq < s.requestSeq.Load()
		}
	}
	if req.CancelOverride != nil {
		opts.Cancel = req.CancelOverride
	}
	var matches []Entry
	var err error
	var fuzzyApplied bool
	searchStart := time.Now()
	if len(s.volumes) == len(s.indexes) {
		volumes := snapshotServiceVolumesForSearch(s.volumes)
		// Hold the read lock through the whole query: a background persist
		// swaps the memory-mapped index under indexMu.Lock(), and an
		// in-flight search must not touch an un-mapped view.
		if req.CountOnly {
			if count, ok, countErr := countServiceVolumes(volumes, opts); ok {
				s.indexMu.RUnlock()
				searchMS := float64(time.Since(searchStart).Nanoseconds()) / 1_000_000
				if countErr != nil {
					_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: countErr.Error()})
				} else {
					trace.setSource("count-fast-posting", count)
					_ = json.NewEncoder(w).Encode(serviceResponse{OK: true, Count: count, SearchMS: searchMS, Source: trace.Source, Decline: trace.Decline, Candidates: trace.Candidates, PlannerMode: trace.PlannerMode, EligibleVolumes: trace.EligibleVolumes, BlocksDecoded: trace.BlocksDecoded, BlocksSkipped: trace.BlocksSkipped, ScalarDriver: trace.ScalarDriver, ScalarInterval: trace.ScalarInterval, RecordsVerified: trace.ScalarRecordsVerified, ComponentDriver: trace.ComponentDriver, ComponentRoots: trace.ComponentRoots, ComponentIntervals: trace.ComponentIntervals, ComponentCardinality: trace.ComponentCardinality, ComponentSelfHits: trace.ComponentSelfHits, ComponentBounds: trace.ComponentBounds, ComponentRecordsVerified: trace.ComponentRecordsVerified, FilenameDriver: trace.FilenameDriver, FilenameRequiredGrams: trace.FilenameRequiredGrams, FilenamePostingHint: trace.FilenamePostingHint, FilenameRecordsVerified: trace.FilenameRecordsVerified, OverlayBaseWindow: trace.OverlayBaseWindow, PostingPrefetchBytes: trace.PostingPrefetchBytes, PostingPrefetchRanges: trace.PostingPrefetchRanges, PostingPrefetchPages: trace.PostingPrefetchPages, Terms: trace.Terms, Declines: trace.Declines, Fallback: trace.Fallback, Complete: trace.completePtr(), Content: s.searchContentHealth(trace)})
				}
				return
			}
		}
		matches, err = searchServiceVolumes(volumes, opts, req.CountOnly)
		fuzzied := false
		if err == nil && !req.CountOnly {
			// Fuzzy only runs when the exact tier underfilled the limit,
			// and must stay under the read lock: it reads compact records
			// that a background persist may unmap.
			if limit := normalizedLimit(opts.Limit, false); len(matches) < limit {
				if !tryMultiTermFuzzyRewrite(volumes, &opts, &matches, &fuzzied) {
					matches, fuzzied = appendFuzzyServiceMatches(volumes, opts, matches)
				}
			}
		}
		s.indexMu.RUnlock()
		if fuzzied {
			opts.Trace.setPlannerMode("fuzzy-trigram")
		}
		fuzzyApplied = fuzzied
	} else {
		matches, err = searchAll(s.indexes, opts, req.CountOnly)
		s.indexMu.RUnlock()
	}
	searchMS := float64(time.Since(searchStart).Nanoseconds()) / 1_000_000
	if err != nil {
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: err.Error()})
		return
	}
	if caps.Remote {
		// Remote logs use only the coarse public source category and omit
		// planner/decline detail: internal sources can embed query-derived
		// extension or path terms, which must not reach the log.
		serviceLog("search remote ms=%.1f source=%s results=%d", searchMS, remoteSearchSource(trace.Source, req.CountOnly), len(matches))
	} else {
		serviceLog("search query=%q ms=%.1f planner=%s source=%s decline=%s filename_driver=%s candidates=%d results=%d", req.Query, searchMS, trace.PlannerMode, trace.Source, trace.Decline, trace.FilenameDriver, trace.Candidates, len(matches))
	}
	resp := serviceResponse{OK: true, Count: len(matches), SearchMS: searchMS, Fuzzy: fuzzyApplied, Source: trace.Source, Decline: trace.Decline, Candidates: trace.Candidates, PlannerMode: trace.PlannerMode, EligibleVolumes: trace.EligibleVolumes, BlocksDecoded: trace.BlocksDecoded, BlocksSkipped: trace.BlocksSkipped, ScalarDriver: trace.ScalarDriver, ScalarInterval: trace.ScalarInterval, RecordsVerified: trace.ScalarRecordsVerified, ComponentDriver: trace.ComponentDriver, ComponentRoots: trace.ComponentRoots, ComponentIntervals: trace.ComponentIntervals, ComponentCardinality: trace.ComponentCardinality, ComponentSelfHits: trace.ComponentSelfHits, ComponentBounds: trace.ComponentBounds, ComponentRecordsVerified: trace.ComponentRecordsVerified, FilenameDriver: trace.FilenameDriver, FilenameRequiredGrams: trace.FilenameRequiredGrams, FilenamePostingHint: trace.FilenamePostingHint, FilenameRecordsVerified: trace.FilenameRecordsVerified, OverlayBaseWindow: trace.OverlayBaseWindow, PostingPrefetchBytes: trace.PostingPrefetchBytes, PostingPrefetchRanges: trace.PostingPrefetchRanges, PostingPrefetchPages: trace.PostingPrefetchPages, Terms: trace.Terms, Declines: trace.Declines, Fallback: trace.Fallback, Complete: trace.completePtr()}
	if !req.CountOnly {
		resp.Results = make([]string, len(matches))
		for i, entry := range matches {
			resp.Results[i] = entry.Path
		}
		resp.Rows = entriesToJSON(matches)
		if caps.Remote {
			// Content text is local-only. Strip it at the command boundary so a
			// future transport cannot leak a snippet even if its own projection
			// forgets to.
			resp.redactContentText()
		}
	}
	resp.Content = s.searchContentHealth(trace)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *goSearchService) serviceCommandWatchDelta(w io.Writer, req *serviceRequest) {
	serviceNoteQueryActivity()
	s.indexMu.RLock()
	if len(s.indexes) == 0 {
		loading := s.loading
		loadErr := s.loadErr
		s.indexMu.RUnlock()
		message := "service has no search indexes loaded"
		if loading {
			message = "loading indexes"
		} else if loadErr != "" {
			message = loadErr
		}
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: message, Loading: loading})
		return
	}
	opts := requestToOptionsFromService(*req)
	if opts.DeadlineUnix == 0 {
		opts.DeadlineUnix = time.Now().Add(serviceQueryTimeout - 250*time.Millisecond).UnixNano()
	}
	trace := &searchTrace{}
	opts.Trace = trace
	if req.RequestSeq > 0 {
		for {
			current := s.requestSeq.Load()
			if req.RequestSeq <= current || s.requestSeq.CompareAndSwap(current, req.RequestSeq) {
				break
			}
		}
		opts.Cancel = func() bool {
			return req.RequestSeq < s.requestSeq.Load()
		}
	}
	if req.CancelOverride != nil {
		opts.Cancel = req.CancelOverride
	}
	pq, parseErr := parseQuery(opts)
	if parseErr != nil {
		s.indexMu.RUnlock()
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: parseErr.Error()})
		return
	}
	volumes := append([]*serviceVolumeIndex(nil), s.volumes...)
	var sinceCursors []watchVolumeCursor
	if req.Baseline || len(req.SinceVolumes) == 0 {
		// Silent baseline: establish the current watermark for every
		// volume so the next delta request only sees changes made after
		// watch started.
		for _, vol := range volumes {
			if vol == nil || vol.index == nil {
				continue
			}
			wm := uint64(0)
			if snap := vol.snap.Load(); snap != nil {
				wm = uint64(snap.watermark)
			}
			sinceCursors = append(sinceCursors, watchVolumeCursor{Volume: vol.volume, Seq: wm})
		}
		s.indexMu.RUnlock()
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: true, WatchVolumes: sinceCursors})
		return
	}
	sinceCursors = req.SinceVolumes
	nextCursors, events, err := serviceWatchDelta(volumes, sinceCursors, pq, trace)
	s.indexMu.RUnlock()
	if err != nil {
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: err.Error()})
		return
	}
	serviceLog("watch-delta query=%q volumes=%d events=%d", req.Query, len(nextCursors), len(events))
	_ = json.NewEncoder(w).Encode(serviceResponse{OK: true, WatchVolumes: nextCursors, WatchEvents: events, Source: trace.Source, Decline: trace.Decline})
}

func (s *goSearchService) serviceCommandIndexUSN(w io.Writer, req *serviceRequest) {
	serviceLog("index-usn start volume=%s db=%s", req.Volume, req.DB)
	idx, err := indexUSNVolume(req.Volume)
	if err != nil {
		serviceLog("index-usn error volume=%s err=%v", req.Volume, err)
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: err.Error()})
		return
	}
	serviceLog("index-usn built volume=%s entries=%d", req.Volume, idx.entryCount())
	buildOrders(idx)
	serviceLog("index-usn orders volume=%s", req.Volume)
	// Stage the multi-GB v9 write outside the global lock so pipe
	// requests keep serving while it runs; only the fast swap-in below
	// takes indexMu.Lock().
	tmp, stageErr := stageIndexFile(req.DB, idx)
	if stageErr != nil {
		serviceLog("index-usn stage error volume=%s err=%v", req.Volume, stageErr)
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: stageErr.Error()})
		return
	}
	s.indexMu.Lock()
	// The low-memory mmap pins the target file open, so unmap any existing
	// volume for this DB before the temp-file rename replaces the file.
	for _, existing := range s.volumes {
		if existing != nil && samePath(existing.dbPath, req.DB) {
			if err := closeIndexMMapRecords(existing.index); err != nil {
				serviceLog("index-usn close mmap volume=%s err=%v", req.Volume, err)
			}
			break
		}
	}
	if err := commitStageIndexFile(req.DB, tmp); err != nil {
		s.indexMu.Unlock()
		serviceLog("index-usn save error volume=%s err=%v", req.Volume, err)
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: err.Error()})
		return
	}
	if err := removeWAL(req.DB); err != nil {
		serviceLog("wal cleanup error volume=%s db=%s err=%v", req.Volume, req.DB, err)
	}
	// Reload from disk so the swapped-in volume carries the persisted
	// derived sections (RANK/PNGR/PNGC etc.).  The in-memory idx built by
	// indexUSNVolume never populates Derived; using it here would leave
	// the volume without SelfNameTrigrams and force every multi-term name
	// query onto the slow bounded-scan path.
	loaded, loadErr := loadIndexForService(req.DB)
	if loadErr != nil {
		s.indexMu.Unlock()
		serviceLog("index-usn reload error volume=%s err=%v", req.Volume, loadErr)
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: loadErr.Error()})
		return
	}
	vol := s.replaceLoadedVolumeLocked(req.DB, loaded)
	s.indexMu.Unlock()
	releaseServiceMemoryAfterSave()
	s.startBackgroundNameOrderBuilds([]*serviceVolumeIndex{vol})
	s.startBackgroundNameTrigramBuilds([]*serviceVolumeIndex{vol})
	// A runtime-added or re-indexed volume gets content attached or scheduled
	// (PF-3), instead of staying content-unavailable until a restart.
	s.ensureContentBuild(vol)
	serviceLog("index-usn complete volume=%s entries=%d", req.Volume, idx.entryCount())
	_ = json.NewEncoder(w).Encode(serviceResponse{OK: true, Message: "indexed", Entries: idx.entryCount()})
}

func (s *goSearchService) serviceCommandStatus(w io.Writer) {
	_ = json.NewEncoder(w).Encode(serviceInfoResponseFor(serviceResponse{OK: true, Message: "service running", Content: s.contentHealthSnapshot()}, s.pipeName, s.processMode))
}

func serviceResidentBackgroundLoading(volumes []*serviceVolumeIndex) bool {
	for _, vol := range volumes {
		if vol == nil {
			continue
		}
		nameOrderState := vol.nameOrderStateString()
		if nameOrderState == "pending" || nameOrderState == "building" {
			return true
		}
		nameTrigramState := vol.nameTrigramStateString()
		if nameTrigramState == "pending" || nameTrigramState == "building" {
			return true
		}
	}
	return false
}

func (s *goSearchService) replaceLoadedVolume(dbPath string, idx *Index) {
	if s == nil || idx == nil || dbPath == "" {
		return
	}
	s.indexMu.Lock()
	vol := s.replaceLoadedVolumeLocked(dbPath, idx)
	s.indexMu.Unlock()
	s.startBackgroundNameOrderBuilds([]*serviceVolumeIndex{vol})
	s.startBackgroundNameTrigramBuilds([]*serviceVolumeIndex{vol})
	s.ensureContentBuild(vol)
}

func (s *goSearchService) replaceLoadedVolumeLocked(dbPath string, idx *Index) *serviceVolumeIndex {
	if s == nil || idx == nil || dbPath == "" {
		return nil
	}
	vol := newServiceVolumeIndex(dbPath, idx)
	for i, existing := range s.volumes {
		if existing != nil && (samePath(existing.dbPath, dbPath) || strings.EqualFold(existing.volume, idx.Volume)) {
			replaceServiceVolumeContents(existing, vol)
			if i < len(s.indexes) {
				s.indexes[i] = existing.index
			}
			return existing
		}
	}
	s.volumes = append(s.volumes, vol)
	s.indexes = append(s.indexes, idx)
	return vol
}

func replaceServiceVolumeContents(dst, src *serviceVolumeIndex) {
	if dst == nil || src == nil {
		return
	}
	// The destination's content coordinator is preserved below (content is
	// generation-independent), so its drain keeps running across a base swap. A
	// replacement object that carries a different coordinator must not leave a
	// drain of its own behind: retire it (P6-4). This is a no-op today because
	// the replacement is always a fresh, undrained volume.
	if src.contentCoord != nil && src.contentCoord != dst.contentCoord {
		src.stopContentDrain()
	}
	prevJournalID := dst.journalID
	dst.dbPath = src.dbPath
	dst.index = src.index
	dst.volume = src.volume
	dst.journalID = src.journalID
	dst.checkpoint = src.checkpoint
	dst.baseCheckpoint = src.baseCheckpoint
	dst.state = src.state
	dst.staleReason = src.staleReason
	dst.frnToID = src.frnToID
	dst.frns = src.frns
	dst.frnRecordIDs = src.frnRecordIDs
	dst.children = src.children
	dst.childOffsets = src.childOffsets
	dst.childIDs = src.childIDs
	dst.rootIDs = src.rootIDs
	dst.subtreeOrder = src.subtreeOrder
	dst.subtreeStart = src.subtreeStart
	dst.subtreeEnd = src.subtreeEnd
	dst.subtreeSizeRank = src.subtreeSizeRank
	dst.subtreeModRank = src.subtreeModRank
	dst.subtreeExtRank = src.subtreeExtRank
	dst.subtreeTypeRank = src.subtreeTypeRank
	dst.subtreePathRank = src.subtreePathRank
	dst.subtreeBytes = src.subtreeBytes
	dst.dirSizeDelta = nil
	dst.exactNames = src.exactNames
	dst.pathCache = src.pathCache
	dst.queryIndex = src.queryIndex
	dst.nameOrderState.Store(src.nameOrderState.Load())
	dst.nameOrderMillis.Store(src.nameOrderMillis.Load())
	dst.nameTrigrams.Store(src.nameTrigramIndex())
	dst.nameQuadgrams.Store(src.nameQuadgramIndex())
	dst.nameTrigramState.Store(src.nameTrigramState.Load())
	dst.nameTrigramMillis.Store(src.nameTrigramMillis.Load())
	dst.termCache = src.termCache
	dst.pathTermCache = src.pathTermCache
	dst.extCache = src.extCache
	dst.recentIDs = src.recentIDs
	dst.nameTrigramRecent = src.nameTrigramRecent
	dst.recentSeq = src.recentSeq
	dst.underCache = src.underCache
	dst.underRootCache = src.underRootCache
	dst.overlay = src.overlay
	dst.snap.Store(src.snap.Load())
	dst.snapshotGen.Store(src.snapshotGen.Load())
	dst.dirty = src.dirty
	dst.lastPersist = src.lastPersist
	dst.searchCount = src.searchCount
	// Content is generation-independent: keep dst's loaded content state and
	// rebind its resolver against the new record table. A base swap must never
	// re-extract. A real journal reset (the journal id changed) is different:
	// the FRN-keyed content docs belong to the old journal generation, so the
	// content index is invalidated rather than served stale.
	if prevJournalID != 0 && src.journalID != 0 && prevJournalID != src.journalID {
		invalidateContentAfterBaseReset(dst, fmt.Sprintf("journal id changed from %d to %d", prevJournalID, src.journalID))
		return
	}
	rebindContentAfterBaseSwap(dst)
}

func snapshotServiceVolumesForSearch(volumes []*serviceVolumeIndex) []*serviceVolumeIndex {
	out := make([]*serviceVolumeIndex, 0, len(volumes))
	for _, vol := range volumes {
		out = append(out, snapshotServiceVolumeForSearch(vol))
	}
	return out
}

func snapshotServiceVolumeForSearch(vol *serviceVolumeIndex) *serviceVolumeIndex {
	if vol == nil {
		return nil
	}
	snap := vol.snap.Load()
	idx := vol.index
	if snap != nil && snap.base != nil {
		idx = snap.base
	}
	view := &serviceVolumeIndex{
		dbPath:            vol.dbPath,
		index:             idx,
		volume:            vol.volume,
		journalID:         vol.journalID,
		checkpoint:        vol.checkpoint,
		state:             vol.state,
		staleReason:       vol.staleReason,
		frnToID:           vol.frnToID,
		frns:              vol.frns,
		frnRecordIDs:      vol.frnRecordIDs,
		children:          vol.children,
		childOffsets:      vol.childOffsets,
		childIDs:          vol.childIDs,
		rootIDs:           vol.rootIDs,
		subtreeOrder:      vol.subtreeOrder,
		subtreeStart:      vol.subtreeStart,
		subtreeEnd:        vol.subtreeEnd,
		subtreeSizeRank:   vol.subtreeSizeRank,
		subtreeModRank:    vol.subtreeModRank,
		subtreeExtRank:    vol.subtreeExtRank,
		subtreeTypeRank:   vol.subtreeTypeRank,
		subtreePathRank:   vol.subtreePathRank,
		exactNames:        vol.exactNames,
		pathCache:         make(map[int]string),
		queryIndex:        vol.queryIndex,
		termCache:         make(map[string]postingCacheEntry),
		pathTermCache:     make(map[string]postingCacheEntry),
		extCache:          make(map[string]postingCacheEntry),
		underCache:        make(map[int]postingCacheEntry),
		underRootCache:    make(map[string]postingCacheEntry),
		dirty:             vol.dirty,
		lastPersist:       vol.lastPersist,
		persistFailures:   vol.persistFailures,
		persistRetryAfter: vol.persistRetryAfter,
		lastPersistErr:    vol.lastPersistErr,
	}
	if snap != nil {
		view.recentSeq = snap.gen
		view.snap.Store(snap)
	} else {
		view.recentSeq = vol.cacheGeneration()
	}
	if trigrams := vol.nameTrigramIndex(); trigrams != nil {
		view.nameTrigrams.Store(trigrams)
	}
	if quadgrams := vol.nameQuadgramIndex(); quadgrams != nil {
		view.nameQuadgrams.Store(quadgrams)
	}
	view.nameOrderState.Store(vol.nameOrderState.Load())
	view.nameOrderMillis.Store(vol.nameOrderMillis.Load())
	view.nameTrigramState.Store(vol.nameTrigramState.Load())
	view.nameTrigramMillis.Store(vol.nameTrigramMillis.Load())
	return view
}

func searchService(pipeName string, opts queryOptions, countOnly bool, jsonOut bool) error {
	resp, err := callService(pipeName, serviceRequestFromOptions(opts, countOnly))
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Message)
	}
	if jsonOut {
		jsonResp := newJSONSearchResponse(resp, opts, countOnly)
		return writeJSON(os.Stdout, jsonResp)
	}
	if countOnly {
		fmt.Println(resp.Count)
		return nil
	}
	w := bufio.NewWriter(os.Stdout)
	if resp.Fuzzy {
		fmt.Fprintf(w, "(showing close matches for %q)\n", opts.Query)
	}
	for _, result := range resp.Results {
		fmt.Fprintln(w, result)
	}
	return w.Flush()
}

// newJSONSearchResponse projects a service response onto the CLI JSON shape. It
// carries the content health (state/partial/incomplete/count_divergent and the
// degraded volumes) that the service already surfaces on serviceResponse, so a
// CLI/JSON caller can tell why a content result is not complete instead of
// seeing only complete=false.
func newJSONSearchResponse(resp serviceResponse, opts queryOptions, countOnly bool) jsonSearchResponse {
	out := jsonSearchResponse{
		OK:                       true,
		Query:                    opts.Query,
		Count:                    resp.Count,
		Limit:                    opts.Limit,
		SearchMS:                 resp.SearchMS,
		Source:                   resp.Source,
		Decline:                  resp.Decline,
		Candidates:               resp.Candidates,
		BlocksDecoded:            resp.BlocksDecoded,
		BlocksSkipped:            resp.BlocksSkipped,
		ScalarDriver:             resp.ScalarDriver,
		ScalarInterval:           resp.ScalarInterval,
		RecordsVerified:          resp.RecordsVerified,
		ComponentDriver:          resp.ComponentDriver,
		ComponentRoots:           resp.ComponentRoots,
		ComponentIntervals:       resp.ComponentIntervals,
		ComponentCardinality:     resp.ComponentCardinality,
		ComponentSelfHits:        resp.ComponentSelfHits,
		ComponentBounds:          resp.ComponentBounds,
		ComponentRecordsVerified: resp.ComponentRecordsVerified,
		FilenameDriver:           resp.FilenameDriver,
		FilenameRequiredGrams:    resp.FilenameRequiredGrams,
		FilenamePostingHint:      resp.FilenamePostingHint,
		FilenameRecordsVerified:  resp.FilenameRecordsVerified,
		OverlayBaseWindow:        resp.OverlayBaseWindow,
		PostingPrefetchBytes:     resp.PostingPrefetchBytes,
		PostingPrefetchRanges:    resp.PostingPrefetchRanges,
		PostingPrefetchPages:     resp.PostingPrefetchPages,
		PlannerMode:              resp.PlannerMode,
		Fuzzy:                    resp.Fuzzy,
		EligibleVolumes:          resp.EligibleVolumes,
		Terms:                    resp.Terms,
		Declines:                 resp.Declines,
		Fallback:                 resp.Fallback,
		Complete:                 resp.Complete,
		Content:                  resp.Content,
	}
	if !countOnly {
		if len(resp.Rows) > 0 {
			out.Results = resp.Rows
		} else {
			out.Results = pathsToJSON(resp.Results)
		}
	}
	return out
}
