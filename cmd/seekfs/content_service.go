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
	"fmt"
	"strings"
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

// contentChangeExcluded reports whether a USN change names a search-index file
// that content extraction must never process: the `.gsx` content sidecar (and
// its `.gsx.<rand>.tmp` atomic-write temp) and the `.gsi`/`.gsi.wal` record
// index (and its temp). These live on the indexed volume, so the service's own
// writes are re-observed as changes; charging their size to catch-up's byte
// budget wedges it in a rebuild loop (and extraction would skip them anyway —
// no registered extractor). Substring match, so temp names are covered; mirrors
// the walk builder's `.gsx` / `.seekfs` skip.
func contentChangeExcluded(name string) bool {
	n := strings.ToLower(name)
	if strings.HasPrefix(n, ".seekfs") {
		return true
	}
	return strings.Contains(n, ".gsx") || strings.Contains(n, ".gsi")
}

// contentHealth is the content telemetry surfaced through the service response
// and `loaded --json`.
type contentHealth struct {
	State string `json:"state,omitempty"`
	Docs  int    `json:"docs,omitempty"`
	// DeltaBytes is the resident delta's normalized-text size. It grows while a
	// fold cannot publish, so a delta that is accumulating (M1) is observable
	// instead of only surfacing as memory pressure.
	DeltaBytes int64 `json:"delta_bytes,omitempty"`
	// Deferred counts distinct changes parked at the delta's hard ceiling
	// (memory-bounded, replayed once the delta drains). Non-zero means the
	// volume is serving content that is missing those changes until replay.
	Deferred         int    `json:"deferred_docs,omitempty"`
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
	// CountDivergent is set on a content count whose shape (under:/Exists)
	// would stat on search but not on count, so the count can exceed the search
	// result set. It is a visibility flag only: the count is still returned.
	CountDivergent bool `json:"count_divergent,omitempty"`
	// BuildDone/BuildTotal expose a service-owned background build's progress
	// while the volume is in the `indexing` state. BuildError records why the
	// last build failed and left the volume degraded (PF-3).
	BuildDone  int    `json:"build_done,omitempty"`
	BuildTotal int    `json:"build_total,omitempty"`
	BuildError string `json:"build_error,omitempty"`
	// FoldError records why the last delta fold failed. The delta is retained
	// and search stays correct, but a fold that keeps failing accumulates the
	// delta; surfacing the reason makes that visible instead of silent.
	FoldError string `json:"fold_error,omitempty"`
	// MaxRaw/MaxText are the attached base's extraction policy caps and
	// Skipped/Truncated count what the policy excluded or cut at build time
	// (PB7). They come from the `.gsx` CXPL section so a size cap is visible,
	// never a silent drop.
	MaxRaw    int64 `json:"max_raw,omitempty"`
	MaxText   int64 `json:"max_text,omitempty"`
	Skipped   int   `json:"skipped,omitempty"`
	Truncated int   `json:"truncated,omitempty"`
	// SidecarBytes/SidecarCap report the attached `.gsx` encoded size and the
	// size cap (WP10/M7), so unbounded sidecar growth is observable in
	// `loaded --json` instead of only failing silently.
	SidecarBytes int64 `json:"sidecar_bytes,omitempty"`
	SidecarCap   int64 `json:"sidecar_cap,omitempty"`
	// ExtractorRefresh/StaleExtractorDocs report a targeted per-doc extractor
	// version refresh (WP11): the attached base still holds StaleExtractorDocs
	// docs produced by an older extractor Version(), so the volume is usable
	// (the old text is a valid, older extraction) but surfaced degraded until a
	// fold publishes a base re-extracted at the current versions.
	ExtractorRefresh   bool `json:"extractor_refresh,omitempty"`
	StaleExtractorDocs int  `json:"stale_extractor_docs,omitempty"`
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
	// ContentType/ExtractorVersion mirror the base doc fields so a fold carries
	// the same extractor identity the base would have (P1).
	ContentType      uint16
	ExtractorVersion uint16
}

// contentDelta is the in-memory overlay of changed documents. Its own mutex
// guards docs/byFRN so the coordinator and a drain can mutate it without a
// shared lock regime. liveDocCount/liveTextBytes track the resident live
// payload so the M1 fold trigger is O(1) rather than a scan.
type contentDelta struct {
	mu            sync.Mutex
	docs          []contentDeltaDoc
	byFRN         map[uint64]int
	liveDocCount  int
	liveTextBytes int64
}

func newContentDelta() *contentDelta {
	return &contentDelta{byFRN: make(map[uint64]int)}
}

