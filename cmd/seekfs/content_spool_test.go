package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A compressed spool round-trips every doc byte-exact in any read order, and
// stages many fewer bytes than the raw text.
func TestContentSpoolRoundTrip(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "text.spool"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := newContentSpoolWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	texts := [][]byte{
		[]byte("hello needle one"),
		bytes.Repeat([]byte("abcdefghij"), 10000), // compressible
		[]byte("Ünïcödé — Sydney Январь †"),
		[]byte("x"),
	}
	type frame struct {
		off, comp, orig int64
	}
	var frames []frame
	var raw int64
	for _, tx := range texts {
		off, comp, err := w.Append(tx)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame{off, int64(comp), int64(len(tx))})
		raw += int64(len(tx))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := w.StagedBytes(); got <= 0 || got >= raw {
		t.Fatalf("staged = %d; want positive and below raw %d", got, raw)
	}
	if st, err := f.Stat(); err != nil || st.Size() != w.StagedBytes() {
		t.Fatalf("file size = %v; staged = %d", st, w.StagedBytes())
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f, err = os.Open(filepath.Join(dir, "text.spool"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := newContentSpoolReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Reverse read order: frames are independent.
	for i := len(frames) - 1; i >= 0; i-- {
		got, err := r.Read(frames[i].off, int(frames[i].comp), int(frames[i].orig))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, texts[i]) {
			t.Fatalf("doc %d mismatch: %q vs %q", i, got, texts[i])
		}
	}
}

// A corrupt or truncated spool frame fails the read instead of returning
// wrong text.
func TestContentSpoolCorruptFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "text.spool")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := newContentSpoolWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	off, comp, err := w.Append([]byte("hello needle, this is a longer doc so a flip lands in payload"))
	if err != nil {
		t.Fatal(err)
	}
	orig := len("hello needle, this is a longer doc so a flip lands in payload")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	read := func() ([]byte, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r, err := newContentSpoolReader(f)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return r.Read(off, comp, orig)
	}
	if _, err := read(); err != nil {
		t.Fatalf("pristine read: %v", err)
	}
	// Flip a payload byte: the frame checksum must fail the decode.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-3] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := read(); err == nil {
		t.Fatalf("corrupt frame read %q without error", got)
	}
	// A wrong original length fails too.
	if err := os.WriteFile(path, data[:len(data):len(data)], 0o600); err != nil {
		t.Fatal(err)
	}
	_ = data
	f2, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	r2, err := newContentSpoolReader(f2)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if _, err := r2.Read(off, comp, orig+1); err == nil {
		t.Fatal("wrong original length read without error")
	}
	// A truncated file fails.
	if err := os.Truncate(path, int64(comp)-1); err != nil {
		t.Fatal(err)
	}
	if _, err := read(); err == nil {
		t.Fatal("truncated spool read without error")
	}
}

