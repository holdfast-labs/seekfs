package main

// P2 service-side content machinery. Content search stays off by default; when
// SEEKFS_CONTENT_SEARCH=1 the per-volume coordinator observes the same USN
// change stream the record index uses and marks files whose bytes changed for
// re-extraction.
//
// The mechanisms here are deliberately separate from the record engine so they
// can be unit-tested without a real volume: contentVolumeState (index + resolver
// + delta + health), contentCoordinator (dirty/promote/drain/invalidate), and the
// extraction-eligibility gate (directories, cloud placeholders, reparse points).
//
// The coordinator only consumes the USN stream once a drain is attached
// (enableDrain). Until the service's periodic drain loop lands, attaching is
// deliberately deferred so enabling the flag cannot grow an undrained map.

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"
)

// Content health states. "unavailable" means the feature is enabled but the
// live service wiring (per-volume index load, drain loop) has not run yet, so
// content queries are refused rather than reported as no matches.
const (
	contentStateOff         = "off"
	contentStateUnavailable = "unavailable"
	contentStateIndexing    = "indexing"
	contentStateReady       = "ready"
	contentStateStale       = "stale"
	contentStateDegraded    = "degraded"
)

// Windows file attributes the content pipeline cares about.
const (
	contentAttrDirectory          = 0x00000010
	contentAttrReparsePoint       = 0x00000400
	contentAttrOffline            = 0x00001000
	contentAttrRecallOnOpen       = 0x00040000
	contentAttrRecallOnDataAccess = 0x00400000
)

// usnReasonNeedsContentRefresh is the D3 mask: byte changes plus create. Close
// is a flush signal, not a trigger, and rename does not change content.
const usnReasonNeedsContentRefresh = usnReasonDataOverwrite | usnReasonDataExtend |
	usnReasonDataTruncation | usnReasonFileCreate

// contentEligibleForExtraction rejects directories and any file whose bytes may
// not be safely read locally: cloud placeholders (offline / recall-on-open /
// recall-on-data-access) and reparse points. attrs is the Windows attribute
// word; mode is the os.FileMode-style word when available (0 otherwise), where
// os.ModeDir is bit 31.
func contentEligibleForExtraction(mode, attrs uint32) bool {
	if mode&(uint32(1)<<31) != 0 {
		return false
	}
	if attrs&contentAttrDirectory != 0 {
		return false
	}
	if attrs&(contentAttrOffline|contentAttrRecallOnOpen|contentAttrRecallOnDataAccess|contentAttrReparsePoint) != 0 {
		return false
	}
	return true
}

// contentHealth is the content telemetry surfaced through the service response
// and `loaded --json`.
type contentHealth struct {
	State            string `json:"state,omitempty"`
	Docs             int    `json:"docs,omitempty"`
	Bytes            int    `json:"bytes,omitempty"`
	ExtractorVersion int    `json:"extractor_version,omitempty"`
	QueueDepth       int    `json:"queue_depth,omitempty"`
	LastRebuild      string `json:"last_rebuild,omitempty"`
	LastDrain        string `json:"last_drain,omitempty"`
	Evictions        int    `json:"evictions,omitempty"`
	ExtractionErrors int    `json:"extract_errors,omitempty"`
	// Partial is set when the content query answered from only a subset of the
	// eligible volumes; DegradedVolumes names the volumes that could not.
	Partial         bool     `json:"partial,omitempty"`
	DegradedVolumes []string `json:"degraded_volumes,omitempty"`
	// Incomplete is set when a content query's candidate set exceeded the
	// memory budget, so the answer is a visible subset, never a silent
	// truncation. A count in this state is refused instead of answered.
	Incomplete bool `json:"incomplete,omitempty"`
	// BuildDone/BuildTotal expose a service-owned background build's progress
	// while the volume is in the `indexing` state. BuildError records why the
	// last build failed and left the volume degraded (PF-3).
	BuildDone  int    `json:"build_done,omitempty"`
	BuildTotal int    `json:"build_total,omitempty"`
	BuildError string `json:"build_error,omitempty"`
}

// contentDeltaDoc is an extracted document for a file changed since the base,
// keyed on FRN. Its text is verified at query time; the base index is untouched.
type contentDeltaDoc struct {
	FRN     uint64
	Path    string
	Text    []byte
	LastUSN int64
	Deleted bool
	Hash    [contentHashLen]byte
}