func (d *contentDelta) upsert(doc contentDeltaDoc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	doc.Deleted = false
	if i, ok := d.byFRN[doc.FRN]; ok {
		old := &d.docs[i]
		if old.Deleted {
			d.liveDocCount++
			d.liveTextBytes += int64(len(doc.Text))
		} else {
			d.liveTextBytes += int64(len(doc.Text)) - int64(len(old.Text))
		}
		d.docs[i] = doc
		return
	}
	d.byFRN[doc.FRN] = len(d.docs)
	d.docs = append(d.docs, doc)
	d.liveDocCount++
	d.liveTextBytes += int64(len(doc.Text))
}

// delete tombstones an FRN. A base-indexed file deleted without a prior delta
// entry has no slot, so one is created: without a tombstone the fold would keep
// re-encoding the deleted base doc forever (M7/WP10). The tombstone is dropped
// by the next fold; foldDue counts total entries, so a burst of deletes still
// triggers a fold instead of growing the delta slice without bound.
func (d *contentDelta) delete(frn uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if i, ok := d.byFRN[frn]; ok {
		if !d.docs[i].Deleted {
			d.liveDocCount--
			d.liveTextBytes -= int64(len(d.docs[i].Text))
		}
		d.docs[i].Deleted = true
		d.docs[i].Text = nil
		return
	}
	d.byFRN[frn] = len(d.docs)
	d.docs = append(d.docs, contentDeltaDoc{FRN: frn, Deleted: true})
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

// contains reports whether the FRN has a delta slot at all (live or tombstoned).
// Used by the hard-bound admission gate: updating an existing slot does not grow
// the resident entry count, admitting a new FRN does.
func (d *contentDelta) contains(frn uint64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.byFRN[frn]
	return ok
}

// atHardBound reports whether the resident delta has reached the absolute
// ceiling. It is consulted only while the sidecar is capped, when the normal
// fold that would drain the delta cannot publish.
func (d *contentDelta) atHardBound(maxDocs int, maxBytes int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.docs) >= maxDocs || d.liveTextBytes >= maxBytes
}

func (d *contentDelta) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.docs)
}

// liveCount returns the number of non-tombstoned entries (the resident content
// payload the fold trigger cares about).
func (d *contentDelta) liveCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.liveDocCount
}

// liveBytes returns the resident live normalized-text byte count.
func (d *contentDelta) liveBytes() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.liveTextBytes
}

// snapshot copies every entry (live and tombstoned) under one lock, so a fold
// can build from a consistent view and later remove exactly what it folded.
func (d *contentDelta) snapshot() []contentDeltaDoc {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]contentDeltaDoc(nil), d.docs...)
}

