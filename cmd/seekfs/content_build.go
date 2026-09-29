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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// contentBuildOptions bounds and scopes a build. MaxFiles caps files scanned;
// MaxRaw/MaxText cap the raw source and extracted text a file contributes (0 =
// default 32/16 MiB, always clamped to the hard safety ceiling); Encoding is a
// WHATWG label ("auto", "none", "latin1", ...); Under limits to a path prefix;
// Exts is an optional lowercase extension allowlist (with dots). An empty Exts
// means "any extractable text/code file".
type contentBuildOptions struct {
	MaxFiles int
	MaxRaw   int64
	MaxText  int64
	Encoding string
	Under    string
	Exts     map[string]struct{}
	// Scope, when non-nil, replaces the extension + single-Under gate with the
	// resolved auto scope (roots, excludes, git worktrees, extension registry).
	// The caller resolves it (contentScope.resolve) after detecting repos.
	Scope *contentScopeResolved
}

func defaultContentBuildOptions() contentBuildOptions {
	return contentBuildOptions{
		MaxFiles: 2_000_000,
		MaxRaw:   contentExtractMaxRawBytes,
		MaxText:  contentExtractMaxTextBytes,
		Encoding: "auto",
	}
}

// extractSettings resolves the options into the per-build extraction policy
// (encoding + normalized, clamped caps) that extractors read from the context.
func (o contentBuildOptions) extractSettings() contentExtractSettings {
	s := contentDefaultExtractSettings()
	if o.Encoding != "" {
		if m, err := parseContentEncoding(o.Encoding); err == nil {
			s.encoding = m
		}
	}
	if o.MaxRaw > 0 {
		s.maxRaw = o.MaxRaw
	}
	if o.MaxText > 0 {
		s.maxText = o.MaxText
	}
	s.maxRaw, s.maxText = contentNormalizeExtractCaps(s.maxRaw, s.maxText)
	return s
}

// allows reports whether a path is in scope for a build.
func (o contentBuildOptions) allows(path string) bool {
	if o.Scope != nil {
		_, _, ok := o.Scope.allows(path)
		return ok
	}
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
	// class and version identify the extractor that produced text: the
	// contentClass* and its Version(). assembleContentIndexStream stamps them
	// into the doc table so per-class invalidation can see a version bump.
	class   uint16
	version uint16
}

