package main

// WP11 PDF extractor tests. Every fixture is a tiny, syntactically valid PDF
// built in-test: a classic xref table, an xref stream, an object stream, and the
// text-layer cases (Flate/ASCII85/ASCIIHex/LZW, /ToUnicode). The malformed cases
// are the ones that must stay a Skipped — never a panic, a hang, or an
// attacker-sized allocation — so they run behind a hang guard and a loose
// allocation bound, mirroring the MSG tests.

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/ascii85"
	"encoding/hex"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ----- fixtures -----

func contentPDFStreamBytes(data []byte, extraDict string) []byte {
	dict := fmt.Sprintf("/Length %d", len(data))
	if extraDict != "" {
		dict = extraDict + " " + dict
	}
	return []byte("<< " + dict + " >>\nstream\n" + string(data) + "\nendstream")
}

func contentPDFBaseObjects(content []byte, contentDict string) [][]byte {
	return [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
		contentPDFStreamBytes(content, contentDict),
	}
}

func contentPDFAssembleWithTrailer(objects [][]byte, root int, extraTrailer string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n")
	offs := make([]int, len(objects)+1)
	for i, o := range objects {
		offs[i+1] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n", i+1)
		b.Write(o)
		b.WriteString("\nendobj\n")
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n", len(objects)+1)
	b.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", offs[i])
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root %d 0 R", len(objects)+1, root)
	if extraTrailer != "" {
		fmt.Fprintf(&b, " %s", extraTrailer)
	}
	fmt.Fprintf(&b, " >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return b.Bytes()
}

func contentPDFAssemble(objects [][]byte, root int) []byte {
	return contentPDFAssembleWithTrailer(objects, root, "")
}

// contentPDFTestDoc keeps the helper the identity test uses: a minimal
// single-content-stream PDF, optionally Flate-compressed.
func contentPDFTestDoc(content []byte, compress bool) []byte {
	data := content
	extra := ""
	if compress {
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		zw.Write(content)
		zw.Close()
		data = z.Bytes()
		extra = "/Filter /FlateDecode"
	}
	return contentPDFAssemble(contentPDFBaseObjects(data, extra), 1)
}

func contentPDFWriteXRefEntry(buf *bytes.Buffer, typ byte, f1, f2 uint32) {
	buf.WriteByte(typ)
	buf.WriteByte(byte(f1 >> 24))
	buf.WriteByte(byte(f1 >> 16))
	buf.WriteByte(byte(f1 >> 8))
	buf.WriteByte(byte(f1))
	buf.WriteByte(byte(f2 >> 8))
	buf.WriteByte(byte(f2))
}

// contentPDFAssembleXRefStream builds the same document with a /Type /XRef
// cross-reference stream as the newest section and only entry.
func contentPDFAssembleXRefStream(objects [][]byte, root int) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n")
	n := len(objects)
	offs := make([]int, n+2)
	for i, o := range objects {
		offs[i+1] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n", i+1)
		b.Write(o)
		b.WriteString("\nendobj\n")
	}
	xrefNum := n + 1
	xrefOff := b.Len()
	offs[xrefNum] = xrefOff
	var payload bytes.Buffer
	contentPDFWriteXRefEntry(&payload, 0, 0, 0)
	for i := 1; i <= n; i++ {
		contentPDFWriteXRefEntry(&payload, 1, uint32(offs[i]), 0)
	}
	contentPDFWriteXRefEntry(&payload, 1, uint32(xrefOff), 0)
	data := payload.Bytes()
	fmt.Fprintf(&b, "%d 0 obj\n<< /Type /XRef /Size %d /W [1 4 2] /Root %d 0 R /Length %d >>\nstream\n", xrefNum, xrefNum+1, root, len(data))
	b.Write(data)
	b.WriteString("\nendstream\nendobj\n")
	fmt.Fprintf(&b, "startxref\n%d\n%%%%EOF\n", xrefOff)
	return b.Bytes()
}

// contentPDFAssembleObjStm puts objects 1..4 in a Flate object stream (object 6)
// and the content stream in object 5; the xref stream (object 7) maps 1..4 as
// compressed entries.
func contentPDFAssembleObjStm(content []byte, root int) []byte {
	inner := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var payload bytes.Buffer
	positions := make([]int, len(inner))
	for i, s := range inner {
		positions[i] = payload.Len()
		payload.WriteString(s)
		payload.WriteByte(' ')
	}
	var header bytes.Buffer
	for i := range inner {
		fmt.Fprintf(&header, "%d %d ", i+1, positions[i])
	}
	objStmData := append(header.Bytes(), payload.Bytes()...)
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(objStmData)
	zw.Close()

	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n")
	off5 := b.Len()
	fmt.Fprintf(&b, "5 0 obj\n")
	b.Write(contentPDFStreamBytes(content, ""))
	b.WriteString("\nendobj\n")
	off6 := b.Len()
	fmt.Fprintf(&b, "6 0 obj\n<< /Type /ObjStm /N %d /First %d /Filter /FlateDecode /Length %d >>\nstream\n", len(inner), header.Len(), z.Len())
	b.Write(z.Bytes())
	b.WriteString("\nendstream\nendobj\n")
	xrefOff := b.Len()
	var payloadX bytes.Buffer
	contentPDFWriteXRefEntry(&payloadX, 0, 0, 0)
	for i := 0; i < len(inner); i++ {
		contentPDFWriteXRefEntry(&payloadX, 2, 6, uint32(i))
	}
	contentPDFWriteXRefEntry(&payloadX, 1, uint32(off5), 0)
	contentPDFWriteXRefEntry(&payloadX, 1, uint32(off6), 0)
	contentPDFWriteXRefEntry(&payloadX, 1, uint32(xrefOff), 0)
	data := payloadX.Bytes()
	fmt.Fprintf(&b, "7 0 obj\n<< /Type /XRef /Size 8 /W [1 4 2] /Index [0 8] /Root %d 0 R /Length %d >>\nstream\n", root, len(data))
	b.Write(data)
	b.WriteString("\nendstream\nendobj\n")
	fmt.Fprintf(&b, "startxref\n%d\n%%%%EOF\n", xrefOff)
	return b.Bytes()
}

func contentPDFTestExtract(t *testing.T, doc []byte) contentExtractResult {
	t.Helper()
	got, err := contentPDFExtractor{}.Extract(context.Background(), bytes.NewReader(doc), int64(len(doc)))
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	return got
}

func contentPDFAssertContains(t *testing.T, got contentExtractResult, want string) {
	t.Helper()
	if !strings.Contains(string(got.Text), want) {
		t.Fatalf("text %q missing %q; result %+v", got.Text, want, got)
	}
	if got.Class != contentClassPDF || got.Skipped {
		t.Fatalf("result = %+v; want class PDF, not skipped", got)
	}
}

func contentPDFStartXRefOffset(doc []byte) (int, int) {
	i := bytes.LastIndex(doc, []byte("startxref"))
	j := i + len("startxref")
	for j < len(doc) && (doc[j] == ' ' || doc[j] == '\n' || doc[j] == '\r') {
		j++
	}
	k := j
	for k < len(doc) && doc[k] >= '0' && doc[k] <= '9' {
		k++
	}
	return j, k
}

// ----- text-layer cases -----

func TestContentPDFExtractsPlainText(t *testing.T) {
	got := contentPDFTestExtract(t, contentPDFTestDoc([]byte("BT /F1 12 Tf (alpha needle beta) Tj ET"), false))
	contentPDFAssertContains(t, got, "alpha needle beta")
}

func TestContentPDFVersionIsTwo(t *testing.T) {
	if v := (contentPDFExtractor{}).Version(); v != 2 {
		t.Fatalf("PDF extractor version = %d; want 2 (bump re-extracts old doc v1 text)", v)
	}
}

func TestContentPDFExtractsFlateText(t *testing.T) {
	got := contentPDFTestExtract(t, contentPDFTestDoc([]byte("BT /F1 12 Tf (alpha needle beta) Tj ET"), true))
	contentPDFAssertContains(t, got, "alpha needle beta")
}

func TestContentPDFExtractsTJArray(t *testing.T) {
	got := contentPDFTestExtract(t, contentPDFTestDoc([]byte("BT /F1 12 Tf [(hel) -50 (lo)] TJ ET"), false))
	text := string(got.Text)
	if !strings.Contains(text, "hel") || !strings.Contains(text, "lo") {
		t.Fatalf("text %q missing TJ operands", text)
	}
}

func TestContentPDFPositioningSeparatesWords(t *testing.T) {
	content := []byte("BT /F1 12 Tf 40 700 Td (firstneedle) Tj 0 -20 Td (secondneedle) Tj ET")
	got := contentPDFTestExtract(t, contentPDFTestDoc(content, false))
	text := string(got.Text)
	if !strings.Contains(text, "firstneedle") || !strings.Contains(text, "secondneedle") {
		t.Fatalf("text %q missing positioned words", text)
	}
	if !strings.Contains(text, "\n") {
		t.Fatalf("newline on vertical move missing: %q", text)
	}
}

func TestContentPDFASCII85(t *testing.T) {
	content := []byte("BT /F1 12 Tf (ascii85needle) Tj ET")
	var buf bytes.Buffer
	w := ascii85.NewEncoder(&buf)
	w.Write(content)
	w.Close()
	got := contentPDFTestExtract(t, contentPDFAssemble(contentPDFBaseObjects(buf.Bytes(), "/Filter /ASCII85Decode"), 1))
	contentPDFAssertContains(t, got, "ascii85needle")
}

func TestContentPDFASCIIHex(t *testing.T) {
	content := []byte("BT /F1 12 Tf (asciihexneedle) Tj ET")
	encoded := strings.ToUpper(hex.EncodeToString(content)) + ">"
	got := contentPDFTestExtract(t, contentPDFAssemble(contentPDFBaseObjects([]byte(encoded), "/Filter /ASCIIHexDecode"), 1))
	contentPDFAssertContains(t, got, "asciihexneedle")
}

func TestContentPDFRunLength(t *testing.T) {
	content := []byte("BT /F1 12 Tf (runlengthneedle) Tj ET")
	var enc []byte
	for i := 0; i < len(content); i += 128 {
		end := i + 128
		if end > len(content) {
			end = len(content)
		}
		enc = append(enc, byte(end-i-1))
		enc = append(enc, content[i:end]...)
	}
	enc = append(enc, 128)
	got := contentPDFTestExtract(t, contentPDFAssemble(contentPDFBaseObjects(enc, "/Filter /RunLengthDecode"), 1))
	contentPDFAssertContains(t, got, "runlengthneedle")
}

func TestContentPDFASCII85RejectsOverflow(t *testing.T) {
	// A malformed all-'u' group would overflow uint32; it must be rejected, not
	// silently wrapped into four bytes.
	if out, _ := contentPDFASCII85([]byte("uuuuu~>"), 1<<20); len(out) != 0 {
		t.Fatalf("overflowing group emitted %d bytes: %x", len(out), out)
	}
	// The largest valid group still decodes to 0xFFFFFFFF.
	got, _ := contentPDFASCII85([]byte("s8W-!~>"), 1<<20)
	if !bytes.Equal(got, []byte{0xFF, 0xFF, 0xFF, 0xFF}) {
		t.Fatalf("max valid group = %x; want ffffffff", got)
	}
}

func TestContentPDFRunLengthRespectsCap(t *testing.T) {
	// Two 128-byte literal runs, a cap of 50: the literal branch must clamp.
	var enc []byte
	for i := 0; i < 2; i++ {
		enc = append(enc, 127) // copy the next 128 bytes literally
		enc = append(enc, bytes.Repeat([]byte{'A'}, 128)...)
	}
	enc = append(enc, 128)
	const maxOut = 50
	out, truncated := contentPDFRunLength(enc, maxOut)
	if !truncated {
		t.Fatalf("want truncated=true at cap")
	}
	if int64(len(out)) > maxOut {
		t.Fatalf("runlength output = %d bytes; cap %d", len(out), maxOut)
	}
}

func TestContentPDFLZW(t *testing.T) {
	content := []byte("BT /F1 12 Tf (lzwneedle) Tj ET")
	got := contentPDFTestExtract(t, contentPDFAssemble(contentPDFBaseObjects(contentPDFLZWEncodeLiterals(content), "/Filter /LZWDecode"), 1))
	contentPDFAssertContains(t, got, "lzwneedle")
}

// contentPDFLZWEncodeLiterals emits a valid PDF-LZW stream using only clear,
// literal and EOD codes (no table growth, so the 9-bit width never changes).
func contentPDFLZWEncodeLiterals(data []byte) []byte {
	var out []byte
	var acc uint32
	n := 0
	put := func(code, width int) {
		acc = acc<<uint(width) | uint32(code)
		n += width
		for n >= 8 {
			out = append(out, byte(acc>>uint(n-8)))
			n -= 8
		}
	}
	put(256, 9)
	for _, b := range data {
		if b >= 128 {
			b = 127
		}
		put(int(b), 9)
	}
	put(257, 9)
	if n > 0 {
		out = append(out, byte(acc<<uint(8-n)))
	}
	return out
}

func TestContentPDFXRefStream(t *testing.T) {
	got := contentPDFTestExtract(t, contentPDFAssembleXRefStream(contentPDFBaseObjects([]byte("BT /F1 12 Tf (xrefstreamneedle) Tj ET"), ""), 1))
	contentPDFAssertContains(t, got, "xrefstreamneedle")
}

func TestContentPDFObjectStream(t *testing.T) {
	got := contentPDFTestExtract(t, contentPDFAssembleObjStm([]byte("BT /F1 12 Tf (objstmneedle) Tj ET"), 1))
	contentPDFAssertContains(t, got, "objstmneedle")
}

// Text drawn through a Form XObject (`/Fm0 Do`) must be extracted: many
// LaTeX/print PDFs put the whole page in a form with the page carrying only
// /XObject (no /Font). Found by production testing (a 32-page paper extracted
// zero text). Nested forms are followed too.
func TestContentPDFFormXObject(t *testing.T) {
	form := contentPDFStreamBytes([]byte("BT /F1 12 Tf (formxobjneedle) Tj ET"),
		"/Type /XObject /Subtype /Form /BBox [0 0 612 792] /Resources << /Font << /F1 6 0 R >> >>")
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /XObject << /Fm0 4 0 R >> >> /Contents 5 0 R >>"),
		form,
		contentPDFStreamBytes([]byte("q /Fm0 Do Q"), ""),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
	}
	contentPDFAssertContains(t, contentPDFTestExtract(t, contentPDFAssemble(objs, 1)), "formxobjneedle")
}

