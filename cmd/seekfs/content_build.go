package main

// Offline content-index builder. It walks a directory, extracts searchable text
// from each file with the registered extractors, and writes the CXDT/CXTR/CXGR/
// CXST/CXPT sections into a new `.gsx`.
//
// This is the P1 path used by `seekfs content-index` and `seekfs content`; the
// service-driven incremental path is P2.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// contentBuildOptions bounds and scopes a build. MaxFiles caps files scanned;
// MaxRaw caps the raw size of any one file; Under limits to a path prefix; Exts
// is an optional lowercase extension allowlist (with dots). An empty Exts means
// "any extractable text/code file".
type contentBuildOptions struct {
	MaxFiles int
	MaxRaw   int64
	Under    string
	Exts     map[string]struct{}
}

func defaultContentBuildOptions() contentBuildOptions {
	return contentBuildOptions{MaxFiles: 2_000_000, MaxRaw: contentExtractMaxRawBytes}
}

// allows reports whether a path is in scope for a build.
func (o contentBuildOptions) allows(path string) bool {
	if len(o.Exts) > 0 {
		if _, ok := o.Exts[contentPathExtension(path)]; !ok {
			return false
		}
	}
	if o.Under != "" && !contentPathUnder(path, o.Under) {
		return false
	}
	return true
}

// contentPathUnder reports whether path is under under, with a separator
// boundary so `sub` does not match `submarine`. Separators are normalized by
// filepath.Clean.
func contentPathUnder(path, under string) bool {
	p := strings.ToLower(filepath.Clean(path))
	u := strings.ToLower(filepath.Clean(under))
	if p == u {
		return true
	}
	if !strings.HasSuffix(u, string(filepath.Separator)) {
		u += string(filepath.Separator)
	}
	return strings.HasPrefix(p, u)
}

type contentBuildDoc struct {
	path    string
	frn     uint64
	text    []byte
	modUnix int64
}

