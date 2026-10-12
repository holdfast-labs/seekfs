package main

// Content search for walk-indexed volumes (no USN journal).
//
// A walk record's FRN is positional (`FRN = i+1` in walk order, assigned by
// ensureCompactIndexForService), so it is NOT stable across rescans: every
// watcher-triggered filename rebuild renumbers the table, and joining content
// docs on it would resolve results to the wrong records. Walk content docs
// are therefore keyed on a stable path hash — contentWalkKey, the FNV-1a hash
// of the cleaned, slash-normalized absolute path — stored in the doc's FRN
// field (no new doc-table field; the resolver and delta are key-agnostic
// uint64 maps). The key is deliberately case-sensitive: on a case-sensitive
// filesystem two names differing only by case are different files and must not
// share a key, while on Windows a case-only rename becomes a delete+create
// pair, which is correct (if slightly more work) rather than a misjoin.
//
// Change detection without a journal diffs (path, size, mtime) of the live
// filename index against the content doc table: contentReconcileWalkVolume
// enqueues changed and deleted keys into the existing delta/fold pipeline,
// whose extraction hash-verifies before upserting. The v6 doc fingerprint
// (FileSize/FileModNano, source bytes and nanosecond mtime at extraction) is
// what the diff compares; RawSize/ModUnix stay what they were (stored-text
// length and second-precision mtime) for USN compatibility.
//
// Freshness layers, cheapest first:
//  1. the existing ReadDirectoryChangesW watcher rebuilds the filename index
//     on change; rebuildWalkVolumeInPlace rebinds the content resolver and
//     reconciles right after, so an edit is one drain tick away;
//  2. the drain loop reconciles whenever the filename generation moved;
//  3. a periodic safety-net reconcile (contentWalkReconcilePeriod) covers a
//     missed watcher event and platforms without a watcher.
//
// The watermark is origin-appropriate, not the USN (JournalID, CheckpointUSN)
// pair: a walk base stamps JournalID 0 (which also marks it walk-origin next
// to the Origin field) and CheckpointUSN with the wall-clock reconcile time
// in nanoseconds. There is no journal to validate or catch up from; restart
// freshness is a reconcile at attach plus the watcher's next rebuild.

import (
	"os"
	"path/filepath"
	"time"
)

// contentWalkReconcilePeriod is the safety-net cadence for walk reconciles
// that no filename generation change triggered. A var so tests run without
// the delay.
var contentWalkReconcilePeriod = 60 * time.Second

// contentWalkPrint is the (size, nanosecond mtime) fingerprint a walk
// reconcile diffs. Named so the delta skip-set and the live/base maps share it.
type contentWalkPrint struct {
	size int64
	mod  int64
}

// contentWalkKey returns the stable content key for an absolute file path:
// the FNV-1a hash of its cleaned, slash-normalized form. Every walk content
// path (build, delta, resolver join, query) must derive the key through this
// one function, or keys silently stop joining. It is case-sensitive on
// purpose; see the package comment.
func contentWalkKey(absPath string) uint64 {
	if absPath == "" {
		return 0
	}
	return contentPathKey(filepath.ToSlash(filepath.Clean(absPath)))
}

// walkKeySet is a walk volume's content join column: the stable key of every
// live file record, sorted ascending with record IDs parallel to it — the
// same shape as the persisted USN FRN column, built on the fly because walk
// FRNs are positional. The set is immutable once built; refresh replaces the
// pointer. Guarded by indexMu like the index it derives from: readers run
// under indexMu.RLock, refreshes under indexMu.Lock.
type walkKeySet struct {
	idx  *Index
	keys []uint64
	ids  []uint32
}

