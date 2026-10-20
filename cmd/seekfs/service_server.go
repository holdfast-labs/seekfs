package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
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

// errServicePipeStopping reports that service stop closed a pipe instance
// before it could be handed to a connection handler.
var errServicePipeStopping = errors.New("service pipe stopping")

// pipeConnectedTestHook, when set by a test, runs after ConnectNamedPipe
// returns and before the stop watcher is released, to force the stop/connect
// race deterministically. Always nil in production.
var pipeConnectedTestHook func()

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
	// Stop closes the handle to unblock ConnectNamedPipe. The watcher must be
	// finished before the handle is used or closed here: when stop and a
	// client connect race, select may pick stop after the connect succeeded,
	// and returning that closed handle lets the handler read and write a
	// handle number the process may already have reused for another file.
	done := make(chan struct{})
	watchDone := make(chan struct{})
	closedByStop := false
	go func() {
		defer close(watchDone)
		select {
		case <-s.stop:
			closedByStop = true
			_ = windows.CloseHandle(handle)
		case <-done:
		}
	}()
	err = windows.ConnectNamedPipe(handle, nil)
	if pipeConnectedTestHook != nil {
		pipeConnectedTestHook()
	}
	close(done)
	<-watchDone
	if closedByStop {
		return nil, errServicePipeStopping
	}
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
	// A client that gives up closes the pipe. Watch for that end-of-stream
	// while the query runs, so an abandoned content request stops verifying
	// instead of reading files for a response nobody reads. PeekNamedPipe is
	// non-blocking, so it shares the synchronous pipe handle with the response
	// write without consuming request bytes.
	clientGone := make(chan struct{})
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-ticker.C:
				if servicePipeClientGone(conn.Fd()) {
					close(clientGone)
					return
				}
			}
		}
	}()
	defer func() {
		close(stopWatch)
		<-watchDone
	}()
	priorCancel := req.CancelOverride
	req.CancelOverride = func() bool {
		if priorCancel != nil && priorCancel() {
			return true
		}
		select {
		case <-clientGone:
			return true
		default:
			return false
		}
	}
	// Phase 1: derive the caller's capabilities from its Windows identity by
	// impersonating the pipe client.  Failures fail closed (read-only).  A
	// RevertToSelf failure is process-fatal and does not return.
	principal := servicePrincipalForPipeConn(conn)
	caps := localServiceCapabilities(principal)
	s.handleServiceCommand(conn, principal, caps, &req)
}

// procPeekNamedPipe exposes PeekNamedPipe, which x/sys/windows does not declare.
var procPeekNamedPipe = syscall.NewLazyDLL("kernel32.dll").NewProc("PeekNamedPipe")

// servicePipeClientGone reports whether the other end of the pipe has closed.
// PeekNamedPipe is non-blocking and does not consume bytes. Only the
// disconnect errors count as gone; any other failure keeps the request alive,
// because canceling a live client's query would be worse than finishing it.
func servicePipeClientGone(handle uintptr) bool {
	var available uint32
	r1, _, err := procPeekNamedPipe.Call(handle, 0, 0, 0, uintptr(unsafe.Pointer(&available)), 0)
	if r1 != 0 {
		return false
	}
	return servicePipeDisconnectErr(err)
}