// buildContentIndexFromDir builds an in-memory `.gsx` for root.
func buildContentIndexFromDir(ctx context.Context, root string, opts contentBuildOptions) (*contentIndex, error) {
	docs := make([]contentBuildDoc, 0, 4096)
	var scanned int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			name := strings.ToLower(d.Name())
			if name == ".git" || name == "node_modules" || name == ".opencode" || name == "dist" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		name := strings.ToLower(d.Name())
		// Never feed a content index (its own output, or a previous build) back
		// into the corpus.
		if strings.HasSuffix(name, ".gsx") || strings.HasPrefix(name, ".seekfs") {
			return nil
		}
		if !opts.allows(path) {
			return nil
		}
		scanned++
		if opts.MaxFiles > 0 && scanned > opts.MaxFiles {
			return filepath.SkipAll
		}
		f, ferr := os.Open(path)
		if ferr != nil {
			return nil
		}
		defer f.Close()
		info, serr := f.Stat()
		if serr != nil || info.IsDir() {
			return nil
		}
		size := info.Size()
		if opts.MaxRaw > 0 && size > opts.MaxRaw {
			return nil
		}
		head, _ := contentReadBounded(f, size, 512)
		e := contentExtractorForPath(path, head)
		if e == nil {
			return nil
		}
		res, eerr := e.Extract(ctx, f, size)
		if eerr != nil || res.Skipped || len(res.Text) == 0 {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		docs = append(docs, contentBuildDoc{path: filepath.ToSlash(rel), frn: contentPathKey(rel), text: res.Text, modUnix: info.ModTime().Unix()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	tmpDir, err := os.MkdirTemp("", "seekfs-content-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)
	return assembleContentIndex(docs, tmpDir)
}

// contentBuildArenaBytes bounds the external builder's raw-posting buffer; when
// it fills, sorted runs spill to disk and merge.
const contentBuildArenaBytes = 64 << 20

// assembleContentIndex turns extracted documents into a `.gsx`, assigning
// DocID = position in the FRN-sorted table. Postings are accumulated through the
// bounded external builder so the raw (key, docID, tf) maps are never held in
// memory; only the encoded section and the text store are.
func assembleContentIndex(build []contentBuildDoc, tmpDir string) (*contentIndex, error) {
	sort.Slice(build, func(i, j int) bool {
		if build[i].frn != build[j].frn {
			return build[i].frn < build[j].frn
		}
		return build[i].path < build[j].path
	})

	termBuilder, err := newContentExternalSectionBuilder(tmpDir, contentBuildArenaBytes)
	if err != nil {
		return nil, err
	}
	defer termBuilder.Close()
	gramBuilder, err := newContentExternalSectionBuilder(tmpDir, contentBuildArenaBytes)
	if err != nil {
		return nil, err
	}
	defer gramBuilder.Close()

	idx := newContentIndex()
	idx.Origin = contentOriginWalk
	idx.Docs = make([]contentDoc, len(build))
	var textStore bytes.Buffer
	paths := make([]string, len(build))

	for i := range build {
		d := &build[i]
		docID := uint32(i)
		paths[i] = d.path
		// Index and store lowercased text so grams, terms, and verification all
		// agree on a case-insensitive match. v1 content search is always
		// case-insensitive; per-leaf case handling is a later addition.
		lower := contentNormalizeText(d.text)
		textStore.Write(lower)
		idx.Docs[i] = contentDoc{
			DocID:       docID,
			FRN:         d.frn,
			DocLen:      uint32(len(lower)),
			RawSize:     int64(len(lower)),
			ModUnix:     d.modUnix,
			ContentHash: sha256Of(lower),
			TextOff:     uint64(textStore.Len() - len(lower)),
			TextLen:     uint32(len(lower)),
		}
		// Documents are visited in ascending docID order, so each key's Adds
		// arrive in ascending docID order as the builder requires.
		for term, tf := range contentTermsOf(lower) {
			termBuilder.Add(term, docID, tf)
		}
		for gram := range contentGramsOf(lower) {
			gramBuilder.Add(gram, docID, 1)
		}
	}

	if terms, err := termBuilder.Finish(); err != nil {
		return nil, err
	} else if len(terms) > 0 {
		idx.Sections[contentSectionTerms] = terms
	}
	if grams, err := gramBuilder.Finish(); err != nil {
		return nil, err
	} else if len(grams) > 0 {
		idx.Sections[contentSectionGrams] = grams
	}
	idx.Sections[contentSectionText] = textStore.Bytes()
	idx.Sections[contentSectionPaths] = encodeContentPaths(paths)
	return idx, nil
}

// buildContentIndexForIndex builds an FRN-keyed `.gsx` from a record index by
// reading each live file through its reconstructed path. This is the builder a
// USN volume needs: the offline directory builder keys docs by path hash and
// cannot join to record FRNs.
func buildContentIndexForIndex(ctx context.Context, idx *Index, opts contentBuildOptions) (*contentIndex, error) {
	if idx == nil {
		return nil, errors.New("nil record index")
	}
	cache := make(map[int]string, 1024)
	docs := make([]contentBuildDoc, 0, 4096)
	count := idx.compactRecordCount()
	for id := 0; id < count; id++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rec := idx.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) != 0 || rec.FRN == 0 {
			continue
		}
		if opts.MaxRaw > 0 && rec.Size > opts.MaxRaw {
			continue
		}
		path := idx.reconstructCompactPathCached(id, cache)
		if path == "" || !opts.allows(path) {
			continue
		}
		f, err := contentOpenNoRecall(path)
		if err != nil {
			continue
		}
		info, serr := f.Stat()
		if serr != nil || info.IsDir() {
			f.Close()
			continue
		}
		size := info.Size()
		head, _ := contentReadBounded(f, size, 512)
		e := contentExtractorForPath(path, head)
		if e == nil {
			f.Close()
			continue
		}
		res, eerr := e.Extract(ctx, f, size)
		f.Close()
		if eerr != nil || res.Skipped || len(res.Text) == 0 {
			continue
		}
		docs = append(docs, contentBuildDoc{path: path, frn: rec.FRN, text: res.Text, modUnix: info.ModTime().Unix()})
	}
	tmpDir, err := os.MkdirTemp("", "seekfs-content-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)
	cidx, err := assembleContentIndex(docs, tmpDir)
	if err != nil {
		return nil, err
	}
	cidx.Origin = contentOriginUSN
	// Stamp the content USN checkpoint from the record index metadata: the
	// base is complete as of the .gsi checkpoint, so restart catch-up resumes
	// from here rather than re-reading the whole journal (WP0/PB3).
	cidx.JournalID = idx.JournalID
	if idx.Checkpoint > 0 {
		cidx.CheckpointUSN = uint64(idx.Checkpoint)
	}
	return cidx, nil
}

// contentNormalizeText is the one normalization every builder and the delta
// extractor must share: lowercase, then lossy repair. Hashing the normalized
// text makes base and delta ContentHash comparable, so a touch-only write is
// recognized as unchanged.
func contentNormalizeText(decoded []byte) []byte {
	lower := []byte(strings.ToLower(string(decoded)))
	return []byte(contentDecodeForIndex(lower, contentEncodingMode{auto: true}))
}

// contentPathKey is the stable 64-bit identity of a walk-relative path: FNV-1a
// 64-bit. The value is persisted in contentDoc.FRN and resolved by content
// lookups, so the constants are frozen by TestContentPathKeyIsFrozen; changing
// them silently invalidates every built `.gsx`.
func contentPathKey(rel string) uint64 {
	const (
		fnvOffset64 = 14695981039346656037
		fnvPrime64  = 1099511628211
	)
	h := uint64(fnvOffset64)
	for i := 0; i < len(rel); i++ {
		h ^= uint64(rel[i])
		h *= fnvPrime64
	}
	return h
}

// contentTermsOf tokenizes text into lowercase words with term frequencies.
// Word bytes are ASCII alphanumerics, underscore, and any byte >= 0x80 (so a
// UTF-8 word stays one term).
func contentTermsOf(text []byte) map[string]uint32 {
	out := make(map[string]uint32)
	start := -1
	for i := 0; i <= len(text); i++ {
		var isWord bool
		if i < len(text) {
			b := text[i]
			isWord = (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b >= 0x80
		}
		if isWord {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			term := strings.ToLower(string(text[start:i]))
			out[term]++
			start = -1
		}
	}
	return out
}

// contentGramsOf returns the distinct 3-byte trigrams of text, skipping grams
// that contain a control byte.
func contentGramsOf(text []byte) map[string]struct{} {
	out := make(map[string]struct{}, len(text)/4)
	for i := 0; i+3 <= len(text); i++ {
		g := text[i : i+3]
		if g[0] < 32 || g[1] < 32 || g[2] < 32 {
			continue
		}
		out[string(g)] = struct{}{}
	}
	return out
}

// encodeContentPaths serializes docID -> path.
func encodeContentPaths(paths []string) []byte {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(paths)))
	for _, p := range paths {
		if len(p) > int(^uint16(0)) {
			p = p[:int(^uint16(0))]
		}
		_ = binary.Write(&buf, binary.LittleEndian, uint16(len(p)))
		buf.WriteString(p)
	}
	return buf.Bytes()
}

func decodeContentPaths(data []byte, docCount int) ([]string, error) {
	if len(data) < 4 {
		return nil, errors.New("content paths truncated")
	}
	n := int(binary.LittleEndian.Uint32(data[:4]))
	if n != docCount {
		return nil, errors.New("content paths count mismatch")
	}
	out := make([]string, n)
	off := 4
	for i := 0; i < n; i++ {
		if off+2 > len(data) {
			return nil, errors.New("content paths truncated")
		}
		l := int(binary.LittleEndian.Uint16(data[off:]))
		off += 2
		if off+l > len(data) {
			return nil, errors.New("content paths truncated")
		}
		out[i] = string(data[off : off+l])
		off += l
	}
	return out, nil
}
