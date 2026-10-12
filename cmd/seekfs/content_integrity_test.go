package main

// Tests for `.gsx` per-section integrity: every section kind is checksummed,
// a flipped byte is detected (eagerly for small sections, by the verify pass
// for large ones), a corrupt base is never served, and a rebuild is scheduled
// exactly once per corrupt base. Legacy files without checksums keep loading.

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// contentCorruptSection rewrites path with one byte flipped inside the payload
// of the given section tag, locating it through the section table.
func contentCorruptSection(t *testing.T, path string, tag uint32) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	off, length := contentSectionRange(t, raw, tag)
	if length == 0 {
		t.Fatalf("section %s is empty; nothing to corrupt", contentSectionTagName(tag))
	}
	raw[off+length/2] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// contentSectionRange returns the file offset and length of a section payload.
func contentSectionRange(t *testing.T, raw []byte, tag uint32) (uint64, uint64) {
	t.Helper()
	if len(raw) < contentHeaderSize() {
		t.Fatal("file too short for a header")
	}
	tableOffset := binary.LittleEndian.Uint64(raw[32:])
	sectionCount := int(binary.LittleEndian.Uint32(raw[40:]))
	for i := 0; i < sectionCount; i++ {
		p := int(tableOffset) + 4 + i*contentSectionEntrySize
		if binary.LittleEndian.Uint32(raw[p:]) == tag {
			return binary.LittleEndian.Uint64(raw[p+4:]), binary.LittleEndian.Uint64(raw[p+12:])
		}
	}
	t.Fatalf("section %s not found", contentSectionTagName(tag))
	return 0, 0
}

// contentIntegrityTestFile builds a small USN-keyed index with every section
// kind present (doc table, terms, grams, text, paths, policy, scope) and saves
// it, returning the path and the loaded index.
func contentIntegrityTestFile(t *testing.T) (string, *contentIndex) {
	t.Helper()
	docs := contentStreamTestDocs(30)
	tmp := t.TempDir()
	built, err := assembleContentIndexStreamed(contentStreamTestSource(docs), tmp)
	if err != nil {
		t.Fatal(err)
	}
	hdr := &contentIndex{
		Version:       contentIndexVersion,
		Origin:        contentOriginUSN,
		JournalID:     11,
		CheckpointUSN: 13,
		Docs:          built.docs,
		Sections:      map[uint32][]byte{contentSectionPaths: encodeContentPaths(built.paths)},
		Policy:        contentBuildPolicy{MaxRaw: 1 << 20, MaxText: 1 << 19, Skipped: 1},
		ScopeHash:     defaultContentScope().resolve("C:", []string{`C:\repo`}).fingerprint(),
	}
	path := filepath.Join(tmp, "integrity.gsx")
	if err := contentSaveAssembled(path, hdr, contentBuildFiles(built)); err != nil {
		t.Fatal(err)
	}
	loaded, err := contentLoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loaded.Release)
	if err := verifyContentSectionCRCs(loaded); err != nil {
		t.Fatalf("fresh file fails verification: %v", err)
	}
	return path, loaded
}

// TestContentIntegrityDetectsSmallSectionCorruption flips one byte in each
// eagerly-verified section kind and asserts the load fails instead of serving.
func TestContentIntegrityDetectsSmallSectionCorruption(t *testing.T) {
	for _, tag := range []uint32{
		contentSectionDocTable,
		contentSectionPaths,
		contentSectionPolicy,
		contentSectionScope,
	} {
		t.Run(contentSectionTagName(tag), func(t *testing.T) {
			path, loaded := contentIntegrityTestFile(t)
			loaded.Release()
			contentCorruptSection(t, path, tag)
			_, err := contentLoadFile(path)
			var crcErr *contentSectionCRCError
			if !errors.As(err, &crcErr) {
				t.Fatalf("corrupt %s loaded with err=%v; want a checksum error", contentSectionTagName(tag), err)
			}
			if crcErr.tag != tag {
				t.Fatalf("checksum error names %s; want %s", contentSectionTagName(crcErr.tag), contentSectionTagName(tag))
			}
		})
	}
}

