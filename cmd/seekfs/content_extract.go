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
)

// Bounds. Extraction reads at most maxRaw; the extracted text is capped at
// maxText. A file larger than maxRaw is skipped entirely (the coordinator logs
// it); this is the disk/CPU guard that keeps a content index bounded.
const (
	contentExtractMaxRawBytes  = 32 << 20 // 32 MiB of source bytes
	contentExtractMaxTextBytes = 16 << 20 // 16 MiB of extracted text
	contentExtractBinarySniff  = 8 << 10  // first 8 KiB NUL check, as ripgrep
)

// contentExtractResult is what an extractor returns.
type contentExtractResult struct {
	Text  []byte
	Class uint16
	// Skipped is true when the extractor recognised the format but declined to
	// extract (encrypted, scanned image, oversized, ...). The coordinator marks
	// such docs Evaluated=true, Matched=false.
	Skipped bool
	Reason  string
}

// contentExtractor turns file bytes into text. Implementations live in their own
// files and register through contentRegisterExtractor.
type contentExtractor interface {
	Name() string
	Version() uint16
	// Extensions lists lowercase extensions including the dot, e.g. ".docx".
	Extensions() []string
	// Sniff reports whether this extractor claims a file whose first bytes are
	// head. Used only for extensionless files.
	Sniff(head []byte) bool
	// Extract returns the searchable text. It must not read more than
	// contentExtractMaxRawBytes from r.
	Extract(ctx context.Context, r io.ReaderAt, size int64) (contentExtractResult, error)
}

var contentExtractors []contentExtractor

func contentRegisterExtractor(e contentExtractor) { contentExtractors = append(contentExtractors, e) }

func init() {
	contentRegisterExtractor(contentTextExtractor{})
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

// contentHasBOM reports whether b starts with a UTF-8/UTF-16 BOM. A BOM-marked
// file is text even though its UTF-16 bytes contain NULs.
func contentHasBOM(b []byte) bool {
	_, n := sniffContentBOM(b)
	return n > 0
}

// contentLooksBinary reports whether head indicates a binary file: a NUL byte in
// the first contentExtractBinarySniff bytes, matching ripgrep.
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
