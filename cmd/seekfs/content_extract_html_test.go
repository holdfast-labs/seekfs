package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/japanese"
)

func contentHTMLExtract(t *testing.T, raw []byte) contentExtractResult {
	t.Helper()
	got, err := contentHTMLExtractor{}.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	return got
}

func contentHTMLExtractCaps(t *testing.T, raw []byte, maxRaw, maxText int64) contentExtractResult {
	t.Helper()
	ctx := contentWithExtractSettings(context.Background(), contentExtractSettings{maxRaw: maxRaw, maxText: maxText})
	got, err := contentHTMLExtractor{}.Extract(ctx, bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	return got
}

// A large (but within-budget) token must not discard the rest of the document.
// Regression: a >1 MiB inline script or text run silently dropped every term
// after it.
func TestContentHTMLTextAfterLargeTokenPreserved(t *testing.T) {
	filler := strings.Repeat("y", contentHTMLMaxToken+64*1024)
	cases := map[string]string{
		"text-run": "<html><body>alpha FIRSTWORD " + filler + " LASTWORD omega</body></html>",
		"script":   "<html><body>alpha FIRSTWORD <script>" + filler + "</script> LASTWORD omega</body></html>",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got := contentHTMLExtract(t, []byte(raw))
			text := string(got.Text)
			for _, want := range []string{"FIRSTWORD", "LASTWORD"} {
				if !strings.Contains(text, want) {
					t.Fatalf("lost %q after large token (Truncated=%v, %d bytes)", want, got.Truncated, len(text))
				}
			}
		})
	}
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
	// Cap the text budget so the pathological token exceeds the tokenizer
	// buffer; extraction must then be a bounded prefix, not a wedge.
	const cap = contentHTMLMaxToken
	huge := "<html><body><p>before needle</p><script>" + strings.Repeat("a", cap*2)
	got := contentHTMLExtractCaps(t, []byte(huge), int64(len(huge))+1, cap)
	if !got.Truncated {
		t.Fatalf("unterminated script must mark Truncated: %+v", got)
	}
	if !strings.Contains(string(got.Text), "before needle") {
		t.Fatalf("text %q lost pre-script content", got.Text)
	}
	if len(got.Text) > cap {
		t.Fatalf("output not bounded: %d bytes", len(got.Text))
	}

	comment := "<html><body><p>before needle</p><!--" + strings.Repeat("a", cap*2)
	got = contentHTMLExtractCaps(t, []byte(comment), int64(len(comment))+1, cap)
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

// contentHTMLShiftJIS rounds-trips s through Shift-JIS, so a fixture's bytes
// genuinely are a non-UTF-8 non-CP1252 legacy encoding.
func contentHTMLShiftJIS(t *testing.T, s string) []byte {
	t.Helper()
	b, err := japanese.ShiftJIS.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatalf("shift-jis encode: %v", err)
	}
	return b
}

// A <meta charset> declaring Shift-JIS is honored: the legacy bytes decode to
// the intended text and a non-ASCII term is findable through a built index.
func TestContentHTMLMetaCharsetShiftJIS(t *testing.T) {
	term := "検索"
	raw := append([]byte(`<!DOCTYPE html><html><head><meta charset="Shift_JIS"><title>`), contentHTMLShiftJIS(t, term+"タイトル")...)
	raw = append(raw, []byte(`</title></head><body><p>`)...)
	raw = append(raw, contentHTMLShiftJIS(t, "これは日本語の文書です")...)
	raw = append(raw, []byte(`</p></body></html>`)...)

	got := contentHTMLExtract(t, raw)
	text := string(got.Text)
	for _, want := range []string{term, "日本語", "文書"} {
		if !strings.Contains(text, want) {
			t.Fatalf("shift-jis meta charset not honored; text %q missing %q", text, want)
		}
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "jp.html"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	if hits := r.search(term, 0); len(hits) != 1 || hits[0].Path != "jp.html" {
		t.Fatalf("non-ASCII term %q not findable: %v", term, contentPathsOf(hits))
	}
}

// The legacy <meta http-equiv=Content-Type content="...; charset=..."> form is
// honored the same way.
func TestContentHTMLMetaHTTPEquivCharset(t *testing.T) {
	raw := append([]byte(`<html><head><meta http-equiv="Content-Type" content="text/html; charset=Shift_JIS"></head><body><p>`), contentHTMLShiftJIS(t, "日本語テスト")...)
	raw = append(raw, []byte(`</p></body></html>`)...)
	text := string(contentHTMLExtract(t, raw).Text)
	if !strings.Contains(text, "日本語") {
		t.Fatalf("http-equiv charset not honored: %q", text)
	}
}

// A UTF-8 BOM wins over a conflicting <meta charset>: the UTF-8 bytes stay
// intact rather than being re-decoded as the declared legacy encoding.
func TestContentHTMLBOMWinsOverMetaCharset(t *testing.T) {
	raw := append([]byte{0xEF, 0xBB, 0xBF},
		[]byte(`<html><head><meta charset="Shift_JIS"></head><body><p>`+"日本語テスト"+`</p></body></html>`)...)
	text := string(contentHTMLExtract(t, raw).Text)
	if !strings.Contains(text, "日本語") {
		t.Fatalf("BOM did not win over meta charset: %q", text)
	}
	if strings.Contains(text, string(contentReplacementChar)) {
		t.Fatalf("BOM-prefixed UTF-8 content was mangled: %q", text)
	}
}

// A declared UTF-8 (and plain ASCII) document is unaffected: no transcode is
// applied, so valid UTF-8 text survives byte-for-byte.
func TestContentHTMLUTF8Unaffected(t *testing.T) {
	raw := []byte(`<!DOCTYPE html><html><head><meta charset="utf-8"></head>` +
		`<body><p>café 日本語 plain ascii</p></body></html>`)
	text := string(contentHTMLExtract(t, raw).Text)
	for _, want := range []string{"café", "日本語", "plain ascii"} {
		if !strings.Contains(text, want) {
			t.Fatalf("utf-8 document changed; text %q missing %q", text, want)
		}
	}
}

// With no declaration, and with an unusable declared label, the shared auto
// decoder's legacy behavior is preserved (Latin-1/CP1252 bytes decode, not
// mojibake). This is the "fall back to current behavior" guarantee.
func TestContentHTMLUndeclaredLegacyUnchanged(t *testing.T) {
	latin1 := []byte("<html><body><p>caf\xe9 r\xe9sum\xe9</p></body></html>")
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"no-declaration", latin1},
		{"unusable-label", append([]byte(`<html><head><meta charset="not-a-charset"></head><body><p>caf`+"\xe9"+` r`+"\xe9"+`sum`+"\xe9"), []byte(`</p></body></html>`)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := string(contentHTMLExtract(t, tc.raw).Text)
			if !strings.Contains(text, "café") || !strings.Contains(text, "résumé") {
				t.Fatalf("legacy fallback lost: %q", text)
			}
		})
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
