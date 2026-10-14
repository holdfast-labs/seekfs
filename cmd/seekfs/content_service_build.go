package main

// PF-3 (WP1d/WP1e): the service-owned background content build and self-heal.
//
// Before PF-3 the service only *attached* an existing `.gsx`; a fresh install
// with the flag on reported content `unavailable` until an undocumented manual
// `content-index -db` step, and a journal reset left content permanently
// `stale`. This file makes the service build and rebuild its own content index:
//
//   - startup: a ready USN volume with no usable sidecar schedules a build;
//   - journal reset / filename rebuild: the rebuild chain re-attaches or
//     reschedules a content build, so content self-heals instead of staying
//     stale forever.
//
// Locking. The build only reads the live record index for metadata, and does so
// under s.indexMu.RLock (the same lock persist/rebuild take exclusively while
// closing the mmap). The metadata copy is chunked: the read lock is released and
// reacquired every contentBuildSnapshotBatch records (with a bounded path
// cache), so a persist/rebuild can proceed between batches instead of waiting
// for an O(records) pass. If the index pointer or the base generation moves
// mid-snapshot the copy aborts and reschedules. Extraction never runs under
// indexMu or vol.mu and never touches the mmap: copied plain values only. A
// persist/rebuild can therefore proceed while a build extracts; the build aborts
// if the journal generation changed under it and reschedules (with a bounded
// attempt budget).
//
// Memory. The build spools extracted text to one temporary file in priority
// order, then streams selected docs to assembleContentIndexStream in FRN order;
// it never retains the raw corpus in memory. Peak transient heap is one
// document's text plus posting arenas, assembled text, and capped metadata.
//
// IO governor. Extraction runs on contentBuildWorkerCount workers (dropped to
// one while the user queries or the machine is on battery) with per-handle
// low disk priority (contentOpenNoRecall), and pauses every batch so filename
// search and USN replay keep the disk; the loop is cancelable through s.stop
// and the per-volume build generation.
//
// Build vs catch-up. A base build extracts the base corpus while restart
// catch-up and the drain loop may extract changed files into the in-memory
// delta. They are deliberately not serialized: they read the volume and write
// different places (the build writes the `.gsx`; catch-up/drain mutate only the
// delta), and a duplicate extraction is an idempotent upsert. Publishing
// attaches the fresh base and leaves the delta overlay intact; catch-up resumes
// from the new base's checkpoint, so no change is double-counted or lost.
//
// Default scope (M5, sniff-selected since the recall follow-up). Unlike the
// offline CLI (which indexes any extractable file when no -ext is given),
// the service build used to be allowlisted; now, without a configured exts
// list, it admits every file the scope roots allow (minus known-binary
// extensions) and head-sniffs each at build time, ripgrep-style. A
// whole-volume build is never the default.

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// contentBuildBatchSize is the number of documents extracted between IO
// governor pauses; contentBuildBatchPause is the pause itself. Both are vars so
// tests can drive a build synchronously without the pacing delay.
var (
	contentBuildBatchSize  = 64
	contentBuildBatchPause = 25 * time.Millisecond
)

// contentBuildRun launches the build. It is a var so tests can run the build
// inline; production runs it on its own goroutine.
var contentBuildRun = func(fn func()) { go fn() }

// contentBuildMaxConcurrent bounds how many service content builds extract at
// once. One build is already serial and paced; this keeps several volumes from
// saturating the disk together.
const contentBuildMaxConcurrent = 1

// contentShardBuildConcurrency bounds how many shards assemble concurrently
// in one service build. Shards are independent (own posting builders, own
// section files, spool reads via concurrent ReadAt), so assembling several at
// once uses idle CPUs while extraction is done. The per-shard memory bound is
// unchanged; concurrency multiplies the transient peak, so this stays small.
// A var so tests and tight-memory machines can pin it to 1 (sequential).
var contentShardBuildConcurrency = 2

// contentBuildGate is the process-wide build concurrency limiter.
var contentBuildGate = make(chan struct{}, contentBuildMaxConcurrent)

// contentBuildSnapshotBatch is the number of compact records copied under one
// indexMu.RLock window. The snapshot reacquires the read lock per batch so a
// persist/rebuild (which needs indexMu.Lock) can proceed between batches rather
// than waiting for an O(records) pass; contentBuildPathCacheMax bounds the path
// memo so the snapshot never retains O(records) reconstructed paths.
const (
	contentBuildSnapshotBatch = 1 << 14
	contentBuildPathCacheMax  = 1 << 12
)

// contentBuildMaxAttempts caps consecutive generation-change retries so a volume
// whose journal generation never settles cannot spin builds forever; hitting the
// cap surfaces as degraded + BuildError. contentBuildRetryBackoff paces those
// retries. Both are vars so tests run without the delay.
var (
	contentBuildMaxAttempts  = int32(4)
	contentBuildRetryBackoff = 500 * time.Millisecond
)

// errContentBuildAborted aborts the streaming assembler when a running build
// must stop (generation change or shutdown) without publishing a partial index.
var errContentBuildAborted = errors.New("content build aborted")

// errContentBuildStale reports that the base generation moved before a built
// sidecar could be published.
var errContentBuildStale = errors.New("content build stale")

// contentSnapshotPersistWait bounds how long a content snapshot waits for an
// in-flight background persist (which takes seconds to a minute on a large
// volume) before starting its copy.
var contentSnapshotPersistWait = 3 * time.Minute

