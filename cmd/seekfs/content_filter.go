package main

// Filter tier for whole-drive content coverage (§6y).
//
// The full tier (v9 `.gsx`: trigram postings plus stored compressed text) is
// ~0.31x of extracted bytes. When the free-space reserve (§6t) or an explicit
// budget bites, files past the budget get no content search at all. This file
// implements a second, much smaller tier covering those dropped files, in the
// spirit of ugrep-indexer / Russ Cox codesearch / fsearch: a per-file
// membership filter over the same folded trigrams the full tier indexes, with
// NO stored text.
//
// Per covered file the filter stores one fixed-size bloom signature (size
// class picked from the extracted-text length: 128 B–2 KiB, ~250 B/file mean
// on the Chromium sample) plus a size+mtime fingerprint and the path. A
// content query intersects the full tier's postings as today, then scans the
// filter records in key order: records whose bits cannot contain the query's
// required grams are skipped, the rest are verified by reading the file from
// disk (open → extract with the same extractor and limits → match), honoring
// the verify/snippet/limit/order contracts. Tier-1 results and their order
// are unchanged; tier-2 hits append in filter scan order up to the limit.
//
// Exactness comes from verification: the filter never produces false
// negatives (every folded gram of a true match is in the file's bitset —
// proven by TestContentFilterNoFalseNegatives over the query-shape matrix),
// only false positives, bounded per shape in §6y. A record whose fingerprint
// no longer matches the file on disk is skipped, never served stale: the
// USN/walk change pipeline re-extracts the file and the fold refreshes the
// record, so coverage resumes without a rebuild.
//
// Format: a NEW sidecar file beside the `.gsx` (`<gsx>.filter`, format v1)
// so the v9 full-tier sections stay byte-identical. The name contains ".gsx"
// so contentChangeExcluded keeps it out of every corpus. A corrupt, missing,
// or scope-mismatched filter only disables tier 2; the base still attaches.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// contentFilterMagic identifies a `.gsx.filter` file; the trailing digits are
// the format version, matching the `.gsx` habit.
var contentFilterMagic = [8]byte{'G', 'O', 'S', 'C', 'F', 'L', '0', '1'}

// contentFilterVersion is the filter sidecar format version. A sidecar whose
// version does not match is ignored (tier 2 disabled), never served.
const contentFilterVersion = 1

// contentFilterHashes is the bloom hash count (k). k=1 measured best on the
// Chromium sample at every candidate size: k=2 saturates the small filters
// and loses on every query shape (see §6y). It rides the header so a future
// k can be told apart instead of misread.
const contentFilterHashes = 1

// contentFilterClassBits lists the bloom bit sizes by size class: 1024<<class
// bits (128 B–2 KiB). The class is picked from the extracted-text length so
// small files stay small and large files do not saturate into uselessness.
var contentFilterClassBits = []uint32{1024, 2048, 4096, 8192, 16384}

// contentFilterClassForGrams picks the smallest class whose bits outnumber
// twice the file's distinct trigram count (n/m ≤ 0.5, occupancy ≤ 0.39 with
// k=1). Sizing by the measured n — not by text length — keeps dense files
// (minified assets, test data: distinct ≈ windows) from saturating into
// uselessness while small files stay at the 128 B floor. Past class 4
// (n > 8192) the filter saturates gracefully: every extra gram only raises
// the false-positive rate, never a false negative.
func contentFilterClassForGrams(n int) int {
	for c, b := range contentFilterClassBits {
		if int64(b) >= int64(n)*2 {
			return c
		}
	}
	return len(contentFilterClassBits) - 1
}

