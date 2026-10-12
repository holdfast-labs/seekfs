package main

// Content extraction: turning a file's bytes into searchable text. Extractors
// are pluggable and versioned; bumping Version() invalidates only the documents
// that extractor produced, so shipping PDF support later re-extracts just PDFs.
//
// Every extractor is called with an io.ReaderAt and the file size, so it can
// stream and never has to materialise the whole file on the heap. Extraction is
// untrusted input: implementations must bound recursion and decompression, and
// the coordinator recovers from a panic per document.

import (
	"context"
	"errors"
	"io"
	"strings"
)

// contentClass* discriminates how a document's text was produced, for telemetry
// and for extractor-version invalidation.
const (
	contentClassText   uint16 = 1
	contentClassOOXML  uint16 = 2
	contentClassPDF    uint16 = 3
	contentClassHTML   uint16 = 4
	contentClassEmail  uint16 = 5
	contentClassLegacy uint16 = 6
	contentClassRTF    uint16 = 7
	contentClassMbox   uint16 = 8
	contentClassMSG    uint16 = 9
)

// Bounds. Extraction reads at most maxRaw; the extracted text is capped at
// maxText. Both are policy values (contentBuildOptions / -max-raw / -max-text)
// whose defaults are the historical 32 MiB / 16 MiB and whose hard ceilings stop
// a policy from unbounded allocation. A plain-text file larger than maxRaw is
// indexed up to the cap (a bounded prefix) and marked Truncated; a container
// format that cannot be prefixed (zip/OOXML, PDF) is skipped with a Reason. No
// over-cap file is ever dropped silently.
const (
	contentExtractMaxRawBytes  = 32 << 20 // default: 32 MiB of source bytes
	contentExtractMaxTextBytes = 16 << 20 // default: 16 MiB of extracted text
	// Hard safety ceilings: a policy may raise the defaults but never past
	// these, so extraction memory stays bounded regardless of configuration.
	contentExtractHardMaxRawBytes  = 512 << 20 // 512 MiB of source bytes
	contentExtractHardMaxTextBytes = 256 << 20 // 256 MiB of extracted text
	contentExtractBinarySniff      = 8 << 10   // first 8 KiB NUL check, as ripgrep
)

// contentExtractSettings is the per-build extraction policy carried on the
// context: the encoding override and the raw/text caps. Extractors read it with
// contentExtractSettingsFromContext, so no global state is shared between a
// service build and the offline CLI.
type contentExtractSettings struct {
	encoding contentEncodingMode
	maxRaw   int64
	maxText  int64
}

type contentExtractSettingsKey struct{}

func contentDefaultExtractSettings() contentExtractSettings {
	return contentExtractSettings{
		encoding: contentAutoEncoding,
		maxRaw:   contentExtractMaxRawBytes,
		maxText:  contentExtractMaxTextBytes,
	}
}

// contentNormalizeExtractCaps resolves a policy's raw/text caps: zero means the
// default, and anything above the hard ceiling is clamped to it.
func contentNormalizeExtractCaps(maxRaw, maxText int64) (int64, int64) {
	if maxRaw <= 0 {
		maxRaw = contentExtractMaxRawBytes
	}
	if maxText <= 0 {
		maxText = contentExtractMaxTextBytes
	}
	if maxRaw > contentExtractHardMaxRawBytes {
		maxRaw = contentExtractHardMaxRawBytes
	}
	if maxText > contentExtractHardMaxTextBytes {
		maxText = contentExtractHardMaxTextBytes
	}
	return maxRaw, maxText
}

// contentWithExtractSettings stamps an extraction policy onto ctx. The zero
// encoding resolves to the default auto policy; caps are normalized and clamped.
func contentWithExtractSettings(ctx context.Context, s contentExtractSettings) context.Context {
	if s.encoding == (contentEncodingMode{}) {
		s.encoding = contentAutoEncoding
	}
	s.maxRaw, s.maxText = contentNormalizeExtractCaps(s.maxRaw, s.maxText)
	return context.WithValue(ctx, contentExtractSettingsKey{}, s)
}

// contentExtractSettingsFromContext returns the ctx's policy or the defaults.
func contentExtractSettingsFromContext(ctx context.Context) contentExtractSettings {
	if ctx != nil {
		if s, ok := ctx.Value(contentExtractSettingsKey{}).(contentExtractSettings); ok {
			return s
		}
	}
	return contentDefaultExtractSettings()
}

// contentExtractResult is what an extractor returns.
type contentExtractResult struct {
	Text  []byte
	Class uint16
	// Skipped is true when the extractor recognised the format but declined to
	// extract (encrypted, scanned image, oversized, ...). The coordinator marks
	// such docs Evaluated=true, Matched=false.
	Skipped bool
	// Truncated is true when the extractor indexed a bounded prefix of the file
	// (raw over the policy cap, or text cut at the text cap) rather than the
	// whole document. It is surfaced in health/policy counts, never silent.
	Truncated bool
	Reason    string
}

// contentExtractor turns file bytes into text. Implementations live in their own
// files and register through contentRegisterExtractor.
type contentExtractor interface {
	Name() string
	Version() uint16
	// Class declares the contentClass* this extractor produces. It is stamped
	// into each doc's ContentType; a class's registered version is what
	// invalidation compares against, so a Version() bump re-extracts the docs
	// that extractor produced (targeted per-doc refresh) rather than serving
	// text from the old extractor.
	Class() uint16
	// Extensions lists lowercase extensions including the dot, e.g. ".docx".
	Extensions() []string
	// Sniff reports whether this extractor claims a file whose first bytes are
	// head. Used only for extensionless files.
	Sniff(head []byte) bool
	// Extract returns the searchable text, reading at most the ctx policy's
	// maxRaw bytes from r (contentExtractSettingsFromContext) and returning at
	// most its maxText bytes.
	Extract(ctx context.Context, r io.ReaderAt, size int64) (contentExtractResult, error)
}

