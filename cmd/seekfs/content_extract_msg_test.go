package main

// WP11 .msg extractor tests: a synthetic CFB/MSG fixture built in-test (so the
// reader is exercised end to end without committing a binary blob), the three
// body encodings (UTF-16LE plain, HTML, LZFu-compressed RTF), the shared bounds,
// and the malformed-compound-file cases (bad signature, cyclic FAT, absurd
// stream size) that must skip rather than panic, hang, or allocate wildly.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

const (
	msgTestSectorSize = 512
	msgTestMiniSector = 64
	msgTestEndOfChain = 0xFFFFFFFE
	msgTestFreeSector = 0xFFFFFFFF
	msgTestFATSector  = 0xFFFFFFFD
	msgTestNoStream   = 0xFFFFFFFF
)

type msgTestStream struct {
	name string
	data []byte
}

func msgTestPropName(id, typ uint32) string {
	return "__substg1.0_" + strings.ToUpper(fmt.Sprintf("%04X%04X", id, typ))
}

func msgTestUTF16(s string) []byte {
	units := utf16.Encode([]rune(s))
	b := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(b[i*2:], u)
	}
	return b
}

func msgTestDirEntry(name string, objType byte, left, right, child, start uint32, size uint64) []byte {
	e := make([]byte, 128)
	units := utf16.Encode([]rune(name))
	nb := make([]byte, 0, 64)
	for _, u := range units {
		nb = append(nb, byte(u), byte(u>>8))
	}
	nb = append(nb, 0, 0)
	if len(nb) > 64 {
		nb = nb[:64]
	}
	copy(e, nb)
	binary.LittleEndian.PutUint16(e[64:], uint16(len(nb)))
	e[66] = objType
	e[67] = 1
	binary.LittleEndian.PutUint32(e[68:], left)
	binary.LittleEndian.PutUint32(e[72:], right)
	binary.LittleEndian.PutUint32(e[76:], child)
	binary.LittleEndian.PutUint32(e[116:], start)
	binary.LittleEndian.PutUint64(e[120:], size)
	return e
}