// removeUnchanged drops the entries that are identical (FRN, path, content
// hash, and deleted state) to the fold snapshot, then compacts the backing
// slice so tombstoned slots do not accumulate. An entry mutated after the
// snapshot (an edit, a delete, a rename, or a resurrect) differs and is left for
// the next fold, so a change racing the fold is never lost.
func (d *contentDelta) removeUnchanged(snapshot []contentDeltaDoc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(snapshot) == 0 {
		return
	}
	folded := make(map[uint64]contentDeltaDoc, len(snapshot))
	for _, s := range snapshot {
		folded[s.FRN] = s
	}
	kept := d.docs[:0]
	d.byFRN = make(map[uint64]int, len(d.docs))
	d.liveDocCount = 0
	d.liveTextBytes = 0
	for _, cur := range d.docs {
		if s, ok := folded[cur.FRN]; ok && s.Deleted == cur.Deleted && s.Hash == cur.Hash && s.Path == cur.Path {
			continue
		}
		d.byFRN[cur.FRN] = len(kept)
		kept = append(kept, cur)
		if !cur.Deleted {
			d.liveDocCount++
			d.liveTextBytes += int64(len(cur.Text))
		}
	}
	d.docs = kept
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
	// catchUpPending is true from a base attach until restart catch-up reaches
	// the volume checkpoint. observedUSN spans the live replay and the catch-up
	// streams, so while catch-up is pending the watermark can run ahead of the
	// delta's coverage; a fold must not publish then. A capped or aborted
	// catch-up never clears it, so the volume stays gated and degraded until a
	// rebuild.
	catchUpPending bool
	// catchUpFailed is the fold gate for a capped/aborted restart catch-up whose
	// skipped records may lie below observedUSN. It is distinct from
	// health.Incomplete, which is also raised by a delta parked at its hard
	// ceiling (markDeltaCapped): that deferral is safe to fold because the
	// checkpoint is held below the parked changes, so it must not gate folds.
	// setReady clears it for a freshly attached base.
	catchUpFailed bool
	// sidecarCapped is set when the encoded `.gsx` exceeds contentGSXMaxBytes
	// (WP10/M7). The attached base and the delta are left in place, so content
	// stays correct and usable; the flag only keeps the volume surfaced as
	// degraded across drain ticks until a base that fits is published.
	sidecarCapped bool
	// refreshActive is true while a targeted extractor-version refresh is in
	// flight for this volume (set by attach, cleared once a fold publishes a
	// base with no stale docs or a rebuild/stale transition drops the base).
	// refreshPending is the number of docs in the attached base still stamped
	// with an older extractor Version(); the count (and the degraded signal)
	// clears when a fold republishes the base at the current versions.
	refreshActive  bool
	refreshPending int
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

// contentStateSeverity orders content health states from best to worst so a
// multi-volume snapshot can report the worst one: ready and off (content
// disabled for the volume) are benign, then unavailable (enabled but not wired),
// a build in flight, a degraded volume, and finally stale (needs a rebuild).
func contentStateSeverity(state string) int {
	switch state {
	case contentStateStale:
		return 4
	case contentStateDegraded:
		return 3
	case contentStateIndexing:
		return 2
	case contentStateUnavailable:
		return 1
	default: // ready, off, ""
		return 0
	}
}

// mergeContentHealth folds one volume's health into the aggregate using the
// worst-case rules documented on contentHealthSnapshot. BuildDone/BuildTotal are
// handled by the caller (only indexing volumes contribute).
func mergeContentHealth(dst *contentHealth, src contentHealth) {
	if contentStateSeverity(src.State) > contentStateSeverity(dst.State) {
		dst.State = src.State
	}
	dst.Docs += src.Docs
	dst.DeltaBytes += src.DeltaBytes
	dst.Deferred += src.Deferred
	dst.QueueDepth += src.QueueDepth
	dst.Evictions += src.Evictions
	dst.ExtractionErrors += src.ExtractionErrors
	dst.Skipped += src.Skipped
	dst.Truncated += src.Truncated
	dst.SidecarBytes += src.SidecarBytes
	dst.SidecarCap += src.SidecarCap
	// Extractor versions should agree across volumes; keep the highest so a
	// version-skewed host is visible and the field never goes backwards.
	if src.ExtractorVersion > dst.ExtractorVersion {
		dst.ExtractorVersion = src.ExtractorVersion
	}
	dst.Partial = dst.Partial || src.Partial
	dst.Incomplete = dst.Incomplete || src.Incomplete
	dst.CountDivergent = dst.CountDivergent || src.CountDivergent
	dst.ExtractorRefresh = dst.ExtractorRefresh || src.ExtractorRefresh
	dst.StaleExtractorDocs += src.StaleExtractorDocs
	for _, v := range src.DegradedVolumes {
		if !containsVolumeName(dst.DegradedVolumes, v) {
			dst.DegradedVolumes = append(dst.DegradedVolumes, v)
		}
	}
	if src.BuildError != "" && !strings.Contains(dst.BuildError, src.BuildError) {
		if dst.BuildError == "" {
			dst.BuildError = src.BuildError
		} else {
			dst.BuildError += "; " + src.BuildError
		}
	}
	if src.FoldError != "" && !strings.Contains(dst.FoldError, src.FoldError) {
		if dst.FoldError == "" {
			dst.FoldError = src.FoldError
		} else {
			dst.FoldError += "; " + src.FoldError
		}
	}
	dst.MaxRaw = minContentCap(dst.MaxRaw, src.MaxRaw)
	dst.MaxText = minContentCap(dst.MaxText, src.MaxText)
	dst.LastRebuild = oldestContentTimestamp(dst.LastRebuild, src.LastRebuild)
	dst.LastDrain = newestContentTimestamp(dst.LastDrain, src.LastDrain)
}

func containsVolumeName(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// minContentCap returns the smaller non-zero cap: the effective extraction cap
// when one volume allows a larger file than another. Zero means "no cap".
func minContentCap(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case b < a:
		return b
	default:
		return a
	}
}

// oldestContentTimestamp compares RFC3339 UTC timestamps lexicographically (the
// fixed-width format sorts chronologically). Empty means unset.
func oldestContentTimestamp(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	case b < a:
		return b
	default:
		return a
	}
}

func newestContentTimestamp(a, b string) string {
	if b > a {
		return b
	}
	return a
}

// aggregateContentHealth merges per-volume health snapshots using the rules
// documented on contentHealthSnapshot.
func aggregateContentHealth(healths []contentHealth) contentHealth {
	out := healths[0]
	// Build progress is only meaningful while a volume is indexing; a ready
	// volume can carry a stale BuildTotal from its last build, so only indexing
	// volumes contribute.
	out.BuildDone, out.BuildTotal = 0, 0
	for i, h := range healths {
		if h.State == contentStateIndexing {
			out.BuildDone += h.BuildDone
			out.BuildTotal += h.BuildTotal
		}
		if i > 0 {
			mergeContentHealth(&out, h)
		}
	}
	return out
}