// contentFilterRecordSize returns the exact encoded bytes of a record with
// the given class and path: fixed overhead + class bits + path + its length
// prefix. The build's reserve accounting (§6t) admits dropped files at this
// exact size, measured from the extracted text the drop tally already paid
// for — no estimate, so repetitive files (small n, big text) are covered
// exactly like dense ones.
func contentFilterRecordSize(class int, path string) int64 {
	if class < 0 || class >= len(contentFilterClassBits) {
		class = len(contentFilterClassBits) - 1
	}
	return int64(contentFilterRecordOverhead) + int64(contentFilterClassBits[class]/8) + int64(len(path)) + int64(binary.PutUvarint(make([]byte, binary.MaxVarintLen64), uint64(len(path))))
}

// contentFilterRecordOverhead is the fixed per-record wire cost beside the
// bits and path: key(8)+fileSize(8)+modNano(8)+textLen(8)+class(1)+reserved(7).
const contentFilterRecordOverhead = 40

// contentFilterHash maps a folded trigram key to a bit index in an m-bit
// (power-of-two) filter. SplitMix64 over the key with a fixed seed; the mask
// needs no modulo. The function is FROZEN by TestContentFilterHashIsFrozen:
// the bits are persisted, so changing it silently invalidates every filter.
func contentFilterHash(g uint32, m uint32) uint32 {
	x := uint64(g)*0x9E3779B97F4A7C15 + 0x12345ABCDEF6789
	x ^= x >> 29
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 32
	return uint32(x & uint64(m-1))
}