func TestContentPDFNestedFormXObject(t *testing.T) {
	inner := contentPDFStreamBytes([]byte("BT /F1 12 Tf (nestedformneedle) Tj ET"),
		"/Type /XObject /Subtype /Form /BBox [0 0 10 10] /Resources << /Font << /F1 7 0 R >> >>")
	outer := contentPDFStreamBytes([]byte("/FmInner Do"),
		"/Type /XObject /Subtype /Form /BBox [0 0 10 10] /Resources << /XObject << /FmInner 6 0 R >> >>")
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /XObject << /FmOuter 4 0 R >> >> /Contents 5 0 R >>"),
		outer,
		contentPDFStreamBytes([]byte("/FmOuter Do"), ""),
		inner,
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
	}
	contentPDFAssertContains(t, contentPDFTestExtract(t, contentPDFAssemble(objs, 1)), "nestedformneedle")
}

// A form that draws itself must terminate at the depth bound, not recurse
// forever or allocate unboundedly.
func TestContentPDFFormCycleTerminates(t *testing.T) {
	self := contentPDFStreamBytes([]byte("/FmSelf Do"),
		"/Type /XObject /Subtype /Form /BBox [0 0 10 10] /Resources << /XObject << /FmSelf 4 0 R >> >>")
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /XObject << /FmSelf 4 0 R >> >> /Contents 5 0 R >>"),
		self,
		contentPDFStreamBytes([]byte("/FmSelf Do"), ""),
	}
	done := make(chan contentExtractResult, 1)
	go func() { done <- contentPDFTestExtract(t, contentPDFAssemble(objs, 1)) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("self-referential form did not terminate")
	}
}