// buildContentIndexFromDir builds an in-memory `.gsx` for root.
func buildContentIndexFromDir(ctx context.Context, root string, opts contentBuildOptions) (*contentIndex, error) {
	settings := opts.extractSettings()
	ctx = contentWithExtractSettings(ctx, settings)
	docs := make([]contentBuildDoc, 0, 4096)
	var scanned int
	var skipped, truncated, declined int64
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// A failure on the root itself (a typo'd or inaccessible path) is
			// fatal — otherwise an empty index is silently published. Failures
			// below the root stay best-effort.
			if d == nil || path == root {
				return err
			}
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
		head, _ := contentReadBounded(f, size, 512)
		e := contentExtractorForPath(path, head)
		if e == nil {
			// No extractor: a binary/unsupported file, or an empty one the
			// sniffer declined. Tallied so the counters reconcile.
			declined++
			return nil
		}
		// The extractor applies the raw/text policy: an over-cap container is
		// Skipped with a reason, an over-cap text file contributes a bounded,
		// Truncated prefix. Both are tallied into the policy section. The call
		// is panic-isolated and deadline-bounded (contentExtractSafely), so one
		// hostile file cannot crash or wedge the CLI build.
		res, eerr := contentExtractSafely(ctx, e, f, size)
		if eerr != nil {
			skipped++
			return nil
		}
		if res.Skipped {
			skipped++
			return nil
		}
		// Count a bounded result even when it produced no text (e.g. a depth-cap
		// hit), so the policy counters reflect every file the extractor handled.
		if res.Truncated {
			truncated++
		}
		if len(res.Text) == 0 {
			declined++
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		docs = append(docs, contentBuildDoc{path: filepath.ToSlash(rel), frn: contentPathKey(rel), text: res.Text, modUnix: info.ModTime().Unix(), class: res.Class, version: e.Version()})
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
	idx, err := assembleContentIndex(docs, tmpDir)
	if err != nil {
		return nil, err
	}
	idx.Policy = contentBuildPolicy{MaxRaw: settings.maxRaw, MaxText: settings.maxText, Skipped: skipped, Truncated: truncated}
	idx.Scanned = int64(scanned)
	idx.Declined = declined
	return idx, nil
}

// contentBuildArenaBytes bounds the external builder's raw-posting buffer; when
// it fills, sorted runs spill to disk and merge.
const contentBuildArenaBytes = 64 << 20

// contentBuildSource yields extracted documents one at a time for
// assembleContentIndexStream. It must return docs in ascending (frn, path)
// order, the order docIDs are assigned in. ok=false ends the stream; a non-nil
// error aborts the build.
type contentBuildSource func() (doc contentBuildDoc, ok bool, err error)

// contentAssembleStreamHook runs after each document has been normalized and
// its postings added, before the next one is pulled from the source. Tests use
// it to prove the assembler processes docs one at a time instead of buffering.
var contentAssembleStreamHook = func(docID int) {}

// assembleContentIndexStream is the streaming core of assembleContentIndex. It
// consumes documents from source in ascending (frn, path) order, assigning
// DocID = arrival position, and never retains the raw extracted corpus: each
// doc's text is normalized into the text store and tokenized into the bounded
// external posting builders as it arrives, then released. Postings spill to
// disk (contentBuildArenaBytes per builder). Peak transient heap is therefore
// one document's raw text plus the two posting arenas (plus O(docs) metadata:
// the doc table and paths) and the text store being assembled; the returned
// index is bounded by its encoded sections (text + postings + doc table +
// paths).
func assembleContentIndexStream(source contentBuildSource, tmpDir string) (*contentIndex, error) {
	if source == nil {
		return nil, errors.New("nil content build source")
	}
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
	var textStore bytes.Buffer
	var paths []string

	for {
		d, ok, err := source()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		docID := uint32(len(idx.Docs))
		paths = append(paths, d.path)
		// Store case-preserving repaired text (PF-6a): a case-sensitive query
		// verifies against these exact bytes, and snippets show the real case.
		// The index (terms and grams) is built from a case-folded copy, so the
		// case-insensitive prefilter stays a superset of every case-sensitive
		// match (tgrep's merged-case index, case-aware verify).
		stored := contentRepairText(d.text)
		folded := contentFoldText(stored)
		textStore.Write(stored)
		idx.Docs = append(idx.Docs, contentDoc{
			DocID:            docID,
			FRN:              d.frn,
			ContentType:      d.class,
			ExtractorVersion: d.version,
			DocLen:           uint32(len(folded)),
			RawSize:          int64(len(stored)),
			ModUnix:          d.modUnix,
			ContentHash:      sha256Of(stored),
			TextOff:          uint64(textStore.Len() - len(stored)),
			TextLen:          uint32(len(stored)),
		})
		// Documents arrive in ascending docID order, so each key's Adds arrive
		// in ascending docID order as the builder requires.
		for term, tf := range contentTermsOf(folded) {
			termBuilder.Add(term, docID, tf)
		}
		for gram := range contentGramsOf(folded) {
			gramBuilder.Add(gram, docID, 1)
		}
		contentAssembleStreamHook(int(docID))
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

// assembleContentIndex turns extracted documents into a `.gsx`, assigning
// DocID = position in the FRN-sorted table. It sorts build and streams it
// through assembleContentIndexStream, so the raw-posting maps and the raw
// extracted corpus are never all held at once.
func assembleContentIndex(build []contentBuildDoc, tmpDir string) (*contentIndex, error) {
	sort.Slice(build, func(i, j int) bool {
		if build[i].frn != build[j].frn {
			return build[i].frn < build[j].frn
		}
		return build[i].path < build[j].path
	})
	i := 0
	return assembleContentIndexStream(func() (contentBuildDoc, bool, error) {
		if i >= len(build) {
			return contentBuildDoc{}, false, nil
		}
		d := build[i]
		i++
		return d, true, nil
	}, tmpDir)
}

// buildContentIndexForIndex builds an FRN-keyed `.gsx` from a record index by
// reading each live file through its reconstructed path. This is the builder a
// USN volume needs: the offline directory builder keys docs by path hash and
// cannot join to record FRNs.
func buildContentIndexForIndex(ctx context.Context, idx *Index, opts contentBuildOptions) (*contentIndex, error) {
	if idx == nil {
		return nil, errors.New("nil record index")
	}
	settings := opts.extractSettings()
	ctx = contentWithExtractSettings(ctx, settings)
	cache := make(map[int]string, 1024)
	docs := make([]contentBuildDoc, 0, 4096)
	var skipped, truncated int64
	var scopeBytes int64
	count := idx.compactRecordCount()
	for id := 0; id < count; id++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rec := idx.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) != 0 || rec.FRN == 0 {
			continue
		}
		path := idx.reconstructCompactPathCached(id, cache)
		if path == "" || !opts.allows(path) {
			continue
		}
		doc, res, ok := contentBuildDocSafe(ctx, contentBuildItem{frn: rec.FRN, path: path})
		if res.Truncated {
			truncated++
		}
		if ok {
			if opts.Scope != nil {
				n := int64(len(doc.text))
				if n > opts.Scope.BudgetBytes-scopeBytes {
					return nil, fmt.Errorf("content-index: scoped build exceeds %d-byte extracted-text budget; narrow the scope or raise budget_bytes", opts.Scope.BudgetBytes)
				}
				scopeBytes += n
			}
			docs = append(docs, doc)
			continue
		}
		if res.Skipped {
			skipped++
		}
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
	cidx.Policy = contentBuildPolicy{MaxRaw: settings.maxRaw, MaxText: settings.maxText, Skipped: skipped, Truncated: truncated}
	// Stamp the content USN checkpoint from the record index metadata: the
	// base is complete as of the .gsi checkpoint, so restart catch-up resumes
	// from here rather than re-reading the whole journal (WP0/PB3).
	cidx.JournalID = idx.JournalID
	if idx.Checkpoint > 0 {
		cidx.CheckpointUSN = uint64(idx.Checkpoint)
	}
	return cidx, nil
}

// contentRepairText is the repair half of the old contentNormalizeText: it
// decodes under the auto (legacy-aware) policy and PRESERVES CASE. This is the
// one repair every builder, the delta extractor, and a later search must share,
// so the bytes stored in the text store and the bytes a query matches against
// are identical. The input is already-decoded text, so the re-decode is
// normally the identity. It deliberately uses contentAutoEncoding here -- not
// the extractor's own mode -- which is not the same decode as the extractor's
// under -encoding <label> or none, but is benign: those modes already produce
// valid UTF-8, which auto returns unchanged. Using auto (rather than the bare
// lossy-repair zero mode) keeps base and delta from decoding a stray byte
// differently. Hashing the repaired text makes base and delta ContentHash
// comparable, so a touch-only write is recognized as unchanged (and a
// case-only change is, correctly, a change).
func contentRepairText(decoded []byte) []byte {
	return []byte(contentDecodeForIndex(decoded, contentAutoEncoding, false))
}

// contentFoldText is the case-fold half of the old contentNormalizeText: the
// lowercase view the index (terms and grams) and case-insensitive verification
// use. strings.ToLower is Unicode-aware (one rune in, one rune out), so a
// case-insensitive query folds both sides with the same rule and never loses a
// case-sensitive candidate. The input is the repaired, case-preserving stored
// text; a query leaf is folded directly.
func contentFoldText(repaired []byte) []byte {
	return []byte(strings.ToLower(string(repaired)))
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