// msgTestBuildCFB lays out a minimal version-3 compound file: a FAT sector, the
// directory, a mini-FAT, and a mini-stream holding every (sub-4096-byte) stream.
// The root's child tree is a right-sibling chain, which the reader traverses
// without requiring a balanced red-black tree.
func msgTestBuildCFB(streams []msgTestStream) []byte {
	n := len(streams)
	miniStarts := make([]uint32, n)
	miniSecs := make([]int, n)
	cur := uint32(0)
	for i, st := range streams {
		miniStarts[i] = cur
		miniSecs[i] = (len(st.data) + msgTestMiniSector - 1) / msgTestMiniSector
		cur += uint32(miniSecs[i])
	}
	totalMini := int(cur)
	miniStream := make([]byte, totalMini*msgTestMiniSector)
	miniFAT := make([]uint32, totalMini)
	for i := range miniFAT {
		miniFAT[i] = msgTestFreeSector
	}
	for i, st := range streams {
		copy(miniStream[int(miniStarts[i])*msgTestMiniSector:], st.data)
		for k := 0; k < miniSecs[i]; k++ {
			s := int(miniStarts[i]) + k
			if k == miniSecs[i]-1 {
				miniFAT[s] = msgTestEndOfChain
			} else {
				miniFAT[s] = uint32(s + 1)
			}
		}
	}

	dirSectors := (n + 1 + 3) / 4
	miniFATSectors := (totalMini*4 + msgTestSectorSize - 1) / msgTestSectorSize
	miniStreamSectors := (len(miniStream) + msgTestSectorSize - 1) / msgTestSectorSize

	fatSectors := 1
	for {
		total := fatSectors + dirSectors + miniFATSectors + miniStreamSectors
		need := (total + 127) / 128
		if need == fatSectors {
			break
		}
		fatSectors = need
	}
	dirStart := uint32(fatSectors)
	miniFATStart := dirStart + uint32(dirSectors)
	miniStreamStart := miniFATStart + uint32(miniFATSectors)
	totalSectors := fatSectors + dirSectors + miniFATSectors + miniStreamSectors

	child := uint32(msgTestNoStream)
	if n > 0 {
		child = 1
	}
	entries := make([]byte, 0, (n+1)*128)
	entries = append(entries, msgTestDirEntry("Root Entry", 5, msgTestNoStream, msgTestNoStream, child, miniStreamStart, uint64(len(miniStream)))...)
	for i, st := range streams {
		right := uint32(msgTestNoStream)
		if i+1 < n {
			right = uint32(i + 2)
		}
		entries = append(entries, msgTestDirEntry(st.name, 2, msgTestNoStream, right, msgTestNoStream, miniStarts[i], uint64(len(st.data)))...)
	}

	fat := make([]uint32, fatSectors*128)
	for i := range fat {
		fat[i] = msgTestFreeSector
	}
	for i := 0; i < fatSectors; i++ {
		fat[i] = msgTestFATSector
	}
	chainSectors := func(start, count int) {
		for i := 0; i < count; i++ {
			s := start + i
			if i == count-1 {
				fat[s] = msgTestEndOfChain
			} else {
				fat[s] = uint32(s + 1)
			}
		}
	}
	chainSectors(int(dirStart), dirSectors)
	chainSectors(int(miniFATStart), miniFATSectors)
	chainSectors(int(miniStreamStart), miniStreamSectors)

	file := make([]byte, msgTestSectorSize+totalSectors*msgTestSectorSize)
	hdr := file[:msgTestSectorSize]
	binary.LittleEndian.PutUint64(hdr[0:], contentCFBSignature)
	binary.LittleEndian.PutUint16(hdr[24:], 0x003E)
	binary.LittleEndian.PutUint16(hdr[26:], 0x0003)
	binary.LittleEndian.PutUint16(hdr[28:], 0xFFFE)
	binary.LittleEndian.PutUint16(hdr[30:], 9)
	binary.LittleEndian.PutUint16(hdr[32:], 6)
	binary.LittleEndian.PutUint32(hdr[40:], 0) // directory sectors (v3)
	binary.LittleEndian.PutUint32(hdr[44:], uint32(fatSectors))
	binary.LittleEndian.PutUint32(hdr[48:], dirStart)
	binary.LittleEndian.PutUint32(hdr[56:], contentCFBMiniCutoff)
	firstMiniFAT := uint32(msgTestEndOfChain)
	if miniFATSectors > 0 {
		firstMiniFAT = miniFATStart
	}
	binary.LittleEndian.PutUint32(hdr[60:], firstMiniFAT)
	binary.LittleEndian.PutUint32(hdr[64:], uint32(miniFATSectors))
	binary.LittleEndian.PutUint32(hdr[68:], msgTestEndOfChain)
	binary.LittleEndian.PutUint32(hdr[72:], 0)
	for i := 0; i < 109; i++ {
		binary.LittleEndian.PutUint32(hdr[76+i*4:], msgTestFreeSector)
	}
	for i := 0; i < fatSectors; i++ {
		binary.LittleEndian.PutUint32(hdr[76+i*4:], uint32(i))
	}

	put := func(sector uint32, b []byte) {
		copy(file[msgTestSectorSize+int(sector)*msgTestSectorSize:], b)
	}
	fatBytes := make([]byte, fatSectors*msgTestSectorSize)
	for i, v := range fat {
		binary.LittleEndian.PutUint32(fatBytes[i*4:], v)
	}
	for i := 0; i < fatSectors; i++ {
		put(uint32(i), fatBytes[i*msgTestSectorSize:(i+1)*msgTestSectorSize])
	}
	dirBytes := make([]byte, dirSectors*msgTestSectorSize)
	copy(dirBytes, entries)
	for i := 0; i < dirSectors; i++ {
		put(dirStart+uint32(i), dirBytes[i*msgTestSectorSize:(i+1)*msgTestSectorSize])
	}
	if miniFATSectors > 0 {
		mf := make([]byte, miniFATSectors*msgTestSectorSize)
		for i, v := range miniFAT {
			binary.LittleEndian.PutUint32(mf[i*4:], v)
		}
		for i := 0; i < miniFATSectors; i++ {
			put(miniFATStart+uint32(i), mf[i*msgTestSectorSize:(i+1)*msgTestSectorSize])
		}
	}
	for i := 0; i < miniStreamSectors; i++ {
		chunk := miniStream[i*msgTestSectorSize:]
		if len(chunk) > msgTestSectorSize {
			chunk = chunk[:msgTestSectorSize]
		}
		put(miniStreamStart+uint32(i), chunk)
	}
	return file
}