// contentWalkKeys returns the volume's cached join column, rebuilding it when
// the index pointer moved (every walk filename rebuild swaps it). ok is false
// when the volume has no walk index to join. Callers must hold indexMu
// (read or write); the build itself only reads the index through the same
// accessors the snapshot path uses.
func (s *goSearchService) contentWalkKeys(vol *serviceVolumeIndex) (keys []uint64, ids []uint32, ok bool) {
	if s == nil || vol == nil {
		return nil, nil, false
	}
	if vol.index == nil || vol.index.Source != "walk" {
		return nil, nil, false
	}
	if set := vol.walkKeys; set != nil && set.idx == vol.index {
		return set.keys, set.ids, true
	}
	keys, ids = buildContentWalkKeys(vol.index)
	vol.walkKeys = &walkKeySet{idx: vol.index, keys: keys, ids: ids}
	return keys, ids, true
}

// buildContentWalkKeys scans the compact records once, keying every live file
// by its reconstructed absolute path. Deleted records and directories carry
// no content and are skipped. The output is sorted by key with record IDs
// parallel, so buildContentResolver can join it in one merge pass.
func buildContentWalkKeys(idx *Index) ([]uint64, []uint32) {
	if idx == nil {
		return nil, nil
	}
	count := idx.compactRecordCount()
	cache := make(map[int]string, 1024)
	keys := make([]uint64, 0, count)
	ids := make([]uint32, 0, count)
	for id := 0; id < count; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) != 0 {
			continue
		}
		path := idx.reconstructCompactPathCached(id, cache)
		if path == "" {
			continue
		}
		if key := contentWalkKey(path); key != 0 {
			keys = append(keys, key)
			ids = append(ids, uint32(id))
		}
		if len(cache) > contentBuildPathCacheMax {
			cache = make(map[int]string, 1024)
		}
	}
	sortFRNIndexEntries(keys, ids)
	return keys, ids
}

// contentWalkKeyOfEntry derives the content key for a query Entry on a
// walk-origin volume. Walk record FRNs are positional and meaningless across
// rescans; the path is the stable identity.
func contentWalkKeyOfEntry(entry *Entry) (uint64, bool) {
	if entry == nil || entry.Path == "" {
		return 0, false
	}
	if key := contentWalkKey(entry.Path); key != 0 {
		return key, true
	}
	return 0, false
}

// contentWalkOriginAttached reports whether the volume currently serves a
// walk-origin content base on a walk-indexed volume. It decides which key
// (positional FRN vs stable path hash) the query and drain paths must use.
// Both sides must agree: a hand-built fixture may carry the assembler's
// default walk origin on a USN volume (or vice versa), and the key space is
// only valid when it matches the volume source. Call with indexMu held
// (read or write); every production caller (search, drain, reconcile) does.
func (vol *serviceVolumeIndex) contentWalkOriginAttached() bool {
	if vol == nil || vol.content == nil || vol.index == nil || vol.index.Source != "walk" {
		return false
	}
	vol.content.mu.RLock()
	defer vol.content.mu.RUnlock()
	return vol.content.idx != nil && vol.content.idx.Origin == contentOriginWalk &&
		vol.content.reader != nil && vol.content.resolver != nil
}

// contentResolveWalkPath maps a walk content key to its current absolute
// path through the cached join column. A key with no live record (deleted or
// renamed away) resolves false so the drain tombstones it. Callers must hold
// indexMu (read or write); pathCache is the caller's private memo.
func (s *goSearchService) contentResolveWalkPath(vol *serviceVolumeIndex, key uint64, pathCache map[int]string) (string, bool) {
	if key == 0 || vol == nil || vol.index == nil {
		return "", false
	}
	keys, ids, ok := s.contentWalkKeys(vol)
	if !ok {
		return "", false
	}
	i, found := contentLookupFRNColumn(keys, key)
	if !found || i >= len(ids) {
		return "", false
	}
	id := int(ids[i])
	if id < 0 || id >= vol.index.compactRecordCount() {
		return "", false
	}
	if rec := vol.index.compactRecord(id); rec.Deleted {
		return "", false
	}
	if p := vol.index.reconstructCompactPathCached(id, pathCache); p != "" {
		return p, true
	}
	return "", false
}