// servicePipeDisconnectErr reports whether a pipe call failed because the
// other end is gone.
func servicePipeDisconnectErr(err error) bool {
	errno, ok := err.(syscall.Errno)
	if !ok {
		return false
	}
	switch errno {
	case 109, 232, 233: // ERROR_BROKEN_PIPE, ERROR_NO_DATA, ERROR_PIPE_NOT_CONNECTED
		return true
	}
	return false
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
	case "search", "info", "status", "settings-get":
		return serviceCommandReadOnly
	case "watch-delta", "plugin-list", "open", "recent", "input-heartbeat":
		// watch-delta is a read-only local operation but is deferred remotely
		// until Phase 7: it exposes cursor semantics that need scoped identity.
		// open/recent are local-only usage signals (frecency store); they
		// mutate only the local signals file, need no elevation, and never
		// leave the machine. input-heartbeat is the same class: the UI's
		// session input time, local only, never leaves the machine.
		return serviceCommandLocalOnly
	case "index-usn", "plugin-reload", "settings-set":
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
		if req.Command == "settings-set" && !caps.Mutate {
			message = "settings-set requires an elevated caller"
		} else if classifyServiceCommand(req.Command) == serviceCommandMutate {
			message = "this command requires elevation; rerun it from an elevated prompt"
		}
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: message})
		return
	}
	switch req.Command {
	case "info":
		s.serviceCommandInfo(w, req)
	case "search":
		s.serviceCommandSearch(w, caps, principal, req)
	case "open":
		s.serviceCommandOpen(w, principal, req)
	case "recent":
		s.serviceCommandRecent(w, principal, req)
	case "input-heartbeat":
		s.serviceCommandInputHeartbeat(w, req)
	case "watch-delta":
		s.serviceCommandWatchDelta(w, req)
	case "index-usn":
		s.serviceCommandIndexUSN(w, req)
	case "status":
		s.serviceCommandStatus(w)
	case "plugin-list", "plugin-reload":
		s.serviceCommandPlugins(w, req)
	case "settings-get":
		s.serviceCommandSettingsGet(w, caps)
	case "settings-set":
		s.serviceCommandSettingsSet(w, caps, req)
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
	_ = json.NewEncoder(w).Encode(serviceInfoResponseFor(serviceResponse{OK: loadErr == "", Message: message, Entries: total, Loading: loading, DBs: infos, Runtime: runtimeMemorySnapshot(), Health: health, HealthMessage: healthMessage, Content: s.contentHealthSnapshot(), Governor: contentGovernorHealthSnapshot(), Features: s.featureHealthSnapshot(), PluginAPI: 1, PluginConfig: s.pluginFilePath()}, s.pipeName, s.processMode))
}