// contentHealthSnapshot aggregates content health across every content-enabled
// volume, or returns nil when content search is disabled or no volume carries
// content state. Exactly one content volume is returned verbatim, so the
// single-volume shape is unchanged; with more than one the snapshots merge
// worst-case:
//
//   - State is the most severe state: ready/off < unavailable < indexing <
//     degraded < stale (contentStateSeverity).
//   - Docs, DeltaBytes, Deferred, QueueDepth, Evictions, ExtractionErrors,
//     Skipped, Truncated, SidecarBytes and SidecarCap are summed;
//     BuildDone/BuildTotal sum only while indexing.
//   - Partial, Incomplete and CountDivergent are OR-ed.
//   - ExtractorRefresh is OR-ed and StaleExtractorDocs summed, so a targeted
//     extractor-version refresh in progress on any volume is visible.
//   - DegradedVolumes is the deduped stable union.
//   - ExtractorVersion takes the highest.
//   - BuildError and FoldError each join the distinct non-empty reasons with "; ".
//   - MaxRaw/MaxText keep the smallest non-zero cap (the effective cap).
//   - LastRebuild takes the oldest, so "stale since" stays visible; LastDrain
//     takes the newest.
func (s *goSearchService) contentHealthSnapshot() *contentHealth {
	if s == nil || !contentSearchEnabled() {
		return nil
	}
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	var healths []contentHealth
	for _, vol := range s.volumes {
		if vol == nil || vol.content == nil {
			continue
		}
		depth := 0
		if vol.contentCoord != nil {
			depth = vol.contentCoord.queued()
		}
		healths = append(healths, vol.content.healthSnapshot(depth))
	}
	switch len(healths) {
	case 0:
		return nil
	case 1:
		h := healths[0]
		return &h
	default:
		h := aggregateContentHealth(healths)
		return &h
	}
}

