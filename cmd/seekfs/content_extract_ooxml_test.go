package main

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"
)

// contentOOXMLTestZip builds an in-memory ZIP from name->body entries.
func contentOOXMLTestZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func contentOOXMLExtract(t *testing.T, raw []byte) contentExtractResult {
	t.Helper()
	got, err := contentOOXMLExtractor{}.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	return got
}

func TestContentOOXMLDocx(t *testing.T) {
	raw := contentOOXMLTestZip(t, map[string]string{
		"word/document.xml": `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://x"><w:body><w:p><w:r><w:t>alpha needle beta</w:t></w:r></w:p></w:body></w:document>`,
	})
	got := contentOOXMLExtract(t, raw)
	if got.Class != contentClassOOXML {
		t.Fatalf("class = %d; want %d", got.Class, contentClassOOXML)
	}
	if !strings.Contains(string(got.Text), "alpha needle beta") {
		t.Fatalf("text %q missing needle", got.Text)
	}
}

func TestContentOOXMLXlsx(t *testing.T) {
	raw := contentOOXMLTestZip(t, map[string]string{
		"xl/sharedStrings.xml": `<sst xmlns="http://x"><si><t>xlsx needle</t></si></sst>`,
	})
	got := contentOOXMLExtract(t, raw)
	if !strings.Contains(string(got.Text), "xlsx needle") {
		t.Fatalf("text %q missing needle", got.Text)
	}
}

func TestContentOOXMLPptx(t *testing.T) {
	raw := contentOOXMLTestZip(t, map[string]string{
		"ppt/slides/slide1.xml": `<p:sld xmlns:p="http://x" xmlns:a="http://y"><a:t>pptx needle</a:t></p:sld>`,
	})
	got := contentOOXMLExtract(t, raw)
	if !strings.Contains(string(got.Text), "pptx needle") {
		t.Fatalf("text %q missing needle", got.Text)
	}
}

func TestContentOOXMLEpub(t *testing.T) {
	raw := contentOOXMLTestZip(t, map[string]string{
		"OEBPS/chapter1.xhtml": `<html><body><p>epub needle</p></body></html>`,
	})
	got := contentOOXMLExtract(t, raw)
	if !strings.Contains(string(got.Text), "epub needle") {
		t.Fatalf("text %q missing needle", got.Text)
	}
}

func TestContentOOXMLNotZip(t *testing.T) {
	got := contentOOXMLExtract(t, []byte("this is not a zip archive"))
	if !got.Skipped || got.Reason != "not a zip" {
		t.Fatalf("got %+v; want Skipped/not a zip", got)
	}
}

func TestContentOOXMLEntity(t *testing.T) {
	raw := contentOOXMLTestZip(t, map[string]string{
		"word/document.xml": `<w:document xmlns:w="http://x"><w:t>a &amp; b</w:t></w:document>`,
	})
	got := contentOOXMLExtract(t, raw)
	if !strings.Contains(string(got.Text), "a & b") {
		t.Fatalf("text %q did not decode &amp;", got.Text)
	}
}
