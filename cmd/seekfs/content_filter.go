package main

// Per-file bloom filter (§6y, v11 default): the fsearch-model content index
// is trigram postings (CXGR, with dense grams pruned into CXGP) plus this
// per-file membership filter (CXFL) over the same folded trigrams — with NO
// stored text. Every match and snippet verifies by reading the file from
// disk; the filter narrows candidates before any read, so dropping stored
// text does not bring back the §6m full-scan verification blowup.
//
// One k=1 bloom signature per document, sized by its measured distinct
// trigram count (n/m ≤ 0.5; classes 128 B–2 KiB). The filter never produces
// false negatives (every folded gram of a true match sets its bit — proven
// by TestContentFilterNoFalseNegatives); only false positives, bounded per
// shape in §6y. Verification reads the file's CURRENT bytes (verify-direct
// freshness: a size/mtime mismatch against the fingerprint means stale index
// entries, but the bytes on disk still decide — the drain re-extracts the
// file into the delta through the existing USN/walk pipeline).
//
// CXFL layout (CRC-covered like every section): count u32, maxRaw u64,
// maxText u64, root (uvarint len + bytes), encoding (uvarint len + bytes),
// then per document in DocID order: class u8 + class bits. Rows carry no
// keys, paths, or fingerprints — they join by DocID to the doc table (keys),
// CXPT (paths) and the doc fingerprint (FileSize/FileModNano). Root is the
// walk build's absolute root (offline queries resolve relative CXPT paths
// against it); service builds store "" and verify through the live
// record-index path.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"
)

// contentFilterHashes is the bloom hash count (k). k=1 measured best on the
// Chromium sample at every candidate size: k=2 saturates the small filters
// and loses on every query shape (see §6y). It rides the section header so a
// future k is told apart instead of misread.
const contentFilterHashes = 1

// contentFilterClassBits lists the bloom bit sizes by size class: 1024<<class
// bits (128 B–2 KiB). The class is picked from the measured distinct trigram
// count so small files stay small and dense files do not saturate.
var contentFilterClassBits = []uint32{1024, 2048, 4096, 8192, 16384}

// contentFilterClassForGrams picks the smallest class whose bits outnumber
// twice the file's distinct trigram count (n/m ≤ 0.5, occupancy ≤ 0.39 with
// k=1). Past class 4 (n > 8192) the filter saturates gracefully: every extra
// gram only raises the false-positive rate, never a false negative.
func contentFilterClassForGrams(n int) int {
	for c, b := range contentFilterClassBits {
		if int64(b) >= int64(n)*2 {
			return c
		}
	}
	return len(contentFilterClassBits) - 1
}

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

// contentFilterRow is one document's filter bits: the class sizes the bits,
// which alias the mapped CXFL section (or heap copies in tests).
type contentFilterRow struct {
	Class int
	Bits  []byte
}

// contentFilterMayMatch reports whether bits contain every query gram: a
// necessary (not sufficient) condition for the file to match. A true match
// always passes; anything else is a false positive the disk verification
// rejects. Empty grams (sub-3-byte leaves) constrain nothing.
func contentFilterMayMatch(row contentFilterRow, grams []uint32) bool {
	if len(grams) == 0 {
		return true
	}
	if len(row.Bits) == 0 {
		return false
	}
	m := uint32(len(row.Bits)) * 8
	for _, g := range grams {
		b := contentFilterHash(g, m)
		if row.Bits[b/8]&(1<<(b%8)) == 0 {
			return false
		}
	}
	return true
}

// contentFilterSection is a decoded CXFL section: the build identity plus one
// row per document in DocID order.
type contentFilterSection struct {
	MaxRaw       int64
	MaxText      int64
	CapsExplicit bool
	Root         string
	Encoding     string
	Rows         []contentFilterRow
}