// A ligature mapped to its Unicode presentation form (U+FB01/U+FB02) is
// expanded to ASCII letters, so a word set with an fi/fl glyph stays searchable
// as plain text.
func TestContentPDFLigatureExpanded(t *testing.T) {
	cmap := "/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n1 beginbfchar\n<90> <FB02>\nendbfchar\nendcmap\nend\n"
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /ToUnicode 6 0 R >>"),
		contentPDFStreamBytes([]byte("BT /F1 12 Tf (work) Tj <90> Tj (ows) Tj ET"), ""),
		contentPDFStreamBytes([]byte(cmap), ""),
	}
	contentPDFAssertContains(t, contentPDFTestExtract(t, contentPDFAssemble(objs, 1)), "workflows")
}

// A /ToUnicode destination may be several runes (a producer spelling the fi
// ligature as the ASCII letters "fi"); all of them must be kept, not just the
// first.
func TestContentPDFToUnicodeMultiRune(t *testing.T) {
	cmap := "/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n1 beginbfchar\n<1C> <00660069>\nendbfchar\nendcmap\nend\n"
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /ToUnicode 6 0 R >>"),
		contentPDFStreamBytes([]byte("BT /F1 12 Tf (con) Tj <1C> Tj (gurations) Tj ET"), ""),
		contentPDFStreamBytes([]byte(cmap), ""),
	}
	contentPDFAssertContains(t, contentPDFTestExtract(t, contentPDFAssemble(objs, 1)), "configurations")
}

