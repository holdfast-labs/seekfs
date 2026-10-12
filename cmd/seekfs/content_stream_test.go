package main

// Tests for the streaming build/save path: the spooled assembly and the
// streamed `.gsx` write must be byte-identical to the in-memory encode for the
// same input, honor the size cap without writing, and reload through the mmap
// loader.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// contentStreamTestDocs builds a deterministic synthetic corpus: mixed
// scripts, case, multibyte text and long words so terms, grams, text, paths,
// policy and scope sections are all non-trivial.
func contentStreamTestDocs(n int) []contentBuildDoc {
	docs := make([]contentBuildDoc, 0, n)
	for i := 0; i < n; i++ {
		body := fmt.Sprintf("Needle Doc %d caf\u00e9 \u4e2d\u6587 %s tail-%d", i, "lorem ipsum dolor sit amet ", i%7)
		if i%11 == 0 {
			body += " " + string(make([]byte, 0))
		}
		if i%13 == 0 {
			body += " UNIQUEMARKER" + fmt.Sprintf("%d", i)
		}
		path := fmt.Sprintf("sub/dir%02d/file%04d.txt", i%17, i)
		docs = append(docs, contentBuildDoc{
			path:    path,
			frn:     uint64(1000 + i*3),
			text:    []byte(body),
			modUnix: 1_700_000_000 + int64(i),
			class:   1,
			version: 2,
		})
	}
	return docs
}

func contentStreamTestSource(docs []contentBuildDoc) contentBuildSource {
	i := 0
	return func() (contentBuildDoc, bool, error) {
		if i >= len(docs) {
			return contentBuildDoc{}, false, nil
		}
		d := docs[i]
		i++
		return d, true, nil
	}
}