// contentDelta is the in-memory overlay of changed documents. Its own mutex
// guards docs/byFRN so the coordinator and a drain can mutate it without a
// shared lock regime.
type contentDelta struct {
	mu    sync.Mutex
	docs  []contentDeltaDoc
	byFRN map[uint64]int
}

func newContentDelta() *contentDelta {
	return &contentDelta{byFRN: make(map[uint64]int)}
}

func (d *contentDelta) upsert(doc contentDeltaDoc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	doc.Deleted = false
	if i, ok := d.byFRN[doc.FRN]; ok {
		d.docs[i] = doc
		return
	}
	d.byFRN[doc.FRN] = len(d.docs)
	d.docs = append(d.docs, doc)
}

func (d *contentDelta) delete(frn uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if i, ok := d.byFRN[frn]; ok {
		d.docs[i].Deleted = true
		d.docs[i].Text = nil
	}
}

func (d *contentDelta) live() []contentDeltaDoc {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]contentDeltaDoc, 0, len(d.docs))
	for _, doc := range d.docs {
		if !doc.Deleted {
			out = append(out, doc)
		}
	}
	return out
}

// textFor returns a delta doc's normalized text keyed by FRN. found is true
// when the FRN is present at all; deleted marks a tombstoned doc whose text
// must not be served even though it is still in the map.
func (d *contentDelta) textFor(frn uint64) (text []byte, found, deleted bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	i, ok := d.byFRN[frn]
	if !ok {
		return nil, false, false
	}
	doc := d.docs[i]
	if doc.Deleted {
		return nil, true, true
	}
	return doc.Text, true, false
}

func (d *contentDelta) priorHash(frn uint64) ([contentHashLen]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if i, ok := d.byFRN[frn]; ok && !d.docs[i].Deleted {
		return d.docs[i].Hash, true
	}
	return [contentHashLen]byte{}, false
}

// has reports whether a live (non-deleted) delta doc exists for the FRN.
func (d *contentDelta) has(frn uint64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	i, ok := d.byFRN[frn]
	return ok && !d.docs[i].Deleted
}

func (d *contentDelta) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.docs)
}

// contentVolumeState holds one volume's decoded content index plus the overlay
// delta and the resolver that maps docs to record IDs.
type contentVolumeState struct {
	mu       sync.RWMutex
	volume   string
	idx      *contentIndex
	reader   *contentReader
	resolver *contentResolver
	delta    *contentDelta
	state    string
	health   contentHealth
}

func newContentVolumeState(volume string) *contentVolumeState {
	state := contentStateOff
	health := contentHealth{State: contentStateOff}
	if contentSearchEnabled() {
		// Enabled but not yet wired to a loaded index/drain: report that
		// honestly instead of implying content is live.
		state = contentStateUnavailable
		health.State = contentStateUnavailable
	}
	return &contentVolumeState{
		volume: volume,
		delta:  newContentDelta(),
		state:  state,
		health: health,
	}
}

// contentHealthSnapshot returns the content health of the first volume that has
// content state, or nil when content search is disabled. It is surfaced through
// `loaded --json` and the search response.
func (s *goSearchService) contentHealthSnapshot() *contentHealth {
	if s == nil || !contentSearchEnabled() {
		return nil
	}
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	for _, vol := range s.volumes {
		if vol == nil || vol.content == nil {
			continue
		}
		depth := 0
		if vol.contentCoord != nil {
			depth = vol.contentCoord.queued()
		}
		h := vol.content.healthSnapshot(depth)
		return &h
	}
	return nil
}

// searchContentHealth is contentHealthSnapshot augmented with the query's
// degraded (partial) state: a content query that had to skip an unusable volume
// must not look complete to the caller.
func (s *goSearchService) searchContentHealth(trace *searchTrace) *contentHealth {
	h := s.contentHealthSnapshot()
	if trace == nil || (!trace.ContentPartial && !trace.ContentIncomplete) {
		return h
	}
	if h == nil {
		h = &contentHealth{}
	}
	if trace.ContentPartial {
		h.Partial = true
		h.DegradedVolumes = append([]string(nil), trace.ContentSkippedVolumes...)
	}
	if trace.ContentIncomplete {
		h.Incomplete = true
	}
	h.State = contentStateDegraded
	return h
}