// A /Differences array may name plain Latin letters; those names must map even
// though the table does not list every letter.
func TestContentPDFDifferencesLetterNames(t *testing.T) {
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding << /Type /Encoding /Differences [65 /W /A /T /E /R] >> >>"),
		contentPDFStreamBytes([]byte("BT /F1 12 Tf <4142434445> Tj ET"), ""),
	}
	contentPDFAssertContains(t, contentPDFTestExtract(t, contentPDFAssemble(objs, 1)), "WATER")
}

func TestContentPDFToUnicodeMapping(t *testing.T) {
	cmap := "/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n1 beginbfchar\n<90> <00E9>\nendbfchar\nendcmap\nend\n"
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /ToUnicode 6 0 R >>"),
		contentPDFStreamBytes([]byte("BT /F1 12 Tf (caf) Tj <90> Tj ET"), ""),
		contentPDFStreamBytes([]byte(cmap), ""),
	}
	got := contentPDFTestExtract(t, contentPDFAssemble(objs, 1))
	if !strings.Contains(string(got.Text), "café") {
		t.Fatalf("text %q missing ToUnicode-decoded café", got.Text)
	}
}

func contentPDFCMapHexValue(code uint32) contentPDFValue {
	return contentPDFValue{kind: contentPDFString, str: []byte{byte(code >> 24), byte(code >> 16), byte(code >> 8), byte(code)}}
}