// encodeContentFilterSection serializes rows in DocID order with the build
// identity. Row order is canonical, so encoding the same table twice yields
// identical bytes.
func encodeContentFilterSection(maxRaw, maxText int64, capsExplicit bool, root, encoding string, rows []contentFilterRow) []byte {
	var body bytes.Buffer
	var tmp [binary.MaxVarintLen64]byte
	header := make([]byte, 4+8+8+4)
	binary.LittleEndian.PutUint32(header[0:], uint32(len(rows)))
	binary.LittleEndian.PutUint64(header[4:], uint64(maxRaw))
	binary.LittleEndian.PutUint64(header[12:], uint64(maxText))
	binary.LittleEndian.PutUint32(header[20:], contentFilterHashes)
	body.Write(header)
	n := binary.PutUvarint(tmp[:], uint64(len(root)))
	body.Write(tmp[:n])
	body.WriteString(root)
	n = binary.PutUvarint(tmp[:], uint64(len(encoding)))
	body.Write(tmp[:n])
	body.WriteString(encoding)
	// Flags: bit 0 set when the caps are uniform operator values.
	var flags uint64
	if capsExplicit {
		flags |= 1
	}
	n = binary.PutUvarint(tmp[:], flags)
	body.Write(tmp[:n])
	for i := range rows {
		r := &rows[i]
		class := r.Class
		if class < 0 || class >= len(contentFilterClassBits) ||
			int(contentFilterClassBits[class]/8) != len(r.Bits) {
			// A caller-supplied bitset of non-class width takes the class
			// matching its width (or the top class). Builders always supply
			// the exact width.
			class = len(contentFilterClassBits) - 1
			for c, b := range contentFilterClassBits {
				if int(b/8) >= len(r.Bits) {
					class = c
					break
				}
			}
		}
		body.WriteByte(byte(class))
		want := int(contentFilterClassBits[class] / 8)
		if len(r.Bits) == want {
			body.Write(r.Bits)
		} else {
			buf := make([]byte, want)
			copy(buf, r.Bits)
			body.Write(buf)
		}
	}
	return body.Bytes()
}

// decodeContentFilterSection parses a CXFL section for a doc table of
// docCount documents. A hash-count mismatch, a truncated row, or a count
// mismatch is an error: the caller treats the index as corrupt (never serves
// from suspect bytes). Bit slices alias data (the mapped section).
func decodeContentFilterSection(data []byte, docCount int) (*contentFilterSection, error) {
	if len(data) < 24 {
		return nil, errors.New("content filter section truncated")
	}
	count := int(binary.LittleEndian.Uint32(data[0:]))
	if count != docCount {
		return nil, fmt.Errorf("content filter row count %d != doc count %d", count, docCount)
	}
	if hashes := binary.LittleEndian.Uint32(data[20:]); hashes != contentFilterHashes {
		return nil, fmt.Errorf("unsupported content filter hash count %d", hashes)
	}
	f := &contentFilterSection{
		MaxRaw:  int64(binary.LittleEndian.Uint64(data[4:])),
		MaxText: int64(binary.LittleEndian.Uint64(data[12:])),
	}
	off := 24
	rl, n := binary.Uvarint(data[off:])
	if n <= 0 || rl > 1<<20 {
		return nil, errors.New("content filter root truncated")
	}
	off += n
	if off+int(rl) > len(data) {
		return nil, errors.New("content filter root truncated")
	}
	f.Root = string(data[off : off+int(rl)])
	off += int(rl)
	el, n := binary.Uvarint(data[off:])
	if n <= 0 || el > 1<<10 {
		return nil, errors.New("content filter encoding truncated")
	}
	off += n
	if off+int(el) > len(data) {
		return nil, errors.New("content filter encoding truncated")
	}
	f.Encoding = string(data[off : off+int(el)])
	off += int(el)
	fl, n := binary.Uvarint(data[off:])
	if n <= 0 {
		return nil, errors.New("content filter flags truncated")
	}
	off += n
	f.CapsExplicit = fl&1 != 0
	if fl&^uint64(1) != 0 {
		return nil, fmt.Errorf("unsupported content filter flags %d", fl)
	}
	f.Rows = make([]contentFilterRow, 0, count)
	for i := 0; i < count; i++ {
		if off+1 > len(data) {
			return nil, errors.New("content filter row truncated")
		}
		class := int(data[off])
		off++
		if class < 0 || class >= len(contentFilterClassBits) {
			return nil, errors.New("content filter row class out of range")
		}
		want := int(contentFilterClassBits[class] / 8)
		if off+want > len(data) {
			return nil, errors.New("content filter row truncated")
		}
		f.Rows = append(f.Rows, contentFilterRow{Class: class, Bits: data[off : off+want]})
		off += want
	}
	if off != len(data) {
		return nil, errors.New("content filter length mismatch")
	}
	return f, nil
}