func (s *contentVolumeState) setReady(idx *contentIndex, reader *contentReader, resolver *contentResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idx = idx
	s.reader = reader
	s.resolver = resolver
	s.state = contentStateReady
	s.health.State = contentStateReady
	// A freshly attached base is complete as of its checkpoint; any earlier
	// catch-up truncation belonged to the replaced base.
	s.health.Incomplete = false
	s.health.Docs = len(idx.Docs)
	s.health.LastRebuild = time.Now().UTC().Format(time.RFC3339)
}

// rebuildResolver replaces the docID -> recordID map after a base swap. The
// content index and delta are untouched: content is generation-independent.
func (s *contentVolumeState) rebuildResolver(baseFRNs []uint64, baseIDs []uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx == nil {
		return
	}
	s.resolver = buildContentResolver(s.idx.Docs, baseFRNs, baseIDs)
}

// markStale is the journal-reset / index-rebuild invalidation: the decoded
// index and any delta are dropped and a full content rebuild is required.
func (s *contentVolumeState) markStale(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = contentStateStale
	s.health.State = contentStateStale
	s.reader = nil
	s.resolver = nil
	s.delta = newContentDelta()
}

// markIndexing is the service-owned background build's visible state. A
// partially built index is never served: any prior reader/resolver is dropped
// so a query during the build sees `indexing` (unavailable) rather than a
// torn or stale base. PF-3/WP1d.
func (s *contentVolumeState) markIndexing(total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = contentStateIndexing
	s.health.State = contentStateIndexing
	s.health.BuildTotal = total
	s.health.BuildDone = 0
	s.health.BuildError = ""
	s.health.Incomplete = false
	s.reader = nil
	s.resolver = nil
}

// setBuildProgress publishes extraction progress for an in-flight build so
// `loaded --json`/health can show it. A progress update for a build that has
// already left the indexing state is ignored, so a late tick cannot resurrect
// a finished or canceled build.
func (s *contentVolumeState) setBuildProgress(done, total int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != contentStateIndexing {
		return
	}
	s.health.BuildDone = done
	s.health.BuildTotal = total
}

// markBuildFailed leaves the volume visibly degraded (never ready with an
// unusable index) and records the reason. It clears any decoder so a query is
// refused rather than answered from a partial or stale base.
func (s *contentVolumeState) markBuildFailed(reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reader = nil
	s.resolver = nil
	s.state = contentStateDegraded
	s.health.State = contentStateDegraded
	s.health.Incomplete = true
	s.health.BuildError = reason
}

// markBuildCanceled returns an interrupted build to `unavailable` (not ready)
// so a shutdown or aborted build is never mistaken for a usable index.
func (s *contentVolumeState) markBuildCanceled(reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != contentStateIndexing {
		return
	}
	s.state = contentStateUnavailable
	s.health.State = contentStateUnavailable
	s.health.BuildError = reason
}

func (s *contentVolumeState) stateOf() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *contentVolumeState) readerView() *contentReader {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reader
}

// readerResolverView returns the reader and resolver under one lock so a
// concurrent base swap cannot hand back a reader and a resolver from different
// generations.
func (s *contentVolumeState) readerResolverView() (*contentReader, *contentResolver) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reader, s.resolver
}

// usableForQuery reports whether the volume can answer content semantics: it
// has a decoded base index joined to records, or at least a populated delta.
// A stale/unavailable volume with neither must be refused, never treated as
// "no matches". An `indexing` volume is refused even with a populated delta:
// markIndexing drops the reader/resolver but leaves the delta, so without this
// guard a content query could serve delta-only partial results and report
// complete while the fresh base is still being built.
func (s *contentVolumeState) usableForQuery() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	state := s.state
	reader, resolver, delta := s.reader, s.resolver, s.delta
	s.mu.RUnlock()
	if state == contentStateIndexing {
		return false
	}
	if reader != nil && resolver != nil {
		return true
	}
	return delta != nil && delta.len() > 0
}

func (s *contentVolumeState) deltaView() *contentDelta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.delta
}

func (s *contentVolumeState) healthSnapshot(queueDepth int) contentHealth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.health
	h.QueueDepth = queueDepth
	if h.State == "" {
		h.State = s.state
	}
	return h
}