// msgTestSetFATEntry patches FAT[sector]; fixtures use a single FAT sector at
// sector 0, so the entry lives 512 bytes into the file.
func msgTestSetFATEntry(raw []byte, sector, value uint32) {
	binary.LittleEndian.PutUint32(raw[msgTestSectorSize+int(sector)*4:], value)
}

// msgTestPatchDirEntry finds a directory entry by name and lets the caller patch
// its raw 128 bytes.
func msgTestPatchDirEntry(t *testing.T, raw []byte, name string, patch func(e []byte)) {
	t.Helper()
	dirStart := binary.LittleEndian.Uint32(raw[48:])
	base := msgTestSectorSize + int(dirStart)*msgTestSectorSize
	for off := base; off+128 <= len(raw); off += 128 {
		e := raw[off : off+128]
		nlen := int(binary.LittleEndian.Uint16(e[64:]))
		if nlen < 2 || nlen > 64 {
			continue
		}
		units := make([]uint16, 0, (nlen-2)/2)
		for i := 0; i+1 < nlen-2; i += 2 {
			units = append(units, binary.LittleEndian.Uint16(e[i:]))
		}
		if string(utf16.Decode(units)) == name {
			patch(e)
			return
		}
	}
	t.Fatalf("directory entry %q not found", name)
}

// msgTestLZFu encodes raw RTF as an LZFu stream using only literal tokens (a
// valid compressed stream the decompressor's compressed branch must handle).
func msgTestLZFu(rtf []byte) []byte {
	var payload []byte
	for i := 0; i < len(rtf); {
		payload = append(payload, 0)
		for b := 0; b < 8 && i < len(rtf); b++ {
			payload = append(payload, rtf[i])
			i++
		}
	}
	total := make([]byte, 16+len(payload))
	binary.LittleEndian.PutUint32(total[0:], uint32(len(total)-4))
	binary.LittleEndian.PutUint32(total[4:], uint32(len(rtf)))
	binary.LittleEndian.PutUint32(total[8:], contentLZFuCompressedMagic)
	copy(total[16:], payload)
	return total
}

func contentMSGExtract(t *testing.T, raw []byte) contentExtractResult {
	t.Helper()
	got, err := contentMSGLExtractor{}.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	return got
}

func contentMSGSimpleFixture() []byte {
	return msgTestBuildCFB([]msgTestStream{
		{name: msgTestPropName(contentMSGPRSubject, contentMSGTypeString), data: msgTestUTF16("subjectneedle")},
		{name: msgTestPropName(contentMSGPRSenderName, contentMSGTypeString), data: msgTestUTF16("senderneedle")},
		{name: msgTestPropName(contentMSGPRDisplayTo, contentMSGTypeString), data: msgTestUTF16("toneedle")},
		{name: msgTestPropName(contentMSGPRDisplayCc, contentMSGTypeString), data: msgTestUTF16("ccneedle")},
		{name: msgTestPropName(contentMSGPRBody, contentMSGTypeString), data: msgTestUTF16("bodyneedle here")},
	})
}

// Subject and a UTF-16LE PR_BODY are searchable through a built index, and the
// doc is stamped class=MSG / version=1.
func TestContentMSGSimpleSubjectAndBody(t *testing.T) {
	raw := contentMSGSimpleFixture()
	got := contentMSGExtract(t, raw)
	if got.Class != contentClassMSG || got.Skipped {
		t.Fatalf("result = %+v; want class MSG, not skipped", got)
	}
	for _, want := range []string{"subjectneedle", "senderneedle", "toneedle", "ccneedle", "bodyneedle"} {
		if !strings.Contains(string(got.Text), want) {
			t.Fatalf("text %q missing %q", got.Text, want)
		}
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mail.msg"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := buildContentIndexFromDir(context.Background(), dir, defaultContentBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Docs) != 1 {
		t.Fatalf("built %d docs; want 1", len(idx.Docs))
	}
	d := idx.Docs[0]
	if d.ContentType != contentClassMSG || d.ExtractorVersion != (contentMSGLExtractor{}).Version() {
		t.Fatalf("doc identity = (%d,%d); want MSG v1", d.ContentType, d.ExtractorVersion)
	}
	r, err := openContentReader(idx)
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range []string{"subjectneedle", "bodyneedle"} {
		if hits := r.search(term, 0); len(hits) != 1 || hits[0].Path != "mail.msg" {
			t.Fatalf("term %q not findable: %v", term, contentPathsOf(hits))
		}
	}
}

