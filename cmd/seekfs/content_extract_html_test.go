package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func contentHTMLExtract(t *testing.T, raw []byte) contentExtractResult {
	t.Helper()
	got, err := contentHTMLExtractor{}.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	return got
}

// Body and <title> text are searchable; script/style/comments/doctype are not,
// and emitted text carries no tags.
func TestContentHTMLExtractsBodyAndTitle(t *testing.T) {
	raw := []byte(`<!DOCTYPE html>
<html><head><title>Alpha Page Title</title>
<style>.x{color:red}</style></head>
<body><h1>Heading</h1><p>Body needle here.</p>
<script>var secretneedle=1;</script>
<!-- commentneedle -->
</body></html>`)
	got := contentHTMLExtract(t, raw)
	if got.Class != contentClassHTML {
		t.Fatalf("class = %d; want %d", got.Class, contentClassHTML)
	}
	text := string(got.Text)
	for _, want := range []string{"Alpha Page Title", "Heading", "Body needle here."} {
		if !strings.Contains(text, want) {
			t.Fatalf("text %q missing %q", text, want)
		}
	}
	for _, banned := range []string{"secretneedle", "commentneedle", "color:red", "<h1>", "<!DOCTYPE"} {
		if strings.Contains(text, banned) {
			t.Fatalf("extracted excluded content %q: %q", banned, text)
		}
	}
}

func TestContentHTMLBlockTagsSeparateWords(t *testing.T) {
	got := contentHTMLExtract(t, []byte("<div>alpha</div><div>beta</div><p>gamma</p><p>delta</p>"))
	text := string(got.Text)
	if !strings.Contains(text, "alpha beta") || !strings.Contains(text, "gamma delta") {
		t.Fatalf("block tags did not separate words: %q", text)
	}
}

func TestContentHTMLEntitiesDecode(t *testing.T) {
	got := contentHTMLExtract(t, []byte("<p>Tom &amp; Jerry caf&#233; r&eacute;sum&eacute;</p>"))
	text := string(got.Text)
	for _, want := range []string{"Tom & Jerry", "café", "résumé"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text %q missing decoded %q", text, want)
		}
	}
}

// Malformed markup must not panic and yields the recoverable text.
func TestContentHTMLMalformedDoesNotPanic(t *testing.T) {
	got := contentHTMLExtract(t, []byte("<html><body><p>unclosed <b>bold text"))
	if !strings.Contains(string(got.Text), "bold text") {
		t.Fatalf("text %q missing recovered text", got.Text)
	}
}

// A pathological unterminated script (or comment) trips SetMaxBuf: no crash,
// bounded output, and text before it is still indexed.
func TestContentHTMLUnterminatedScriptIsBounded(t *testing.T) {
	huge := "<html><body><p>before needle</p><script>" + strings.Repeat("a", contentHTMLMaxToken*2)
	got := contentHTMLExtract(t, []byte(huge))
	if !got.Truncated {
		t.Fatalf("unterminated script must mark Truncated: %+v", got)
	}
	if !strings.Contains(string(got.Text), "before needle") {
		t.Fatalf("text %q lost pre-script content", got.Text)
	}
	if len(got.Text) > contentHTMLMaxToken {
		t.Fatalf("output not bounded: %d bytes", len(got.Text))
	}

	comment := "<html><body><p>before needle</p><!--" + strings.Repeat("a", contentHTMLMaxToken*2)
	got = contentHTMLExtract(t, []byte(comment))
	if !got.Truncated || !strings.Contains(string(got.Text), "before needle") {
		t.Fatalf("unterminated comment not bounded: %+v", got)
	}
}

func TestContentHTMLTruncatesAtMaxText(t *testing.T) {
	raw := []byte("<p>alpha needle beta gamma delta epsilon</p>")
	ctx := contentWithExtractSettings(context.Background(), contentExtractSettings{maxRaw: 1 << 20, maxText: 8})
	got, err := contentHTMLExtractor{}.Extract(ctx, bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || got.Reason == "" {
		t.Fatalf("over-text-cap doc must be Truncated with a reason: %+v", got)
	}
	if len(got.Text) > 8 {
		t.Fatalf("text cap not respected: %d bytes", len(got.Text))
	}
}

// A large document runs through the panic/deadline-isolated coordinator without
// wedging and with bounded output.
func TestContentHTMLExtractSafelyBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("<html><body>")
	for i := 0; i < 100000; i++ {
		b.WriteString("<p>needle</p>")
	}
	b.WriteString("</body></html>")
	raw := []byte(b.String())
	ctx := contentWithExtractSettings(context.Background(), contentExtractSettings{maxRaw: int64(len(raw)) + 1, maxText: 4096})
	done := make(chan contentExtractResult, 1)
	go func() {
		res, err := contentExtractSafely(ctx, contentHTMLExtractor{}, bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Errorf("contentExtractSafely: %v", err)
		}
		done <- res
	}()
	select {
	case res := <-done:
		if len(res.Text) > 4096 {
			t.Fatalf("output not bounded: %d bytes", len(res.Text))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("html extraction wedged")
	}
}

func TestContentServiceAllowlistIncludesHTML(t *testing.T) {
	if _, ok := contentServiceExtensions()[".html"]; !ok {
		t.Fatal(".html missing from the service content allowlist")
	}
	if e := contentExtractorForPath("page.html", []byte("<html><body>hi</body></html>")); e == nil || e.Name() != "html" {
		t.Fatalf(".html claimed by %v; want html", e)
	}
}

// A .html file is extracted (class HTML) rather than raw-markup indexed: body
// and title terms are findable, script/style terms are not.
func TestContentBuildIndexesHTMLNotRawMarkup(t *testing.T) {
	dir := t.TempDir()
	page := `<!DOCTYPE html><html><head><title>titleneedle</title>` +
		`<style>.x{content:"styleneedle"}</style></head>` +
		`<body><p>bodyneedle</p><script>var scriptneedle=1;</script></body></html>`
	if err := os.WriteFile(filepath.Join(dir, "page.html"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Docs) != 1 {
		t.Fatalf("built %d docs; want 1", len(idx.Docs))
	}
	if idx.Docs[0].ContentType != contentClassHTML || idx.Docs[0].ExtractorVersion != (contentHTMLExtractor{}).Version() {
		t.Fatalf("doc identity = (%d,%d); want HTML v1", idx.Docs[0].ContentType, idx.Docs[0].ExtractorVersion)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range []string{"bodyneedle", "titleneedle"} {
		if hits := r.search(term, 0); len(hits) != 1 || hits[0].Path != "page.html" {
			t.Fatalf("term %q not findable: %v", term, contentPathsOf(hits))
		}
	}
	for _, term := range []string{"scriptneedle", "styleneedle"} {
		if hits := r.search(term, 0); len(hits) != 0 {
			t.Fatalf("excluded term %q indexed: %v", term, contentPathsOf(hits))
		}
	}
}