// TestContentExternalBuilderFinishFileParity proves the file-spilling merge is
// byte-identical to the in-memory Finish, including the empty-builder case.
func TestContentExternalBuilderFinishFileParity(t *testing.T) {
	tmp := t.TempDir()
	mk := func() *contentExternalSectionBuilder {
		b, err := newContentExternalSectionBuilder(tmp, 1<<16)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	// Empty builder: Finish returns nil and FinishFile creates nothing.
	empty := mk()
	if out, err := empty.Finish(); err != nil || out != nil {
		t.Fatalf("empty Finish = %v, %v; want nil, nil", len(out), err)
	}
	emptyPath := filepath.Join(tmp, "empty.sec")
	if err := empty.FinishFile(emptyPath); err != nil {
		t.Fatalf("empty FinishFile: %v", err)
	}
	if _, err := os.Stat(emptyPath); !os.IsNotExist(err) {
		t.Fatalf("empty FinishFile created %s", emptyPath)
	}
	empty.Close()

	feed := func(b *contentExternalSectionBuilder) {
		for doc := uint32(0); doc < 300; doc++ {
			for k := 0; k < 40; k++ {
				b.Add(fmt.Sprintf("term-%d-%d", doc%37, k), doc, uint32(k%5+1))
			}
		}
	}
	mem := mk()
	feed(mem)
	want, err := mem.Finish()
	mem.Close()
	if err != nil {
		t.Fatal(err)
	}
	disk := mk()
	feed(disk)
	diskPath := filepath.Join(tmp, "terms.sec")
	if err := disk.FinishFile(diskPath); err != nil {
		t.Fatal(err)
	}
	disk.Close()
	got, err := os.ReadFile(diskPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("FinishFile differs from Finish: %d vs %d bytes", len(got), len(want))
	}
}

// TestContentStreamedSaveMatchesInMemoryEncode proves the streaming save is
// byte-identical to contentIndexEncode for the same input (including policy
// and scope sections), and that the streamed file decodes and searches.
func TestContentStreamedSaveMatchesInMemoryEncode(t *testing.T) {
	docs := contentStreamTestDocs(500)
	tmp := t.TempDir()

	mem, err := assembleContentIndex(docs, tmp)
	if err != nil {
		t.Fatal(err)
	}
	mem.Policy = contentBuildPolicy{MaxRaw: 1 << 20, MaxText: 1 << 19, Skipped: 3, Truncated: 2}
	mem.ScopeHash = defaultContentScope().resolve("C:", []string{`C:\repo`}).fingerprint()
	mem.BuiltAt = time.Unix(0, 5)
	want := contentIndexEncode(mem)

	built, err := assembleContentIndexStreamed(contentStreamTestSource(docs), tmp)
	if err != nil {
		t.Fatal(err)
	}
	hdr := contentShardHeader(mem, built)
	hdr.BuiltAt = time.Unix(0, 5)
	hdr.Sections = map[uint32][]byte{contentSectionPaths: encodeContentPaths(built.paths)}
	path := filepath.Join(tmp, "streamed.gsx")
	if err := contentSaveAssembled(path, hdr, contentBuildFiles(built)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("streamed save differs from in-memory encode: %d vs %d bytes", len(got), len(want))
	}

	loaded, err := contentLoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Release()
	if len(loaded.Docs) != len(docs) {
		t.Fatalf("reloaded %d docs; want %d", len(loaded.Docs), len(docs))
	}
	r, err := openContentReader(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if hits := r.search("needle", 0); len(hits) != len(docs) {
		t.Fatalf("search hits = %d; want %d", len(hits), len(docs))
	}
	// UNIQUEMARKER39 also matches doc 390 (substring), so expect both.
	if hits := r.search("UNIQUEMARKER39", 0); len(hits) != 2 {
		t.Fatalf("selective search hits = %d; want 2", len(hits))
	}
}

// TestContentStreamedSaveRespectsSizeCap proves an over-cap streamed save is
// refused with contentGSXSizeError and writes nothing.
func TestContentStreamedSaveRespectsSizeCap(t *testing.T) {
	docs := contentStreamTestDocs(50)
	tmp := t.TempDir()
	built, err := assembleContentIndexStreamed(contentStreamTestSource(docs), tmp)
	if err != nil {
		t.Fatal(err)
	}
	hdr := &contentIndex{
		Docs:     built.docs,
		Sections: map[uint32][]byte{contentSectionPaths: encodeContentPaths(built.paths)},
	}
	path := filepath.Join(tmp, "capped.gsx")
	restore := contentGSXMaxBytes
	contentGSXMaxBytes = 1024
	defer func() { contentGSXMaxBytes = restore }()
	err = contentSaveAssembled(path, hdr, contentBuildFiles(built))
	var sizeErr *contentGSXSizeError
	if !errors.As(err, &sizeErr) {
		t.Fatalf("over-cap streamed save = %v; want contentGSXSizeError", err)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Fatal("over-cap streamed save wrote a file")
	}
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if e.Name() == "capped.gsx" {
			t.Fatal("over-cap streamed save left output behind")
		}
	}
}

// TestContentStreamedShardSaveRoundTrip proves a shard saved through the
// streaming path loads as a composite member: save two shards plus a manifest
// and query across them.
func TestContentStreamedShardSaveRoundTrip(t *testing.T) {
	docs := contentStreamTestDocs(200)
	tmp := t.TempDir()
	gsx := filepath.Join(tmp, "sharded.gsx")
	dir := contentShardDir(gsx)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var infos []contentShardInfo
	for shard, chunk := range [][]contentBuildDoc{docs[:100], docs[100:]} {
		built, err := assembleContentIndexStreamed(contentStreamTestSource(chunk), tmp)
		if err != nil {
			t.Fatal(err)
		}
		hdr := contentShardHeader(&contentIndex{Origin: contentOriginUSN}, built)
		hdr.Sections = map[uint32][]byte{contentSectionPaths: encodeContentPaths(built.paths)}
		info, err := contentSaveShard(dir, hdr, contentBuildFiles(built), []string{fmt.Sprintf("p%d", shard)})
		if err != nil {
			t.Fatal(err)
		}
		infos = append(infos, info)
	}
	mhdr := &contentIndex{Origin: contentOriginUSN, JournalID: 7, CheckpointUSN: 9}
	if err := contentSaveManifest(gsx, mhdr, infos); err != nil {
		t.Fatal(err)
	}
	loaded, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Release()
	if len(loaded.Docs) != len(docs) {
		t.Fatalf("composite docs = %d; want %d", len(loaded.Docs), len(docs))
	}
	r, err := openContentReader(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if hits := r.search("needle", 0); len(hits) != len(docs) {
		t.Fatalf("composite search hits = %d; want %d", len(hits), len(docs))
	}
}