// searchContentHealth is contentHealthSnapshot augmented with the query's
// degraded (partial) state: a content query that had to skip an unusable volume
// must not look complete to the caller. It also carries the count/search
// divergence flag, which is informational and does not by itself mark the
// volume degraded.
func (s *goSearchService) searchContentHealth(trace *searchTrace) *contentHealth {
	h := s.contentHealthSnapshot()
	if trace == nil || (!trace.ContentPartial && !trace.ContentIncomplete && !trace.ContentCountDivergent) {
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
	if trace.ContentCountDivergent {
		h.CountDivergent = true
	}
	if trace.ContentPartial || trace.ContentIncomplete {
		h.State = contentStateDegraded
	}
	return h
}

// setReady installs a freshly attached base and returns the index it replaced
// (nil when none was attached), so the caller — which holds indexMu for writing
// — can release the old base's mapping once no query can still read it. Content
// is generation-independent, but the old base's Sections may alias a mapping, so
// the replacement is the point that owns the unmap.
func (s *contentVolumeState) setReady(idx *contentIndex, reader *contentReader, resolver *contentResolver) *contentIndex {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.idx
	s.idx = idx
	s.reader = reader
	s.resolver = resolver
	s.state = contentStateReady
	s.health.State = contentStateReady
	// A freshly attached base is complete as of its checkpoint; any earlier
	// catch-up truncation belonged to the replaced base.
	s.health.Incomplete = false
	if idx.Policy.ScopeDropped > 0 {
		s.health.Incomplete = true
		s.state = contentStateDegraded
		s.health.State = contentStateDegraded
	}
	s.catchUpFailed = false
	s.health.Docs = len(idx.Docs)
	s.health.SidecarBytes = idx.EncodedSize
	s.health.SidecarCap = contentGSXMaxBytes
	// Surface the highest attached extractor version (P1), so `loaded --json`
	// shows which extractor generation produced the base.
	var version uint16
	for i := range idx.Docs {
		if idx.Docs[i].ExtractorVersion > version {
			version = idx.Docs[i].ExtractorVersion
		}
	}
	s.health.ExtractorVersion = int(version)
	// A base that attached is not over cap and has no build fault.
	s.sidecarCapped = false
	s.health.BuildError = ""
	s.health.FoldError = ""
	if idx.Policy != (contentBuildPolicy{}) {
		s.health.MaxRaw = idx.Policy.MaxRaw
		s.health.MaxText = idx.Policy.MaxText
		s.health.Skipped = int(idx.Policy.Skipped)
		s.health.Truncated = int(idx.Policy.Truncated)
	}
	// A targeted extractor-version refresh (WP11) keeps the volume usable: its
	// text is a valid, if older, extraction. The refresh signal is only raised
	// by markExtractorRefresh (attach), and recomputed here as folds publish
	// refreshed bases, so an arbitrary base handed to setReady is not degraded.
	if s.refreshActive {
		s.refreshPending = contentStaleExtractorDocCount(idx)
		s.refreshActive = s.refreshPending > 0
		s.health.ExtractorRefresh = s.refreshPending > 0
		s.health.StaleExtractorDocs = s.refreshPending
		if s.refreshPending > 0 {
			s.health.State = contentStateDegraded
		}
	}
	s.health.LastRebuild = time.Now().UTC().Format(time.RFC3339)
	return old
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
	s.catchUpPending = false
	s.catchUpFailed = false
	s.refreshActive = false
	s.refreshPending = 0
	s.health.ExtractorRefresh = false
	s.health.StaleExtractorDocs = 0
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
	s.catchUpPending = false
	s.catchUpFailed = false
	s.refreshActive = false
	s.refreshPending = 0
	s.health.ExtractorRefresh = false
	s.health.StaleExtractorDocs = 0
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
	s.refreshActive = false
	s.refreshPending = 0
	s.health.ExtractorRefresh = false
	s.health.StaleExtractorDocs = 0
}

// markSidecarCapped surfaces an index whose encoded size exceeds
// contentGSXMaxBytes (WP10/M7). A fold keeps the previous base and its decoder,
// so content stays correct and usable and the volume's state stays ready (a
// later fold that fits clears it); only the health state is degraded. A build
// has no publishable base, so its decoder is dropped and the state becomes
// degraded exactly like a build failure.
func (s *contentVolumeState) markSidecarCapped(size, cap int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sidecarCapped = true
	s.health.SidecarBytes = size
	s.health.SidecarCap = cap
	s.health.BuildError = fmt.Sprintf("content index exceeds size cap (%d bytes > %d)", size, cap)
	s.health.State = contentStateDegraded
	if s.state == contentStateIndexing {
		s.reader = nil
		s.resolver = nil
		s.state = contentStateDegraded
	}
}

// markDeltaCapped flags that the delta reached its hard ceiling, so a new
// distinct document was not admitted. It fires whether the ceiling is reached
// because the sidecar is over the size cap (no fold can publish) or because a
// fold is failing for another reason; either way the resident delta must not
// grow without bound. The volume is surfaced incomplete/degraded (base+delta are
// still served by search) rather than the delta growing.
//
// A deferred change is not lost: it is replayed by FRN once the delta drains
// (contentCoordinator.replayDeferred), and while any remain the fold keeps the
// persisted checkpoint below them (runContentFold), so a restart replays their
// USNs too. No rebuild is scheduled here (P6-2, documented limitation): a
// rebuild re-extracts the same live corpus and a failed attempt would drop the
// still-usable base. The operator action for the over-cap case is to lower the
// indexed content or raise contentGSXMaxBytes, then restart; health surfaces
// sidecar_bytes/sidecar_cap, delta_bytes, deferred_docs and this reason.
func (s *contentVolumeState) markDeltaCapped() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.health.Incomplete = true
	s.health.BuildError = "content delta reached its hard bound; changes are deferred and replay once the delta drains (if the sidecar is over the size cap, lower the indexed content or raise contentGSXMaxBytes, then restart)"
	if s.state != contentStateStale && s.state != contentStateIndexing {
		s.state = contentStateDegraded
		s.health.State = contentStateDegraded
	}
}

// sidecarCappedNow reports whether the attached base's fold is currently
// refused for exceeding contentGSXMaxBytes (WP10/M7).
func (s *contentVolumeState) sidecarCappedNow() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sidecarCapped
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

// setDeltaBytes records the resident delta size for health (M1).
func (s *contentVolumeState) setDeltaBytes(n int64) {
	s.mu.Lock()
	s.health.DeltaBytes = n
	s.mu.Unlock()
}

// setDeferred records how many changes are parked at the delta's hard ceiling.
func (s *contentVolumeState) setDeferred(n int) {
	s.mu.Lock()
	s.health.Deferred = n
	s.mu.Unlock()
}