var contentExtractors []contentExtractor

func contentRegisterExtractor(e contentExtractor) { contentExtractors = append(contentExtractors, e) }

func init() {
	contentRegisterExtractor(contentTextExtractor{})
}

// contentExtractorVersionForClass returns the current registered extractor
// version for a content class, and whether any registered extractor declares
// that class. A zero class (an older `.gsx`) has no extractor and reports false.
func contentExtractorVersionForClass(class uint16) (uint16, bool) {
	if class == 0 {
		return 0, false
	}
	for _, e := range contentExtractors {
		if e.Class() == class {
			return e.Version(), true
		}
	}
	return 0, false
}

// contentExtractorForPath selects an extractor for a path. Extension match
// wins; otherwise format sniffers (OOXML, PDF) get their say before the text
// extractor, which claims any non-binary head and would otherwise shadow them
// for an extensionless .docx or .pdf. Returns nil when nothing claims the file.
func contentExtractorForPath(path string, head []byte) contentExtractor {
	ext := contentPathExtension(path)
	if ext != "" {
		for _, e := range contentExtractors {
			for _, want := range e.Extensions() {
				if want == ext {
					return e
				}
			}
		}
	}
	var textFallback contentExtractor
	for _, e := range contentExtractors {
		if e.Name() == "text" {
			textFallback = e
			continue
		}
		if e.Sniff(head) {
			return e
		}
	}
	if textFallback != nil && textFallback.Sniff(head) {
		return textFallback
	}
	return nil
}

// contentPathExtension returns the lowercase extension of the final path
// element, including the dot, without allocating a full filepath.Ext on every
// call in the walker hot path.
func contentPathExtension(path string) string {
	base := path
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '\\' || path[i] == '/' {
			base = path[i+1:]
			break
		}
	}
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 || dot == len(base)-1 {
		return ""
	}
	return strings.ToLower(base[dot:])
}

// contentExtractPanicError is returned when an extractor panics. The caller
// treats it as a per-document skip with a reason, never a crash.
var contentExtractPanicError = errors.New("content extractor panic")

// contentExtractSafely runs one extractor over an already-open file with the
// per-document deadline and panic isolation shared by the offline walk builder
// and the service build. The deadline is enforced at this boundary rather than
// trusted to the extractor: extraction runs in a goroutine and the caller stops
// waiting when the deadline fires, so even a parser that ignores ctx cannot
// wedge the serial build. A panic is recovered and mapped to a skip.
//
// ponytail: an extractor that ignores ctx keeps its goroutine until it returns;
// the raw/text caps bound what it can allocate, and P4's out-of-process path
// would replace the abandoned goroutine with a hard kill. There is also a
// close-race ceiling: on timeout the caller's deferred f.Close() can run while
// the abandoned goroutine is still reading the same io.ReaderAt. That is safe
// for *os.File (its ReadAt/Close are concurrency-safe) but not for a
// non-close-safe library ReaderAt; P4's hard kill removes the sharing.
func contentExtractSafely(ctx context.Context, e contentExtractor, r io.ReaderAt, size int64) (contentExtractResult, error) {
	type outcome struct {
		res contentExtractResult
		err error
	}
	ectx, cancel := contentWithExtractDocTimeout(ctx)
	defer cancel()
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- outcome{res: contentExtractResult{Skipped: true, Reason: "extractor panic"}, err: contentExtractPanicError}
			}
		}()
		res, err := e.Extract(ectx, r, size)
		done <- outcome{res: res, err: err}
	}()
	select {
	case out := <-done:
		return out.res, out.err
	case <-ectx.Done():
		return contentExtractResult{Skipped: true, Reason: "extraction timeout"}, ectx.Err()
	}
}

// contentHasBOM reports whether b starts with a UTF-8/UTF-16 BOM. A BOM-marked
// file is text even though its UTF-16 bytes contain NULs.
func contentHasBOM(b []byte) bool {
	_, n := sniffContentBOM(b)
	return n > 0
}

// contentDetectsTextEncoding reports whether b is text whose bytes legitimately
// contain NULs: a UTF-8/UTF-16 BOM, or a BOM-less UTF-16 NUL pattern. It is the
// gate the binary NUL sniff consults so a BOM-less UTF-16 file is decoded rather
// than rejected as binary.
func contentDetectsTextEncoding(b []byte) bool {
	return contentHasBOM(b) || contentSniffUTF16BOMless(b) != ""
}

// contentLooksBinary reports whether head indicates a binary file: a NUL byte in
// the first contentExtractBinarySniff bytes, matching ripgrep. A NUL-free binary
// file (no NUL in the sniff window) is still indexed as text: a general binary
// classifier is a rabbit hole, so this is an accepted limitation.
func contentLooksBinary(head []byte) bool {
	limit := len(head)
	if limit > contentExtractBinarySniff {
		limit = contentExtractBinarySniff
	}
	for i := 0; i < limit; i++ {
		if head[i] == 0 {
			return true
		}
	}
	return false
}

// contentReadBounded reads up to max bytes from r at offset 0.
func contentReadBounded(r io.ReaderAt, size int64, max int) ([]byte, error) {
	if size < 0 {
		return nil, errors.New("negative content size")
	}
	if size > int64(max) {
		size = int64(max)
	}
	buf := make([]byte, size)
	n, err := r.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}