func (s *goSearchService) serviceCommandSearch(w io.Writer, caps serviceCapabilities, principal servicePrincipal, req *serviceRequest) {
	serviceNoteQueryActivity()
	serviceActiveQueryEnter()
	defer serviceActiveQueryLeave()
	if req.BudgetMS < 0 || (req.BudgetDisabled && req.BudgetMS > 0) {
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: "invalid content budget fields"})
		return
	}
	if queryHasFeatureToken(req.Query) {
		s.serviceCommandFeatureSearch(w, caps, req)
		return
	}
	s.indexMu.RLock()
	if len(s.indexes) == 0 {
		loading := s.loading
		loadErr := s.loadErr
		s.indexMu.RUnlock()
		if !loading && loadErr == "" && !req.CountOnly {
			// Folder-first on-demand answer (fsearch-style): no index
			// covers the scope, so walk the requested folder itself and
			// answer from it. Bounded (100k entries / 2s), read-only, and
			// only when --under names a real directory — every other shape
			// keeps the error contract below. A full background index for
			// the folder is deliberately not started here: it would race
			// the budgeted builder and stall the query behind a build.
			if resp, ok := serviceOnDemandFolderResponse(s, req); ok {
				_ = json.NewEncoder(w).Encode(resp)
				return
			}
		}
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
	cursorOffset := 0
	var v2Cursor *searchCursorV2
	if req.Cursor != "" {
		var cursorErr error
		cursorOffset, v2Cursor, cursorErr = decodeAnySearchCursor(req.Cursor, opts)
		if cursorErr != nil {
			s.indexMu.RUnlock()
			_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: cursorErr.Error()})
			return
		}
	}
	pageLimit := normalizedLimit(opts.Limit, false)
	if req.Cursor != "" && !req.CountOnly && v2Cursor == nil {
		opts.Limit = cursorOffset + pageLimit
	}
	// Start an explicitly requested wall-clock budget once for the complete
	// content request, not once per volume. The historical omitted-budget default
	// is unbounded; only a positive --budget arms a deadline. Parse to scope it
	// to content queries so name-only searches do not pay for it.
	if opts.Trace == nil {
		opts.Trace = &searchTrace{}
	}
	if pq, parseErr := parseQuery(opts); parseErr == nil && queryHasAnyContentLeaf(pq) {
		budgetMS, budgetDisabled := serviceClientContentBudgetMS(*req)
		opts.Trace.armContentDeadline(budgetMS, budgetDisabled)
	}
	// Per-user ranking: the frecency/query-history store is keyed by the
	// caller's SID so one LocalSystem service keeps each local user's
	// history separate. Empty SID gets an ephemeral empty store — no leak,
	// and with no history the order is byte-identical to the base ranker.
	opts.SignalStore = serviceSignalStoreForSID(principal.SID)
	if opts.DeadlineUnix == 0 {
		opts.DeadlineUnix = time.Now().Add(serviceQueryTimeout - 250*time.Millisecond).UnixNano()
	}
	trace := opts.Trace
	if trace == nil {
		trace = &searchTrace{}
	}
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
	// Resolve a content cursor v2 against the current volume identity before
	// the search. An identity mismatch restarts at offset zero; the search
	// below then returns the first page again. A match resumes after the last
	// emitted record and prices the disk budget at one page.
	v2Resume := false
	v2Restarted := false
	v2RestartReason := ""
	resumeBaseFRN := uint64(0)
	var cursorCtx *contentCursorContext
	if !req.CountOnly {
		if pq, parseErr := parseQuery(opts); parseErr == nil {
			if ctx, ok := s.contentCursorContextFor(pq, opts); ok {
				cursorCtx = ctx
				if v2Cursor != nil {
					if ok, reason := v2Cursor.check(*ctx); ok {
						if pq.SortColumn != "" &&
							(v2Cursor.K == nil || !v2Cursor.K.Explicit || v2Cursor.K.SortColumn != pq.SortColumn) {
							// Old tokens carry a rank key, which cannot order an
							// explicit sort. Restart instead of skipping rows.
							v2Restarted = true
							v2RestartReason = "content cursor sort order changed"
						} else {
							v2Resume = true
							resumeBaseFRN = v2Cursor.BK
							if v2Cursor.K == nil {
								// A pre-overlay token carried only the last row's
								// FRN, and that row was always a base row.
								resumeBaseFRN = v2Cursor.ID
							}
							opts.contentAfter = &contentCursorResume{
								FRN:   resumeBaseFRN,
								Found: resumeBaseFRN == 0,
								Key:   v2Cursor.K,
							}
							opts.Limit = pageLimit
						}
					} else {
						v2Restarted = true
						v2RestartReason = reason
					}
				}
			} else if v2Cursor != nil {
				v2Restarted = true
				v2RestartReason = "content cursor no longer applies"
			}
		}
	}
	var matches []Entry
	var err error
	var fuzzyApplied bool
	var fuzzyCorrection string
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
					resp := serviceResponse{OK: false, Message: countErr.Error(), Complete: trace.completePtr(), BudgetExceeded: trace.BudgetExceeded}
					if trace.contentIncomplete() {
						resp.Complete = boolPtr(false)
					}
					resp.Content = s.searchContentHealth(trace)
					if resp.Content != nil && resp.Content.Stale {
						resp.ContentStale = true
						resp.Complete = boolPtr(false)
					}
					if !trace.BudgetDeadline.IsZero() {
						resp.ContentBudgetDeadlineUnix = trace.BudgetDeadline.UnixNano()
					}
					_ = json.NewEncoder(w).Encode(resp)
				} else {
					trace.setSource("count-fast-posting", count)
					fuzzy := trace.FuzzyCorrection != ""
					_ = json.NewEncoder(w).Encode(serviceResponse{OK: true, Count: count, SearchMS: searchMS, Fuzzy: fuzzy, FuzzyCorrection: trace.FuzzyCorrection, Source: trace.Source, Decline: trace.Decline, Candidates: trace.Candidates, PlannerMode: trace.PlannerMode, EligibleVolumes: trace.EligibleVolumes, BlocksDecoded: trace.BlocksDecoded, BlocksSkipped: trace.BlocksSkipped, ScalarDriver: trace.ScalarDriver, ScalarInterval: trace.ScalarInterval, RecordsVerified: trace.ScalarRecordsVerified, ComponentDriver: trace.ComponentDriver, ComponentRoots: trace.ComponentRoots, ComponentIntervals: trace.ComponentIntervals, ComponentCardinality: trace.ComponentCardinality, ComponentSelfHits: trace.ComponentSelfHits, ComponentBounds: trace.ComponentBounds, ComponentRecordsVerified: trace.ComponentRecordsVerified, FilenameDriver: trace.FilenameDriver, FilenameRequiredGrams: trace.FilenameRequiredGrams, FilenamePostingHint: trace.FilenamePostingHint, FilenameRecordsVerified: trace.FilenameRecordsVerified, OverlayBaseWindow: trace.OverlayBaseWindow, PostingPrefetchBytes: trace.PostingPrefetchBytes, PostingPrefetchRanges: trace.PostingPrefetchRanges, PostingPrefetchPages: trace.PostingPrefetchPages, Terms: trace.Terms, Declines: trace.Declines, Fallback: trace.Fallback, Complete: trace.completePtr(), ContentStale: trace.ContentStale, Content: s.searchContentHealth(trace), BudgetExceeded: trace.BudgetExceeded})
				}
				return
			}
		}
		matches, err = searchServiceVolumes(volumes, opts, req.CountOnly)
		fuzzied := false
		if err == nil && !req.CountOnly && v2Cursor == nil {
			// Fuzzy only runs when the exact tier underfilled the limit,
			// and must stay under the read lock: it reads compact records
			// that a background persist may unmap. A v2 content cursor
			// never mixes fuzzy rows into a page: its resumption is exact.
			if limit := normalizedLimit(opts.Limit, false); len(matches) < limit {
				if handled := tryMultiTermFuzzyRewrite(volumes, &opts, &matches, &fuzzied); !handled {
					if contentMatches, correction, ok := tryContentFuzzyRewrite(volumes, opts, matches); ok {
						matches, fuzzied, fuzzyCorrection = contentMatches, true, correction
					} else {
						matches, fuzzied = appendFuzzyServiceMatches(volumes, opts, matches)
					}
				}
			}
		}
		s.indexMu.RUnlock()
		if fuzzied && fuzzyCorrection == "" {
			opts.Trace.setPlannerMode("fuzzy-trigram")
		}
		fuzzyApplied = fuzzied
	} else {
		matches, err = searchAll(s.indexes, opts, req.CountOnly)
		s.indexMu.RUnlock()
	}
	// A post-query client may ask for raw --lines output. Publish the final
	// content accounting after fuzzy top-up too, so the response can carry the
	// exact remaining deadline and disk-read allowance for that optional pass.
	trace.publishDiskBudget()
	searchMS := float64(time.Since(searchStart).Nanoseconds()) / 1_000_000
	if err != nil {
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: err.Error()})
		return
	}
	if v2Resume && opts.contentAfter != nil && !opts.contentAfter.Found {
		// The identity held but the rebuilt order did not contain the last
		// emitted record. The search already answered from offset zero, so
		// report the restart instead of silently skipping a page.
		v2Resume = false
		v2Restarted = true
		v2RestartReason = "content cursor position not found"
		resumeBaseFRN = 0
	}
	rowsBefore := 0
	if v2Resume && v2Cursor != nil {
		rowsBefore = v2Cursor.Offset
	} else if v2Cursor == nil && req.Cursor != "" {
		rowsBefore = cursorOffset
	}
	if req.Cursor != "" && !req.CountOnly {
		if cursorOffset >= len(matches) {
			matches = nil
		} else {
			matches = matches[cursorOffset:]
		}
	}
	if caps.Remote {
		// Remote logs use only the coarse public source category and omit
		// planner/decline detail: internal sources can embed query-derived
		// extension or path terms, which must not reach the log.
		serviceLog("search remote ms=%.1f source=%s results=%d", searchMS, remoteSearchSource(trace.Source, req.CountOnly), len(matches))
	} else {
		serviceLog("search query=%q ms=%.1f planner=%s source=%s decline=%s filename_driver=%s candidates=%d results=%d", req.Query, searchMS, trace.PlannerMode, trace.Source, trace.Decline, trace.FilenameDriver, trace.Candidates, len(matches))
	}
	resp := serviceResponse{OK: true, Count: len(matches), SearchMS: searchMS, Fuzzy: fuzzyApplied, FuzzyCorrection: fuzzyCorrection, Source: trace.Source, Decline: trace.Decline, Candidates: trace.Candidates, PlannerMode: trace.PlannerMode, EligibleVolumes: trace.EligibleVolumes, BlocksDecoded: trace.BlocksDecoded, BlocksSkipped: trace.BlocksSkipped, ScalarDriver: trace.ScalarDriver, ScalarInterval: trace.ScalarInterval, RecordsVerified: trace.ScalarRecordsVerified, ComponentDriver: trace.ComponentDriver, ComponentRoots: trace.ComponentRoots, ComponentIntervals: trace.ComponentIntervals, ComponentCardinality: trace.ComponentCardinality, ComponentSelfHits: trace.ComponentSelfHits, ComponentBounds: trace.ComponentBounds, ComponentRecordsVerified: trace.ComponentRecordsVerified, FilenameDriver: trace.FilenameDriver, FilenameRequiredGrams: trace.FilenameRequiredGrams, FilenamePostingHint: trace.FilenamePostingHint, FilenameRecordsVerified: trace.FilenameRecordsVerified, OverlayBaseWindow: trace.OverlayBaseWindow, PostingPrefetchBytes: trace.PostingPrefetchBytes, PostingPrefetchRanges: trace.PostingPrefetchRanges, PostingPrefetchPages: trace.PostingPrefetchPages, Terms: trace.Terms, Declines: trace.Declines, Fallback: trace.Fallback, Complete: trace.completePtr(), BudgetExceeded: trace.BudgetExceeded}
	if !trace.BudgetDeadline.IsZero() {
		resp.ContentBudgetDeadlineUnix = trace.BudgetDeadline.UnixNano()
	}
	remaining := max(0, trace.DiskVerifyBudget-trace.DiskVerified)
	resp.ContentDiskVerifyRemaining = &remaining
	if !req.CountOnly && len(matches) == pageLimit {
		emitted := rowsBefore + len(matches)
		v2Token := ""
		if cursorCtx != nil && !fuzzyApplied && len(matches) > 0 {
			if pq, parseErr := parseQuery(opts); parseErr == nil {
				if now, ok := s.contentCursorContextFor(pq, opts); ok && *now == *cursorCtx {
					if lastID := matches[len(matches)-1].FRN; lastID != 0 {
						bk := uint64(0)
						if v2Resume {
							bk = resumeBaseFRN
						}
						var key *contentCursorKey
						if vol := s.volumes[0]; vol != nil {
							promo := newContentPromotion(pq)
							for i := range matches {
								entry := matches[i]
								var rowKey contentCursorKey
								if vol.contentCursorEntryIsOverlay(entry) {
									rowKey = vol.contentCursorOverlayKey(
										rankedOverlayEntry{entry: entry, rank: vol.overlayEntryRank(entry, pq)}, pq, promo)
								} else {
									rowKey = vol.contentCursorBaseKey(entry, pq, promo)
									bk = entry.FRN
								}
								if i == len(matches)-1 {
									k := rowKey
									key = &k
								}
							}
						}
						v2Token = encodeSearchCursorV2(opts, *cursorCtx, lastID, emitted, bk, key)
					}
				}
			}
		}
		if v2Token != "" {
			resp.NextCursor = v2Token
		} else {
			resp.NextCursor = encodeSearchCursor(requestToOptionsFromService(*req), emitted)
		}
	}
	resp.CursorRestarted = v2Restarted
	resp.CursorRestartReason = v2RestartReason
	if !req.CountOnly {
		resp.Results = make([]string, len(matches))
		for i, entry := range matches {
			resp.Results[i] = entry.Path
		}
		resp.Rows = entriesToJSON(matches)
		if pq, perr := parseQuery(opts); perr == nil {
			annotateJSONGitStatus(resp.Rows, matches, pq)
		}
		if caps.Remote {
			// Content text is local-only. Strip it at the command boundary so a
			// future transport cannot leak a snippet even if its own projection
			// forgets to.
			resp.redactContentText()
		}
	}
	resp.Content = s.searchContentHealth(trace)
	if resp.Content != nil && resp.Content.Stale {
		resp.ContentStale = true
		resp.Complete = boolPtr(false)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// serviceCommandOpen records one usage-signal access event: the user opened
// path (optionally attributed to the normalized query that led there). It
// feeds the caller's per-user frecency store and the query-history combo
// boost (rank_blend.go). Best-effort and synchronous: opens are user-paced
// and the store file is small, so the write lands before the response.
func (s *goSearchService) serviceCommandOpen(w io.Writer, principal servicePrincipal, req *serviceRequest) {
	path := normalizeSignalPath(req.Path)
	if path == "" {
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: "open requires a path"})
		return
	}
	serviceSignalStoreForSID(principal.SID).recordOpen(path, req.OpenQuery)
	_ = json.NewEncoder(w).Encode(serviceResponse{OK: true, Message: "recorded open for " + path})
}

