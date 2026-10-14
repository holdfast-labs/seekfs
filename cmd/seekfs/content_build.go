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
	"sync"
	"time"
	"unicode/utf8"
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
	// ScanTally, when non-nil, receives per-reason selection rejections
	// (files + source bytes) from the scoped scan for the CXIS stats.
	ScanTally *contentScanTally
	// WorktreesAll indexes nested linked git worktrees in walk builds;
	// false (default) indexes only the main checkout, like the service
	// scope's worktrees=main.
	WorktreesAll bool
}

func defaultContentBuildOptions() contentBuildOptions {
	// MaxRaw/MaxText 0 means the inclusion-policy defaults (per-file
	// caps: 1 MiB text outside git repos, 10 MiB inside). An explicit
	// -max-raw/-max-text overrides both uniformly.
	return contentBuildOptions{
		MaxFiles: 2_000_000,
		Encoding: "auto",
	}
}

// extractSettings resolves the options into the per-build extraction policy
// (encoding + normalized, clamped caps) that extractors read from the context.
// Zero caps stay zero here (policy defaults, resolved per file at
// extraction); contentWithExtractSettings normalizes them for extractors
// that read the context directly.
func (o contentBuildOptions) extractSettings() contentExtractSettings {
	s := contentDefaultExtractSettings()
	s.maxRaw, s.maxText = 0, 0
	s.capsExplicit = o.MaxRaw > 0 || o.MaxText > 0
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
	if s.capsExplicit {
		s.maxRaw, s.maxText = contentNormalizeExtractCaps(s.maxRaw, s.maxText)
	}
	return s
}

// allows reports whether a path is in scope for a build.
func (o contentBuildOptions) allows(path string) bool {
	return o.allowsSize(path, -1)
}

