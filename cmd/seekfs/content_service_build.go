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
// Memory. The build streams extracted docs straight into the text store and the
// external posting builders (assembleContentIndexStream); it never accumulates a
// []contentBuildDoc of the raw corpus. Peak transient heap is one document's raw
// text plus the two contentBuildArenaBytes posting arenas and the text store
// being assembled, adding only metadata (the item list and a bounded path cache)
// per record; the published index is bounded by its encoded sections.
//
// IO governor. Extraction is serial and paused every batch so filename search
// and USN replay keep the disk; the loop is cancelable through s.stop and the
// per-volume build generation.
//
// Build vs catch-up. A base build extracts the base corpus while restart
// catch-up and the drain loop may extract changed files into the in-memory
// delta. They are deliberately not serialized: they read the volume and write
// different places (the build writes the `.gsx`; catch-up/drain mutate only the
// delta), and a duplicate extraction is an idempotent upsert. Publishing
// attaches the fresh base and leaves the delta overlay intact; catch-up resumes
// from the new base's checkpoint, so no change is double-counted or lost.
//
// Default scope (M5). Unlike the offline CLI (which indexes any extractable
// file when no -ext is given), the service build is allowlisted: the curated
// text/code/config set below plus every registered rich-extractor extension. A
// whole-volume build is never the default.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
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

// defaultServiceContentBuildOptions is the service build's default scope (M5):
// a curated text/code/config allowlist unioned with every registered
// extractor's extensions. The offline builder defaults to "any extractable
// file"; the service deliberately does not.
func defaultServiceContentBuildOptions() contentBuildOptions {
	opts := defaultContentBuildOptions()
	opts.Exts = contentServiceExtensions()
	// A service-wide encoding override so the background build and the USN
	// delta extractor decode the same way. Absent/unset means the default auto
	// policy (BOM + UTF-8 exact + Windows-1252/Latin-1 fallback).
	opts.Encoding = contentServiceEncodingLabel()
	return opts
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
	".html", ".htm", ".xhtml", ".xml", ".xsl", ".xslt", ".xsd", ".svg",
	".json", ".jsonc", ".jsonl", ".ndjson", ".yaml", ".yml", ".toml",
	".ini", ".cfg", ".conf", ".config", ".properties", ".env", ".editorconfig",
	".csv", ".tsv", ".tex", ".bib", ".diff", ".patch",
}

// contentBuildItem is the copied metadata for one file to extract. It holds no
// slice into the record index or its mmap, so extraction can run with every
// service lock released.
type contentBuildItem struct {
	frn     uint64
	path    string
	size    int64
	modUnix int64
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
// is usable and the volume is a ready USN volume with a live watermark, it
// schedules exactly one background build.
func (s *goSearchService) scheduleContentBuild(vol *serviceVolumeIndex) {
	if s == nil || vol == nil || !contentSearchEnabled() || vol.content == nil {
		return
	}
	// Reuse an existing valid sidecar when there is one.
	s.attachContentForVolume(vol)
	if vol.content.stateOf() == contentStateReady {
		return
	}
	s.indexMu.RLock()
	ready := vol.state == "ready" && vol.index != nil && vol.index.Source == "usn"
	journalID := vol.journalID
	checkpoint := vol.baseCheckpoint
	if checkpoint <= 0 && vol.index != nil {
		checkpoint = vol.index.Checkpoint
	}
	s.indexMu.RUnlock()
	if !ready || journalID == 0 || checkpoint <= 0 {
		// Not a USN volume, not ready (a rebuild owns it), or no watermark to
		// anchor a sidecar to. The post-rebuild hook reschedules when it is.
		return
	}
	if !vol.contentBuildBusy.CompareAndSwap(false, true) {
		return
	}
	contentBuildRun(func() { s.runContentBuild(vol) })
}

// scheduleContentRebuild schedules a content rebuild for vol without first
// trying to re-attach the existing sidecar. It is the self-heal path for a
// capped or errored restart catch-up: that sidecar is still valid but
// incomplete, so re-attaching it (scheduleContentBuild) would leave the volume
// permanently degraded with a gated fold and an ever-growing delta.
// contentCatchUpFailed bounds its use with contentBuildAttempts.
func (s *goSearchService) scheduleContentRebuild(vol *serviceVolumeIndex) {
	if s == nil || vol == nil || !contentSearchEnabled() || vol.content == nil {
		return
	}
	s.indexMu.RLock()
	ready := vol.state == "ready" && vol.index != nil && vol.index.Source == "usn"
	journalID := vol.journalID
	checkpoint := vol.baseCheckpoint
	if checkpoint <= 0 && vol.index != nil {
		checkpoint = vol.index.Checkpoint
	}
	s.indexMu.RUnlock()
	if !ready || journalID == 0 || checkpoint <= 0 {
		return
	}
	if !vol.contentBuildBusy.CompareAndSwap(false, true) {
		return
	}
	contentBuildRun(func() { s.runContentBuild(vol) })
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
	s.indexMu.RLock()
	if vol.index == nil || vol.index.Source != "usn" {
		s.indexMu.RUnlock()
		return nil, 0, 0, contentBuildSnapshotUnavailable
	}
	idx := vol.index
	journalID = vol.journalID
	checkpoint = vol.baseCheckpoint
	if checkpoint <= 0 {
		checkpoint = idx.Checkpoint
	}
	gen := vol.replayGen.Load()
	count := idx.compactRecordCount()
	s.indexMu.RUnlock()
	if journalID == 0 || checkpoint <= 0 {
		return nil, 0, 0, contentBuildSnapshotUnavailable
	}

	items = make([]contentBuildItem, 0, 1024)
	cache := make(map[int]string, 1024)
	for start := 0; start < count; start += contentBuildSnapshotBatch {
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
			items = append(items, contentBuildItem{frn: rec.FRN, path: path, size: rec.Size, modUnix: rec.ModUnix})
		}
		s.indexMu.RUnlock()
		if len(cache) > contentBuildPathCacheMax {
			cache = make(map[int]string, 1024)
		}
		contentBuildSnapshotBatchHook(s, vol)
		if opts.MaxFiles > 0 && len(items) > opts.MaxFiles {
			items = items[:opts.MaxFiles]
			break
		}
	}
	return items, journalID, checkpoint, contentBuildSnapshotOK
}