// TestContentIntegrityDetectsLargeSectionCorruption flips one byte in each
// lazily-verified section kind and asserts the load succeeds (startup must not
// page them in) but the verify pass reports the mismatch.
func TestContentIntegrityDetectsLargeSectionCorruption(t *testing.T) {
	for _, tag := range []uint32{
		contentSectionText,
		contentSectionTerms,
		contentSectionGrams,
	} {
		t.Run(contentSectionTagName(tag), func(t *testing.T) {
			path, loaded := contentIntegrityTestFile(t)
			loaded.Release()
			contentCorruptSection(t, path, tag)
			reloaded, err := contentLoadFile(path)
			if err != nil {
				t.Fatalf("corrupt %s must still load (lazy verify); got %v", contentSectionTagName(tag), err)
			}
			defer reloaded.Release()
			err = verifyContentSectionCRCs(reloaded)
			var crcErr *contentSectionCRCError
			if !errors.As(err, &crcErr) {
				t.Fatalf("corrupt %s verified clean; want a checksum error", contentSectionTagName(tag))
			}
			if crcErr.tag != tag {
				t.Fatalf("checksum error names %s; want %s", contentSectionTagName(crcErr.tag), contentSectionTagName(tag))
			}
		})
	}
}

// TestContentIntegrityDetectsManifestCorruption covers the shard-manifest
// section kind: it is small, so it fails at load.
func TestContentIntegrityDetectsManifestCorruption(t *testing.T) {
	tmp := t.TempDir()
	gsx := filepath.Join(tmp, "m.gsx")
	dir := contentShardDir(gsx)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	docs := contentStreamTestDocs(10)
	built, err := assembleContentIndexStreamed(contentStreamTestSource(docs), tmp)
	if err != nil {
		t.Fatal(err)
	}
	shard := contentShardHeader(&contentIndex{Origin: contentOriginUSN}, built)
	shard.Sections = map[uint32][]byte{contentSectionPaths: encodeContentPaths(built.paths)}
	info, err := contentSaveShard(dir, shard, contentBuildFiles(built), []string{""})
	if err != nil {
		t.Fatal(err)
	}
	if err := contentSaveManifest(gsx, &contentIndex{Origin: contentOriginUSN, JournalID: 1, CheckpointUSN: 1}, []contentShardInfo{info}); err != nil {
		t.Fatal(err)
	}
	fresh, err := contentLoadFile(gsx)
	if err != nil {
		t.Fatalf("fresh manifest fails to load: %v", err)
	}
	fresh.Release()
	contentCorruptSection(t, gsx, contentSectionShards)
	if _, err := contentLoadFile(gsx); err == nil {
		t.Fatal("corrupt manifest loaded clean")
	} else {
		var crcErr *contentSectionCRCError
		if !errors.As(err, &crcErr) {
			t.Fatalf("corrupt manifest err=%v; want a checksum error", err)
		}
	}
}

// stubContentVerifySync runs the post-attach integrity pass inline so tests
// are deterministic; production runs it on a background goroutine.
func stubContentVerifySync(t *testing.T) {
	t.Helper()
	restore := contentVerifyAsync
	contentVerifyAsync = func(fn func()) { fn() }
	t.Cleanup(func() { contentVerifyAsync = restore })
}