// An HTML-only body is stripped of markup by the shared HTML walk.
func TestContentMSGHTMLBodyOnly(t *testing.T) {
	raw := msgTestBuildCFB([]msgTestStream{
		{name: msgTestPropName(contentMSGPRSubject, contentMSGTypeString), data: msgTestUTF16("html subj")},
		{name: msgTestPropName(contentMSGPRHTML, contentMSGTypeBinary), data: []byte("<html><body><p>htmlneedle</p><b>tagged</b></body></html>")},
	})
	text := string(contentMSGExtract(t, raw).Text)
	for _, want := range []string{"htmlneedle", "tagged"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text %q missing %q", text, want)
		}
	}
	for _, banned := range []string{"<p>", "<b>", "<html>"} {
		if strings.Contains(text, banned) {
			t.Fatalf("extracted markup %q: %q", banned, text)
		}
	}
}

// A compressed-RTF-only body is LZFu-inflated and walked by the RTF extractor.
func TestContentMSGCompressedRTFBodyOnly(t *testing.T) {
	rtf := []byte(`{\rtf1\ansi\deff0{\fonttbl{\f0\fnil Arial;}} rtfneedle \b bodyneedle\b0!}`)
	raw := msgTestBuildCFB([]msgTestStream{
		{name: msgTestPropName(contentMSGPRSubject, contentMSGTypeString), data: msgTestUTF16("rtf subj")},
		{name: msgTestPropName(contentMSGPRRTFCompressed, contentMSGTypeBinary), data: msgTestLZFu(rtf)},
	})
	text := string(contentMSGExtract(t, raw).Text)
	for _, want := range []string{"rtfneedle", "bodyneedle"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text %q missing %q", text, want)
		}
	}
	if strings.Contains(text, "Arial") {
		t.Fatalf("font table leaked into text: %q", text)
	}
}

// PR_BODY wins over HTML and RTF when all three are present.
func TestContentMSGBodyPreference(t *testing.T) {
	raw := msgTestBuildCFB([]msgTestStream{
		{name: msgTestPropName(contentMSGPRBody, contentMSGTypeString), data: msgTestUTF16("plainneedle")},
		{name: msgTestPropName(contentMSGPRHTML, contentMSGTypeBinary), data: []byte("<p>htmlneedle</p>")},
		{name: msgTestPropName(contentMSGPRRTFCompressed, contentMSGTypeBinary), data: msgTestLZFu([]byte(`{\rtf1\ansi rtfneedle}`))},
	})
	text := string(contentMSGExtract(t, raw).Text)
	if !strings.Contains(text, "plainneedle") {
		t.Fatalf("PR_BODY not preferred: %q", text)
	}
	if strings.Contains(text, "htmlneedle") || strings.Contains(text, "rtfneedle") {
		t.Fatalf("fallback body indexed alongside PR_BODY: %q", text)
	}
}

// Malformed compound files skip with a reason, without panicking, hanging, or
// allocating from an attacker-declared size.
func TestContentMSGMalformedSkipped(t *testing.T) {
	// Bad signature.
	badSig := contentMSGSimpleFixture()
	badSig[0] ^= 0xFF

	// Cyclic directory FAT chain: sectors 1 and 2 point at each other.
	cyclic := contentMSGSimpleFixture()
	msgTestSetFATEntry(cyclic, 1, 2)
	msgTestSetFATEntry(cyclic, 2, 1)

	// Absurd stream size on the body property: must be clamped, not allocated.
	absurd := contentMSGSimpleFixture()
	msgTestPatchDirEntry(t, absurd, msgTestPropName(contentMSGPRBody, contentMSGTypeString), func(e []byte) {
		binary.LittleEndian.PutUint64(e[120:], 1<<62)
	})

	// Truncated header.
	short := contentMSGSimpleFixture()[:256]

	for name, raw := range map[string][]byte{
		"bad-signature": badSig,
		"cyclic-fat":    cyclic,
		"absurd-size":   absurd,
		"short-header":  short,
	} {
		raw := raw
		t.Run(name, func(t *testing.T) {
			done := make(chan contentExtractResult, 1)
			go func() {
				res, err := contentMSGLExtractor{}.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)))
				if err != nil {
					t.Errorf("malformed input returned error: %v", err)
				}
				done <- res
			}()
			select {
			case res := <-done:
				if res.Class != contentClassMSG {
					t.Fatalf("class = %d; want MSG", res.Class)
				}
				if name == "bad-signature" || name == "cyclic-fat" || name == "absurd-size" || name == "short-header" {
					if !res.Skipped {
						t.Fatalf("%s: want Skipped, got %+v", name, res)
					}
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s: extraction hung", name)
			}
		})
	}
}