// markDrained records a completed drain pass and returns the volume to ready.
// A truncated catch-up keeps the volume degraded: a later drain cannot make the
// records catch-up missed reappear, so health.Incomplete must stay visible.
func (s *contentVolumeState) markDrained() {
	s.mu.Lock()
	defer s.mu.Unlock()
	// stale (needs a rebuild) and indexing (a rebuild is in flight) are owned
	// by the build lifecycle: a drain tick must not flip an indexing volume to
	// ready with no usable index attached.
	if s.state != contentStateStale && s.state != contentStateIndexing {
		if s.health.Incomplete {
			s.state = contentStateDegraded
			s.health.State = contentStateDegraded
		} else {
			s.state = contentStateReady
			s.health.State = contentStateReady
		}
	}
	s.health.LastDrain = time.Now().UTC().Format(time.RFC3339)
}

func (s *contentVolumeState) noteExtractionError() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.health.ExtractionErrors++
}

// markCatchUpIncomplete flags that restart catch-up stopped before reaching
// the volume checkpoint, so the volume is surfaced as degraded/incomplete
// instead of silently treated as fully caught up. health.Incomplete survives
// markDrained, so a completed drain does not hide the truncation. The volume
// stays usable: the base plus the partial delta is still a superset of what it
// served before catch-up.
func (s *contentVolumeState) markCatchUpIncomplete() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.health.Incomplete = true
	if s.state != contentStateStale {
		s.state = contentStateDegraded
		s.health.State = contentStateDegraded
	}
}

// healthIncomplete reports whether a truncated restart catch-up left this
// volume missing records. The query path turns this into the trace-level
// incomplete signal so a query does not report complete while `loaded --json`
// says incomplete.
func (s *contentVolumeState) healthIncomplete() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.health.Incomplete
}

// hasContentForFRN reports whether a live content doc already exists for the
// FRN, in the base or the delta. A rename-new for an FRN with no doc is a file
// moved into the volume that has never been extracted; an intra-volume rename
// keeps its FRN, so it already has a doc and must not re-extract.
func (s *contentVolumeState) hasContentForFRN(frn uint64) bool {
	if s == nil || frn == 0 {
		return false
	}
	s.mu.RLock()
	resolver := s.resolver
	delta := s.delta
	s.mu.RUnlock()
	if resolver != nil {
		if _, ok := resolver.docForFRN(frn); ok {
			return true
		}
	}
	return delta != nil && delta.has(frn)
}

// contentCoordinator turns the USN change stream into content work. Dirty files
// are promoted for extraction on Close or a quiet window; read-only closes never
// produce work.
type contentCoordinator struct {
	mu           sync.Mutex
	state        *contentVolumeState
	dirty        map[uint64]struct{}
	queue        map[uint64]struct{}
	drainEnabled bool
}

func newContentCoordinator(state *contentVolumeState) *contentCoordinator {
	return &contentCoordinator{
		state: state,
		dirty: make(map[uint64]struct{}),
		queue: make(map[uint64]struct{}),
	}
}

// enableDrain arms USN consumption. Until a drain loop is attached, the
// coordinator ignores the stream so the flag cannot grow an undrained map.
func (c *contentCoordinator) enableDrain() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drainEnabled = true
}

// observeChanges records FRNs whose bytes changed. Directories, ineligible
// files, and non-content reasons are ignored. It is a no-op until enableDrain.
func (c *contentCoordinator) observeChanges(changes []usnChange) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.drainEnabled {
		return
	}
	for i := range changes {
		ch := &changes[i]
		if ch.FRN == 0 {
			continue
		}
		if !contentEligibleForExtraction(0, ch.Attr) {
			continue
		}
		if ch.Reason&usnReasonFileDelete != 0 {
			c.state.deltaView().delete(ch.FRN)
			delete(c.dirty, ch.FRN)
			delete(c.queue, ch.FRN)
			continue
		}
		if ch.Reason&usnReasonNeedsContentRefresh != 0 {
			if _, queued := c.queue[ch.FRN]; !queued {
				c.dirty[ch.FRN] = struct{}{}
			}
		}
		// A file moved into the volume while the service was stopped has no
		// content doc for its (stable) FRN, so enqueue it and let the drain
		// extract it. An intra-volume rename already has a doc, so it is
		// skipped here and never re-extracted.
		if ch.Reason&usnReasonRenameNew != 0 && !c.state.hasContentForFRN(ch.FRN) {
			if _, queued := c.queue[ch.FRN]; !queued {
				c.dirty[ch.FRN] = struct{}{}
			}
		}
		// A close that coincides with a write in the same record promotes the
		// file immediately. A close alone was never marked dirty, so a
		// read-only open still produces no work.
		if ch.Reason&usnReasonClose != 0 {
			if _, ok := c.dirty[ch.FRN]; ok {
				delete(c.dirty, ch.FRN)
				c.queue[ch.FRN] = struct{}{}
			}
		}
	}
}