// frameContentFilterFile reads DocID-ordered filter rows (one class byte
// plus the class bits per document, as the assembler spooled them) and
// frames them with the build identity into a CXFL section file. Rows are
// self-delimiting by class, so a short file fails instead of misaligning.
func frameContentFilterFile(rowsPath, filterPath string, id contentFilterID, count int) error {
	raw, err := os.ReadFile(rowsPath)
	if err != nil {
		return err
	}
	rows := make([]contentFilterRow, 0, count)
	off := 0
	for len(rows) < count {
		if off+1 > len(raw) {
			return errors.New("content filter rows truncated")
		}
		class := int(raw[off])
		off++
		if class < 0 || class >= len(contentFilterClassBits) {
			return errors.New("content filter row class out of range")
		}
		want := int(contentFilterClassBits[class] / 8)
		if off+want > len(raw) {
			return errors.New("content filter rows truncated")
		}
		rows = append(rows, contentFilterRow{Class: class, Bits: raw[off : off+want]})
		off += want
	}
	if off != len(raw) {
		return errors.New("content filter rows length mismatch")
	}
	return os.WriteFile(filterPath, encodeContentFilterSection(id.MaxRaw, id.MaxText, id.CapsExplicit, id.Root, id.Encoding, rows), 0o600)
}

// contentFilterLeafGrams returns the folded distinct trigram keys constraining
// one term/phrase leaf for the prefilter. The leaf is always folded — even
// for a case-sensitive query, whose raw match implies the folded match — so
// the prefilter stays a superset. Empty for sub-3-byte leaves.
func contentFilterLeafGrams(leafText string) []uint32 {
	return contentFilterGramsOf(contentFoldAccent([]byte(leafText)))
}

// contentFilterRegexGramsMatch reports whether precomputed required-literal
// grams can hold in the row: some disjunct's every run must may-match. A nil
// disjunct set means no sound prefilter exists (no provable literals, or an
// unconstrained disjunct a match may take while containing none of the other
// disjuncts' runs): no constraint.
func contentFilterRegexGramsMatch(row contentFilterRow, disjuncts [][][]uint32) bool {
	if disjuncts == nil {
		return true
	}
	for _, runs := range disjuncts {
		whole := true
		for _, grams := range runs {
			if len(grams) > 0 && !contentFilterMayMatch(row, grams) {
				whole = false
				break
			}
		}
		if whole {
			return true
		}
	}
	return false
}