// setFoldError records why the last delta fold failed (empty clears it). A
// successful fold, or a base publish, clears it.
func (s *contentVolumeState) setFoldError(msg string) {
	s.mu.Lock()
	s.health.FoldError = msg
	s.mu.Unlock()
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
		switch {
		case s.refreshPending > 0:
			// A targeted extractor-version refresh is still unfolding: the old
			// (but valid) text is served, and health stays degraded until a fold
			// publishes the refreshed base.
			s.health.State = contentStateDegraded
		case s.health.Incomplete:
			s.state = contentStateDegraded
			s.health.State = contentStateDegraded
		case s.sidecarCapped:
			// The base is still attached and usable; keep the volume's state
			// (ready after a fold over cap, so a later fold can retry) and only
			// report the health state as degraded.
			s.health.State = contentStateDegraded
		default:
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
	s.catchUpFailed = true
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

// markCatchUpPending records that restart catch-up for the just-attached base
// has not finished. observedUSN spans the live replay and catch-up streams, so
// a fold must not publish a checkpoint until catch-up completes.
func (s *contentVolumeState) markCatchUpPending() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.catchUpPending = true
}

// markCatchUpComplete clears the catch-up gate after catch-up replayed every
// record up to the volume checkpoint. A capped or failed catch-up never calls
// this, so health.Incomplete keeps the volume degraded and the fold gated.
func (s *contentVolumeState) markCatchUpComplete() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.catchUpPending = false
	s.catchUpFailed = false
}

// catchUpPendingNow reports whether restart catch-up for the attached base has
// not yet finished. It is used to verify that a successful self-heal clears the
// gate (a capped/failed catch-up clears it only when a rebuild attaches and its
// catch-up completes).
func (s *contentVolumeState) catchUpPendingNow() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.catchUpPending
}

// markExtractorRefresh records that attach has enqueued n stale base docs for a
// targeted re-extraction at the current extractor versions. The volume stays
// usable and its state ready, but health is surfaced degraded until a fold
// publishes a base with no stale docs (setReady recomputes it).
func (s *contentVolumeState) markExtractorRefresh(n int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshActive = n > 0
	s.refreshPending = n
	s.health.ExtractorRefresh = n > 0
	s.health.StaleExtractorDocs = n
	if n > 0 && s.state == contentStateReady {
		s.health.State = contentStateDegraded
	}
}

// refreshPendingNow reports whether the attached base still holds docs produced
// by an older extractor Version() that a targeted refresh has not yet folded
// back. The drain uses it to force a fold once the refresh extraction is done,
// even when the delta is below its normal size trigger.
func (s *contentVolumeState) refreshPendingNow() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.refreshPending > 0
}

// catchUpIncomplete reports whether a fold must wait: catch-up is still in
// progress, or it was capped/failed and may have skipped records. A delta
// parked at its hard ceiling (health.Incomplete via markDeltaCapped) is not
// this case — its checkpoint is held below the parked changes — so it does not
// gate folds.
func (s *contentVolumeState) catchUpIncomplete() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.catchUpPending || s.catchUpFailed
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
	extractCtx context.Context
	mu         sync.Mutex
	state      *contentVolumeState
	dirty      map[uint64]struct{}
	queue      map[uint64]struct{}
	// deferred parks distinct changes that could not be admitted because the
	// delta is at its hard ceiling. Keyed by FRN; replayed once the delta
	// drains. Bounded in memory (an FRN per change, not its text).
	deferred     map[uint64]struct{}
	drainEnabled bool
	// observedUSN is the highest change USN handed to observeChanges. It is a
	// contiguous persistence bound only when no observed-but-unextracted work
	// remains and catch-up is complete, which foldSnapshot enforces before a
	// fold may durably claim it (PB2/WP1c).
	observedUSN uint64
	// foldRetryAfter paces fold retries after a failure; the delta is kept.
	foldRetryAfter time.Time
	// done is closed by stopDrain to retire this coordinator's drain loop when
	// its volume is replaced or removed, without touching s.stop. It is created
	// with the coordinator and closed at most once.
	done     chan struct{}
	doneOnce sync.Once
}

func newContentCoordinator(state *contentVolumeState) *contentCoordinator {
	return &contentCoordinator{
		state:    state,
		dirty:    make(map[uint64]struct{}),
		queue:    make(map[uint64]struct{}),
		deferred: make(map[uint64]struct{}),
		done:     make(chan struct{}),
	}
}