func contentPDFCMapRuneValue(r rune) contentPDFValue {
	return contentPDFValue{kind: contentPDFString, str: []byte{byte(r >> 8), byte(r)}}
}

// TestContentPDFCMapEntryCapEnforced drives the bfchar and bfrange-array
// insertion paths with more entries than contentPDFMaxCMapEntry allows. The
// lexer's operand budget normally bounds a real /ToUnicode stream, so the cap is
// exercised directly at the insertion functions.
func TestContentPDFCMapEntryCapEnforced(t *testing.T) {
	const over = 10

	bfcharOps := make([]contentPDFValue, 0, 2*(contentPDFMaxCMapEntry+over))
	for i := 0; i < contentPDFMaxCMapEntry+over; i++ {
		bfcharOps = append(bfcharOps, contentPDFCMapHexValue(uint32(i)), contentPDFCMapRuneValue('x'))
	}
	out := make(map[uint32]string)
	contentPDFCMapBFChar(out, bfcharOps)
	if len(out) > contentPDFMaxCMapEntry {
		t.Fatalf("bfchar map = %d entries; cap %d", len(out), contentPDFMaxCMapEntry)
	}

	arr := make([]contentPDFValue, contentPDFMaxCMapEntry+over)
	for i := range arr {
		arr[i] = contentPDFCMapRuneValue('x')
	}
	out2 := make(map[uint32]string)
	contentPDFCMapBFRange(out2, []contentPDFValue{
		contentPDFCMapHexValue(0),
		contentPDFCMapHexValue(0xFFFFFFFF),
		{kind: contentPDFArray, arr: arr},
	})
	if len(out2) > contentPDFMaxCMapEntry {
		t.Fatalf("bfrange-array map = %d entries; cap %d", len(out2), contentPDFMaxCMapEntry)
	}
}