// serviceCommandRecent lists recently opened files, newest first, from the
// caller's per-user frecency store (local only, never leaves the machine).
func (s *goSearchService) serviceCommandRecent(w io.Writer, principal servicePrincipal, req *serviceRequest) {
	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	recent := serviceSignalStoreForSID(principal.SID).recent(limit)
	rows := make([]recentFileJSON, 0, len(recent))
	for _, r := range recent {
		row := recentFileJSON{
			Path:     r.Path,
			Name:     filepath.Base(r.Path),
			Opens:    r.Opens,
			Score:    r.Score,
			LastOpen: time.Unix(0, r.LastOpenUnix).Format(time.RFC3339Nano),
		}
		if fi, err := os.Stat(r.Path); err == nil {
			row.Modified = fi.ModTime().Format(time.RFC3339Nano)
		}
		rows = append(rows, row)
	}
	_ = json.NewEncoder(w).Encode(serviceResponse{OK: true, Count: len(rows), Recent: rows})
}

// serviceCommandInputHeartbeat records the UI session's last-input time for
// the build governor. The UI runs in the interactive session and reports its
// GetLastInputInfo time every few seconds; the service keeps the freshest
// report across sessions and lets it expire after uiHeartbeatExpiry with no
// refresh. Local-only (remote rejected by the capability gate, like
// open/recent); any local caller may report — the timestamp is advisory
// presence, not a privilege. A zero/missing timestamp is rejected; future
// timestamps (clock skew) are clamped to now so they cannot pin the governor
// active forever.
func (s *goSearchService) serviceCommandInputHeartbeat(w io.Writer, req *serviceRequest) {
	if req == nil || req.InputUnixNano <= 0 {
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: "input-heartbeat requires input_unix_nano"})
		return
	}
	at := time.Unix(0, req.InputUnixNano).UTC()
	if now := time.Now().UTC(); at.After(now) {
		at = now
	}
	// Reject absurdly old reports (a client bug, not presence): anything
	// older than the input quiet window plus the heartbeat expiry cannot be
	// active anyway and would only pollute the freshest-wins slot.
	if time.Since(at) > contentBuildInputQuietWindow+uiHeartbeatExpiry {
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: "input-heartbeat timestamp too old"})
		return
	}
	noteUIHeartbeat(at)
	_ = json.NewEncoder(w).Encode(serviceResponse{OK: true})
}