// A file over the raw cap is skipped; a body over the text cap truncates with a
// reason and Truncated set.
func TestContentMSGBounds(t *testing.T) {
	raw := contentMSGSimpleFixture()

	overRaw := contentWithExtractSettings(context.Background(), contentExtractSettings{maxRaw: 64, maxText: 1 << 20})
	got, err := contentMSGLExtractor{}.Extract(overRaw, bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Skipped || got.Reason != "raw size over cap" {
		t.Fatalf("over-raw-cap = %+v; want Skipped raw size over cap", got)
	}

	tiny := contentWithExtractSettings(context.Background(), contentExtractSettings{maxRaw: 1 << 20, maxText: 8})
	got, err = contentMSGLExtractor{}.Extract(tiny, bytes.NewReader(raw), int64(len(raw)))
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

// The compound reader enumerates the root storage's named streams.
func TestContentCFBStreamEnumeration(t *testing.T) {
	raw := contentMSGSimpleFixture()
	cfb, err := newContentCFBReader(context.Background(), bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	names := cfb.streamNames()
	if len(names) != 5 {
		t.Fatalf("enumerated %d streams; want 5: %v", len(names), names)
	}
	found := false
	for _, n := range names {
		if n == msgTestPropName(contentMSGPRSubject, contentMSGTypeString) {
			found = true
		}
	}
	if !found {
		t.Fatalf("subject stream missing from enumeration: %v", names)
	}
}

// LZFu back-references resolve through the dictionary and the output is hard
// capped at maxOut.
func TestContentLZFuBoundsAndBackReference(t *testing.T) {
	// Literal 'A' then a 3-byte back-reference at offset 207 (the first byte past
	// the dictionary preload), producing "AAAA".
	payload := []byte{0x02, 'A', 0x0C, 0xF1}
	total := make([]byte, 16+len(payload))
	binary.LittleEndian.PutUint32(total[0:], uint32(len(total)-4))
	binary.LittleEndian.PutUint32(total[4:], 4)
	binary.LittleEndian.PutUint32(total[8:], contentLZFuCompressedMagic)
	copy(total[16:], payload)
	out, _, err := contentLZFuDecompress(context.Background(), total, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "AAAA" {
		t.Fatalf("back-reference output = %q; want AAAA", out)
	}

	// A stream declaring an absurd rawSize must not allocate it, and the output
	// is capped at maxOut.
	litPayload := append([]byte{0x00}, []byte("abcdefgh")...)
	big := make([]byte, 16+len(litPayload))
	binary.LittleEndian.PutUint32(big[0:], uint32(len(big)-4))
	binary.LittleEndian.PutUint32(big[4:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(big[8:], contentLZFuCompressedMagic)
	copy(big[16:], litPayload)
	out, truncated, err := contentLZFuDecompress(context.Background(), big, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(out) > 3 {
		t.Fatalf("hard cap not honoured: len=%d truncated=%v", len(out), truncated)
	}
}

func TestContentServiceAllowlistIncludesMSG(t *testing.T) {
	if _, ok := contentServiceExtensions()[".msg"]; !ok {
		t.Fatal(".msg missing from the service content allowlist")
	}
	if e := contentExtractorForPath("mail.msg", contentMSGSimpleFixture()[:8]); e == nil || e.Name() != "msg" {
		t.Fatalf(".msg claimed by %v; want msg", e)
	}
}