// allowsSize is allows with the source size for the size-gated inclusion
// rules (large JSON). Walk builders pass the directory entry's size.
func (o contentBuildOptions) allowsSize(path string, size int64) bool {
	if o.Scope != nil {
		_, _, ok := o.Scope.allowsSize(path, size)
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
	// fileSize and fileModNano are the source file's byte size and nanosecond
	// mtime at extraction time. The walk reconcile diffs them against the
	// filename index; every producer stamps them from the same stat the
	// extraction read.
	fileSize    int64
	fileModNano int64
}

// contentWalkDoc is one extracted file awaiting assembly: the spool offset of
// its compressed frame plus the identity the doc table needs. The walk builders
// spool text to disk during the walk and assemble in FRN order afterwards, so
// a large corpus never sits in memory as raw text. offset addresses the
// frame in the compressed spool (see content_spool.go); spoolLen is the
// frame's compressed length and length the doc's original length.
type contentWalkDoc struct {
	item     contentBuildItem
	offset   int64
	length   int
	spoolLen int
	modUnix  int64
	class    uint16
	version  uint16
	// fileSize and fileModNano are the source file's byte size and nanosecond
	// mtime at extraction time (walk reconcile fingerprint); every producer
	// stamps them from the same stat the extraction read.
	fileSize    int64
	fileModNano int64
}

// contentWalkSpool streams extracted documents back out of a compressed spool
// file in the order order lists (indexes into spooled).
func contentWalkSpool(f *os.File, spooled []contentWalkDoc, order []int) (contentBuildSource, error) {
	r, err := newContentSpoolReader(f)
	if err != nil {
		return nil, err
	}
	next := 0
	return func() (contentBuildDoc, bool, error) {
		if next == len(order) {
			return contentBuildDoc{}, false, nil
		}
		s := spooled[order[next]]
		next++
		text, err := r.Read(s.offset, s.spoolLen, s.length)
		if err != nil {
			return contentBuildDoc{}, false, err
		}
		return contentBuildDoc{path: s.item.path, frn: s.item.frn, text: text, modUnix: s.modUnix, class: s.class, version: s.version, fileSize: s.fileSize, fileModNano: s.fileModNano}, true, nil
	}, nil
}

// buildContentIndexFromDir builds an in-memory `.gsx` for root.
func buildContentIndexFromDir(ctx context.Context, root string, opts contentBuildOptions) (*contentIndex, error) {
	settings := opts.extractSettings()
	ctx = contentWithExtractSettings(ctx, settings)
	tmpDir, err := os.MkdirTemp("", "seekfs-content-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)
	spool, err := os.Create(filepath.Join(tmpDir, "text.spool"))
	if err != nil {
		return nil, err
	}
	defer spool.Close()
	spoolWriter, err := newContentSpoolWriter(spool)
	if err != nil {
		return nil, err
	}
	defer spoolWriter.Close()
	var spooled []contentWalkDoc
	var scanned int
	var skipped, truncated, declined, binary int64
	var inclusion contentInclusionStats
	// Inside a git worktree, git's ignore rules decide what is skipped; the
	// name-based skips below only apply outside any worktree.
	absRootOrig, aerr := filepath.Abs(root)
	if aerr != nil {
		absRootOrig = root
	}
	// absRoot is the folded form for worktree registration and lookup (the
	// filesystem is case-insensitive); ignore-pattern matching always runs on
	// original-case absolute paths via absOfOrig, because patterns match
	// case-sensitively like git and ripgrep.
	absRoot := contentNormPath(absRootOrig)
	ignore := newContentGitIgnore(nil)
	for dir := absRoot; ; {
		if _, serr := os.Lstat(filepath.Join(dir, ".git")); serr == nil {
			ignore.addRepo(dir)
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if ignore.ignored(absRootOrig, true) {
		// An explicitly requested root that git ignores (repo\out) is
		// indexed under the outside-worktree rules, not skipped entirely.
		ignore = newContentGitIgnore(nil)
	}
	absOf := func(path string) string {
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return contentNormPath(path)
		}
		return contentNormPath(filepath.Join(absRoot, rel))
	}
	absOfOrig := func(path string) string {
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return path
		}
		return filepath.Join(absRootOrig, rel)
	}
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
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
			if name == ".git" {
				return filepath.SkipDir
			}
			abs := absOf(path)
			if path != root {
				if _, serr := os.Lstat(filepath.Join(path, ".git")); serr == nil {
					ignore.addRepo(abs)
					// A nested linked worktree holds a second copy of
					// the main checkout: skip it unless worktrees=all.
					// The root itself is always indexed (explicitly
					// requested), even when it is a linked checkout.
					if !opts.WorktreesAll && contentLinkedWorktreeDir(path) {
						return filepath.SkipDir
					}
				}
			}
			if _, inRepo := ignore.repoFor(abs); inRepo {
				if path != root && ignore.ignored(absOfOrig(path), true) {
					return filepath.SkipDir
				}
				return nil
			}
			if name == "node_modules" || name == ".opencode" || name == "dist" || name == "vendor" {
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
		if ignore.ignored(absOfOrig(path), false) {
			return nil
		}
		// d.Info carries the size without an extra stat on Windows (the
		// find data already holds it); unknown sizes stay admitted for
		// the size-gated rules and meet them at extraction.
		var entrySize int64 = -1
		if info, ierr := d.Info(); ierr == nil && info != nil {
			entrySize = info.Size()
		}
		if !opts.allowsSize(path, entrySize) {
			return nil
		}
		_, inRepo := ignore.repoFor(absOf(path))
		fctx, fileRaw, _ := contentFileExtractContext(ctx, inRepo)
		scanned++
		if opts.MaxFiles > 0 && scanned > opts.MaxFiles {
			return filepath.SkipAll
		}
		// Single-read open without recalling cloud placeholders (the §4
		// invariant); the extractor reuses the slurped bytes in place.
		s, serr := contentSlurpNoRecall(path, fileRaw)
		if serr != nil {
			return nil
		}
		if reason, skip := contentExtractionSkip(path, s.size, fileRaw); skip {
			inclusion.add(reason, s.size)
			skipped++
			s.close()
			return nil
		}
		head := s.headBytes()
		e := contentExtractorForPath(path, head)
		if e == nil {
			// No extractor claimed the head: binary (NUL in the sniff
			// window), tallied apart from declined so the counters reconcile
			// as indexed == eligible - declined - skipped - binary.
			binary++
			s.close()
			return nil
		}
		// The extractor applies the raw/text policy: an over-cap container is
		// Skipped with a reason, an over-cap text file contributes a bounded,
		// Truncated prefix. Both are tallied into the policy section. The call
		// is panic-isolated and deadline-bounded (contentExtractSafely), so one
		// hostile file cannot crash or wedge the CLI build.
		res, eerr := contentExtractSafely(fctx, e, s.reader(), s.size)
		size, modUnix, modNano := s.size, s.modUnix, s.modNano
		s.close()
		if eerr != nil {
			skipped++
			return nil
		}
		if res.Binary {
			binary++
			return nil
		}
		if res.Skipped {
			skipped++
			if contentInclusionReason(res.Reason) {
				inclusion.add(res.Reason, size)
			}
			return nil
		}
		if res.Class == contentClassText && contentLooksMinified(res.Text) {
			inclusion.add(contentSkipMinified, size)
			skipped++
			return nil
		}
		// Count a bounded result even when it produced no text (e.g. a depth-cap
		// hit), so the policy counters reflect every file the extractor handled.
		if res.Truncated {
			truncated++
			inclusion.Truncated++
			if cut := size - int64(len(res.Text)); cut > 0 {
				inclusion.TruncatedBytes += cut
			}
		}
		if len(res.Text) == 0 {
			declined++
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		offset, compLen, serr := spoolWriter.Append(res.Text)
		if serr != nil {
			return serr
		}
		spooled = append(spooled, contentWalkDoc{
			item:        contentBuildItem{frn: contentPathKey(rel), path: rel},
			offset:      offset,
			length:      len(res.Text),
			spoolLen:    compLen,
			modUnix:     modUnix,
			class:       res.Class,
			version:     e.Version(),
			fileSize:    size,
			fileModNano: modNano,
		})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Slice(spooled, func(i, j int) bool {
		if spooled[i].item.frn != spooled[j].item.frn {
			return spooled[i].item.frn < spooled[j].item.frn
		}
		return spooled[i].item.path < spooled[j].item.path
	})
	order := make([]int, len(spooled))
	for i := range order {
		order[i] = i
	}
	idx, err := assembleContentIndexFromSpool(spool, spooled, order, tmpDir, contentFilterID{MaxRaw: settings.maxRaw, MaxText: settings.maxText, CapsExplicit: settings.capsExplicit, Root: absRootOrig, Encoding: opts.Encoding})
	if err != nil {
		return nil, err
	}
	polRaw, polText := contentInclusionCapsForFile(true, 0, 0)
	if settings.capsExplicit {
		polRaw, polText = settings.maxRaw, settings.maxText
	}
	idx.Policy = contentBuildPolicy{MaxRaw: polRaw, MaxText: polText, Skipped: skipped, Truncated: truncated, Binary: binary}
	// The assembler already stamped the dedupe totals; merge the
	// selection/extraction tallies around them.
	idx.Inclusion.merge(inclusion)
	idx.Scanned = int64(scanned)
	idx.Declined = declined
	return idx, nil
}

// assembleContentIndexFromSpool runs the streaming assembler over spooled docs
// and materializes the in-memory index (tests and small-corpus CLI builds).
// Production sharded paths use assembleContentIndexStreamed + the streaming
// save instead of materializing.
func assembleContentIndexFromSpool(spool *os.File, spooled []contentWalkDoc, order []int, tmpDir string, filterID contentFilterID) (*contentIndex, error) {
	src, err := contentWalkSpool(spool, spooled, order)
	if err != nil {
		return nil, err
	}
	return assembleContentIndexStream(src, tmpDir, filterID)
}

// contentBuildArenaBytes bounds the external builder's raw-posting buffer; when
// it fills, sorted runs spill to disk and merge. The counted bytes understate
// the resident records (string headers and slice growth), so the arena is sized
// well below the per-shard heap budget: a 64 MiB shard must assemble under
// ~250 MB of heap (see the shard-scale harness). A var so benchmarks can
// explore the run-count tradeoff without changing the production bound.
var contentBuildArenaBytes = 12 << 20

// contentBuildSource yields extracted documents one at a time for
// assembleContentIndexStream. It must return docs in ascending (frn, path)
// order, the order docIDs are assigned in. ok=false ends the stream; a non-nil
// error aborts the build.
type contentBuildSource func() (doc contentBuildDoc, ok bool, err error)

// contentAssembleStreamHook runs after each document has been normalized and
// its postings added, before the next batch is pulled from the source, in
// docID order. Tests use it to prove the assembler streams instead of
// buffering the whole source: at most one read-ahead batch
// (contentAssembleBatchDocs/Bytes) may be pulled ahead of the hook.
var contentAssembleStreamHook = func(docID int) {}

// assembleContentIndexStream is the streaming core of assembleContentIndex. It
// consumes documents from source in ascending (frn, path) order, assigning
// DocID = arrival position, and never retains the raw extracted corpus: each
// doc's text is tokenized into the bounded external posting builders as it
// arrives, then released. Postings spill to disk (contentBuildArenaBytes per
// builder). Peak transient heap is therefore one document's raw text plus the
// two posting arenas (plus O(docs) metadata: the doc table and paths); the
// returned index is bounded by its encoded sections (filter + postings + doc
// table + paths). (v11: no text store.)
func assembleContentIndexStream(source contentBuildSource, tmpDir string, filterID contentFilterID) (*contentIndex, error) {
	if source == nil {
		return nil, errors.New("nil content build source")
	}
	built, err := assembleContentIndexStreamed(source, tmpDir, filterID)
	if err != nil {
		return nil, err
	}
	idx := newContentIndex()
	idx.Origin = contentOriginWalk
	idx.Docs = built.docs
	idx.Aliases = built.aliasPairs
	idx.Inclusion.DedupeFiles = built.dedupeFiles
	idx.Inclusion.DedupeBytes = built.dedupeBytes
	if built.termPath != "" {
		terms, err := os.ReadFile(built.termPath)
		if err != nil {
			return nil, err
		}
		idx.Sections[contentSectionTerms] = terms
	}
	if built.gramPath != "" {
		grams, err := os.ReadFile(built.gramPath)
		if err != nil {
			return nil, err
		}
		idx.Sections[contentSectionGrams] = grams
	}
	if built.prunePath != "" {
		pruned, err := os.ReadFile(built.prunePath)
		if err != nil {
			return nil, err
		}
		idx.Sections[contentSectionPruned] = pruned
	}
	if built.filterPath != "" {
		filter, err := os.ReadFile(built.filterPath)
		if err != nil {
			return nil, err
		}
		idx.Sections[contentSectionFilter] = filter
	}
	idx.Sections[contentSectionPaths] = encodeContentPaths(built.paths)
	return idx, nil
}

// contentStreamedBuild is the assembled form of one shard: the doc table and
// paths in memory, the posting and filter sections spooled to temp files.
// It is what the production build/fold/reshard paths save with
// contentSaveAssembled and then reload through the mmap loader; the in-memory
// assembleContentIndexStream above stays for tests and small corpora.
type contentStreamedBuild struct {
	docs []contentDoc
	// paths parallels docs by DocID.
	paths []string
	// textSize is the extracted corpus size: shard planning and the reshard
	// threshold budget work by corpus size, not by encoded bytes.
	textSize uint64
	// termPath/gramPath hold the exact CXTR/CXGR section bytes, or "" when
	// the corpus has no terms/grams.
	termPath string
	gramPath string
	// prunePath holds the exact CXGP pruned-gram section bytes, or "" when
	// no gram exceeded contentGramPruneDF.
	prunePath string
	// filterPath holds the exact CXFL filter section bytes.
	filterPath string
	// aliasPairs holds the §6z dedupe map (alias DocID -> primary
	// DocID); dedupeFiles/Bytes tally the shared documents and the
	// extracted-text bytes sharing one postings entry.
	aliasPairs  []contentAliasPair
	dedupeFiles int64
	dedupeBytes int64
}

// contentAssembleBatchDocs and contentAssembleBatchBytes bound the streaming
// assembler's tokenize read-ahead: at most this many documents or bytes are
// pulled from the source before earlier docs are emitted in order. The bound
// keeps the "never retain the corpus" streaming guarantee (peak is one batch
// of raw text plus its token maps) while letting the CPU-bound normalize /
// hash / tokenize stage run on contentBuildWorkerCount workers. Vars so tests
// can pin the batching.
var (
	contentAssembleBatchDocs  = 64
	contentAssembleBatchBytes = int64(64 << 20)
)

// contentTokenizedDoc is one document after the parallel normalize stage:
// the extracted length and content hash plus the precomputed term/gram sets
// the serial emit stage merges into the posting builders in docID order. The
// normalized bytes themselves are transient (v11 stores no text); the filter
// row derives from the gram set.
type contentTokenizedDoc struct {
	textLen uint32
	docLen  uint32
	hash    [contentHashLen]byte
	terms   map[string]uint32
	grams   map[uint32]struct{}
}

// contentTokenizeDocForBuild normalizes one extracted document exactly the
// way the historical serial assembler did (repair, fold, hash, terms, grams),
// so parallel assembly produces byte-identical output.
func contentTokenizeDocForBuild(text []byte) contentTokenizedDoc {
	stored := contentStoredText(text)
	folded := contentFoldText(stored)
	return contentTokenizedDoc{
		textLen: uint32(len(stored)),
		docLen:  uint32(len(folded)),
		hash:    sha256Of(stored),
		terms:   contentTermsOfFolded(folded),
		grams:   contentGramsOfU32(folded),
	}
}

// contentGramU32String renders a trigram key back to its 3-byte string form
// for the posting builder.
func contentGramU32String(g uint32) string {
	var b [3]byte
	b[0] = byte(g >> 16)
	b[1] = byte(g >> 8)
	b[2] = byte(g)
	return string(b[:])
}

// contentFilterID carries the filter section's build identity: the caps the
// disk verification re-applies, the root relative document paths resolve
// against ("" for service builds, which verify through the live record-index
// path), and the encoding label the query decodes with. CapsExplicit records
// whether the caps are uniform operator values (verified exactly) or policy
// defaults (verified with the repo-aware per-file caps, failing open to the
// in-repo caps when membership is unknown). Fresh builds fill it from their
// extraction settings; folds copy it from the base section they replace.
type contentFilterID struct {
	MaxRaw       int64
	MaxText      int64
	CapsExplicit bool
	Root         string
	Encoding     string
}

// assembleContentIndexStreamed consumes documents like assembleContentIndexStream
// but never retains the corpus: postings merge straight into section files
// and filter rows spool to a temp file as docs arrive, so peak heap is one
// document's text plus the two posting arenas plus O(docs) metadata and the
// posting entry/block tables. (v11: no text store, no text dictionary.)
func assembleContentIndexStreamed(source contentBuildSource, tmpDir string, filterID contentFilterID) (*contentStreamedBuild, error) {
	if source == nil {
		return nil, errors.New("nil content build source")
	}
	// The terms section is dictionary-only in v7 (no postings: no query
	// path reads term postings), so it builds through the key-counting dict
	// builder; grams keep full postings through the section builder.
	dictBuilder, err := newContentExternalDictBuilder(tmpDir, contentBuildArenaBytes)
	if err != nil {
		return nil, err
	}
	defer dictBuilder.Close()
	gramBuilder, err := newContentExternalSectionBuilder(tmpDir, contentBuildArenaBytes)
	if err != nil {
		return nil, err
	}
	defer gramBuilder.Close()

	// The per-file filter rows spool in DocID order as docs arrive (one
	// class byte plus the class bits per doc, self-delimiting); the finish
	// step frames them with filterID into the CXFL section file.
	filterRows, err := os.Create(filepath.Join(tmpDir, "cxfilter.rows"))
	if err != nil {
		return nil, err
	}
	defer filterRows.Close()

	built := &contentStreamedBuild{}
	var docs []contentDoc
	var paths []string
	// Content-hash dedupe (§6z): extracted-text length + sha256 (already
	// computed per doc as the doc table's ContentHash) maps to the
	// primary's docID. Later identical documents become aliases:
	// full doc-table rows sharing the primary's postings entry (no
	// postings of their own) with their own filter row derivation, each
	// verifying from its own bytes on disk. Empty text never aliases.
	dedupeSeen := make(map[contentDedupeKey]uint32)
	var aliasPairs []contentAliasPair
	var dedupeFiles, dedupeBytes int64
	var textSize uint64

	asmErr := func() error {
		workers := contentBuildWorkerCount()
		if workers < 1 {
			workers = 1
		}
		for {
			// Bounded read-ahead: pull the next batch, then emit it in
			// order. The source is still consumed strictly front-to-back,
			// so DocID assignment and the text-store order are unchanged.
			// Each doc's bytes are copied at pull time: a source may reuse
			// its backing buffer on the next call, so retaining the slice
			// across pulls would alias overwritten bytes.
			var raws []contentBuildDoc
			var rawBytes int64
			for len(raws) < contentAssembleBatchDocs && rawBytes < contentAssembleBatchBytes {
				d, ok, err := source()
				if err != nil {
					return err
				}
				if !ok {
					break
				}
				d.text = bytes.Clone(d.text)
				rawBytes += int64(len(d.text))
				raws = append(raws, d)
				// A single huge document must still make progress when the
				// byte cap is already met by it alone.
				if len(raws) == 1 && rawBytes >= contentAssembleBatchBytes {
					break
				}
			}
			if len(raws) == 0 {
				break
			}
			toks := make([]contentTokenizedDoc, len(raws))
			tokStart := time.Now()
			if len(raws) == 1 || workers == 1 {
				for i := range raws {
					toks[i] = contentTokenizeDocForBuild(raws[i].text)
				}
			} else {
				n := workers
				if n > len(raws) {
					n = len(raws)
				}
				var wg sync.WaitGroup
				for w := 0; w < n; w++ {
					wg.Add(1)
					go func(w int) {
						defer wg.Done()
						for i := w; i < len(raws); i += n {
							toks[i] = contentTokenizeDocForBuild(raws[i].text)
						}
					}(w)
				}
				wg.Wait()
			}
			if pc := contentAssemblePhaseCapture; pc != nil {
				contentPhaseAdd(&pc.tokenize, time.Since(tokStart))
			}
			emitStart := time.Now()
			for i := range raws {
				d := &raws[i]
				t := &toks[i]
				docID := uint32(len(docs))
				paths = append(paths, d.path)
				// Identical content shares one postings entry (§6z): the
				// alias keeps its own identity (DocID, FRN, path,
				// fingerprint, hash) and its own filter row, contributing
				// no postings of its own; it verifies from its own bytes
				// on disk. Empty text never aliases.
				isAlias := false
				if t.textLen > 0 {
					key := contentDedupeKey{length: int(t.textLen), hash: t.hash}
					if prim, dup := dedupeSeen[key]; dup {
						aliasPairs = append(aliasPairs, contentAliasPair{Alias: docID, Primary: prim})
						dedupeFiles++
						dedupeBytes += int64(t.textLen)
						isAlias = true
					} else {
						dedupeSeen[key] = docID
					}
				}

				docs = append(docs, contentDoc{
					DocID:            docID,
					FRN:              d.frn,
					ContentType:      d.class,
					ExtractorVersion: d.version,
					DocLen:           t.docLen,
					RawSize:          int64(t.textLen),
					ModUnix:          d.modUnix,
					ContentHash:      t.hash,
					TextLen:          t.textLen,
					FileSize:         d.fileSize,
					FileModNano:      d.fileModNano,
				})
				// Documents arrive in ascending docID order, so each key's Adds arrive
				// in ascending docID order as the builder requires. The per-doc
				// term set is identical to the serial path's, and the builder
				// sorts on spill, so Add order within a doc cannot change the
				// encoded bytes. Each term is added once per document, so the
				// dict builder's Add count is exactly the document frequency.
				// Aliases contribute no postings (they share the primary's
				// entry); their filter row below still derives from the same
				// gram set, so every row stays dense in DocID order.
				if !isAlias {
					for term := range t.terms {
						dictBuilder.Add(term)
					}
					for gram := range t.grams {
						gramBuilder.Add(contentGramU32String(gram), docID)
					}
				}
				// The filter row derives from the same gram set (no extra
				// pass): class by measured distinct count, bits set inline.
				// Rows spool in DocID order (one class byte plus the class
				// bits per doc, self-delimiting); the finish step frames
				// them with filterID into the CXFL section file.
				class := contentFilterClassForGrams(len(t.grams))
				bits := make([]byte, contentFilterClassBits[class]/8)
				contentFilterBitsOfU32(bits, t.grams)
				if _, err := filterRows.Write([]byte{byte(class)}); err != nil {
					return err
				}
				if _, err := filterRows.Write(bits); err != nil {
					return err
				}
				textSize += uint64(t.textLen)
				contentAssembleStreamHook(int(docID))
			}
			if pc := contentAssemblePhaseCapture; pc != nil {
				contentPhaseAdd(&pc.emit, time.Since(emitStart))
			}
		}
		return nil
	}()
	if asmErr != nil {
		return nil, asmErr
	}
	// The rows spool is complete: close the writer so the framer reads a
	// stable file (the deferred Close is only the backstop).
	if err := filterRows.Close(); err != nil {
		return nil, err
	}
	rowsPath := filepath.Join(tmpDir, "cxfilter.rows")
	termPath := filepath.Join(tmpDir, "cxterm.sec")
	gramPath := filepath.Join(tmpDir, "cxgram.sec")
	filterPath := filepath.Join(tmpDir, "cxfilter.sec")
	// The term, gram and filter sections are independent files; finishing
	// them concurrently uses spare cores for the historically serial
	// FinishFile phase. The bytes of each file are unaffected.
	var finishWG sync.WaitGroup
	var termErr, gramErr, filterErr error
	finishWG.Add(3)
	go func() { defer finishWG.Done(); termErr = dictBuilder.FinishFile(termPath) }()
	go func() { defer finishWG.Done(); gramErr = gramBuilder.FinishFile(gramPath) }()
	go func() {
		defer finishWG.Done()
		filterErr = frameContentFilterFile(rowsPath, filterPath, filterID, len(docs))
	}()
	finishWG.Wait()
	if termErr != nil {
		_ = os.Remove(gramPath)
		_ = os.Remove(filterPath)
		return nil, termErr
	}
	if gramErr != nil {
		_ = os.Remove(termPath)
		_ = os.Remove(filterPath)
		return nil, gramErr
	}
	if filterErr != nil {
		_ = os.Remove(termPath)
		_ = os.Remove(gramPath)
		return nil, filterErr
	}
	// The pruned-gram set rides its own section file, written once the gram
	// merge (which decides the prunes) is done.
	prunePath := filepath.Join(tmpDir, "cxprune.sec")
	if sec := encodeContentPrunedGrams(gramBuilder.PrunedKeys()); len(sec) > 0 {
		if err := os.WriteFile(prunePath, sec, 0o600); err != nil {
			_ = os.Remove(termPath)
			_ = os.Remove(gramPath)
			_ = os.Remove(filterPath)
			return nil, err
		}
		built.prunePath = prunePath
	}
	built.docs = docs
	built.paths = paths
	built.aliasPairs = aliasPairs
	built.dedupeFiles = dedupeFiles
	built.dedupeBytes = dedupeBytes
	// textSize is the extracted corpus size: shard planning and the reshard
	// threshold budget work by corpus size, not by encoded bytes.
	built.textSize = textSize
	built.filterPath = filterPath
	if st, err := os.Stat(termPath); err == nil && st.Size() > 0 {
		built.termPath = termPath
	}
	if st, err := os.Stat(gramPath); err == nil && st.Size() > 0 {
		built.gramPath = gramPath
	}
	return built, nil
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
	}, tmpDir, contentFilterID{})
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
	tmpDir, err := os.MkdirTemp("", "seekfs-content-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)
	spool, err := os.Create(filepath.Join(tmpDir, "text.spool"))
	if err != nil {
		return nil, err
	}
	defer spool.Close()
	spoolWriter, err := newContentSpoolWriter(spool)
	if err != nil {
		return nil, err
	}
	defer spoolWriter.Close()
	var spooled []contentWalkDoc
	var skipped, truncated, binary int64
	var inclusion contentInclusionStats
	var scopeBytes int64
	count := idx.compactRecordCount()
	// Without a resolved scope, repo membership for the per-file caps
	// comes from the index's own git markers.
	var detectedRepos []string
	if opts.Scope == nil {
		detectedRepos = contentDetectGitRepos(idx)
	}
	inRepoOf := func(path string) bool {
		if opts.Scope != nil {
			_, ok := opts.Scope.Ignore.repoFor(contentNormPath(path))
			return ok
		}
		p := contentNormPath(path)
		for _, r := range detectedRepos {
			if p == r || len(p) > len(r) && (p[len(r)] == '\\' || p[len(r)] == '/') && p[:len(r)] == r {
				return true
			}
		}
		return false
	}
	var items []contentBuildItem
	for id := 0; id < count; id++ {
		rec := idx.compactRecord(id)
		if rec.Deleted || rec.Mode&uint32(os.ModeDir) != 0 || rec.FRN == 0 {
			continue
		}
		path := idx.reconstructCompactPathCached(id, cache)
		size := rec.Size
		if size < 0 {
			size = 0
		}
		if path == "" || !opts.allowsSize(path, size) {
			if opts.Scope != nil && path != "" {
				if _, _, reason, ok := opts.Scope.allowsReason(path, size); !ok {
					inclusion.add(reason, size)
				}
			}
			continue
		}
		items = append(items, contentBuildItem{frn: rec.FRN, path: path, inRepo: inRepoOf(path)})
	}
	next, stop := contentPrefetchExtract(ctx, items, contentBuildWorkers, contentBuildDocSafe)
	defer stop()
	for _, item := range items {
		got, live := next()
		if err := ctx.Err(); err != nil || !live {
			if err == nil {
				err = context.Canceled
			}
			return nil, err
		}
		doc, res, ok := got.doc, got.res, got.ok
		if res.Truncated {
			truncated++
			inclusion.Truncated++
			if cut := doc.fileSize - int64(len(doc.text)); cut > 0 {
				inclusion.TruncatedBytes += cut
			}
		}
		if !ok && res.Skipped && contentInclusionReason(res.Reason) {
			inclusion.add(res.Reason, res.Size)
		}
		if ok {
			// Only an explicit budget caps the offline build: the default
			// is reserve-governed, and the offline CLI (unlike the service
			// build) has no sidecar volume to project a reserve against at
			// selection time — the save fails loudly if the disk fills.
			if opts.Scope != nil && opts.Scope.BudgetExplicit {
				// Charged in projected index bytes (contentIndexCostEstimate):
				// the index stores no text.
				cost := contentIndexCostEstimate(int64(len(doc.text)))
				if cost > opts.Scope.BudgetBytes-scopeBytes {
					return nil, fmt.Errorf("content-index: scoped build exceeds %d-byte index budget; narrow the scope or raise budget_bytes", opts.Scope.BudgetBytes)
				}
				scopeBytes += cost
			}
			offset, compLen, serr := spoolWriter.Append(doc.text)
			if serr != nil {
				return nil, serr
			}
			spooled = append(spooled, contentWalkDoc{
				item:        item,
				offset:      offset,
				length:      len(doc.text),
				spoolLen:    compLen,
				modUnix:     doc.modUnix,
				class:       doc.class,
				version:     doc.version,
				fileSize:    doc.fileSize,
				fileModNano: doc.fileModNano,
			})
			continue
		}
		if res.Binary {
			binary++
			continue
		}
		if res.Skipped {
			skipped++
		}
	}
	sort.Slice(spooled, func(i, j int) bool {
		if spooled[i].item.frn != spooled[j].item.frn {
			return spooled[i].item.frn < spooled[j].item.frn
		}
		return spooled[i].item.path < spooled[j].item.path
	})
	order := make([]int, len(spooled))
	for i := range order {
		order[i] = i
	}
	cidx, err := assembleContentIndexFromSpool(spool, spooled, order, tmpDir, contentFilterID{MaxRaw: settings.maxRaw, MaxText: settings.maxText, CapsExplicit: settings.capsExplicit, Encoding: opts.Encoding})
	if err != nil {
		return nil, err
	}
	cidx.Origin = contentOriginUSN
	polRaw, polText := contentInclusionCapsForFile(true, 0, 0)
	if settings.capsExplicit {
		polRaw, polText = settings.maxRaw, settings.maxText
	}
	cidx.Policy = contentBuildPolicy{MaxRaw: polRaw, MaxText: polText, Skipped: skipped, Truncated: truncated, Binary: binary}
	cidx.Inclusion.merge(inclusion)
	// Stamp the content USN checkpoint from the record index metadata: the
	// base is complete as of the .gsi checkpoint, so restart catch-up resumes
	// from here rather than re-reading the whole journal (WP0/PB3).
	cidx.JournalID = idx.JournalID
	if idx.Checkpoint > 0 {
		cidx.CheckpointUSN = uint64(idx.Checkpoint)
	}
	return cidx, nil
}

// contentStoredText is the zero-copy form of contentRepairText for bytes an
// extractor already decoded: decoded text is always valid UTF-8, and repair
// is the identity on valid UTF-8 that carries no BOM and no BOM-less UTF-16
// NUL pattern, so such input is returned as-is with no copy. Anything else
// (a leading UTF-8 BOM, which repair strips and a legacy-decoded span can
// re-emit; valid UTF-8 whose NUL pattern repair would reinterpret as UTF-16;
// a never-in-practice invalid buffer) takes the exact repair path. Either way
// the bytes are exactly what contentRepairText would have produced.
func contentStoredText(decoded []byte) []byte {
	if len(decoded) >= 3 && decoded[0] == 0xEF && decoded[1] == 0xBB && decoded[2] == 0xBF {
		return contentRepairText(decoded)
	}
	if utf8.Valid(decoded) && contentSniffUTF16BOMless(decoded) == "" {
		return decoded
	}
	return contentRepairText(decoded)
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
//
// The ASCII fast path folds in a single pass into a single fresh buffer.
// strings.ToLower on the same bytes would copy three times (string + lower +
// []byte); the bytes are identical because ASCII case mapping is trivial.
func contentFoldText(repaired []byte) []byte {
	ascii := true
	for i := 0; i < len(repaired); i++ {
		if repaired[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		out := make([]byte, len(repaired))
		for i, b := range repaired {
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			out[i] = b
		}
		return out
	}
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

// contentTokenMapHint caps a per-document tokenizer map's size hint: the hint
// is allocation only (never output), and an uncapped hint on a huge document
// preallocates gigabytes of empty buckets. Distinct terms/grams saturate, so
// the cap only trims waste on large docs.
const contentTokenMapHintMax = 1 << 20

func contentTokenMapHint(n int) int {
	if n < 0 {
		n = 0
	}
	if n > contentTokenMapHintMax {
		n = contentTokenMapHintMax
	}
	return n
}

// contentTermsOfFolded tokenizes already-folded text into words with term
// frequencies. It is contentTermsOf without the per-word strings.ToLower:
// folding is idempotent, so lowering an already-folded word is the identity
// and skipping it produces the identical map with one fewer allocation per
// word. The word rule matches contentTermsOf exactly.
func contentTermsOfFolded(folded []byte) map[string]uint32 {
	out := make(map[string]uint32, contentTokenMapHint(len(folded)/8+1))
	start := -1
	for i := 0; i <= len(folded); i++ {
		var isWord bool
		if i < len(folded) {
			b := folded[i]
			isWord = (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b >= 0x80
		}
		if isWord {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			out[string(folded[start:i])]++
			start = -1
		}
	}
	return out
}

// contentTermsOf tokenizes text into lowercase words with term frequencies.
// Word bytes are ASCII alphanumerics, underscore, and any byte >= 0x80 (so a
// UTF-8 word stays one term).
func contentTermsOf(text []byte) map[string]uint32 {
	out := make(map[string]uint32, contentTokenMapHint(len(text)/8+1))
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

// contentGramsOfU32 returns the distinct 3-byte trigrams of text as uint32
// keys, skipping grams that contain a control byte. It is contentGramsOf
// without the per-position 3-byte string allocation: the key set is identical
// (a trigram is a bijection with its uint32), and the assembler renders keys
// only for distinct grams via contentGramU32String.
func contentGramsOfU32(text []byte) map[uint32]struct{} {
	out := make(map[uint32]struct{}, contentTokenMapHint(len(text)/4))
	for i := 0; i+3 <= len(text); i++ {
		a, b, c := text[i], text[i+1], text[i+2]
		if a < 32 || b < 32 || c < 32 {
			continue
		}
		out[uint32(a)<<16|uint32(b)<<8|uint32(c)] = struct{}{}
	}
	return out
}

// contentGramsOf returns the distinct 3-byte trigrams of text, skipping grams
// that contain a control byte.
func contentGramsOf(text []byte) map[string]struct{} {
	out := make(map[string]struct{}, contentTokenMapHint(len(text)/4))
	for i := 0; i+3 <= len(text); i++ {
		g := text[i : i+3]
		if g[0] < 32 || g[1] < 32 || g[2] < 32 {
			continue
		}
		out[string(g)] = struct{}{}
	}
	return out
}

// encodeContentPaths serializes docID -> path (format v7, CXPT v2).
//
// Paths share long prefixes (repo/dir names) but are stored in docID (FRN)
// order, so adjacent paths share little. Instead of front-coding in stored
// order, v7 sorts a copy (keeping a docID -> sorted-position permutation),
// front-codes the sorted paths where prefixes actually share, and stores the
// permutation for O(1) docID lookup. Measured on the 65k-doc Chromium sample:
// 4.56 MB raw -> 1.15 MB blob + 0.26 MB permutation.
//
// Layout:
//
//	u32 count | u32 restartInterval | u32 restartCount | u32 blobLen
//	restarts: restartCount x {blobOff u32, pathLen u32, path bytes}
//	blob: count x {[prefixLen uvarint][suffixLen uvarint][suffix bytes]}
//	  over paths in sorted order (restart records self-contained)
//	perm: count x u32 LE (perm[docID] = index into the sorted paths)
const contentPathsRestartInterval = 64

func encodeContentPaths(paths []string) []byte {
	t0 := time.Now()
	defer func() {
		if pc := contentAssemblePhaseCapture; pc != nil {
			contentPhaseAdd(&pc.pathsEncode, time.Since(t0))
		}
	}()
	n := len(paths)
	if n == 0 {
		return nil
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool { return paths[order[i]] < paths[order[j]] })
	sorted := make([]string, n)
	perm := make([]uint32, n)
	for pos, doc := range order {
		p := paths[doc]
		if len(p) > int(^uint16(0)) {
			p = p[:int(^uint16(0))]
		}
		sorted[pos] = p
		perm[doc] = uint32(pos)
	}

	const interval = contentPathsRestartInterval
	restartCount := (n + interval - 1) / interval
	var blob bytes.Buffer
	blobOffs := make([]uint32, n)
	prev := ""
	for i, k := range sorted {
		blobOffs[i] = uint32(blob.Len())
		shared := 0
		if i%interval != 0 {
			for shared < len(prev) && shared < len(k) && prev[shared] == k[shared] {
				shared++
			}
		}
		// Paths keep the legacy two-uvarint framing: repository paths share
		// long directory prefixes and carry long file names, so both
		// lengths almost always exceed the term dictionary's one-byte
		// header and an escape marker would only add a byte per path
		// (measured +1 B/doc on the Chromium sample).
		var tmp [binary.MaxVarintLen64]byte
		m := binary.PutUvarint(tmp[:], uint64(shared))
		blob.Write(tmp[:m])
		m = binary.PutUvarint(tmp[:], uint64(len(k)-shared))
		blob.Write(tmp[:m])
		blob.WriteString(k[shared:])
		prev = k
	}

	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, uint32(n))
	_ = binary.Write(&out, binary.LittleEndian, uint32(interval))
	_ = binary.Write(&out, binary.LittleEndian, uint32(restartCount))
	_ = binary.Write(&out, binary.LittleEndian, uint32(blob.Len()))
	for r := 0; r < restartCount; r++ {
		i := r * interval
		_ = binary.Write(&out, binary.LittleEndian, blobOffs[i])
		_ = binary.Write(&out, binary.LittleEndian, uint32(len(sorted[i])))
		out.WriteString(sorted[i])
	}
	out.Write(blob.Bytes())
	for _, v := range perm {
		_ = binary.Write(&out, binary.LittleEndian, v)
	}
	return out.Bytes()
}

func decodeContentPaths(data []byte, docCount int) ([]string, error) {
	if len(data) < 16 {
		return nil, errors.New("content paths truncated")
	}
	n := int(binary.LittleEndian.Uint32(data[0:]))
	interval := int(binary.LittleEndian.Uint32(data[4:]))
	restartCount := int(binary.LittleEndian.Uint32(data[8:]))
	blobLen := int(binary.LittleEndian.Uint32(data[12:]))
	if n != docCount {
		return nil, errors.New("content paths count mismatch")
	}
	if n == 0 {
		if restartCount != 0 || blobLen != 0 {
			return nil, errors.New("content paths length mismatch")
		}
		return nil, nil
	}
	if interval <= 0 || restartCount != (n+interval-1)/interval || blobLen < 0 {
		return nil, errors.New("content paths header mismatch")
	}
	off := 16
	restarts := make([]string, 0, restartCount)
	restartIx := make([]int, 0, restartCount)
	for r := 0; r < restartCount; r++ {
		if off+8 > len(data) {
			return nil, errors.New("content paths restarts truncated")
		}
		blobOff := int(binary.LittleEndian.Uint32(data[off:]))
		pathLen := int(binary.LittleEndian.Uint32(data[off+4:]))
		off += 8
		if pathLen < 0 || off+pathLen > len(data) {
			return nil, errors.New("content paths restart truncated")
		}
		restarts = append(restarts, string(data[off:off+pathLen]))
		restartIx = append(restartIx, blobOff)
		off += pathLen
	}
	if off+blobLen+n*4 > len(data) {
		return nil, errors.New("content paths length mismatch")
	}
	blob := data[off : off+blobLen]
	off += blobLen
	// Decode the sorted paths, verifying order and restart alignment: a
	// corrupt blob fails here, not in a later lookup. Equal adjacent paths
	// are allowed (two docs may share a stored path across origins).
	sorted := make([]string, 0, n)
	prev := ""
	pos := 0
	for i := 0; i < n; i++ {
		if i%interval == 0 {
			if pos != restartIx[i/interval] {
				return nil, errors.New("content paths restart offset mismatch")
			}
		}
		key, next, ok := decodeLegacyFrontCodedKey(blob, pos, prev)
		if !ok {
			return nil, errors.New("content paths key truncated")
		}
		if key < prev {
			return nil, errors.New("content paths out of order")
		}
		if i%interval == 0 && key != restarts[i/interval] {
			return nil, errors.New("content paths restart key mismatch")
		}
		prev = key
		pos = next
		sorted = append(sorted, key)
	}
	if pos != len(blob) {
		return nil, errors.New("content paths blob length mismatch")
	}
	// The permutation must be exact: every sorted position named exactly
	// once, or a docID would resolve to another document's path.
	out := make([]string, n)
	seen := make([]bool, n)
	for doc := 0; doc < n; doc++ {
		p := int(binary.LittleEndian.Uint32(data[off+doc*4:]))
		if p < 0 || p >= n || seen[p] {
			return nil, errors.New("content paths permutation mismatch")
		}
		seen[p] = true
		out[doc] = sorted[p]
	}
	return out, nil
}