// TestContentVerifyPassReleaseMidHash proves the mapping lifetime, not a
// probe, protects the pass: with the pass paused mid-hash, releasing the
// index blocks until the pass drops its pin (no fault), and the pass then
// reports the release cleanly instead of hashing a gone mapping.
func TestContentVerifyPassReleaseMidHash(t *testing.T) {
	path, loaded := contentIntegrityTestFile(t)
	// The fixture registers a cleanup Release; this test also releases
	// mid-pass, which is safe because Release is once-only.

	// Pause the pass on the first checksummed section it hashes.
	entered := make(chan struct{})
	releaseGate := make(chan struct{})
	var once sync.Once
	restoreHook := contentVerifySectionHook
	contentVerifySectionHook = func(tag uint32) {
		once.Do(func() { close(entered) })
		<-releaseGate
	}
	t.Cleanup(func() { contentVerifySectionHook = restoreHook })

	passDone := make(chan error, 1)
	go func() { passDone <- verifyContentSectionCRCs(loaded) }()

	<-entered
	released := make(chan struct{})
	go func() {
		loaded.Release()
		close(released)
	}()

	// The Release must wait for the pinned pass: it cannot complete while the
	// pass is still parked inside the hook.
	select {
	case <-released:
		t.Fatal("Release completed while the verify pass still held its pin")
	case <-time.After(time.Second):
	}
	close(releaseGate)
	select {
	case <-released:
	case <-time.After(10 * time.Second):
		t.Fatal("Release never completed after the pass dropped its pin")
	}

	// The pass stopped early on the release instead of faulting on an
	// unmapped view.
	select {
	case err := <-passDone:
		if err == nil {
			t.Fatal("pass over a released index must not report success")
		}
		t.Logf("pass stopped early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("pass never returned after release")
	}

	// The mapping is really gone: the file is writable again on Windows.
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("released mapping still holds the file: %v", err)
	}
	_ = f.Close()
}