// contentFilterGramsOf returns the distinct folded-trigram keys of folded text
// as u32s. It is contentGramsOfU32 with identical skip rules (grams containing
// a control byte are never indexed), so a query gram the builder skipped can
// never be asked of a filter: no false negatives by construction.
func contentFilterGramsOf(folded []byte) []uint32 {
	set := contentGramsOfU32(folded)
	out := make([]uint32, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	return out
}

// contentFilterRecord is one covered file: its join key (FRN for USN volumes,
// stable path hash for walk volumes — the same key space as contentDoc.FRN),
// the source fingerprint the walk reconcile diffs (FileSize/FileModNano), the
// stored path (relative for walk builds, absolute for service builds), and
// the bloom bits over the file's folded trigrams.
type contentFilterRecord struct {
	Key         uint64
	FileSize    int64
	FileModNano int64
	// TextLen is the extracted-text byte length the record covers. It is the
	// exact tally basis for health's uncovered bytes (build-dropped bytes
	// minus filter-covered bytes, floored at zero).
	TextLen int64
	Class   int
	Path    string
	Bits    []byte
}

// contentFilterBitsOf sets every gram of folded text in bits (m = len*8).
func contentFilterBitsOf(bits []byte, folded []byte) {
	contentFilterBitsOfU32(bits, contentGramsOfU32(folded))
}

// contentFilterBitsOfU32 sets precomputed gram keys in bits.
func contentFilterBitsOfU32(bits []byte, grams map[uint32]struct{}) {
	m := uint32(len(bits)) * 8
	for g := range grams {
		b := contentFilterHash(g, m)
		bits[b/8] |= 1 << (b % 8)
	}
}

// mayMatch reports whether the record's bits contain every query gram: a
// necessary (not sufficient) condition for the file to match. A true match
// always passes; anything else is a false positive the disk verification
// rejects.
func (r *contentFilterRecord) mayMatch(grams []uint32) bool {
	if len(grams) == 0 {
		return true
	}
	if len(r.Bits) == 0 {
		return false
	}
	m := uint32(len(r.Bits)) * 8
	for _, g := range grams {
		b := contentFilterHash(g, m)
		if r.Bits[b/8]&(1<<(b%8)) == 0 {
			return false
		}
	}
	return true
}

// contentFilter is a decoded `.gsx.filter` sidecar: the filter records sorted
// by key plus the build identity. Bits alias the mmap backing (see release)
// when loaded from disk; Records/Bits are heap copies in tests.
type contentFilter struct {
	Version   int
	Origin    uint32
	MaxRaw    int64
	MaxText   int64
	ScopeHash [32]byte
	// Root is the walk build's absolute root: walk records store absolute
	// paths for verification, and queries display them relative to Root so
	// tier-2 hits read like tier-1 hits. Service builds store absolute
	// record paths with an empty Root (verification uses the live
	// record-index path instead).
	Root      string
	BuiltAt   time.Time
	Records   []contentFilterRecord
	// EncodedSize is the filter file's byte size, surfaced in health.
	EncodedSize int64
	release     func()
	releaseOnce sync.Once
}

// contentFilterPathForGSX names the filter sidecar for a `.gsx` path. The
// name contains ".gsx" so contentChangeExcluded keeps it out of every corpus.
func contentFilterPathForGSX(gsx string) string {
	if gsx == "" {
		return ""
	}
	return gsx + ".filter"
}

// Release unmaps the filter's backing file exactly once. A heap-decoded
// filter is a no-op. Callers replace the filter only where they replace the
// base index (attach/publish/fold, under the same locks), so the discipline
// matches the `.gsx` mapping it shadows.
func (f *contentFilter) Release() {
	if f == nil {
		return
	}
	f.releaseOnce.Do(func() {
		if f.release != nil {
			f.release()
		}
	})
}

// lookup binary-searches the key-sorted records.
func (f *contentFilter) lookup(key uint64) (contentFilterRecord, bool) {
	i := sort.Search(len(f.Records), func(i int) bool { return f.Records[i].Key >= key })
	if i < len(f.Records) && f.Records[i].Key == key {
		return f.Records[i], true
	}
	return contentFilterRecord{}, false
}

// contentFilterHeaderSize is magic(8)+version(4)+origin(4)+hashes(4)+count(4)+
// built(8)+maxRaw(8)+maxText(8)+scopeHash(32) = 80, followed by a uvarint
// root length and the root bytes.
const contentFilterHeaderSize = 80

// contentFilterDisplayPath renders a record path for results: relative to the
// walk root when the filter carries one (matching tier-1's root-relative
// form), otherwise the stored path as-is.
func (f *contentFilter) displayPath(rec *contentFilterRecord) string {
	if f != nil && f.Root != "" {
		if rel, err := filepath.Rel(f.Root, rec.Path); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return rec.Path
}

// contentFilterEncode serializes the filter: header, key-sorted records, then
// a CRC32C trailer over all preceding bytes. Record order is canonical, so
// encoding the same set twice yields identical bytes.
func contentFilterEncode(f *contentFilter) []byte {
	recs := append([]contentFilterRecord(nil), f.Records...)
	sort.Slice(recs, func(i, j int) bool { return recs[i].Key < recs[j].Key })
	var body bytes.Buffer
	header := make([]byte, contentFilterHeaderSize)
	copy(header[0:8], contentFilterMagic[:])
	binary.LittleEndian.PutUint32(header[8:], uint32(f.Version))
	if f.Version == 0 {
		binary.LittleEndian.PutUint32(header[8:], contentFilterVersion)
	}
	binary.LittleEndian.PutUint32(header[12:], f.Origin)
	binary.LittleEndian.PutUint32(header[16:], contentFilterHashes)
	binary.LittleEndian.PutUint32(header[20:], uint32(len(recs)))
	built := f.BuiltAt
	if built.IsZero() {
		built = time.Now()
	}
	binary.LittleEndian.PutUint64(header[24:], uint64(built.UnixNano()))
	binary.LittleEndian.PutUint64(header[32:], uint64(f.MaxRaw))
	binary.LittleEndian.PutUint64(header[40:], uint64(f.MaxText))
	copy(header[48:80], f.ScopeHash[:])
	body.Write(header)
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], uint64(len(f.Root)))
	body.Write(tmp[:n])
	body.WriteString(f.Root)
	for i := range recs {
		r := &recs[i]
		class := r.Class
		if class < 0 || class >= len(contentFilterClassBits) ||
			int(contentFilterClassBits[class]/8) != len(r.Bits) {
			// A caller-supplied bitset of non-class width takes the class
			// matching its width (or the top class), and the bytes below
			// are re-sized to it. Builders always supply the exact width.
			class = len(contentFilterClassBits) - 1
			for c, b := range contentFilterClassBits {
				if int(b/8) >= len(r.Bits) {
					class = c
					break
				}
			}
		}
		var fixed [contentFilterRecordOverhead]byte
		binary.LittleEndian.PutUint64(fixed[0:], r.Key)
		binary.LittleEndian.PutUint64(fixed[8:], uint64(r.FileSize))
		binary.LittleEndian.PutUint64(fixed[16:], uint64(r.FileModNano))
		binary.LittleEndian.PutUint64(fixed[24:], uint64(r.TextLen))
		fixed[32] = byte(class)
		body.Write(fixed[:])
		var tmp [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(tmp[:], uint64(len(r.Path)))
		body.Write(tmp[:n])
		body.WriteString(r.Path)
		want := int(contentFilterClassBits[class] / 8)
		if len(r.Bits) == want {
			body.Write(r.Bits)
		} else {
			// A caller-supplied bitset of the wrong width is re-sized, not
			// misread: missing bytes read as zero (more selective), extra
			// bytes are dropped. Builders always supply the exact width.
			buf := make([]byte, want)
			copy(buf, r.Bits)
			body.Write(buf)
		}
	}
	out := body.Bytes()
	var crc [4]byte
	binary.LittleEndian.PutUint32(crc[:], crc32.Checksum(out, contentSectionCRCModel))
	out = append(out, crc[:]...)
	return out
}

// contentFilterDecode parses and integrity-checks encoded filter bytes. A
// version or hash-count mismatch, a truncated record, or a CRC failure is an
// error: the caller disables tier 2, never serves from suspect bytes.
func contentFilterDecode(data []byte) (*contentFilter, error) {
	if len(data) < contentFilterHeaderSize+4 {
		return nil, errors.New("content filter truncated")
	}
	if !bytes.Equal(data[0:8], contentFilterMagic[:]) {
		return nil, errors.New("unsupported content filter format")
	}
	version := int(binary.LittleEndian.Uint32(data[8:]))
	if version != contentFilterVersion {
		return nil, fmt.Errorf("unsupported content filter version %d", version)
	}
	if hashes := binary.LittleEndian.Uint32(data[16:]); hashes != contentFilterHashes {
		return nil, fmt.Errorf("unsupported content filter hash count %d", hashes)
	}
	count := int(binary.LittleEndian.Uint32(data[20:]))
	if count < 0 {
		return nil, errors.New("content filter count mismatch")
	}
	want := binary.LittleEndian.Uint32(data[len(data)-4:])
	if got := crc32.Checksum(data[:len(data)-4], contentSectionCRCModel); got != want {
		return nil, fmt.Errorf("content filter checksum mismatch (want %08x, got %08x)", want, got)
	}
	f := &contentFilter{
		Version:     version,
		Origin:      binary.LittleEndian.Uint32(data[12:]),
		MaxRaw:      int64(binary.LittleEndian.Uint64(data[32:])),
		MaxText:     int64(binary.LittleEndian.Uint64(data[40:])),
		BuiltAt:     time.Unix(0, int64(binary.LittleEndian.Uint64(data[24:]))),
		EncodedSize: int64(len(data)),
	}
	copy(f.ScopeHash[:], data[48:80])
	off := contentFilterHeaderSize
	rl, n := binary.Uvarint(data[off:])
	if n <= 0 || rl > 1<<20 {
		return nil, errors.New("content filter root truncated")
	}
	off += n
	if off+int(rl) > len(data)-4 {
		return nil, errors.New("content filter root truncated")
	}
	f.Root = string(data[off : off+int(rl)])
	off += int(rl)
	end := len(data) - 4
	f.Records = make([]contentFilterRecord, 0, count)
	for i := 0; i < count; i++ {
		if off+contentFilterRecordOverhead > end {
			return nil, errors.New("content filter record truncated")
		}
		r := contentFilterRecord{
			Key:         binary.LittleEndian.Uint64(data[off:]),
			FileSize:    int64(binary.LittleEndian.Uint64(data[off+8:])),
			FileModNano: int64(binary.LittleEndian.Uint64(data[off+16:])),
			TextLen:     int64(binary.LittleEndian.Uint64(data[off+24:])),
			Class:       int(data[off+32]),
		}
		off += contentFilterRecordOverhead
		if r.Class < 0 || r.Class >= len(contentFilterClassBits) {
			return nil, errors.New("content filter record class out of range")
		}
		pl, n := binary.Uvarint(data[off:end])
		if n <= 0 || pl > 1<<20 {
			return nil, errors.New("content filter path truncated")
		}
		off += n
		if off+int(pl) > end {
			return nil, errors.New("content filter path truncated")
		}
		r.Path = string(data[off : off+int(pl)])
		off += int(pl)
		want := int(contentFilterClassBits[r.Class] / 8)
		if off+want > end {
			return nil, errors.New("content filter bits truncated")
		}
		r.Bits = data[off : off+want]
		off += want
		f.Records = append(f.Records, r)
	}
	if off != end {
		return nil, errors.New("content filter length mismatch")
	}
	for i := 1; i < len(f.Records); i++ {
		if f.Records[i].Key <= f.Records[i-1].Key {
			return nil, errors.New("content filter records out of order")
		}
	}
	return f, nil
}

// contentSaveFilter writes the filter to path atomically (temp + fsync +
// rename), like contentSaveFile: a crash leaves the old filter or none,
// never a half-written one.
func contentSaveFilter(path string, f *contentFilter) error {
	if path == "" {
		return errors.New("empty content filter path")
	}
	encoded := contentFilterEncode(f)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	f.EncodedSize = int64(len(encoded))
	return os.Rename(name, path)
}

// contentLoadFilter loads a `.gsx.filter` sidecar: mmap with zero-copy bit
// views (heap fallback), CRC-verified at load. Any error (missing,
// corrupt, version mismatch) is returned for the caller to treat as
// "no tier 2", never as an attach failure.
func contentLoadFilter(path string) (*contentFilter, error) {
	data, release, err := contentMapFileFn(path)
	if err != nil {
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		release = nil
	}
	f, err := contentFilterDecode(data)
	if err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	if release != nil {
		f.release = release
	}
	return f, nil
}

// contentFilterBuilder accumulates filter records during extraction. It is
// the contained build-side surface the spool-peak rework stays clear of: the
// extraction loop feeds dropped files here, and the finished filter saves
// beside the `.gsx`.
type contentFilterBuilder struct {
	origin  uint32
	maxRaw  int64
	maxText int64
	scope   [32]byte
	root    string
	recs    []contentFilterRecord
	bytes   int64
}

// newContentFilterBuilder starts a builder for one sidecar's dropped files.
// root is the walk build's absolute root (record paths are stored absolute
// so verification works from any directory); service builds pass "".
func newContentFilterBuilder(origin uint32, maxRaw, maxText int64, scope [32]byte, root string) *contentFilterBuilder {
	return &contentFilterBuilder{origin: origin, maxRaw: maxRaw, maxText: maxText, scope: scope, root: root}
}

// Add records one dropped file's extracted text. The bits cover the folded
// view of the case-preserving stored text — exactly what the full tier
// indexes — so the prefilter is a superset of every full-tier match.
func (b *contentFilterBuilder) Add(key uint64, path string, fileSize int64, modNano int64, text []byte) {
	if b == nil {
		return
	}
	stored := contentStoredText(text)
	grams := contentGramsOfU32(contentFoldText(stored))
	b.AddRecord(key, path, fileSize, modNano, int64(len(text)), grams)
}

// AddRecord records one file from its measured gram set, sizing the class
// exactly (no estimate). The build loop measures the set once and reuses it
// for the admission check, so covering a dropped file costs one gram pass.
func (b *contentFilterBuilder) AddRecord(key uint64, path string, fileSize int64, modNano int64, textLen int64, grams map[uint32]struct{}) {
	if b == nil {
		return
	}
	class := contentFilterClassForGrams(len(grams))
	bits := make([]byte, contentFilterClassBits[class]/8)
	contentFilterBitsOfU32(bits, grams)
	b.recs = append(b.recs, contentFilterRecord{Key: key, FileSize: fileSize, FileModNano: modNano, TextLen: textLen, Class: class, Path: path, Bits: bits})
	b.bytes += contentFilterRecordSize(class, path)
}

// Len and Bytes report the accumulated records and their estimated wire size.
func (b *contentFilterBuilder) Len() int {
	if b == nil {
		return 0
	}
	return len(b.recs)
}

// Bytes reports the accumulated records' estimated wire size.
func (b *contentFilterBuilder) Bytes() int64 {
	if b == nil {
		return 0
	}
	return b.bytes
}

// Finish sorts the records by key and returns the filter, or nil when empty
// (no sidecar is written for an empty tier 2).
func (b *contentFilterBuilder) Finish() *contentFilter {
	if b == nil || len(b.recs) == 0 {
		return nil
	}
	recs := append([]contentFilterRecord(nil), b.recs...)
	sort.Slice(recs, func(i, j int) bool { return recs[i].Key < recs[j].Key })
	return &contentFilter{Version: contentFilterVersion, Origin: b.origin, MaxRaw: b.maxRaw, MaxText: b.maxText, ScopeHash: b.scope, Root: b.root, BuiltAt: time.Now(), Records: recs}
}

// contentFilterMaxVerify caps one query's disk verifications (open → extract
// → match). Past it the query stops and reports complete:false rather than
// reading the whole drive. A var so tests pin it.
var contentFilterMaxVerify = 512

// contentFilterVerifyStored opens path, checks the record fingerprint against
// the open's own stat, and extracts with the same extractor and limits the
// build used. ok=false means skip (changed/missing/unextractable): the file
// is never served stale, and the change pipeline's re-extract restores
// coverage. The returned bytes are the case-preserving stored text, like the
// full tier's CXST.
func contentFilterVerifyStored(path string, rec contentFilterRecord, settings contentExtractSettings) (stored []byte, ok bool) {
	s, err := contentSlurpNoRecall(path, settings.maxRaw)
	if err != nil {
		return nil, false
	}
	defer s.close()
	// Invalidation: the filter record's (size, mtime) must match the bytes
	// being verified. A file rewritten since the record was written verifies
	// wrong, so it verifies not at all.
	if s.size != rec.FileSize || s.modNano != rec.FileModNano {
		return nil, false
	}
	e := contentExtractorForPath(path, s.headBytes())
	if e == nil {
		return nil, false
	}
	ctx := contentWithExtractSettings(context.Background(), settings)
	res, err := contentExtractSafely(ctx, e, s.reader(), s.size)
	if err != nil || res.Skipped || res.Binary || len(res.Text) == 0 {
		return nil, false
	}
	return contentStoredText(res.Text), true
}

// contentFilterLeafGrams returns the folded distinct trigram keys constraining
// one term/phrase leaf for the prefilter. The leaf is always folded — even
// for a case-sensitive query, whose raw match implies the folded match — so
// the prefilter stays a superset. Empty for sub-3-byte leaves (no constraint).
func contentFilterLeafGrams(leafText string) []uint32 {
	return contentFilterGramsOf(contentFoldText([]byte(leafText)))
}

// contentFilterRegexGramsOk reports whether a regex leaf's required literals
// can hold in the record: some disjunct's every run must may-match (a match
// contains one whole disjunct). No provable literals means no constraint.
func contentFilterRegexGramsOk(rec *contentFilterRecord, pattern string) bool {
	disjuncts := regexRequiredLiteralAlternatives(pattern)
	if len(disjuncts) == 0 {
		return true
	}
	for _, runs := range disjuncts {
		whole := true
		any := false
		for _, run := range runs {
			if !contentTermTrigramSafe(run) {
				continue
			}
			any = true
			if !rec.mayMatch(contentFilterGramsOf([]byte(run))) {
				whole = false
				break
			}
		}
		if any && whole {
			return true
		}
	}
	return false
}

// contentFilterAltOk reports whether one OR alternative's positive content
// constraints can hold in the record: every term leaf's grams must may-match
// (AND), and regex leaves constrain only for pure-regex queries — the same
// rule as the tier-1 candidate streams, as a boolean prefilter.
func contentFilterAltOk(rec *contentFilterRecord, alt parsedQuery, pureRegex bool) bool {
	for _, leaf := range alt.Content {
		if leaf.Kind == contentLeafRegex {
			if pureRegex && !contentFilterRegexGramsOk(rec, leaf.Text) {
				return false
			}
			continue
		}
		if grams := contentFilterLeafGrams(leaf.Text); len(grams) > 0 && !rec.mayMatch(grams) {
			return false
		}
	}
	for _, group := range alt.OrGroups {
		if !contentFilterGroupOk(rec, group, pureRegex) {
			return false
		}
	}
	return true
}

// contentFilterGroupOk reports whether an OR group's constraints can hold: an
// alternative without content constraints (a filename-only alternative, or an
// unconstrained regex) always can, so the group passes when any alternative
// passes — mirroring the tier-1 union streams.
func contentFilterGroupOk(rec *contentFilterRecord, group []parsedQuery, pureRegex bool) bool {
	for i := range group {
		if contentFilterAltOk(rec, group[i], pureRegex) {
			return true
		}
	}
	return false
}

// contentFilterPrefilterOk is the tier-2 candidate gate: every top-level
// positive term leaf must may-match, pure-regex leaves must hold a disjunct,
// and content-driven OR groups need a passing alternative. NOT leaves are
// exclusion and constrain nothing. Anything it cannot prove is verified, so
// it is sound (no false negatives) by construction: every check is a
// mayMatch superset test.
func contentFilterPrefilterOk(rec *contentFilterRecord, pq parsedQuery) bool {
	pureRegex := contentLeavesAllRegex(pq)
	for _, leaf := range pq.Content {
		if leaf.Kind == contentLeafRegex {
			if pureRegex && !contentFilterRegexGramsOk(rec, leaf.Text) {
				return false
			}
			continue
		}
		if grams := contentFilterLeafGrams(leaf.Text); len(grams) > 0 && !rec.mayMatch(grams) {
			return false
		}
	}
	for _, group := range pq.OrGroups {
		driven := false
		for i := range group {
			if queryHasAnyContentLeaf(group[i]) {
				driven = true
				break
			}
		}
		if !driven {
			continue
		}
		if !contentFilterGroupOk(rec, group, pureRegex) {
			return false
		}
	}
	return true
}

// contentFilterMatchText evaluates one extracted file's text against the
// query's content leaves with a hoisted matcher, returning the per-leaf
// verdicts for entryMatchesContentAt. It mirrors the service verify loop's
// fold discipline (matchRawFirst with one shared fold).
func contentFilterMatchText(text []byte, m *contentLeafMatcher) []bool {
	matched := make([]bool, m.size)
	var folded []byte
	foldedReady := false
	for _, leaf := range m.leaves {
		matched[leaf.LeafID] = m.matchRawFirst(text, &folded, &foldedReady, leaf)
	}
	return matched
}
