package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// searchCursorMaxBytes is the largest accepted cursor token. Content cursor v2
// tokens carry small fixed identity fields; anything larger is malformed input.
const searchCursorMaxBytes = 16 << 10

type searchCursorName struct {
	JournalID  uint64 `json:"j"`
	Checkpoint int64  `json:"c"`
	Records    int    `json:"n"`
}

type searchCursorSide struct {
	Version       int    `json:"v"`
	JournalID     uint64 `json:"j"`
	CheckpointUSN uint64 `json:"c"`
	ScopeHash     string `json:"s,omitempty"`
	BuiltAt       int64  `json:"b,omitempty"`
	EncodedSize   int64  `json:"e,omitempty"`
}

// searchCursorV2 is the content cursor v2 payload: the last emitted record
// (FRN) plus the identities needed to reject a stale resume.
type searchCursorV2 struct {
	Version  int              `json:"v"`
	Query    uint64           `json:"q"`
	Volume   string           `json:"vol"`
	ID       uint64           `json:"id"`
	Offset   int              `json:"off"`
	Name     searchCursorName `json:"n"`
	Side     searchCursorSide `json:"s"`
	Delta    uint64           `json:"d"`
	Instance string           `json:"i"`
	// BK is the last emitted base row (FRN), or 0 when no base row has been
	// emitted yet. The base side resumes after it.
	BK uint64 `json:"bk,omitempty"`
	// K is the page key of the last emitted row (base or overlay). The overlay
	// side keeps only rows whose key is greater.
	K *contentCursorKey `json:"k,omitempty"`
}

// contentCursorKey is a row's page position: (tier, rank, tiebreak).
//
// tier is the default-sort promotion tier (0 = symbol-named header, 1 =
// symbol-named other, 2 = rest); explicit sorts use 0. rank is the same value
// mergeContentOverlayMatches uses (baseEntryRank / overlayEntryRank). tiebreak
// orders equal ranks: base rows sort first, then overlay rows in their ranked
// overlay order (name/path). Sorting rows by key reproduces the merged page
// order.
type contentCursorKey struct {
	Tier     int    `json:"t"`
	Rank     int    `json:"r"`
	Tiebreak string `json:"x,omitempty"`
}

func (k contentCursorKey) compare(other contentCursorKey) int {
	if k.Tier != other.Tier {
		return k.Tier - other.Tier
	}
	if k.Rank != other.Rank {
		return k.Rank - other.Rank
	}
	return strings.Compare(k.Tiebreak, other.Tiebreak)
}

// contentCursorTier is the promotion tier a row occupies. Promotion only runs
// for the default sort; every explicit sort is a single tier.
func contentCursorTier(entry Entry, pq parsedQuery) int {
	if pq.SortColumn != "" {
		return 0
	}
	key := contentSymbolQueryKey(pq)
	if key == "" || !contentEntrySymbolNamed(entry, key) {
		return 2
	}
	name := entry.Name
	if name == "" {
		name = filepath.Base(entry.Path)
	}
	if contentIsHeaderName(name) {
		return 0
	}
	return 1
}

// contentCursorOverlayTiebreak mirrors the final tie-break
// overlayRankedMatches applies inside one rank.
func contentCursorOverlayTiebreak(entry Entry, pq parsedQuery) string {
	if pq.SortColumn == "path" {
		return entryLowerPath(entry) + "\x00" + entry.Path
	}
	return entryLowerName(entry) + "\x00" + entry.Path
}

func (vol *serviceVolumeIndex) contentCursorOverlayKey(item rankedOverlayEntry, pq parsedQuery) contentCursorKey {
	return contentCursorKey{
		Tier:     contentCursorTier(item.entry, pq),
		Rank:     item.rank,
		Tiebreak: contentCursorOverlayTiebreak(item.entry, pq),
	}
}

func (vol *serviceVolumeIndex) contentCursorBaseKey(entry Entry, pq parsedQuery) contentCursorKey {
	return contentCursorKey{
		Tier: contentCursorTier(entry, pq),
		Rank: vol.baseEntryRank(entry, pq),
	}
}

// contentCursorEntryIsOverlay reports whether an emitted row came from the
// overlay side. Any overlay record for a base FRN shadows that base record, so
// a base FRN is either hidden (overlay row) or the row itself. A FRN absent
// from the base index can only be an overlay creation.
func (vol *serviceVolumeIndex) contentCursorEntryIsOverlay(entry Entry) bool {
	if vol == nil || vol.index == nil {
		return false
	}
	snap := vol.snap.Load()
	if snap == nil || len(snap.records) == 0 {
		return false
	}
	id, ok := vol.idForFRN(entry.FRN)
	if !ok {
		return true
	}
	return hiddenBaseIDs{
		tombstone: snap.tombstoneIDs,
		shadowed:  snap.shadowedIDs,
	}.contains(id)
}

// contentCursorContext is the identity captured for one request. It is
// comparable so a pre-search capture can be compared with a post-search read.
// Overlay names and content-delta generation are intentionally absent: they
// change between pages and the resume re-evaluates them on each page.
type contentCursorContext struct {
	Volume   string
	Name     searchCursorName
	Side     searchCursorSide
	Instance string
}

var contentCursorInstanceID = newContentCursorInstanceID()

func newContentCursorInstanceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func encodeSearchCursorV2(opts queryOptions, ctx contentCursorContext, id uint64, offset int, bk uint64, key *contentCursorKey) string {
	c := searchCursorV2{
		Version:  2,
		Query:    queryOptionsHash(opts),
		Volume:   ctx.Volume,
		ID:       id,
		Offset:   offset,
		Name:     ctx.Name,
		Side:     ctx.Side,
		Instance: ctx.Instance,
		BK:       bk,
		K:        key,
	}
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeAnySearchCursor accepts both cursor versions. A v1 token returns its
// offset with a nil v2 payload; a v2 token returns a nil offset.
func decodeAnySearchCursor(token string, opts queryOptions) (int, *searchCursorV2, error) {
	if len(token) > searchCursorMaxBytes {
		return 0, nil, errors.New("malformed cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, nil, errors.New("malformed cursor")
	}
	var probe struct {
		Version int `json:"v"`
	}
	if json.Unmarshal(b, &probe) != nil {
		return 0, nil, errors.New("malformed cursor")
	}
	switch probe.Version {
	case 1:
		var c searchCursor
		if json.Unmarshal(b, &c) != nil || c.Offset < 0 {
			return 0, nil, errors.New("malformed cursor")
		}
		if c.Query != queryOptionsHash(opts) {
			return 0, nil, errors.New("cursor does not match this query")
		}
		return c.Offset, nil, nil
	case 2:
		var c searchCursorV2
		if json.Unmarshal(b, &c) != nil || c.Offset < 0 || c.Volume == "" || c.ID == 0 {
			return 0, nil, errors.New("malformed cursor")
		}
		if c.Query != queryOptionsHash(opts) {
			return 0, nil, errors.New("cursor does not match this query")
		}
		return 0, &c, nil
	default:
		return 0, nil, errors.New("malformed cursor")
	}
}

func (c *searchCursorV2) check(ctx contentCursorContext) (bool, string) {
	switch {
	case c.Volume != ctx.Volume:
		return false, "content volume changed"
	case c.Name != ctx.Name:
		return false, "name index changed"
	case c.Side != ctx.Side:
		return false, "content index changed"
	case c.Instance != ctx.Instance:
		return false, "service instance changed"
	}
	return true, ""
}

func (s *contentVolumeState) cursorSideIdentity() (searchCursorSide, bool) {
	if s == nil {
		return searchCursorSide{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx := s.idx
	if idx == nil {
		return searchCursorSide{}, false
	}
	return searchCursorSide{
		Version:       idx.Version,
		JournalID:     idx.JournalID,
		CheckpointUSN: idx.CheckpointUSN,
		ScopeHash:     hex.EncodeToString(idx.ScopeHash[:]),
		BuiltAt:       idx.BuiltAt.UnixNano(),
		EncodedSize:   idx.EncodedSize,
	}, true
}

// contentCursorContextFor captures the eligible v2 cursor identity for one
// content request. It refuses every shape outside the v2 scope: more than one
// volume, relevance sort, and non-content queries. A populated filename
// overlay and content delta stay in scope: the resume re-evaluates both on
// every page (see overlayRankedMatches) instead of restarting.
func (s *goSearchService) contentCursorContextFor(pq parsedQuery, opts queryOptions) (*contentCursorContext, bool) {
	if s == nil || len(s.volumes) != 1 || len(s.indexes) != len(s.volumes) {
		return nil, false
	}
	vol := s.volumes[0]
	if vol == nil || vol.index == nil || vol.content == nil {
		return nil, false
	}
	if !queryHasAnyContentLeaf(pq) || pq.SortColumn == "relevance" {
		return nil, false
	}
	if !vol.contentUsableForQuery() {
		return nil, false
	}
	side, ok := vol.content.cursorSideIdentity()
	if !ok {
		return nil, false
	}
	return &contentCursorContext{
		Volume: contentVolumeName(vol),
		Name: searchCursorName{
			JournalID:  vol.journalID,
			Checkpoint: vol.baseCheckpoint,
			Records:    vol.index.compactRecordCount(),
		},
		Side:     side,
		Instance: contentCursorInstanceID,
	}, true
}

// contentAfterCandidateStart locates the resume record in a rebuilt candidate
// order and returns the position to verify from. A missing record leaves the
// start at zero (restart) and reports Found=false through the shared state.
func (vol *serviceVolumeIndex) contentAfterCandidateStart(ids []int, resume *contentCursorResume) int {
	if resume == nil {
		return 0
	}
	if resume.FRN == 0 {
		// No base row has been emitted yet: the base side starts at the
		// beginning. Still report Found so a valid resume is not mistaken
		// for a missing record.
		resume.Found = true
		return 0
	}
	for i, id := range ids {
		if vol.index.compactRecord(id).FRN == resume.FRN {
			resume.Found = true
			return i + 1
		}
	}
	return 0
}

// contentAfterScanStart is contentAfterCandidateStart for a compact scan order.
func (vol *serviceVolumeIndex) contentAfterScanStart(order []uint32, recordCount int, resume *contentCursorResume) int {
	if resume == nil {
		return 0
	}
	if resume.FRN == 0 {
		resume.Found = true
		return 0
	}
	for pos := 0; pos < compactUint32OrderLen(order, recordCount); pos++ {
		id := compactUint32OrderAt(order, pos)
		if vol.index.compactRecord(id).FRN == resume.FRN {
			resume.Found = true
			return pos + 1
		}
	}
	return 0
}

// contentAfterSlice drops every candidate up to and including the v2 resume
// record. A missing record returns the input unchanged (restart).
func (vol *serviceVolumeIndex) contentAfterSlice(ids []int, resume *contentCursorResume) []int {
	if start := vol.contentAfterCandidateStart(ids, resume); start > 0 {
		return ids[start:]
	}
	return ids
}