// stopDrain retires the coordinator's drain loop by closing its per-volume done
// channel and clearing drainEnabled. It is TERMINAL for this coordinator: done
// stays closed, so a re-attach of the same coordinator would not re-arm (a new
// loop would return immediately on <-done). Re-attaching must build a fresh
// coordinator. It is idempotent and independent of s.stop: shutdown still stops
// every loop, while a volume swap stops only the drain it replaces.
func (c *contentCoordinator) stopDrain() {
	if c == nil {
		return
	}
	c.doneOnce.Do(func() {
		close(c.done)
		c.mu.Lock()
		c.drainEnabled = false
		c.mu.Unlock()
	})
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
		// The watermark advances for every observed record (even an ineligible
		// one, which carries no content). It is only a fold checkpoint once the
		// fold gate confirms no observed-but-unextracted work remains and
		// catch-up is complete; see foldSnapshot.
		if ch.USN > 0 && uint64(ch.USN) > c.observedUSN {
			c.observedUSN = uint64(ch.USN)
		}
		if ch.FRN == 0 {
			continue
		}
		if contentChangeExcluded(ch.Name) {
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

// enqueueFRNs queues FRNs for extraction without a USN change. It is the
// targeted extractor-version refresh path: attach enqueues a base's stale FRNs
// so the drain re-extracts them with the current extractor. A duplicate (also
// observed through the USN stream) collapses in the set.
func (c *contentCoordinator) enqueueFRNs(frns []uint64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, frn := range frns {
		if c.extractCtx != nil && c.extractCtx.Err() != nil {
			break
		}
		if frn == 0 {
			continue
		}
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

// deferChange parks a distinct change that could not be admitted because the
// delta is at its hard ceiling. It is replayed by FRN once the delta drains, so
// it is delayed, never lost; while any remain, a fold keeps the persisted
// checkpoint below them (runContentFold), so a restart replays them too.
func (c *contentCoordinator) deferChange(frn uint64) {
	c.mu.Lock()
	if c.deferred == nil {
		c.deferred = make(map[uint64]struct{})
	}
	c.deferred[frn] = struct{}{}
	c.mu.Unlock()
}

// replayDeferred moves every parked change back onto the extraction queue for
// another attempt. The caller invokes it only when the delta has room, so the
// replayed changes are admitted rather than immediately re-deferred.
func (c *contentCoordinator) replayDeferred() {
	c.mu.Lock()
	if len(c.deferred) == 0 {
		c.mu.Unlock()
		return
	}
	for frn := range c.deferred {
		c.queue[frn] = struct{}{}
	}
	c.deferred = make(map[uint64]struct{})
	c.mu.Unlock()
}

// deferredCount returns the number of changes parked at the hard ceiling.
func (c *contentCoordinator) deferredCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.deferred)
}

// hasDeferred reports whether any change is parked at the hard ceiling; a fold
// must then stop advancing the persisted checkpoint past those changes.
func (c *contentCoordinator) hasDeferred() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.deferred) > 0
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

// foldSnapshot captures the observed watermark and a copy of every delta entry
// under one coordinator lock, so a fold's checkpoint and its input stay
// consistent even while observeChanges keeps running. It refuses (ok=false)
// while observedUSN is not yet a contiguous persistence bound: pending content
// work (dirty/queue) has been observed but not extracted, and restart catch-up
// may still be replaying records below the watermark. In both cases the
// checkpoint would run ahead of the folded content and the next restart would
// skip those USNs. An ineligible record (directory, non-content extension)
// never enters dirty/queue and needs no content representation, so it does not
// block the fold.
func (c *contentCoordinator) foldSnapshot() (uint64, []contentDeltaDoc, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.dirty) > 0 || len(c.queue) > 0 {
		return 0, nil, false
	}
	if c.state.catchUpIncomplete() {
		return 0, nil, false
	}
	return c.observedUSN, c.state.deltaView().snapshot(), true
}

// commitFold drops the delta entries the fold persisted unchanged. Entries
// mutated or added after the snapshot are kept.
func (c *contentCoordinator) commitFold(snapshot []contentDeltaDoc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.deltaView().removeUnchanged(snapshot)
}

// foldDue reports whether the live delta has grown past either bound and is not
// in a retry backoff. A pending targeted extractor refresh also forces a fold
// once the drain has emptied the queue: the refreshed docs must be persisted so
// the refreshed base replaces the stale one and the degraded signal clears, even
// when a small stale set never reaches the delta size trigger.
func (c *contentCoordinator) foldDue(maxDocs int, maxBytes int64, refreshPending bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.drainEnabled {
		return false
	}
	if !c.foldRetryAfter.IsZero() && time.Now().Before(c.foldRetryAfter) {
		return false
	}
	if refreshPending && len(c.dirty) == 0 && len(c.queue) == 0 {
		return true
	}
	delta := c.state.deltaView()
	// Count every entry, not just live ones: a burst of deletes of base-indexed
	// files adds tombstones with no resident text, so liveCount/liveBytes alone
	// would never trigger the fold that prunes them.
	return delta.len() >= maxDocs || delta.liveBytes() >= maxBytes
}

func (c *contentCoordinator) foldSucceeded() {
	c.mu.Lock()
	c.foldRetryAfter = time.Time{}
	c.mu.Unlock()
	c.state.setFoldError("")
}

func (c *contentCoordinator) foldFailed(backoff time.Duration) {
	c.mu.Lock()
	c.foldRetryAfter = time.Now().Add(backoff)
	c.mu.Unlock()
	c.state.setFoldError("content fold failed; the delta is retained and the fold will retry")
}