func (s *goSearchService) serviceCommandWatchDelta(w io.Writer, req *serviceRequest) {
	serviceNoteQueryActivity()
	serviceActiveQueryEnter()
	defer serviceActiveQueryLeave()
	if queryHasFeatureToken(req.Query) {
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: "watch does not support generic feature predicates"})
		return
	}
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
	idx, err := s.buildIndexUSNVolume(req.Volume, req.DB)
	if err != nil {
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(serviceResponse{OK: true, Message: "indexed", Entries: idx.entryCount()})
}

// buildIndexUSNVolume runs the index-usn path for one volume and installs the
// persisted index into the service. The settings API calls the same path when
// an operator adds a volume.
func (s *goSearchService) buildIndexUSNVolume(volume, db string) (*Index, error) {
	serviceLog("index-usn start volume=%s db=%s", volume, db)
	idx, err := serviceIndexUSNVolume(volume)
	if err != nil {
		serviceLog("index-usn error volume=%s err=%v", volume, err)
		return nil, err
	}
	serviceLog("index-usn built volume=%s entries=%d", volume, idx.entryCount())
	buildOrders(idx)
	serviceLog("index-usn orders volume=%s", volume)
	// Stage the multi-GB v9 write outside the global lock so pipe
	// requests keep serving while it runs; only the fast swap-in below
	// takes indexMu.Lock().
	tmp, stageErr := stageIndexFile(db, idx)
	if stageErr != nil {
		serviceLog("index-usn stage error volume=%s err=%v", volume, stageErr)
		return nil, stageErr
	}
	s.indexMu.Lock()
	// The low-memory mmap pins the target file open, so unmap any existing
	// volume for this DB before the temp-file rename replaces the file.
	for _, existing := range s.volumes {
		if existing != nil && samePath(existing.dbPath, db) {
			if err := closeIndexMMapRecords(existing.index); err != nil {
				serviceLog("index-usn close mmap volume=%s err=%v", volume, err)
			}
			break
		}
	}
	if err := commitStageIndexFile(db, tmp); err != nil {
		s.indexMu.Unlock()
		serviceLog("index-usn save error volume=%s err=%v", volume, err)
		return nil, err
	}
	if err := removeWAL(db); err != nil {
		serviceLog("wal cleanup error volume=%s db=%s err=%v", volume, db, err)
	}
	// Reload from disk so the swapped-in volume carries the persisted
	// derived sections (RANK/PNGR/PNGC etc.).  The in-memory idx built by
	// indexUSNVolume never populates Derived; using it here would leave
	// the volume without SelfNameTrigrams and force every multi-term name
	// query onto the slow bounded-scan path.
	loaded, loadErr := loadIndexForService(db)
	if loadErr != nil {
		s.indexMu.Unlock()
		serviceLog("index-usn reload error volume=%s err=%v", volume, loadErr)
		return nil, loadErr
	}
	vol := s.replaceLoadedVolumeLocked(db, loaded)
	s.indexMu.Unlock()
	releaseServiceMemoryAfterSave()
	s.startBackgroundNameOrderBuilds([]*serviceVolumeIndex{vol})
	s.startBackgroundNameTrigramBuilds([]*serviceVolumeIndex{vol})
	s.startBackgroundNameIdentityBuilds([]*serviceVolumeIndex{vol})
	// A runtime-added or re-indexed volume gets content attached or scheduled
	// (PF-3), instead of staying content-unavailable until a restart.
	s.featuresVolumeReady(vol)
	serviceLog("index-usn complete volume=%s entries=%d", volume, idx.entryCount())
	return loaded, nil
}