// TestContentPDFCMapManyBFCharBlocksCapped parses a /ToUnicode stream of many
// bfchar blocks and asserts the decoded map stays within the documented bound.
func TestContentPDFCMapManyBFCharBlocksCapped(t *testing.T) {
	var b strings.Builder
	for i := 0; i < contentPDFMaxCMapEntry+1000; i++ {
		fmt.Fprintf(&b, "beginbfchar <%04X> <0078> endbfchar\n", i)
	}
	m := contentPDFParseCMap([]byte(b.String()))
	if len(m) > contentPDFMaxCMapEntry {
		t.Fatalf("bfchar stream map = %d entries; cap %d", len(m), contentPDFMaxCMapEntry)
	}
}

func TestContentPDFType0WithoutToUnicodeDropped(t *testing.T) {
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"),
		[]byte("<< /Type /Font /Subtype /Type0 /BaseFont /SomeCID /Encoding /Identity-H >>"),
		contentPDFStreamBytes([]byte("BT /F1 12 Tf <0001> Tj ET"), ""),
	}
	got := contentPDFTestExtract(t, contentPDFAssemble(objs, 1))
	if !got.Skipped || got.Reason != "no extractable text" {
		t.Fatalf("CID font without ToUnicode should yield no text: %+v", got)
	}
	if strings.Contains(string(got.Text), "\x01") {
		t.Fatalf("CID codes leaked as text: %q", got.Text)
	}
}

func TestContentPDFDifferencesEncoding(t *testing.T) {
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding << /Differences [65 /eacute] >> >>"),
		contentPDFStreamBytes([]byte("BT /F1 12 Tf (A) Tj ET"), ""),
	}
	got := contentPDFTestExtract(t, contentPDFAssemble(objs, 1))
	if !strings.Contains(string(got.Text), "é") {
		t.Fatalf("Differences glyph not decoded: %q", got.Text)
	}
}

func TestContentPDFSkipsNonPDF(t *testing.T) {
	raw := []byte("this is definitely not a pdf file")
	got := contentPDFTestExtract(t, raw)
	if !got.Skipped || got.Reason != "not a pdf" {
		t.Fatalf("got %+v", got)
	}
}

func TestContentPDFEncryptedSkipped(t *testing.T) {
	objs := contentPDFBaseObjects([]byte("BT (secret) Tj ET"), "")
	objs = append(objs, []byte("<< /Filter /Standard /V 1 /R 2 /O <> /U <> /P -44 >>"))
	got := contentPDFTestExtract(t, contentPDFAssembleWithTrailer(objs, 1, "/Encrypt 6 0 R"))
	if !got.Skipped || !strings.Contains(got.Reason, "encrypted") {
		t.Fatalf("encrypted pdf = %+v; want Skipped with an encryption reason", got)
	}
	if len(got.Text) != 0 {
		t.Fatalf("encrypted pdf leaked text: %q", got.Text)
	}
}

// ----- bounds and malformed inputs -----