// contentExtractDocTimeout bounds one document's extraction so a single
// pathological file cannot stall the serial drain (or the serial build path,
// which uses contentBuildDocSafe). A timeout is treated as an extraction error:
// the caller skips the document and moves on. It is a package var so tests can
// lower it; a non-positive value disables the bound.
var contentExtractDocTimeout = 2 * time.Minute

// contentWithExtractDocTimeout nests a per-document deadline inside ctx,
// preserving the extraction settings it carries. Returns ctx unchanged when the
// bound is disabled.
func contentWithExtractDocTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if contentExtractDocTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, contentExtractDocTimeout)
}

// contentExtractDeltaDoc extracts a changed file into a delta document. When the
// file's content hash matches prior, changed is false and the caller can skip it
// (touch-only writes never reindex).
func contentExtractDeltaDoc(frn uint64, path string, prior [contentHashLen]byte, havePrior bool) (doc contentDeltaDoc, changed bool, err error) {
	return contentExtractDeltaDocContext(context.Background(), frn, path, prior, havePrior)
}

func contentExtractDeltaDocContext(baseCtx context.Context, frn uint64, path string, prior [contentHashLen]byte, havePrior bool) (doc contentDeltaDoc, changed bool, err error) {
	if baseCtx != nil && baseCtx.Err() != nil {
		return contentDeltaDoc{}, false, baseCtx.Err()
	}
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
	// The settings context carries the service's encoding/cap policy. The
	// call goes through contentExtractSafely so the drain shares the build's
	// isolation: a parser panic is a skip, and a ctx-ignoring parser is cut off
	// by the boundary-enforced deadline instead of wedging the serial drain.
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx := contentWithExtractSettings(baseCtx, contentServiceExtractSettings())
	res, err := contentExtractSafely(ctx, e, f, size)
	if err != nil || res.Skipped || len(res.Text) == 0 {
		return contentDeltaDoc{}, false, err
	}
	stored := contentRepairText(res.Text)
	h := sha256Of(stored)
	if havePrior && h == prior {
		return contentDeltaDoc{}, false, nil
	}
	return contentDeltaDoc{FRN: frn, Path: path, Text: stored, Hash: h, ContentType: res.Class, ExtractorVersion: e.Version()}, true, nil
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
//
// The resident delta is bounded by contentDeltaHardMaxDocs/Bytes in every case,
// not only while the sidecar is capped: a distinct document that arrives at the
// ceiling is deferred by FRN rather than admitted, so a stalled or failed fold
// cannot grow the delta without bound. Deferred changes are replayed once the
// delta drains, and while any remain the fold stops advancing the persisted
// checkpoint past them (runContentFold), so a restart replays their USNs too — a
// deferred change is delayed and surfaced incomplete, never lost.
func (c *contentCoordinator) processQueue(resolvePath func(frn uint64) (string, bool)) int {
	if c == nil {
		return 0
	}
	delta := c.state.deltaView()
	// Re-admit previously deferred changes once there is room; otherwise they
	// stay parked (no extraction churn) until a fold drains the delta.
	if !delta.atHardBound(contentDeltaHardMaxDocs, contentDeltaHardMaxBytes) {
		c.replayDeferred()
	}
	frns := c.takeQueued()
	changed := 0
	for _, frn := range frns {
		if c.extractCtx != nil && c.extractCtx.Err() != nil {
			break
		}
		// At the hard ceiling a distinct change cannot be admitted without
		// unbounded memory: defer it and surface the volume incomplete. Updating
		// an existing slot is always allowed, so acknowledged content is never
		// dropped. Checking before extraction also spares the I/O.
		if !delta.contains(frn) && delta.atHardBound(contentDeltaHardMaxDocs, contentDeltaHardMaxBytes) {
			c.deferChange(frn)
			continue
		}
		path, ok := resolvePath(frn)
		if !ok {
			delta.delete(frn)
			continue
		}
		prior, havePrior := delta.priorHash(frn)
		doc, didChange, err := contentExtractDeltaDocContext(c.extractCtx, frn, path, prior, havePrior)
		if err != nil {
			c.state.noteExtractionError()
			continue
		}
		if !didChange {
			// A base doc that re-extracts to nothing — the file is now binary or
			// skipped, or no longer maps to an extractor — must not keep serving
			// its old-version text. Tombstone it so the fold drops the base doc.
			// A prior delta entry (havePrior) is left alone: an unchanged write
			// must not delete acknowledged content.
			if !havePrior && c.state.hasContentForFRN(frn) {
				delta.delete(frn)
			}
			continue
		}
		delta.upsert(doc)
		changed++
	}
	c.state.setDeltaBytes(delta.liveBytes())
	deferred := c.deferredCount()
	c.state.setDeferred(deferred)
	if deferred > 0 {
		c.state.markDeltaCapped()
	}
	c.state.markDrained()
	return changed
}
