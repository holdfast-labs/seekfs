package main

// The OOXML/ODF/e-book extractor: these formats are ZIP archives whose
// text-bearing parts are XML. Extraction opens the archive in memory (bounded
// by contentExtractMaxRawBytes) and streams each selected XML entry through
// encoding/xml, collecting character data. Uncompressed input is capped three
// ways (entry count, per-entry bytes, total bytes) because a small archive can
// decompress to gigabytes.
//
// Extract receives only the bytes, not the path, so entries are selected by
// their in-archive names; the Extensions list is what routes a file here.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"strings"
)

const contentOOXMLMaxEntries = 4096

type contentOOXMLExtractor struct{}

func (contentOOXMLExtractor) Name() string    { return "ooxml" }
func (contentOOXMLExtractor) Version() uint16 { return 1 }

func (contentOOXMLExtractor) Extensions() []string {
	return []string{
		".docx", ".docm",
		".xlsx", ".xlsm",
		".pptx", ".pptm",
		".odt", ".ods", ".odp",
		".epub",
	}
}

// Sniff claims any ZIP local-file header; the coordinator only calls Sniff for
// files no extension claimed, so an archive with an unknown extension still
// gets a look.
func (contentOOXMLExtractor) Sniff(head []byte) bool {
	return bytes.HasPrefix(head, []byte("PK\x03\x04"))
}

// contentOOXMLWildcard reports whether name matches prefix+anything+suffix.
func contentOOXMLWildcard(name, prefix, suffix string) bool {
	return len(name) >= len(prefix)+len(suffix) &&
		strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix)
}

// contentOOXMLKnownEntry reports whether a lowercased entry name is one of the
// format's text-bearing parts.
func contentOOXMLKnownEntry(name string) bool {
	switch name {
	case "word/document.xml", "word/footnotes.xml", "word/endnotes.xml",
		"xl/sharedstrings.xml", "content.xml":
		return true
	}
	return contentOOXMLWildcard(name, "word/header", ".xml") ||
		contentOOXMLWildcard(name, "word/footer", ".xml") ||
		contentOOXMLWildcard(name, "xl/worksheets/", ".xml") ||
		contentOOXMLWildcard(name, "ppt/slides/slide", ".xml") ||
		contentOOXMLWildcard(name, "ppt/notesslides/notesslide", ".xml")
}

// contentOOXMLFallbackEntry accepts markup parts when no known name matched.
func contentOOXMLFallbackEntry(name string) bool {
	for _, suffix := range []string{".xml", ".xhtml", ".html", ".htm"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// contentOOXMLSelect picks the entries to read, preserving archive order. If no
// known names are present it falls back to all markup entries.
func contentOOXMLSelect(zr *zip.Reader) []*zip.File {
	var known, fallback []*zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := strings.ToLower(strings.ReplaceAll(f.Name, "\\", "/"))
		switch {
		case contentOOXMLKnownEntry(name):
			known = append(known, f)
		case contentOOXMLFallbackEntry(name):
			fallback = append(fallback, f)
		}
	}
	if len(known) > 0 {
		return known
	}
	return fallback
}

func (contentOOXMLExtractor) Extract(ctx context.Context, r io.ReaderAt, size int64) (contentExtractResult, error) {
	s := contentExtractSettingsFromContext(ctx)
	// A zip's central directory lives at the end, so a bounded prefix is not a
	// valid archive: an over-cap container is skipped with a visible reason
	// rather than prefix-indexed.
	if size > s.maxRaw {
		return contentExtractResult{Skipped: true, Reason: "raw size over cap", Class: contentClassOOXML}, nil
	}
	raw, err := contentReadBounded(r, size, int(s.maxRaw))
	if err != nil {
		return contentExtractResult{}, err
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return contentExtractResult{Skipped: true, Reason: "not a zip", Class: contentClassOOXML}, nil
	}

	selected := contentOOXMLSelect(zr)
	var out bytes.Buffer
	var total int64
	maxTotal := s.maxText * 4
	truncated := len(selected) > contentOOXMLMaxEntries
	for i, f := range selected {
		if i >= contentOOXMLMaxEntries {
			truncated = true
			break
		}
		if err := ctx.Err(); err != nil {
			return contentExtractResult{}, err
		}
		remaining := maxTotal - total
		if remaining <= 0 {
			truncated = true
			break
		}
		limit := s.maxText
		if remaining < limit {
			limit = remaining
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		cr := &contentOOXMLCountReader{r: io.LimitReader(rc, limit)}
		text, _ := contentOOXMLDecodeText(cr)
		rc.Close()
		total += cr.n
		if len(text) > 0 {
			out.Write(text)
			out.WriteByte('\n')
		}
		// A full read means an input cap was hit: stop with what we have.
		if cr.n >= limit || int64(out.Len()) >= s.maxText {
			truncated = true
			break
		}
	}

	if out.Len() == 0 {
		return contentExtractResult{Class: contentClassOOXML, Truncated: truncated}, nil
	}
	text := truncateUTF8(out.String(), int(s.maxText))
	res := contentExtractResult{Text: []byte(text), Class: contentClassOOXML, Truncated: truncated}
	if truncated {
		res.Reason = "indexed bounded prefix up to policy cap"
	}
	return res, nil
}

type contentOOXMLCountReader struct {
	r io.Reader
	n int64
}

func (c *contentOOXMLCountReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// contentOOXMLDecodeText collects character data from every text node. Only
// UTF-8 is handled natively by encoding/xml; any declared charset goes through
// CharsetReader, which declines rather than pulling in x/text. A decode error
// returns the partial text collected so far.
func contentOOXMLDecodeText(r io.Reader) ([]byte, error) {
	d := xml.NewDecoder(r)
	d.Strict = false
	d.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		return nil, errors.New("ooxml: unsupported charset " + charset)
	}
	var buf bytes.Buffer
	for {
		tok, err := d.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf.Bytes(), nil
			}
			return buf.Bytes(), err
		}
		if cd, ok := tok.(xml.CharData); ok {
			buf.Write(cd)
		}
	}
}

func init() {
	contentRegisterExtractor(contentOOXMLExtractor{})
}