// contentBuildDocSafe extracts one file into a build doc. It recovers from an
// extractor panic so one pathological document cannot kill the service, and
// returns ok=false for missing/oversized/binary/skipped files.
func contentBuildDocSafe(ctx context.Context, item contentBuildItem) (doc contentBuildDoc, res contentExtractResult, ok bool) {
	defer func() {
		if recover() != nil {
			doc = contentBuildDoc{}
			res = contentExtractResult{Skipped: true, Reason: "extractor panic"}
			ok = false
		}
	}()
	f, err := contentOpenNoRecall(item.path)
	if err != nil {
		return contentBuildDoc{}, contentExtractResult{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return contentBuildDoc{}, contentExtractResult{}, false
	}
	size := info.Size()
	head, _ := contentReadBounded(f, size, 512)
	e := contentExtractorForPath(item.path, head)
	if e == nil {
		return contentBuildDoc{}, contentExtractResult{}, false
	}
	// Bound one document's extraction so a hang cannot stall the serial build;
	// contentExtractSafely nests the deadline inside the build's stop/cancel
	// context and isolates a parser panic.
	res, err = contentExtractSafely(ctx, e, f, size)
	if err != nil || res.Skipped || len(res.Text) == 0 {
		return contentBuildDoc{}, res, false
	}
	return contentBuildDoc{path: item.path, frn: item.frn, text: res.Text, modUnix: info.ModTime().Unix(), class: res.Class, version: e.Version()}, res, true
}

// contentBuildCurrent reports whether a build may still publish: the per-volume
// replay generation and the journal id must be unchanged. A journal reset or
// base rebuild bumps replayGen, so a build that spans one aborts.
func (s *goSearchService) contentBuildCurrent(vol *serviceVolumeIndex, gen uint64, journalID uint64) bool {
	if vol.replayGen.Load() != gen {
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
func (s *goSearchService) runContentBuild(vol *serviceVolumeIndex) {
	retry := false
	contentBuildGate <- struct{}{}
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
		if !retry || s.serviceStopping() {
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
			case <-time.After(contentBuildRetryBackoff):
			}
		}
		s.scheduleContentBuild(vol)
	}()
	if vol == nil || vol.content == nil {
		return
	}
	gen := vol.replayGen.Load()
	opts := defaultServiceContentBuildOptions()

	items, journalID, checkpoint, snapshotResult := s.snapshotContentBuildItems(vol, opts)
	contentBuildSnapshotHook(s, vol)
	switch snapshotResult {
	case contentBuildSnapshotUnavailable:
		// Nothing to anchor a sidecar to (not USN, no watermark, or not
		// ready). Leave the state as-is; the post-rebuild hook reschedules.
		return
	case contentBuildSnapshotChanged:
		// The base generation moved while the metadata was copied; reschedule
		// against the new generation instead of building against a torn mix.
		retry = true
		return
	}

	vol.content.markIndexing(len(items))
	contentBuildIndexingHook(vol.content)

	settings := opts.extractSettings()
	ctx, cancel := context.WithCancel(contentWithExtractSettings(context.Background(), settings))
	defer cancel()
	go func() {
		select {
		case <-s.stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	total := len(items)
	// Extract in the (frn, path) order assembleContentIndexStream requires, so
	// docIDs match the offline builder's for the same records.
	sort.Slice(items, func(i, j int) bool {
		if items[i].frn != items[j].frn {
			return items[i].frn < items[j].frn
		}
		return items[i].path < items[j].path
	})

	// source extracts one doc at a time into the streaming assembler, so the raw
	// corpus is never accumulated (see the memory budget in the file comment).
	done := 0
	var skipped, truncated int64
	abortReason := ""
	abortRetry := false
	source := func() (contentBuildDoc, bool, error) {
		for done < total {
			// The atomic generation covers the common abort (a rebuild/journal
			// reset bumps replayGen); the full journal-id check runs per batch
			// and before publish so a replayGen-free base swap is caught too.
			if vol.replayGen.Load() != gen {
				abortReason, abortRetry = "journal generation changed during build", true
				return contentBuildDoc{}, false, errContentBuildAborted
			}
			if s.serviceStopping() {
				abortReason = "service stopping"
				return contentBuildDoc{}, false, errContentBuildAborted
			}
			item := items[done]
			done++
			doc, res, ok := contentBuildDocSafe(ctx, item)
			if ok && res.Truncated {
				truncated++
			}
			if !ok && res.Skipped {
				skipped++
			}
			if done%contentBuildBatchSize == 0 {
				if !s.contentBuildCurrent(vol, gen, journalID) {
					abortReason, abortRetry = "journal generation changed during build", true
					return contentBuildDoc{}, false, errContentBuildAborted
				}
				vol.content.setBuildProgress(done, total)
				if contentBuildBatchPause > 0 {
					select {
					case <-s.stop:
						abortReason = "service stopping"
						return contentBuildDoc{}, false, errContentBuildAborted
					case <-time.After(contentBuildBatchPause):
					}
				}
			}
			if ok {
				return doc, true, nil
			}
		}
		return contentBuildDoc{}, false, nil
	}

	tmpDir, err := os.MkdirTemp("", "seekfs-content-*")
	if err != nil {
		vol.content.markBuildFailed(err.Error())
		return
	}
	cidx, err := assembleContentIndexStream(source, tmpDir)
	_ = os.RemoveAll(tmpDir)
	if err != nil {
		if errors.Is(err, errContentBuildAborted) {
			vol.content.markBuildCanceled(abortReason)
			retry = abortRetry
			return
		}
		vol.content.markBuildFailed(err.Error())
		return
	}
	vol.content.setBuildProgress(done, total)
	cidx.Origin = contentOriginUSN
	cidx.JournalID = journalID
	cidx.CheckpointUSN = uint64(checkpoint)
	cidx.Policy = contentBuildPolicy{MaxRaw: settings.maxRaw, MaxText: settings.maxText, Skipped: skipped, Truncated: truncated}

	// Re-check before publishing: the volume could have been rebuilt (or the
	// service stopped) during the minutes-long extraction.
	if !s.contentBuildCurrent(vol, gen, journalID) {
		vol.content.markBuildCanceled("journal generation changed before publish")
		retry = true
		return
	}
	gsx := contentIndexPathForDB(vol.dbPath)
	if gsx == "" {
		vol.content.markBuildFailed("no content sidecar path")
		return
	}
	if err := contentSaveFile(gsx, cidx); err != nil {
		var sizeErr *contentGSXSizeError
		if errors.As(err, &sizeErr) {
			// Over the size cap: refuse to publish rather than truncate. The
			// volume is surfaced degraded with the size in health; no base is
			// written, so no content is silently dropped.
			vol.content.markSidecarCapped(sizeErr.size, sizeErr.cap)
			return
		}
		vol.content.markBuildFailed(err.Error())
		return
	}
	// Publish through the normal attach path: it loads under indexMu and
	// re-validates the journal generation, so a base swapped in during the
	// save is not overwritten with this build.
	s.attachContentForVolume(vol)
	if vol.content.stateOf() == contentStateReady {
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