// TestContentIntegrityFailureMarksStaleAndRebuilds attaches a base with one
// corrupt text byte and asserts the background pass drops the base (queries
// are refused, never served from corrupt bytes) and schedules exactly one
// rebuild.
func TestContentIntegrityFailureMarksStaleAndRebuilds(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentVerifySync(t)

	dir := t.TempDir()
	aPath := filepath.Join(dir, "alpha.txt")
	if err := os.WriteFile(aPath, []byte("alpha old needlealpha"), 0o644); err != nil {
		t.Fatal(err)
	}

	const journal = uint64(0xC0FFEE)
	const baseCP = int64(100)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 200})

	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(baseCP), []CompactRecord{
		{FRN: 10, ParentFRN: 1, Parent: -1, Name: "alpha.txt", Size: 10},
	})
	base, err := assembleContentIndex([]contentBuildDoc{
		{path: aPath, frn: 10, text: []byte("alpha old needlealpha"), class: contentClassText, version: contentTextExtractor{}.Version()},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base.Origin = contentOriginUSN
	base.JournalID = journal
	base.CheckpointUSN = uint64(baseCP)
	stampContentTestScope(vol.volume, base)
	if err := contentSaveFile(gsx, base); err != nil {
		t.Fatal(err)
	}
	contentCorruptSection(t, gsx, contentSectionText)

	rebuilds := stubContentBuildSchedule(t)
	s := contentTestService(t)
	s.attachContentForVolume(vol)

	if got := vol.content.stateOf(); got != contentStateStale {
		t.Fatalf("corrupt base state = %q; want stale", got)
	}
	if vol.content.readerView() != nil {
		t.Fatal("corrupt base still has a reader; results must not be served")
	}
	if h := vol.content.healthSnapshot(0); h.IntegrityError == "" {
		t.Fatalf("integrity failure not surfaced in health: %+v", h)
	}
	if *rebuilds != 1 {
		t.Fatalf("scheduled %d rebuilds; want exactly 1", *rebuilds)
	}
	if hits, _ := contentServiceSearch(t, vol, "content:needlealpha", false); len(hits) != 0 {
		t.Fatalf("corrupt base served %d hits; want refusal (0)", len(hits))
	}
}

// TestContentIntegrityCleanBasePassesAndServes attaches a valid base and
// asserts the background pass leaves it ready and serving with no rebuild.
func TestContentIntegrityCleanBasePassesAndServes(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	stubContentCatchUpSync(t)
	stubContentVerifySync(t)

	dir := t.TempDir()
	aPath := filepath.Join(dir, "alpha.txt")
	if err := os.WriteFile(aPath, []byte("alpha old needlealpha"), 0o644); err != nil {
		t.Fatal(err)
	}

	const journal = uint64(0xC0FFEE)
	const baseCP = int64(100)
	stubContentCatchUpJournal(t, usnJournalDataV0{UsnJournalID: journal, FirstUsn: 1, LowestValidUsn: 1, NextUsn: 200})

	vol, gsx := contentBuildTestVolume(t, dir, journal, uint64(baseCP), []CompactRecord{
		{FRN: 10, ParentFRN: 1, Parent: -1, Name: "alpha.txt", Size: 10},
	})
	base, err := assembleContentIndex([]contentBuildDoc{
		{path: aPath, frn: 10, text: []byte("alpha old needlealpha"), class: contentClassText, version: contentTextExtractor{}.Version()},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base.Origin = contentOriginUSN
	base.JournalID = journal
	base.CheckpointUSN = uint64(baseCP)
	stampContentTestScope(vol.volume, base)
	if err := contentSaveFile(gsx, base); err != nil {
		t.Fatal(err)
	}

	rebuilds := stubContentBuildSchedule(t)
	s := contentTestService(t)
	s.attachContentForVolume(vol)

	if got := vol.content.stateOf(); got != contentStateReady {
		t.Fatalf("clean base state = %q; want ready", got)
	}
	if *rebuilds != 0 {
		t.Fatalf("clean base scheduled %d rebuilds; want 0", *rebuilds)
	}
	if hits, _ := contentServiceSearch(t, vol, "content:needlealpha", false); len(hits) != 1 {
		t.Fatalf("clean base served %d hits; want 1", len(hits))
	}
}

// TestContentLegacyFileWithoutChecksumsLoads proves an old file (flags clear)
// loads and verifies clean, and is upgraded with checksums on the next save.
func TestContentLegacyFileWithoutChecksumsLoads(t *testing.T) {
	path, loaded := contentIntegrityTestFile(t)
	raw, err := os.ReadFile(path)
	loaded.Release()
	if err != nil {
		t.Fatal(err)
	}
	// Clear every entry's flags/CRC words: the file is now legacy.
	tableOffset := binary.LittleEndian.Uint64(raw[32:])
	sectionCount := int(binary.LittleEndian.Uint32(raw[40:]))
	for i := 0; i < sectionCount; i++ {
		p := int(tableOffset) + 4 + i*contentSectionEntrySize
		binary.LittleEndian.PutUint32(raw[p+20:], 0)
		binary.LittleEndian.PutUint32(raw[p+24:], 0)
	}
	legacyDir := t.TempDir()
	legacy := filepath.Join(legacyDir, "legacy.gsx")
	if err := os.WriteFile(legacy, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded, err := contentLoadFile(legacy)
	if err != nil {
		t.Fatalf("legacy file must keep loading: %v", err)
	}
	defer reloaded.Release()
	if len(reloaded.SectionCRC) != 0 {
		t.Fatalf("legacy file reports %d checksums; want 0", len(reloaded.SectionCRC))
	}
	if err := verifyContentSectionCRCs(reloaded); err != nil {
		t.Fatalf("legacy file must verify clean (nothing to check): %v", err)
	}
	// A corrupt byte in a legacy file is undetected (no checksum to check) —
	// the upgrade happens on the next save, which stamps checksums.
	reloaded.Release()
	contentCorruptSection(t, legacy, contentSectionText)
	corrupt, err := contentLoadFile(legacy)
	if err != nil {
		t.Fatalf("legacy file must keep loading even when corrupt: %v", err)
	}
	defer corrupt.Release()
	upgraded := filepath.Join(legacyDir, "upgraded.gsx")
	if err := contentSaveFile(upgraded, corrupt); err != nil {
		t.Fatal(err)
	}
	fresh, err := contentLoadFile(upgraded)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Release()
	if len(fresh.SectionCRC) == 0 {
		t.Fatal("re-saved legacy file has no checksums")
	}
}