// contentReconcileWalkVolume diffs the live walk filename index against the
// attached content base plus resident delta and enqueues the difference:
// keys for new or changed files go to the extraction queue, keys whose file
// is gone (or newly out of scope) are tombstoned. It is metadata-only — no
// file is opened — so a reconcile pass costs one index scan, not extraction
// I/O; the drain's processQueue does the bounded extraction afterward and
// hash-verifies before upserting, so an mtime-only touch never churns text.
//
// force reconciles even when the filename generation has not moved (attach,
// periodic safety net). Otherwise only a new generation is diffed. It
// returns the enqueued keys with their enqueue-time fingerprints so the
// drain can record which of them produced no indexable text (the
// unindexable skip-set).
func (s *goSearchService) contentReconcileWalkVolume(vol *serviceVolumeIndex, force bool) map[uint64]contentWalkPrint {
	if s == nil || vol == nil || vol.content == nil || vol.contentCoord == nil {
		return nil
	}
	if !contentSearchEnabled() {
		return nil
	}
	if contentScopeForVolume(s.contentConfig(), vol.volume).disabled() {
		return nil
	}
	if !s.beginContentTask(vol) {
		return nil
	}
	defer endContentTask(vol)

	lastGen, lastRun := vol.content.walkReconcileMark()
	if !force && time.Since(lastRun) < contentWalkReconcilePeriod {
		// Fast path without any service lock: a reconcile ran recently and
		// the caller did not force one. The generation check below (under
		// indexMu) still catches a filename rebuild that landed since.
		s.indexMu.RLock()
		sameGen := vol.replayGen.Load() == lastGen
		s.indexMu.RUnlock()
		if sameGen {
			return nil
		}
	}
	// Scope resolution takes indexMu itself, so it runs outside the scan's
	// read lock (nesting RLocks deadlocks once a writer waits).
	scope, ok := s.resolvedContentScope(vol)
	if !ok {
		return nil
	}
	s.indexMu.RLock()
	if vol.index == nil || vol.index.Source != "walk" || vol.state != "ready" {
		s.indexMu.RUnlock()
		return nil
	}
	reader, resolver := vol.content.readerResolverView()
	if reader == nil || reader.idx == nil || resolver == nil {
		s.indexMu.RUnlock()
		return nil
	}
	if reader.idx.Origin != contentOriginWalk {
		s.indexMu.RUnlock()
		return nil
	}
	gen := vol.replayGen.Load()
	keys, ids, ok := s.contentWalkKeys(vol)
	if !ok {
		s.indexMu.RUnlock()
		return nil
	}
	// Index-side fingerprint: key -> (size, modNano) for every live file.
	live := make(map[uint64]contentWalkPrint, len(keys))
	changed := make(map[uint64]contentWalkPrint)
	pathCache := make(map[int]string, 1024)
	var scoped []uint64
	for n, key := range keys {
		id := int(ids[n])
		if id < 0 || id >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(id)
		if rec.Deleted {
			continue
		}
		path := vol.index.reconstructCompactPathCached(id, pathCache)
		if path == "" {
			continue
		}
		if len(pathCache) > contentBuildPathCacheMax {
			pathCache = make(map[int]string, 1024)
		}
		if _, _, allowed := scope.allows(path); !allowed {
			// Out of scope and never indexed: skip silently. Tombstoning
			// it would plant a phantom entry the folds reprocess forever.
			continue
		}
		live[key] = contentWalkPrint{size: rec.Size, mod: rec.ModUnix}
	}
	// Base-side fingerprint from the attached doc table.
	base := make(map[uint64]contentWalkPrint, len(reader.idx.Docs))
	for i := range reader.idx.Docs {
		d := &reader.idx.Docs[i]
		if d.FRN != 0 {
			base[d.FRN] = contentWalkPrint{size: d.FileSize, mod: d.FileModNano}
		}
	}
	delta := vol.content.deltaView()
	s.indexMu.RUnlock()

	// New or changed files enqueue for extraction. A file whose live delta
	// entry already matches the index fingerprint keeps it (its extraction
	// is queued or done at the current values); a delta entry that predates
	// the change must re-extract — skipping it would serve the stale text
	// forever, since nothing else would queue the key again. A key the
	// skip-set remembers at these exact values extracted to nothing last
	// time and is not retried until it changes.
	for key, want := range live {
		if delta != nil {
			// Exact fingerprint match (empty files included: size 0 is a
			// legitimate value, not a missing one).
			if doc, ok := delta.docFor(key); ok && !doc.Deleted &&
				doc.FileSize == want.size && doc.FileModNano == want.mod {
				continue
			}
		}
		if vol.content.walkDeclinedSkipped(key, want) {
			continue
		}
		got, inBase := base[key]
		if !inBase && want.size == 0 {
			// An empty file was never indexed and never will be (every
			// extractor declines text-free input); enqueueing it would
			// fast-fail extraction on every reconcile forever. A
			// truncation to empty of an indexed file still flows below
			// (inBase), where the declining extraction tombstones it.
			continue
		}
		if !inBase || got != want {
			changed[key] = want
		}
	}
	// Vanished files tombstone so the fold drops them; a rename is exactly
	// this (old key tombstoned) plus a new-key enqueue above. A file back at
	// its base fingerprint has its pre-fold tombstone rescinded instead, so
	// restoring a file before the fold cannot drop live content.
	for key, want := range base {
		got, ok := live[key]
		if !ok {
			scoped = append(scoped, key)
			continue
		}
		if got == want && delta != nil {
			delta.rescind(key)
		}
	}
	if delta != nil {
		for _, doc := range delta.snapshot() {
			if doc.Deleted {
				continue
			}
			if _, ok := live[doc.FRN]; !ok {
				scoped = append(scoped, doc.FRN)
			}
		}
	}
	// Tombstones first, so a path that moved between keys never briefly
	// serves both copies.
	seen := make(map[uint64]struct{}, len(scoped))
	if delta != nil {
		for _, key := range scoped {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			delta.delete(key)
		}
	}
	if len(changed) > 0 {
		list := make([]uint64, 0, len(changed))
		for key := range changed {
			list = append(list, key)
		}
		vol.contentCoord.enqueueFRNs(list)
	}
	vol.content.walkReconcileDone(gen)
	if len(changed) > 0 || len(seen) > 0 {
		serviceLog("content walk reconcile volume=%s changed=%d dropped=%d", vol.volume, len(changed), len(seen))
	}
	return changed
}

// contentNoteWalkDeclined records enqueued keys that extraction left
// unindexed, so the next reconcile does not retry them at the same
// fingerprint. A key that did land (live delta or base doc at the
// enqueue-time fingerprint) is left alone; a fingerprint change re-arms it
// in the reconcile. Call after the drain's processQueue.
func (s *goSearchService) contentNoteWalkDeclined(vol *serviceVolumeIndex, enqueued map[uint64]contentWalkPrint) {
	if vol == nil || vol.content == nil || len(enqueued) == 0 {
		return
	}
	reader, resolver := vol.content.readerResolverView()
	delta := vol.content.deltaView()
	for key, want := range enqueued {
		if delta != nil {
			if doc, ok := delta.docFor(key); ok && !doc.Deleted &&
				doc.FileSize == want.size && doc.FileModNano == want.mod {
				continue
			}
		}
		if reader != nil && reader.idx != nil && resolver != nil {
			if di, ok := resolver.docForFRN(key); ok && di < len(reader.idx.Docs) {
				d := &reader.idx.Docs[di]
				if d.FileSize == want.size && d.FileModNano == want.mod {
					continue
				}
			}
		}
		vol.content.walkDeclinedNote(key, want)
	}
}