// holdBackgroundPersists keeps background persists from swapping vol.index
// while a content task reads the whole record index (a build snapshot or a
// reconcile selection scan): it raises contentSnapshotHold, then waits out a
// persist that started before the hold. settled is false when that persist
// did not finish within contentSnapshotPersistWait or the volume stopped.
// Callers must call release, settled or not.
func (s *goSearchService) holdBackgroundPersists(vol *serviceVolumeIndex) (release func(), settled bool) {
	vol.contentSnapshotHold.Add(1)
	release = func() { vol.contentSnapshotHold.Add(-1) }
	for deadline := time.Now().Add(contentSnapshotPersistWait); vol.persistActive.Load() > 0; {
		if s.contentStopped(vol) || time.Now().After(deadline) {
			return release, false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return release, true
}

// contentBuildSnapshotResult tells runContentBuild what a metadata snapshot
// produced.
type contentBuildSnapshotResult int

const (
	// contentBuildSnapshotUnavailable: not anchorable (not USN, no watermark,
	// not ready). Leave the state as-is; do not reschedule.
	contentBuildSnapshotUnavailable contentBuildSnapshotResult = iota
	// contentBuildSnapshotOK: a consistent copy was taken.
	contentBuildSnapshotOK
	// contentBuildSnapshotChanged: the base generation moved mid-pass. Abort
	// and reschedule against the new generation.
	contentBuildSnapshotChanged
	// contentBuildSnapshotLimited: the file-count cap truncated the corpus.
	contentBuildSnapshotLimited
)

// contentBuildSnapshotHook runs immediately after the metadata snapshot lock is
// released and before any file is opened. Tests use it to prove the build does
// not hold indexMu across extraction, to inject a mid-build journal change, or
// to cancel the build.
var contentBuildSnapshotHook = func(*goSearchService, *serviceVolumeIndex) {}

// contentBuildSnapshotBatchHook runs after each snapshot batch releases
// indexMu.RLock. Tests use it to prove a writer can take indexMu.Lock between
// batches and to inject a mid-snapshot generation change.
var contentBuildSnapshotBatchHook = func(*goSearchService, *serviceVolumeIndex) {}

// contentBuildIndexingHook runs right after the volume enters the `indexing`
// state. Tests use it to observe the indexing state/progress deterministically.
var contentBuildIndexingHook = func(*contentVolumeState) {}

// defaultServiceContentBuildOptions is the service build's default scope:
// sniff-based selection (the scope admits everything but known-binary
// extensions; each file's head decides at build time). The curated
// text/code/config allowlist below plus every registered extractor's
// extensions seed the registry the scope fingerprint names and the explicit
// exts opt-in restores.
func defaultServiceContentBuildOptions() contentBuildOptions {
	opts := defaultContentBuildOptions()
	opts.Exts = contentServiceExtensions()
	// A service-wide encoding override so the background build and the USN
	// delta extractor decode the same way. Absent/unset means the default auto
	// policy (BOM + UTF-8 exact + Windows-1252/Latin-1 fallback).
	opts.Encoding = contentServiceEncodingLabel()
	return opts
}

// resolvedContentScope derives the effective content scope for a volume from
// the service config plus the git worktrees detected in the volume's full
// filename index. ok is false when content is disabled for the volume.
func (s *goSearchService) resolvedContentScope(vol *serviceVolumeIndex) (contentScopeResolved, bool) {
	if s == nil || vol == nil {
		return contentScopeResolved{}, false
	}
	scope := contentScopeForVolume(s.contentConfig(), vol.volume)
	if scope.disabled() {
		return contentScopeResolved{}, false
	}
	s.indexMu.RLock()
	if vol.index == nil {
		s.indexMu.RUnlock()
		return contentScopeResolved{}, false
	}
	repos, ignoreFiles := contentDetectGitLayout(vol.index)
	s.indexMu.RUnlock()
	resolved := scope.resolve(vol.volume, repos)
	resolved.attachGitIgnore(ignoreFiles)
	return resolved, true
}

// contentServiceEncodingLabel returns the service's SEEKFS_CONTENT_ENCODING
// value, or "auto". A blank/invalid label resolves to auto. It is read at build
// and delta time so both paths agree without shared mutable state.
func contentServiceEncodingLabel() string {
	label := strings.TrimSpace(os.Getenv("SEEKFS_CONTENT_ENCODING"))
	if label == "" {
		return "auto"
	}
	if _, err := parseContentEncoding(label); err != nil {
		return "auto"
	}
	return label
}

// contentServiceExtractSettings is the default extraction policy for service
// paths that are not a build (the USN delta extractor): default caps + the
// service encoding override.
func contentServiceExtractSettings() contentExtractSettings {
	s := contentDefaultExtractSettings()
	if m, err := parseContentEncoding(contentServiceEncodingLabel()); err == nil {
		s.encoding = m
	}
	return s
}

// contentServiceExtensions returns the union of the curated text/code/config
// allowlist and all registered extractors' Extensions().
func contentServiceExtensions() map[string]struct{} {
	out := make(map[string]struct{}, len(contentDefaultTextExtensions)+16)
	for _, ext := range contentDefaultTextExtensions {
		out[ext] = struct{}{}
	}
	for _, e := range contentExtractors {
		for _, ext := range e.Extensions() {
			out[ext] = struct{}{}
		}
	}
	return out
}

// contentDefaultTextExtensions is the documented service-build text/code/config
// allowlist. The plain-text extractor has no Extensions() of its own (it is the
// fallback for an unknown extension), so the service names the set explicitly
// instead of defaulting to the whole volume. Extensionless files (Makefile,
// LICENSE) and unknown extensions are excluded unless an extractor claims them.
var contentDefaultTextExtensions = []string{
	// Plain text, logs, docs.
	".txt", ".text", ".log", ".md", ".markdown", ".rst", ".adoc", ".org",
	// Source code.
	".go", ".c", ".h", ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".cs",
	".java", ".kt", ".kts", ".scala", ".rs", ".swift", ".m", ".mm",
	".py", ".pyi", ".rb", ".php", ".pl", ".pm", ".lua", ".r",
	".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".vue", ".svelte",
	".dart", ".groovy", ".gradle", ".clj", ".cljs", ".ex", ".exs", ".erl",
	// Shell / scripts.
	".sh", ".bash", ".zsh", ".fish", ".ps1", ".psm1", ".bat", ".cmd",
	// Data / config / markup.
	".sql", ".graphql", ".gql", ".proto", ".css", ".scss", ".sass", ".less",
	".xml", ".xsl", ".xslt", ".xsd", ".svg",
	".json", ".jsonc", ".jsonl", ".ndjson", ".yaml", ".yml", ".toml",
	".ini", ".cfg", ".conf", ".config", ".properties", ".env", ".editorconfig",
	".csv", ".tsv", ".tex", ".bib", ".diff", ".patch",
}

// contentBuildItem is the copied metadata for one file to extract. It holds no
// slice into the record index or its mmap, so extraction can run with every
// service lock released.
type contentBuildItem struct {
	frn      uint64
	path     string
	priority int
}

func contentBuildItemLess(a, b contentBuildItem) bool {
	if a.priority != b.priority {
		return a.priority < b.priority
	}
	if a.path != b.path {
		return a.path < b.path
	}
	return a.frn < b.frn
}

// A max-heap keeps the worst retained candidate at the root when MaxFiles is
// reached, so a later repository file can replace an earlier known-folder file.
type contentBuildItemHeap []contentBuildItem

func (h contentBuildItemHeap) Len() int           { return len(h) }
func (h contentBuildItemHeap) Less(i, j int) bool { return contentBuildItemLess(h[j], h[i]) }
func (h contentBuildItemHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *contentBuildItemHeap) Push(x any)        { *h = append(*h, x.(contentBuildItem)) }
func (h *contentBuildItemHeap) Pop() any {
	n := len(*h) - 1
	x := (*h)[n]
	*h = (*h)[:n]
	return x
}

// ensureContentBuild is the single entry point for service-owned content
// lifecycle, called by external triggers (startup, a rebuild/journal reset, a
// runtime volume swap). It opens a fresh retry budget, then schedules. The
// generation-change retry path calls scheduleContentBuild directly so a volume
// whose generation never settles still hits contentBuildMaxAttempts.
func (s *goSearchService) ensureContentBuild(vol *serviceVolumeIndex) {
	if vol != nil {
		vol.contentBuildAttempts.Store(0)
	}
	s.scheduleContentBuild(vol)
}

// scheduleContentBuild first tries to attach an existing valid sidecar; if none
// is usable and the volume is ready with an anchorable watermark (a live USN
// journal watermark, or a walk index, which needs no journal), it schedules
// exactly one background build.
func (s *goSearchService) scheduleContentBuild(vol *serviceVolumeIndex) {
	if s == nil || vol == nil {
		return
	}
	if !s.beginContentTask(vol) {
		return
	}
	defer endContentTask(vol)
	if !contentSearchEnabled() || vol.content == nil {
		return
	}
	if contentScopeForVolume(s.contentConfig(), vol.volume).disabled() {
		return
	}
	// Reuse an existing valid sidecar when there is one.
	s.attachContentForVolume(vol)
	if vol.content.readerView() != nil {
		return
	}
	s.indexMu.RLock()
	ready := vol.state == "ready" && vol.index != nil &&
		(vol.index.Source == "usn" || vol.index.Source == "walk")
	journalID := vol.journalID
	checkpoint := vol.baseCheckpoint
	if checkpoint <= 0 && vol.index != nil {
		checkpoint = vol.index.Checkpoint
	}
	isWalk := vol.index != nil && vol.index.Source == "walk"
	s.indexMu.RUnlock()
	if !ready || (!isWalk && (journalID == 0 || checkpoint <= 0)) {
		// Not ready (a rebuild owns it), a USN volume with no watermark to
		// anchor a sidecar to, or a non-indexable source. The post-rebuild
		// hook reschedules when it is.
		return
	}
	if !vol.contentBuildBusy.CompareAndSwap(false, true) {
		return
	}
	if !s.beginContentTask(vol) {
		vol.contentBuildBusy.Store(false)
		return
	}
	contentBuildRun(func() { defer endContentTask(vol); s.runContentBuild(vol, true) })
}

// scheduleContentRebuild schedules a content rebuild for vol without first
// trying to re-attach the existing sidecar. It is the self-heal path for a
// capped or errored restart catch-up: that sidecar is still valid but
// incomplete, so re-attaching it (scheduleContentBuild) would leave the volume
// permanently degraded with a gated fold and an ever-growing delta.
// contentCatchUpFailed bounds its use with contentBuildAttempts.
func (s *goSearchService) scheduleContentRebuild(vol *serviceVolumeIndex) {
	if s == nil || vol == nil {
		return
	}
	if !s.beginContentTask(vol) {
		return
	}
	defer endContentTask(vol)
	if !contentSearchEnabled() || vol.content == nil {
		return
	}
	if contentScopeForVolume(s.contentConfig(), vol.volume).disabled() {
		return
	}
	s.indexMu.RLock()
	ready := vol.state == "ready" && vol.index != nil &&
		(vol.index.Source == "usn" || vol.index.Source == "walk")
	journalID := vol.journalID
	checkpoint := vol.baseCheckpoint
	if checkpoint <= 0 && vol.index != nil {
		checkpoint = vol.index.Checkpoint
	}
	isWalk := vol.index != nil && vol.index.Source == "walk"
	s.indexMu.RUnlock()
	if !ready || (!isWalk && (journalID == 0 || checkpoint <= 0)) {
		return
	}
	if !vol.contentBuildBusy.CompareAndSwap(false, true) {
		return
	}
	if !s.beginContentTask(vol) {
		vol.contentBuildBusy.Store(false)
		return
	}
	contentBuildRun(func() { defer endContentTask(vol); s.runContentBuild(vol, false) })
}

// snapshotContentBuildItems copies the FRN/path/size/modtime metadata for every
// in-scope, eligible file, but only ever holds s.indexMu.RLock for one
// contentBuildSnapshotBatch-record window at a time: it reacquires the read
// lock per batch so a persist/rebuild (which needs indexMu.Lock to close and
// swap the mmap) can proceed between batches instead of stalling behind an
// O(records) pass. The path memo is bounded by contentBuildPathCacheMax and
// reset incrementally, so the snapshot does not retain O(records) paths.
//
// Consistency: every batch is read under the read lock only after confirming
// the same index pointer is still live and the base generation/journal are
// unchanged. A persist/rebuild cannot close the mmap while a batch's read lock
// is held, so a batch never reads a torn or freed mmap; a change between
// batches returns contentBuildSnapshotChanged and the caller reschedules.
//
// The returned journalID/checkpoint identify the exact record state the copy
// corresponds to: a content base built from it is complete as of
// baseCheckpoint, and restart catch-up replays anything after it.
func (s *goSearchService) snapshotContentBuildItems(vol *serviceVolumeIndex, opts contentBuildOptions) (items []contentBuildItem, journalID uint64, checkpoint int64, result contentBuildSnapshotResult) {
	if s == nil || vol == nil {
		return nil, 0, 0, contentBuildSnapshotUnavailable
	}
	// Hold background persists for the copy: each one swaps vol.index and
	// would restart the snapshot (see contentSnapshotHold).
	release, settled := s.holdBackgroundPersists(vol)
	defer release()
	if !settled {
		return nil, 0, 0, contentBuildSnapshotChanged
	}
	s.indexMu.RLock()
	if vol.index == nil || (vol.index.Source != "usn" && vol.index.Source != "walk") {
		s.indexMu.RUnlock()
		return nil, 0, 0, contentBuildSnapshotUnavailable
	}
	idx := vol.index
	isWalk := idx.Source == "walk"
	journalID = vol.journalID
	checkpoint = vol.baseCheckpoint
	if checkpoint <= 0 {
		checkpoint = idx.Checkpoint
	}
	if isWalk {
		// Walk volumes have no journal; the sidecar watermark is the
		// wall-clock build time (see runContentBuild's header).
		journalID = 0
		checkpoint = time.Now().UnixNano()
	}
	gen := vol.replayGen.Load()
	count := idx.compactRecordCount()
	s.indexMu.RUnlock()
	if (!isWalk && (journalID == 0 || checkpoint <= 0)) || checkpoint <= 0 {
		return nil, 0, 0, contentBuildSnapshotUnavailable
	}

	// Without a resolved scope (tests and offline callers) the legacy flat
	// loop applies the Exts/Under gates directly; production builds always
	// carry a scope and take the memoized parallel scan below.
	if opts.Scope == nil {
		return s.snapshotContentBuildItemsLegacy(vol, opts, idx, isWalk, journalID, checkpoint, gen, count)
	}
	cfg := contentScopeScanConfig{
		scope:    opts.Scope,
		idx:      idx,
		isWalk:   isWalk,
		maxFiles: opts.MaxFiles,
		prog: func(scanned, total int) {
			vol.content.setBuildProgress(scanned, total)
		},
		stopped: func() bool { return s.contentStopped(vol) },
		genChanged: func() bool {
			return vol.index != idx || vol.replayGen.Load() != gen || vol.journalID != journalID
		},
	}
	lists, ok := scanScopeMembers(s, vol, cfg)
	if !ok {
		// Stopped scans and generation changes share the legacy outcomes:
		// a stop leaves the state as-is, a move reschedules.
		if s.contentStopped(vol) {
			return nil, 0, 0, contentBuildSnapshotUnavailable
		}
		return nil, 0, 0, contentBuildSnapshotChanged
	}
	items, limited := mergeScanMembers(lists, opts.MaxFiles)
	if limited {
		return items, journalID, checkpoint, contentBuildSnapshotLimited
	}
	return items, journalID, checkpoint, contentBuildSnapshotOK
}

// snapshotContentBuildItemsLegacy is the pre-parallel scan: one flat pass
// with a shared path cache, kept for scopeless callers (tests). Production
// builds resolve a scope first and use scanScopeMembers.
func (s *goSearchService) snapshotContentBuildItemsLegacy(vol *serviceVolumeIndex, opts contentBuildOptions, idx *Index, isWalk bool, journalID uint64, checkpoint int64, gen uint64, count int) (items []contentBuildItem, _ uint64, _ int64, result contentBuildSnapshotResult) {
	top := make(contentBuildItemHeap, 0, 1024)
	limited := false
	cache := make(map[int]string, 1024)
	for start := 0; start < count; start += contentBuildSnapshotBatch {
		if s.contentStopped(vol) {
			return nil, 0, 0, contentBuildSnapshotUnavailable
		}
		end := start + contentBuildSnapshotBatch
		if end > count {
			end = count
		}
		s.indexMu.RLock()
		// Guard the batch: the same index must still be live (its mmap is not
		// freed) and the base generation/journal unchanged. Both are checked
		// under the read lock the swap would need to take exclusively.
		if vol.index != idx || vol.replayGen.Load() != gen || vol.journalID != journalID {
			s.indexMu.RUnlock()
			return nil, 0, 0, contentBuildSnapshotChanged
		}
		for id := start; id < end; id++ {
			rec := idx.compactRecord(id)
			if rec.Deleted || rec.FRN == 0 || rec.Mode&uint32(os.ModeDir) != 0 {
				continue
			}
			// No size pre-filter: the extractor applies the raw/text policy
			// (bounded prefix for text, visible Skip for containers), so an
			// over-cap file is never silently excluded here.
			path := idx.reconstructCompactPathCached(id, cache)
			if path == "" || !opts.allows(path) {
				continue
			}
			frn := rec.FRN
			if isWalk {
				// Positional record FRNs renumber on every rescan; key the
				// doc on the stable path hash instead.
				frn = contentWalkKey(path)
				if frn == 0 {
					continue
				}
			}
			item := contentBuildItem{frn: frn, path: path}
			if opts.MaxFiles <= 0 || len(top) < opts.MaxFiles {
				top = append(top, item)
				continue
			}
			if !limited {
				heap.Init(&top)
				limited = true
			}
			if contentBuildItemLess(item, top[0]) {
				top[0] = item
				heap.Fix(&top, 0)
			}
		}
		s.indexMu.RUnlock()
		if len(cache) > contentBuildPathCacheMax {
			cache = make(map[int]string, 1024)
		}
		contentBuildSnapshotBatchHook(s, vol)
	}
	if limited {
		return top, journalID, checkpoint, contentBuildSnapshotLimited
	}
	return top, journalID, checkpoint, contentBuildSnapshotOK
}

// contentBuildDocSafe extracts one file into a build doc. It recovers from an
// extractor panic so one pathological document cannot kill the service, and
// returns ok=false for missing/oversized/binary/skipped files. The file is
// opened with OPEN_NO_RECALL and read exactly once (contentSlurpNoRecall);
// the extractor reuses the slurped bytes in place.
func contentBuildDocSafe(ctx context.Context, item contentBuildItem) (doc contentBuildDoc, res contentExtractResult, ok bool) {
	defer func() {
		if recover() != nil {
			doc = contentBuildDoc{}
			res = contentExtractResult{Skipped: true, Reason: "extractor panic"}
			ok = false
		}
	}()
	settings := contentExtractSettingsFromContext(ctx)
	s, err := contentSlurpNoRecall(item.path, settings.maxRaw)
	if err != nil {
		return contentBuildDoc{}, contentExtractResult{}, false
	}
	defer s.close()
	e := contentExtractorForPath(item.path, s.headBytes())
	if e == nil {
		// No extractor claimed the head: binary (NUL in the sniff window),
		// counted apart from skipped/declined.
		return contentBuildDoc{}, contentExtractResult{Binary: true, Reason: "binary"}, false
	}
	// Bound one document's extraction so a hang cannot stall the serial build;
	// contentExtractSafely nests the deadline inside the build's stop/cancel
	// context and isolates a parser panic.
	res, err = contentExtractSafely(ctx, e, s.reader(), s.size)
	if err != nil || res.Skipped || len(res.Text) == 0 {
		return contentBuildDoc{}, res, false
	}
	return contentBuildDoc{path: item.path, frn: item.frn, text: res.Text, modUnix: s.modUnix, class: res.Class, version: e.Version(), fileSize: s.size, fileModNano: s.modNano}, res, true
}

// contentBuildCurrent reports whether a build may still publish: the per-volume
// replay generation and the journal id must be unchanged. A journal reset or
// base rebuild bumps replayGen, so a build that spans one aborts.
func (s *goSearchService) contentBuildCurrent(vol *serviceVolumeIndex, gen uint64, journalID uint64) bool {
	if s.contentStopped(vol) || vol.replayGen.Load() != gen {
		return false
	}
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	return vol.journalID == journalID
}

// runContentBuild performs one service-owned content build for vol. On success
// it persists the `.gsx` atomically (checkpoint in the v2 header) and re-runs
// attach to publish it. On abort/failure the volume is left visibly
// unavailable/indexing/degraded, never ready with an unusable index, and a
// generation change reschedules.
func (s *goSearchService) runContentBuild(vol *serviceVolumeIndex, reuseSidecar bool) {
	if vol == nil {
		return
	}
	retry := false
	var scopeDropped, scopeDroppedBytes int64
	select {
	case contentBuildGate <- struct{}{}:
	case <-contentActivationDone(vol):
		vol.contentBuildBusy.Store(false)
		return
	case <-s.stop:
		vol.contentBuildBusy.Store(false)
		return
	}
	gateReleased := false
	releaseGate := func() {
		if !gateReleased {
			gateReleased = true
			<-contentBuildGate
		}
	}
	defer func() {
		if vol == nil {
			releaseGate()
			return
		}
		vol.contentBuildBusy.Store(false)
		// Release the gate before a rescheduled build, so the nested build can
		// acquire it (the defer would otherwise still hold it).
		releaseGate()
		if !retry || s.contentStopped(vol) {
			return
		}
		// Backoff and cap consecutive generation-change retries. The budget is
		// only reset by an external trigger (ensureContentBuild), so a volume
		// whose generation never settles cannot spin builds forever.
		if vol.contentBuildAttempts.Add(1) >= contentBuildMaxAttempts {
			vol.content.markBuildFailed(fmt.Sprintf("content build gave up after %d attempts (base generation kept changing)", contentBuildMaxAttempts))
			return
		}
		if contentBuildRetryBackoff > 0 {
			select {
			case <-s.stop:
				return
			case <-contentActivationDone(vol):
				return
			case <-time.After(contentBuildRetryBackoff):
			}
		}
		// Route the retry by the same intent: a rebuild retry must not fall
		// through to the reuse path, which would re-attach (and re-publish) the
		// very sidecar the rebuild is replacing.
		if reuseSidecar {
			s.scheduleContentBuild(vol)
		} else {
			s.scheduleContentRebuild(vol)
		}
	}()
	if vol == nil || vol.content == nil {
		return
	}
	if s.contentStopped(vol) {
		return
	}
	// A valid sidecar may have become attachable since this build was scheduled
	// (startup race: the build was queued while a base rebuild owned the volume,
	// then the post-rebuild hook attached the existing sidecar). Re-attaching
	// here avoids discarding a valid sidecar and blanking content queries for
	// the minutes a needless rebuild would take. Only scheduleContentBuild sets
	// reuseSidecar: scheduleContentRebuild's whole point is to NOT re-attach the
	// still-valid-but-incomplete sidecar it is replacing (re-attaching clears
	// the incomplete flag via setReady and skips the rebuild).
	if reuseSidecar {
		s.attachContentForVolume(vol)
		if vol.content.readerView() != nil {
			return
		}
	}
	gen := vol.replayGen.Load()
	opts := defaultServiceContentBuildOptions()
	// Auto scope: derive content roots from the full filename index (git
	// worktrees + configured/known roots), never the whole volume by default.
	resolved, ok := s.resolvedContentScope(vol)
	if !ok {
		return
	}
	opts.Scope = &resolved

	// Enter indexing for the metadata scan itself: on a large volume the
	// scan runs minutes, and reporting "unavailable" with no progress looks
	// broken. The scan publishes records-scanned progress as it goes.
	s.indexMu.RLock()
	scanTotal := 0
	if vol.index != nil {
		scanTotal = vol.index.compactRecordCount()
	}
	s.indexMu.RUnlock()
	vol.content.markScanning(scanTotal)

	items, journalID, checkpoint, snapshotResult := s.snapshotContentBuildItems(vol, opts)
	contentBuildSnapshotHook(s, vol)
	switch snapshotResult {
	case contentBuildSnapshotUnavailable:
		// Nothing to anchor a sidecar to (not USN, no watermark, or not
		// ready). Leave the state as-is; the post-rebuild hook reschedules.
		// The scan already entered indexing above: a stop returns it to
		// unavailable rather than stranding it mid-scan.
		if s.contentStopped(vol) {
			vol.content.markBuildCanceled("service stopping")
		}
		return
	case contentBuildSnapshotChanged:
		// The base generation moved while the metadata was copied; reschedule
		// against the new generation instead of building against a torn mix.
		retry = true
		return
	case contentBuildSnapshotLimited:
		// The exact number beyond the cap is unknown. Persist a nonzero
		// marker so attach and restart report the partial corpus.
		scopeDropped = 1
	}
	// Choose work before applying either cap. The compact index's record order
	// has no relationship to the scope's priority.
	sort.Slice(items, func(i, j int) bool { return contentBuildItemLess(items[i], items[j]) })

	vol.content.markIndexing(len(items))
	contentBuildIndexingHook(vol.content)

	settings := opts.extractSettings()
	ctx, cancel := context.WithCancel(contentWithExtractSettings(context.Background(), settings))
	defer cancel()
	go func() {
		select {
		case <-s.stop:
			cancel()
		case <-contentActivationDone(vol):
			cancel()
		case <-ctx.Done():
		}
	}()

	gsx := contentIndexPathForDB(vol.dbPath)
	if gsx == "" {
		vol.content.markBuildFailed("no content sidecar path")
		return
	}
	// Scratch lives next to the sidecar, on the volume the output goes to,
	// not in the service account's temp directory on the system drive.
	tmpDir, err := contentMkdirWork(gsx)
	if err != nil {
		vol.content.markBuildFailed(err.Error())
		return
	}
	defer os.RemoveAll(tmpDir)
	if err := contentCheckDiskReserve(tmpDir); err != nil {
		vol.content.markBuildFailed(err.Error())
		return
	}
	spool, err := os.Create(filepath.Join(tmpDir, "text.spool"))
	if err != nil {
		vol.content.markBuildFailed(err.Error())
		return
	}
	defer spool.Close()

	// Selection extracts in scope priority order. A single spool keeps memory
	// bounded while the assembler later consumes selected docs in FRN order.
	type selectedDoc struct {
		item        contentBuildItem
		offset      int64
		length      int
		modUnix     int64
		class       uint16
		version     uint16
		fileSize    int64
		fileModNano int64
	}
	selected := make([]selectedDoc, 0, 1024)
	total := len(items)
	done := 0
	var used, skipped, truncated, binary int64
	var sinceDiskCheck int64
	// overBudget latches once the scope budget bites; see the tally below.
	overBudget := false
	// allowance is the extracted-text budget enforced below: the explicit
	// budget_bytes when configured, else the reserve-derived allowance for
	// the sidecar's volume, refreshed as the spool grows (free space moves
	// under it; the spooled bytes are credited back, since the allowance
	// budgets the build's whole peak). MaxInt64 means unlimited (unknown
	// free space; the write-time guard stays the backstop).
	allowance := int64(^uint64(0) >> 1)
	var sinceAllowanceCheck int64
	explicitBudget := opts.Scope != nil && opts.Scope.BudgetExplicit
	if explicitBudget {
		allowance = opts.Scope.BudgetBytes
	} else {
		allowance = contentScopeAllowance(tmpDir, 0)
	}
	// Tier-2 filter (§6y): files the text budget drops are still covered by a
	// per-file bloom signature verified from disk. The builder is fed from
	// this same extraction loop (the dropped files are extracted anyway for
	// exact drop tallies), so covering them costs only the gram pass. Filter
	// bytes draw from the same pool as text (peak 1.5x per text byte in
	// reserve mode, 1x in explicit mode, 1x per filter byte): files past both
	// are scope_dropped with no coverage at all.
	s.indexMu.RLock()
	buildIsWalk := vol.index != nil && vol.index.Source == "walk"
	s.indexMu.RUnlock()
	filterOrigin := uint32(contentOriginUSN)
	if buildIsWalk {
		filterOrigin = contentOriginWalk
	}
	var filterScope [32]byte
	if opts.Scope != nil {
		filterScope = opts.Scope.fingerprint()
	}
	filterBuilder := newContentFilterBuilder(filterOrigin, settings.maxRaw, settings.maxText, filterScope, "")
	var filterUsed int64
	var headroom uint64
	var headroomOK bool
	if !explicitBudget {
		headroom, headroomOK = contentScopeHeadroom(tmpDir, 0)
	}
	filterFits := func(recSize int64) bool {
		if explicitBudget {
			return filterUsed+recSize <= allowance-used
		}
		if !headroomOK {
			return true
		}
		return float64(used)*contentScopePeakMultiplier+float64(filterUsed)+float64(recSize) <= float64(headroom)
	}
	abortReason := ""
	abortRetry := false
	nextExtracted, stopPrefetch := contentPrefetchExtract(ctx, items, contentBuildWorkerCount(), contentBuildDocSafe)
	defer stopPrefetch()
	for done < total {
		// The atomic generation covers the common abort (a rebuild/journal
		// reset bumps replayGen); the full journal-id check runs per batch
		// and before publish so a replayGen-free base swap is caught too.
		if vol.replayGen.Load() != gen {
			abortReason, abortRetry = "journal generation changed during build", true
			break
		}
		if s.contentStopped(vol) {
			abortReason = "service stopping"
			break
		}
		item := items[done]
		done++
		got, live := nextExtracted()
		if !live {
			abortReason = "service stopping"
			break
		}
		doc, res, ok := got.doc, got.res, got.ok
		if ok && res.Truncated {
			truncated++
		}
		if !ok {
			if res.Binary {
				binary++
			} else if res.Skipped {
				skipped++
			}
		}
		if done%contentBuildBatchSize == 0 {
			if !s.contentBuildCurrent(vol, gen, journalID) {
				abortReason, abortRetry = "journal generation changed during build", true
				break
			}
			vol.content.setBuildProgress(done, total)
			if contentBuildBatchPause > 0 {
				select {
				case <-s.stop:
					abortReason = "service stopping"
					break
				case <-contentActivationDone(vol):
					abortReason = "content plugin stopping"
				case <-time.After(contentBuildBatchPause):
				}
			}
		}
		if abortReason != "" {
			break
		}
		if !ok {
			continue
		}
		n := int64(len(doc.text))
		// Once the budget bites, extraction keeps consuming the ordered
		// stream to the end (abort and pacing checks above are unchanged) so
		// the dropped files and bytes are exact: every remaining file is
		// still extracted, its text tallied and released instead of spooled.
		// Selection order is unchanged; this only costs extraction work on a
		// build that was already going to publish incomplete.
		if !explicitBudget {
			if sinceAllowanceCheck += n; sinceAllowanceCheck >= contentDiskCheckEvery {
				sinceAllowanceCheck = 0
				allowance = contentScopeAllowance(tmpDir, used)
				headroom, headroomOK = contentScopeHeadroom(tmpDir, used)
			}
		}
		if overBudget || n > allowance-used {
			overBudget = true
			// The file is out of tier 1: tally the exact drop (the policy
			// keeps its meaning — files and bytes the text budget excluded
			// — so a filter-off build records identical bytes). Tier 2 then
			// covers it from the same pool at its exact record size,
			// measured from the extracted text the tally already paid for;
			// only a file past both is scope_dropped with no coverage.
			scopeDropped++
			scopeDroppedBytes += n
			grams := contentGramsOfU32(contentFoldText(contentStoredText(doc.text)))
			class := contentFilterClassForGrams(len(grams))
			if recSize := contentFilterRecordSize(class, item.path); filterFits(recSize) {
				filterBuilder.AddRecord(item.frn, item.path, doc.fileSize, doc.fileModNano, n, grams)
				filterUsed += recSize
			}
			continue
		}
		offset, err := spool.Seek(0, io.SeekCurrent)
		if err != nil {
			vol.content.markBuildFailed(err.Error())
			return
		}
		if _, err := spool.Write(doc.text); err != nil {
			vol.content.markBuildFailed(err.Error())
			return
		}
		selected = append(selected, selectedDoc{item: item, offset: offset, length: len(doc.text), modUnix: doc.modUnix, class: doc.class, version: doc.version, fileSize: doc.fileSize, fileModNano: doc.fileModNano})
		used += n
		if sinceDiskCheck += n; sinceDiskCheck >= contentDiskCheckEvery {
			sinceDiskCheck = 0
			if err := contentCheckDiskReserve(tmpDir); err != nil {
				vol.content.markBuildFailed(err.Error())
				return
			}
		}
	}
	if abortReason != "" {
		vol.content.markBuildCanceled(abortReason)
		retry = abortRetry
		return
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].item.frn != selected[j].item.frn {
			return selected[i].item.frn < selected[j].item.frn
		}
		return selected[i].item.path < selected[j].item.path
	})
	// sourceOf streams the selected docs at the given positions (ascending
	// FRN) back out of the spool. Each sourceOf call owns its cursor, but
	// concurrent shard assembles share the abort latch, so it is mutex-held.
	var abortMu sync.Mutex
	sourceOf := func(order []int) contentBuildSource {
		next := 0
		setAbort := func(reason string, retry bool) {
			abortMu.Lock()
			defer abortMu.Unlock()
			if abortReason == "" {
				abortReason, abortRetry = reason, retry
			}
		}
		return func() (contentBuildDoc, bool, error) {
			if vol.replayGen.Load() != gen || next%contentBuildBatchSize == 0 && !s.contentBuildCurrent(vol, gen, journalID) {
				setAbort("journal generation changed during assembly", true)
				return contentBuildDoc{}, false, errContentBuildAborted
			}
			if s.contentStopped(vol) {
				setAbort("service stopping", false)
				return contentBuildDoc{}, false, errContentBuildAborted
			}
			if next == len(order) {
				return contentBuildDoc{}, false, nil
			}
			s := selected[order[next]]
			next++
			text := make([]byte, s.length)
			if _, err := spool.ReadAt(text, s.offset); err != nil {
				return contentBuildDoc{}, false, err
			}
			return contentBuildDoc{path: s.item.path, frn: s.item.frn, text: text, modUnix: s.modUnix, class: s.class, version: s.version, fileSize: s.fileSize, fileModNano: s.fileModNano}, true, nil
		}
	}
	s.indexMu.RLock()
	isWalkBuild := vol.index != nil && vol.index.Source == "walk"
	s.indexMu.RUnlock()
	var budgetBytes int64
	if opts.Scope != nil {
		budgetBytes = opts.Scope.BudgetBytes
	}
	// Name the enforced limit in the policy: the explicit budget, or the
	// reserve when the reserve stopped selection (BudgetBytes stays 0 for
	// the reserve-governed default).
	var reserveBytes int64
	if scopeDropped > 0 && !explicitBudget {
		reserveBytes = int64(contentScopeReserveFor(tmpDir))
	}
	header := &contentIndex{
		Origin:        contentOriginUSN,
		JournalID:     journalID,
		CheckpointUSN: uint64(checkpoint),
		Policy:        contentBuildPolicy{MaxRaw: settings.maxRaw, MaxText: settings.maxText, Skipped: skipped, Truncated: truncated, ScopeDropped: scopeDropped, ScopeDroppedBytes: scopeDroppedBytes, BudgetBytes: budgetBytes, Binary: binary, ReserveBytes: reserveBytes},
	}
	if isWalkBuild {
		// Walk sidecars join on the stable path hash with no journal: the
		// checkpoint is the wall-clock build watermark, and JournalID 0
		// marks the origin next to the Origin field.
		header.Origin = contentOriginWalk
		header.JournalID = 0
	}
	if opts.Scope != nil {
		header.ScopeHash = opts.Scope.fingerprint()
	}
	// assemble runs the streaming assembler over the selected docs at the given
	// positions and returns the spooled shard plus a header index carrying the
	// doc table and sidecar identity for the streaming save. The spooled
	// section files, not a heap index, are what gets saved; attach reloads
	// the sidecar through the mmap loader.
	//
	// Each assemble call gets a private scratch directory: the streaming
	// assembler names its term/gram section files with fixed names, so
	// concurrent shard assembles must not share one directory. Sequential
	// callers could share, but a private directory is equally correct and
	// keeps one code path. Everything under tmpDir is removed with it.
	//
	// saveShard assembles one shard, saves it, and removes its scratch at
	// once: the save copies the section files into the shard, so keeping the
	// scratch until tmpDir goes would hold a second copy of every saved
	// shard on the volume alongside the spool.
	assemble := func(order []int) (*contentStreamedBuild, *contentIndex, string, error) {
		shardTmp, err := os.MkdirTemp(tmpDir, "shard-*")
		if err != nil {
			return nil, nil, "", err
		}
		built, err := assembleContentIndexStreamed(sourceOf(order), shardTmp)
		if err != nil {
			return nil, nil, shardTmp, err
		}
		shard := contentShardHeader(header, built)
		shard.Sections = map[uint32][]byte{contentSectionPaths: encodeContentPaths(built.paths)}
		return built, shard, shardTmp, nil
	}
	saveShard := func(dir string, order []int, prefixes []string) (contentShardInfo, error) {
		built, shard, shardTmp, err := assemble(order)
		if shardTmp != "" {
			defer os.RemoveAll(shardTmp)
		}
		if err != nil {
			return contentShardInfo{}, err
		}
		if err := contentCheckDiskReserve(dir); err != nil {
			return contentShardInfo{}, err
		}
		return contentSaveShard(dir, shard, contentBuildFiles(built), prefixes)
	}
	// save assembles and writes the sidecar: one file for a corpus within a
	// shard, otherwise folder shards plus a manifest. Shards are assembled one
	// at a time, so peak memory is bounded by the largest shard.
	vol.content.setBuildPhase(contentBuildPhaseAssembling)
	save := func() error {
		if used <= contentShardTargetBytes {
			all := make([]int, len(selected))
			for i := range all {
				all[i] = i
			}
			built, shard, _, err := assemble(all)
			if err != nil {
				return err
			}
			if !s.contentBuildCurrent(vol, gen, journalID) {
				return errContentBuildStale
			}
			if err := contentCheckDiskReserve(gsx); err != nil {
				return err
			}
			return contentSaveAssembled(gsx, shard, contentBuildFiles(built))
		}
		paths := make([]string, len(selected))
		sizes := make([]int64, len(selected))
		for i, d := range selected {
			paths[i], sizes[i] = d.item.path, int64(d.length)
		}
		groups := contentGroupByShard(contentPlanShards(paths, sizes, contentShardTargetBytes), paths)
		dir := contentShardDir(gsx)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		infos := make([]contentShardInfo, 0, len(groups))
		if len(groups) == 1 || contentShardBuildConcurrency < 2 {
			for _, g := range groups {
				info, err := saveShard(dir, g.docs, g.prefixes)
				if err != nil {
					return err
				}
				infos = append(infos, info)
			}
		} else {
			// Shards assemble concurrently, bounded by
			// contentShardBuildConcurrency. Manifest order follows groups, so
			// the published layout is identical to the sequential build's.
			par := make([]contentShardInfo, len(groups))
			sem := make(chan struct{}, contentShardBuildConcurrency)
			var wg sync.WaitGroup
			var mu sync.Mutex
			var firstErr error
			for gi := range groups {
				wg.Add(1)
				go func(gi int) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					info, err := saveShard(dir, groups[gi].docs, groups[gi].prefixes)
					if err == nil {
						par[gi] = info
					}
					if err != nil {
						mu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						mu.Unlock()
					}
				}(gi)
			}
			wg.Wait()
			if firstErr != nil {
				return firstErr
			}
			infos = append(infos, par...)
		}
		if !s.contentBuildCurrent(vol, gen, journalID) {
			return errContentBuildStale
		}
		serviceLog("content build volume=%s shards=%d", vol.volume, len(infos))
		return contentSaveManifest(gsx, header, infos)
	}
	err = save()
	vol.content.setBuildProgress(done, total)
	if err != nil {
		// Delete shard files this attempt wrote: no manifest names them, so a
		// failed build must not leave gigabytes behind on the volume.
		contentSweepShards(gsx)
		var sizeErr *contentGSXSizeError
		switch {
		case errors.Is(err, errContentBuildAborted):
			vol.content.markBuildCanceled(abortReason)
			retry = abortRetry
		case errors.Is(err, errContentBuildStale):
			// The volume could have been rebuilt (or the service stopped)
			// during the minutes-long extraction.
			vol.content.markBuildCanceled("journal generation changed before publish")
			retry = true
		case errors.As(err, &sizeErr):
			// Over the size cap: refuse to publish rather than truncate. The
			// volume is surfaced degraded with the size in health; no base is
			// written, so no content is silently dropped.
			vol.content.markSidecarCapped(sizeErr.size, sizeErr.cap)
		default:
			vol.content.markBuildFailed(err.Error())
		}
		return
	}
	// Tier-2 sidecar (§6y): publish the dropped files' filter beside the
	// `.gsx`, guarded by the same reserve. A save failure only disables tier
	// 2 (the base still attaches; health reports the drops uncovered), never
	// fails the build. A build that dropped nothing removes a stale filter so
	// an older selection's coverage cannot outlive its base.
	filterPath := contentFilterPathForGSX(gsx)
	if filterBuilder.Len() > 0 {
		if verr := contentCheckDiskReserve(gsx); verr != nil {
			serviceLog("content filter skipped volume=%s: %v", vol.volume, verr)
		} else if verr := contentSaveFilter(filterPath, filterBuilder.Finish()); verr != nil {
			serviceLog("content filter save failed volume=%s: %v", vol.volume, verr)
		}
	} else if filterPath != "" {
		_ = os.Remove(filterPath)
	}
	// Publish through the normal attach path: it loads under indexMu and
	// re-validates the journal generation, so a base swapped in during the
	// save is not overwritten with this build.
	s.attachContentForVolume(vol)
	if vol.content.readerView() != nil {
		// The replaced base is released by now, so shards it alone named
		// can be deleted.
		contentSweepShards(gsx)
		return
	}
	if !s.contentBuildCurrent(vol, gen, journalID) {
		// A rebuild swapped the journal generation while the sidecar was
		// written; let the rebuild's own hook reschedule against the new one.
		vol.content.markBuildCanceled("journal generation changed before attach")
		retry = true
		return
	}
	// A generation-current sidecar that still will not attach is a persistent
	// fault (e.g. no base FRN column); surface it as degraded, do not spin.
	vol.content.markBuildFailed("freshly built sidecar did not attach")
}

// serviceStopping reports whether the service is shutting down, so a build does
// not reschedule or do more work.
func (s *goSearchService) serviceStopping() bool {
	if s == nil || s.stop == nil {
		return false
	}
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}