// contentFilterGateOk is the tier-2 candidate gate over one document's filter
// row: every top-level positive term leaf must may-match, every regex leaf
// must hold a disjunct, and content-driven OR groups need a passing
// alternative. NOT leaves are exclusion and constrain nothing. Anything it
// cannot prove is verified, so it is sound (no false negatives) by
// construction: every check is a mayMatch superset test. Leaf grams come
// precomputed from the matcher, so the per-record cost is bit tests only.
func contentFilterGateOk(row contentFilterRow, pq parsedQuery, m *contentLeafMatcher) bool {
	for _, leaf := range pq.Content {
		if leaf.Kind == contentLeafRegex {
			if !contentFilterRegexGramsMatch(row, m.filterRegexGramsFor(leaf.LeafID)) {
				return false
			}
			continue
		}
		if grams := m.filterGramsFor(leaf.LeafID); len(grams) > 0 && !contentFilterMayMatch(row, grams) {
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
		if !contentFilterGroupOk(row, group, m) {
			return false
		}
	}
	return true
}

// contentFilterGroupOk reports whether an OR group's constraints can hold: an
// alternative without content constraints always can, so the group passes
// when any alternative passes — mirroring the tier-1 union streams.
func contentFilterGroupOk(row contentFilterRow, group []parsedQuery, m *contentLeafMatcher) bool {
	for i := range group {
		if contentFilterAltOk(row, group[i], m) {
			return true
		}
	}
	return false
}

// contentFilterAltOk reports whether one OR alternative's positive content
// constraints can hold in the row.
func contentFilterAltOk(row contentFilterRow, alt parsedQuery, m *contentLeafMatcher) bool {
	for _, leaf := range alt.Content {
		if leaf.Kind == contentLeafRegex {
			if !contentFilterRegexGramsMatch(row, m.filterRegexGramsFor(leaf.LeafID)) {
				return false
			}
			continue
		}
		if grams := m.filterGramsFor(leaf.LeafID); len(grams) > 0 && !contentFilterMayMatch(row, grams) {
			return false
		}
	}
	for _, group := range alt.OrGroups {
		if !contentFilterGroupOk(row, group, m) {
			return false
		}
	}
	return true
}

// contentDiskReadForVerify opens path and extracts with the given settings
// (the same extractor and limits the build used). It reads the file's CURRENT
// bytes — verify-direct freshness: a size/mtime mismatch against the indexed
// fingerprint means stale index entries, but the bytes on disk still decide,
// and the drain re-extracts the file through the existing pipeline. ok=false
// means skip (missing/unreadable/unextractable/empty). The returned bytes are
// the case-preserving stored text.
//
// It is a var so tests can substitute fixture text for synthetic corpora
// (in-memory indexes over fake paths): the gate/predicate/ordering logic
// still runs exactly, only the read syscall is doubled. Tests that prove
// disk behavior itself (freshness, caps, offline CLI) use real files.
// Production never sets it.
var contentDiskReadForVerify = contentDiskExtractForVerify

// contentResolveForRead remaps a verify path before opening (test seam; nil
// in production). Fixtures whose record index resolves to synthetic paths
// (e.g. a `C:\` volume over temp-dir files) map them to the real files, so
// fold re-reads, queries, and builds all open the same bytes. Reads stay
// real (freshness-preserving); only the path is doubled. Production never
// sets it.
var contentResolveForRead func(path string) string

// contentDiskExtractForVerify is the production disk reader behind
// contentDiskReadForVerify: open path and extract with the given settings.
func contentDiskExtractForVerify(path string, settings contentExtractSettings) (stored []byte, ok bool) {
	stored, err := contentDiskExtractOnce(path, settings)
	if err == nil {
		return stored, true
	}
	// A transient sharing violation (AV indexer, a writer holding the file)
	// usually clears in milliseconds; retry once so a momentarily locked
	// file is not a recall hole. Anything else (missing file, directory,
	// offline placeholder, unextractable content) fails closed immediately.
	if err != errContentReadSkip && isContentSharingViolation(err) {
		time.Sleep(contentDiskLockedRetryDelay)
		stored, err = contentDiskExtractOnce(path, settings)
		return stored, err == nil
	}
	return nil, false
}

// contentDiskLockedRetryDelay paces the single locked-file retry. A var so
// tests run without the delay.
var contentDiskLockedRetryDelay = 20 * time.Millisecond

// errContentReadSkip marks a non-retryable read skip (missing/unreadable
// file surfaces through the slurp error instead; skipped, binary and empty
// extractions land here).
var errContentReadSkip = errors.New("content read skipped")

func contentDiskExtractOnce(path string, settings contentExtractSettings) ([]byte, error) {
	// Inclusion policy at verification time mirrors the build/drain gate
	// (contentBuildDocSafe/contentExtractDeltaDocReason): per-file caps
	// resolved the same way — explicit caps uniformly, policy defaults
	// repo-aware failing open to the in-repo caps when membership is
	// unknown (verify carries no scope matcher; see
	// contentInRepoFromContext) — plus the size-gated skips and the
	// minified-content heuristic. An indexed file passed the same gate
	// with the same bytes, so this only newly excludes files the build
	// would also decline; it never starves an indexed file.
	ctx := contentWithExtractSettings(context.Background(), settings)
	inRepo, _ := contentInRepoFromContext(ctx)
	fctx, fileRaw, _ := contentFileExtractContext(ctx, inRepo)
	s, err := contentSlurpNoRecall(path, fileRaw)
	if err != nil {
		return nil, err
	}
	defer s.close()
	if _, skip := contentExtractionSkip(path, s.size, fileRaw); skip {
		return nil, errContentReadSkip
	}
	e := contentExtractorForPath(path, s.headBytes())
	if e == nil {
		return nil, errContentReadSkip
	}
	res, err := contentExtractSafely(fctx, e, s.reader(), s.size)
	if err != nil || res.Skipped || res.Binary || len(res.Text) == 0 {
		return nil, errContentReadSkip
	}
	if res.Class == contentClassText && contentLooksMinified(res.Text) {
		return nil, errContentReadSkip
	}
	return contentStoredText(res.Text), nil
}