func TestContentPDFTextTruncation(t *testing.T) {
	content := []byte("BT /F1 12 Tf (aaaaaaaaaabbbbbbbbbbcccccccccc) Tj ET")
	doc := contentPDFTestDoc(content, false)
	ctx := contentWithExtractSettings(context.Background(), contentExtractSettings{maxRaw: 1 << 20, maxText: 8})
	got, err := contentPDFExtractor{}.Extract(ctx, bytes.NewReader(doc), int64(len(doc)))
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

func TestContentPDFOverRawCapSkipped(t *testing.T) {
	doc := contentPDFTestDoc([]byte("BT (needle) Tj ET"), false)
	ctx := contentWithExtractSettings(context.Background(), contentExtractSettings{maxRaw: 64, maxText: 1 << 20})
	got, err := contentPDFExtractor{}.Extract(ctx, bytes.NewReader(doc), int64(len(doc)))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Skipped || got.Reason != "raw size over cap" {
		t.Fatalf("over-raw-cap = %+v; want Skipped raw size over cap", got)
	}
}

func TestContentPDFMalformedSkipped(t *testing.T) {
	minimal := contentPDFTestDoc([]byte("BT (needle) Tj ET"), false)

	// Bad xref: startxref points far past EOF.
	badXRef := append([]byte(nil), minimal...)
	js, je := contentPDFStartXRefOffset(badXRef)
	badXRef = append(append(append([]byte(nil), badXRef[:js]...), []byte("9999999999")...), badXRef[je:]...)

	// Cyclic /Prev: the trailer's /Prev points back at the same xref section.
	cyclic := contentPDFWithSelfPrev(minimal)

	// Absurd /Length with no endstream: the stream cannot be delimited.
	absurdObjs := contentPDFBaseObjects(nil, "")
	absurdObjs[4] = []byte("<< /Length 9223372036854775807 >>\nstream\nBT (needle) Tj")
	absurd := contentPDFAssemble(absurdObjs, 1)

	// Truncated file: nothing after the header.
	truncated := minimal[:len(minimal)/2]

	// Decompression bomb: ~64 KiB of Flate decoding to many MiB of NULs, under a
	// 1 MiB decode cap.
	var bombZ bytes.Buffer
	zw := zlib.NewWriter(&bombZ)
	zero := make([]byte, 1<<20)
	for i := 0; i < 8; i++ {
		zw.Write(zero)
	}
	zw.Close()
	bomb := contentPDFAssemble(contentPDFBaseObjects(bombZ.Bytes(), "/Filter /FlateDecode"), 1)

	cases := map[string][]byte{
		"bad-xref":      badXRef,
		"cyclic-prev":   cyclic,
		"absurd-length": absurd,
		"truncated":     truncated,
		"bomb":          bomb,
	}
	for name, doc := range cases {
		doc := doc
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if name == "bomb" {
				ctx = contentWithExtractSettings(ctx, contentExtractSettings{maxRaw: 1 << 20, maxText: 1 << 20})
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			done := make(chan contentExtractResult, 1)
			go func() {
				res, err := contentPDFExtractor{}.Extract(ctx, bytes.NewReader(doc), int64(len(doc)))
				if err != nil {
					t.Errorf("malformed input returned error: %v", err)
				}
				done <- res
			}()
			select {
			case res := <-done:
				runtime.ReadMemStats(&after)
				if res.Class != contentClassPDF {
					t.Fatalf("class = %d; want PDF", res.Class)
				}
				if !res.Skipped {
					t.Fatalf("%s: want Skipped, got %+v", name, res)
				}
				if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 256<<20 {
					t.Fatalf("%s: allocated %d bytes; want bounded", name, allocated)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s: extraction hung", name)
			}
		})
	}
}

// contentPDFWithSelfPrev inserts /Prev pointing at the file's own xref offset so
// the chain revisits a section (a cycle the loader must reject).
func contentPDFWithSelfPrev(doc []byte) []byte {
	js, je := contentPDFStartXRefOffset(doc)
	off := string(doc[js:je])
	marker := []byte("trailer\n<< ")
	idx := bytes.Index(doc, marker)
	if idx < 0 {
		return doc
	}
	at := idx + len(marker)
	out := append([]byte(nil), doc[:at]...)
	out = append(out, []byte("/Prev "+off+" ")...)
	out = append(out, doc[at:]...)
	return out
}

func TestContentPDFServiceAllowlistIncludesPDF(t *testing.T) {
	if _, ok := contentServiceExtensions()[".pdf"]; !ok {
		t.Fatal(".pdf missing from the service content allowlist")
	}
	if e := contentExtractorForPath("doc.pdf", []byte("%PDF-1.7")); e == nil || e.Name() != "pdf" {
		t.Fatalf(".pdf claimed by %v; want pdf", e)
	}
}