// Splitting a spool into per-group files preserves every frame byte-exact:
// each group's file holds its docs' frames in group order with correct
// reframed offsets, and reads back the original text.
func TestContentSpoolSplitGroups(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "text.spool")
	f, err := os.Create(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, err := newContentSpoolWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	texts := [][]byte{
		[]byte("alpha needle one"),
		bytes.Repeat([]byte("beta "), 3000),
		[]byte("gamma needle two"),
		bytes.Repeat([]byte("delta "), 3000),
		[]byte("epsilon"),
	}
	var offsets []int64
	var clens []int
	for _, tx := range texts {
		off, comp, err := w.Append(tx)
		if err != nil {
			t.Fatal(err)
		}
		offsets = append(offsets, off)
		clens = append(clens, comp)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	groups := [][]int{{4, 0}, {3, 1, 2}} // arbitrary (priority-like) order
	split, err := contentSpoolSplitGroups(f, offsets, clens, groups, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range split {
		defer g.File.Close()
	}
	if len(split) != 2 {
		t.Fatalf("split into %d groups; want 2", len(split))
	}
	readGroup := func(gi int) [][]byte {
		gf, err := os.Open(split[gi].Path)
		if err != nil {
			t.Fatal(err)
		}
		defer gf.Close()
		r, err := newContentSpoolReader(gf)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		var out [][]byte
		for k, di := range groups[gi] {
			got, err := r.Read(split[gi].Offsets[k], split[gi].Lens[k], len(texts[di]))
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, got)
		}
		return out
	}
	for gi, want := range [][][]byte{{texts[4], texts[0]}, {texts[3], texts[1], texts[2]}} {
		got := readGroup(gi)
		if len(got) != len(want) {
			t.Fatalf("group %d has %d docs; want %d", gi, len(got), len(want))
		}
		for k := range want {
			if !bytes.Equal(got[k], want[k]) {
				t.Fatalf("group %d doc %d mismatch", gi, k)
			}
		}
	}
	// Reframed offsets are dense from zero within each file.
	for gi := range groups {
		var end int64
		for k := range groups[gi] {
			if split[gi].Offsets[k] != end {
				t.Fatalf("group %d frame %d offset %d; want %d", gi, k, split[gi].Offsets[k], end)
			}
			end += int64(split[gi].Lens[k])
		}
		if st, err := os.Stat(split[gi].Path); err != nil || st.Size() != end {
			t.Fatalf("group %d file size %v; want %d", gi, st, end)
		}
	}
}

// A split failure removes its partial group files instead of leaving them on
// the volume.
func TestContentSpoolSplitGroupsCleansUp(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "text.spool"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, err := newContentSpoolWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	off, comp, err := w.Append([]byte("only doc"))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Second group names a frame past EOF: the split must fail.
	_, err = contentSpoolSplitGroups(f, []int64{off, 1 << 40}, []int{comp, 10}, [][]int{{0}, {1}}, dir)
	if err == nil {
		t.Fatal("split past EOF succeeded; want an error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) >= 6 && e.Name()[:6] == "spool-" {
			t.Fatalf("partial group file %s left behind", e.Name())
		}
	}
}

// Assembly over a compressed spool produces byte-identical sections and doc
// records to assembly over the same docs from memory.
func TestContentSpoolAssembleIdentical(t *testing.T) {
	docs := []contentBuildDoc{
		{path: "a/beta.go", frn: 30, text: []byte("package beta\nfunc Needle() {}\n")},
		{path: "a/alpha.go", frn: 10, text: bytes.Repeat([]byte("package alpha // needle\n"), 500)},
		{path: "z/gamma.md", frn: 20, text: []byte("# Gamma\nÜnïcödé needle needle\n")},
		{path: "m/single", frn: 10, text: []byte("second doc sharing an FRN sorts by path")},
	}
	memSource := func() contentBuildSource {
		i := 0
		sorted := append([]contentBuildDoc(nil), docs...)
		// FRN, path order: the assembler contract.
		for a := 1; a < len(sorted); a++ {
			for b := a; b > 0; b-- {
				ai, bi := sorted[b-1], sorted[b]
				if ai.frn < bi.frn || (ai.frn == bi.frn && ai.path <= bi.path) {
					break
				}
				sorted[b-1], sorted[b] = sorted[b], sorted[b-1]
			}
		}
		return func() (contentBuildDoc, bool, error) {
			if i >= len(sorted) {
				return contentBuildDoc{}, false, nil
			}
			d := sorted[i]
			i++
			return d, true, nil
		}
	}
	dir := t.TempDir()
	build := func(source contentBuildSource) *contentIndex {
		tmp, err := os.MkdirTemp(dir, "asm-*")
		if err != nil {
			t.Fatal(err)
		}
		idx, err := assembleContentIndexStream(source, tmp, contentFilterID{})
		if err != nil {
			t.Fatal(err)
		}
		return idx
	}
	memIdx := build(memSource())

	spoolPath := filepath.Join(dir, "text.spool")
	f, err := os.Create(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := newContentSpoolWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	type ref struct {
		doc      contentBuildDoc
		off      int64
		comp, ln int
	}
	var refs []ref
	// Spool in a different (priority-like) order than assembly reads.
	for _, d := range docs {
		off, comp, err := w.Append(d.text)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref{d, off, comp, len(d.text)})
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f, err = os.Open(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := newContentSpoolReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// FRN, path order, as contentWalkSpool would serve.
	for a := 1; a < len(refs); a++ {
		for b := a; b > 0; b-- {
			ai, bi := refs[b-1].doc, refs[b].doc
			if ai.frn < bi.frn || (ai.frn == bi.frn && ai.path <= bi.path) {
				break
			}
			refs[b-1], refs[b] = refs[b], refs[b-1]
		}
	}
	i := 0
	spoolIdx := build(func() (contentBuildDoc, bool, error) {
		if i >= len(refs) {
			return contentBuildDoc{}, false, nil
		}
		rf := refs[i]
		i++
		text, err := r.Read(rf.off, rf.comp, rf.ln)
		if err != nil {
			return contentBuildDoc{}, false, err
		}
		return contentBuildDoc{path: rf.doc.path, frn: rf.doc.frn, text: text}, true, nil
	})

	if !reflect.DeepEqual(memIdx.Docs, spoolIdx.Docs) {
		t.Fatalf("doc tables differ:\n%+v\n%+v", memIdx.Docs, spoolIdx.Docs)
	}
	if len(memIdx.Sections) != len(spoolIdx.Sections) {
		t.Fatalf("section counts differ: %d vs %d", len(memIdx.Sections), len(spoolIdx.Sections))
	}
	for tag, sec := range memIdx.Sections {
		other, ok := spoolIdx.Sections[tag]
		if !ok || !bytes.Equal(sec, other) {
			t.Fatalf("section %d differs (present=%v equal=%v)", tag, ok, ok && bytes.Equal(sec, other))
		}
	}
}