func (s *goSearchService) serviceCommandStatus(w io.Writer) {
	_ = json.NewEncoder(w).Encode(serviceInfoResponseFor(serviceResponse{OK: true, Message: "service running", Content: s.contentHealthSnapshot(), Governor: contentGovernorHealthSnapshot()}, s.pipeName, s.processMode))
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
	s.startBackgroundNameIdentityBuilds([]*serviceVolumeIndex{vol})
	s.featuresVolumeReady(vol)
}

func (s *goSearchService) replaceLoadedVolumeLocked(dbPath string, idx *Index) *serviceVolumeIndex {
	if s == nil || idx == nil || dbPath == "" {
		return nil
	}
	vol := newServiceVolumeIndex(dbPath, idx)
	s.prepareFeatureVolume(vol)
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
	replaceFeatureVolumeState(dst, src)
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
	// A base swap installs a new index pointer; the walk join column is
	// rebuilt by the content rebind below (featuresBaseReplaced), so drop
	// the old pointer-keyed set here rather than serve it stale.
	dst.walkKeys = src.walkKeys
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
	dst.featuresBaseReplaced(prevJournalID)
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
		dbPath:       vol.dbPath,
		index:        idx,
		volume:       vol.volume,
		journalID:    vol.journalID,
		checkpoint:   vol.checkpoint,
		state:        vol.state,
		staleReason:  vol.staleReason,
		content:      vol.content,
		frnToID:      vol.frnToID,
		frns:         vol.frns,
		frnRecordIDs: vol.frnRecordIDs,
		// The walk join column is immutable and keyed by index pointer
		// (walkKeySet.idx); the view always shares the volume's index
		// (snap.base == vol.index), so sharing the set is exact.
		walkKeys:          vol.walkKeys,
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

// serviceResponseError converts a !OK service response into a CLI error. The
// message is never empty: a blank stderr line with a nonzero exit is nearly
// as unreadable for operators (and as easy to misparse for scripts) as
// silence, so a messageless rejection names itself instead.
func serviceResponseError(resp serviceResponse) error {
	if msg := strings.TrimSpace(resp.Message); msg != "" {
		return errors.New(msg)
	}
	return errors.New("seekfs service rejected the request with an empty error message")
}

func searchService(pipeName string, opts queryOptions, countOnly bool, jsonOut bool) error {
	resp, err := callService(pipeName, serviceRequestFromOptions(opts, countOnly))
	if err != nil {
		return err
	}
	if !resp.OK {
		return serviceResponseError(resp)
	}
	if resp.CursorRestarted {
		reason := resp.CursorRestartReason
		if reason == "" {
			reason = "cursor no longer valid"
		}
		fmt.Fprintf(os.Stderr, "warning: cursor restarted at the first page (%s)\n", reason)
	}
	if resp.ContentDiskVerifyRemaining != nil {
		opts.lineBudgetMetadata = true
		opts.lineDiskVerifyRemaining = resp.ContentDiskVerifyRemaining
		if resp.ContentBudgetDeadlineUnix != 0 {
			opts.lineBudgetDeadline = time.Unix(0, resp.ContentBudgetDeadlineUnix)
		}
	}
	if jsonOut {
		jsonResp := newJSONSearchResponse(resp, opts, countOnly)
		if p := cliContentLinePrinter(opts); p != nil {
			p.annotateJSON(jsonResp.Results)
		}
		return writeJSON(os.Stdout, jsonResp)
	}
	if countOnly {
		fmt.Println(resp.Count)
		return nil
	}
	w := bufio.NewWriter(os.Stdout)
	if resp.FuzzyCorrection != "" {
		fmt.Fprintf(w, "(showing results for %q)\n", resp.FuzzyCorrection)
	} else if resp.Fuzzy {
		fmt.Fprintf(w, "(showing close matches for %q)\n", opts.Query)
	}
	p := cliContentLinePrinter(opts)
	for i, result := range resp.Results {
		if p != nil {
			snippet := ""
			if i < len(resp.Rows) && resp.Rows[i].Path == result {
				snippet = resp.Rows[i].Snippet
			}
			p.print(w, result, snippet)
			continue
		}
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
		FuzzyCorrection:          resp.FuzzyCorrection,
		EligibleVolumes:          resp.EligibleVolumes,
		Terms:                    resp.Terms,
		Declines:                 resp.Declines,
		Fallback:                 resp.Fallback,
		Complete:                 resp.Complete,
		ContentStale:             resp.ContentStale,
		Content:                  resp.Content,
		BudgetExceeded:           resp.BudgetExceeded,
		NextCursor:               resp.NextCursor,
		CursorRestarted:          resp.CursorRestarted,
		CursorRestartReason:      resp.CursorRestartReason,
	}
	if !countOnly {
		if len(resp.Rows) > 0 {
			out.Results = resp.Rows
		} else {
			out.Results = pathsToJSON(resp.Results)
		}
	}
	if out.ContentStale {
		out.Complete = boolPtr(false)
	}
	return out
}