// closeFRN promotes an already-dirty file to the extraction queue.
func (c *contentCoordinator) closeFRN(frn uint64) {
	if c == nil || frn == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.dirty[frn]; !ok {
		return
	}
	delete(c.dirty, frn)
	c.queue[frn] = struct{}{}
}

// promoteAll is the quiet-window flush: every dirty FRN becomes queued work.
func (c *contentCoordinator) promoteAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for frn := range c.dirty {
		delete(c.dirty, frn)
		c.queue[frn] = struct{}{}
	}
}

// invalidate is the D4 recovery path: drop all pending content work and mark the
// volume stale so a full rebuild is scheduled.
func (c *contentCoordinator) invalidate(reason string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.dirty = make(map[uint64]struct{})
	c.queue = make(map[uint64]struct{})
	c.mu.Unlock()
	c.state.markStale(reason)
}

// takeQueued removes and returns the queued FRNs.
func (c *contentCoordinator) takeQueued() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]uint64, 0, len(c.queue))
	for frn := range c.queue {
		out = append(out, frn)
	}
	c.queue = make(map[uint64]struct{})
	return out
}

func (c *contentCoordinator) drainEnabledNow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.drainEnabled
}

func (c *contentCoordinator) queued() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue)
}

func (c *contentCoordinator) dirtyCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.dirty)
}

// contentExtractDeltaDoc extracts a changed file into a delta document. When the
// file's content hash matches prior, changed is false and the caller can skip it
// (touch-only writes never reindex).
func contentExtractDeltaDoc(frn uint64, path string, prior [contentHashLen]byte, havePrior bool) (doc contentDeltaDoc, changed bool, err error) {
	f, err := contentOpenNoRecall(path)
	if err != nil {
		return contentDeltaDoc{}, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return contentDeltaDoc{}, false, err
	}
	size := info.Size()
	head, _ := contentReadBounded(f, size, 512)
	e := contentExtractorForPath(path, head)
	if e == nil {
		return contentDeltaDoc{}, false, nil
	}
	res, err := e.Extract(context.Background(), f, size)
	if err != nil || res.Skipped || len(res.Text) == 0 {
		return contentDeltaDoc{}, false, err
	}
	stored := contentNormalizeText(res.Text)
	h := sha256Of(stored)
	if havePrior && h == prior {
		return contentDeltaDoc{}, false, nil
	}
	return contentDeltaDoc{FRN: frn, Path: path, Text: stored, Hash: h}, true, nil
}

// sha256Of is the shared content hash: sha256 truncated to contentHashLen bytes
// over the normalized stored text.
func sha256Of(text []byte) [contentHashLen]byte {
	sum := sha256.Sum256(text)
	var h [contentHashLen]byte
	copy(h[:], sum[:contentHashLen])
	return h
}

// processQueue resolves and extracts every queued FRN, upserting the delta and
// deleting docs for files that no longer resolve. It returns the number of
// documents added or changed. resolvePath maps an FRN to its current path.
func (c *contentCoordinator) processQueue(resolvePath func(frn uint64) (string, bool)) int {
	if c == nil {
		return 0
	}
	frns := c.takeQueued()
	changed := 0
	for _, frn := range frns {
		path, ok := resolvePath(frn)
		if !ok {
			c.state.deltaView().delete(frn)
			continue
		}
		prior, havePrior := c.state.deltaView().priorHash(frn)
		doc, didChange, err := contentExtractDeltaDoc(frn, path, prior, havePrior)
		if err != nil {
			c.state.noteExtractionError()
			continue
		}
		if !didChange {
			continue
		}
		c.state.deltaView().upsert(doc)
		changed++
	}
	c.state.markDrained()
	return changed
}
