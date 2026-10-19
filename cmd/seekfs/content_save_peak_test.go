package main

// Save-time transient measurement and regression test for the streaming
// shard save. The save must not keep every section file while the encoded
// output grows beside it: the peak on disk during a save must stay within
// what selection accounts for that shard.

import (
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// contentSavePeakDocs builds a deterministic corpus big enough that the
// grams/terms/filter sections are meaty relative to the doc text. Random-ish
// but word-shaped tokens keep the section sizes plausible (all-letter
// trigrams, repeated syllables) instead of the dense-random postings blowup.
func contentSavePeakDocs(t *testing.T, n, wordsPerDoc int) []contentBuildDoc {
	t.Helper()
	rng := rand.New(rand.NewSource(0x5eed))
	vocab := make([]string, 20000)
	for i := range vocab {
		var b strings.Builder
		for k := 0; k < 6+rng.Intn(6); k++ {
			b.WriteByte(byte('a' + rng.Intn(26)))
		}
		vocab[i] = b.String()
	}
	dir := t.TempDir()
	docs := make([]contentBuildDoc, 0, n)
	for i := 0; i < n; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, "doc%05d ", i)
		for k := 0; k < wordsPerDoc; k++ {
			b.WriteString(vocab[rng.Intn(len(vocab))])
			if k%12 == 11 {
				b.WriteByte('\n')
			} else {
				b.WriteByte(' ')
			}
		}
		text := b.String()
		rel := fmt.Sprintf("d%03d/f%05d.txt", i%29, i)
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		docs = append(docs, contentBuildDoc{
			path:    full,
			frn:     uint64(1000 + i*3),
			text:    []byte(text),
			modUnix: 1_700_000_000 + int64(i),
			class:   1,
			version: 2,
		})
	}
	return docs
}

func contentDirBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

// contentMeasureSavePeak assembles one shard, then samples the bytes on disk
// in the scratch and output directories through the save. It returns the
// extracted bytes, the pre-save scratch bytes, the final encoded size, and
// the largest total observed.
func contentMeasureSavePeak(t *testing.T, docs []contentBuildDoc) (extracted, before, encoded, peak int64) {
	t.Helper()
	scratch := t.TempDir()
	out := t.TempDir()
	built, err := assembleContentIndexStreamed(contentStreamTestSource(docs), scratch, contentFilterID{})
	if err != nil {
		t.Fatal(err)
	}
	hdr := contentShardHeader(&contentIndex{Origin: contentOriginUSN}, built)
	hdr.Sections = map[uint32][]byte{contentSectionPaths: encodeContentPaths(built.paths)}
	extracted = int64(built.textSize)
	for _, f := range []struct {
		name string
		path string
	}{{"terms", built.termPath}, {"grams", built.gramPath}, {"filter", built.filterPath}, {"prune", built.prunePath}} {
		if f.path != "" {
			if fi, err := os.Stat(f.path); err == nil {
				t.Logf("section %s=%d", f.name, fi.Size())
			}
		}
	}
	before = contentDirBytes(t, scratch)
	peak = before

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			default:
			}
			if n := contentDirBytes(t, scratch) + contentDirBytes(t, out); n > peak {
				peak = n
			}
			runtime.Gosched()
		}
	}()
	if _, err := contentSaveShard(out, hdr, contentBuildFiles(built), []string{""}); err != nil {
		close(done)
		<-stopped
		t.Fatal(err)
	}
	close(done)
	<-stopped
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) != 1 {
		t.Fatalf("output dir entries = %d, err = %v", len(entries), err)
	}
	if fi, ferr := entries[0].Info(); ferr == nil {
		encoded = fi.Size()
	}
	if after := contentDirBytes(t, scratch) + contentDirBytes(t, out); after > peak {
		peak = after
	}
	return extracted, before, encoded, peak
}

// TestContentSavePeakMeasure records the save-time transient for one shard:
// before, peak during, and after, as multiples of extracted text and encoded
// output. Phase 1 of the save-peak work; see NOTES.md.
func TestContentSavePeakMeasure(t *testing.T) {
	docs := contentSavePeakDocs(t, 240, 1800)
	extracted, before, encoded, peak := contentMeasureSavePeak(t, docs)
	t.Logf("extracted=%d scratch_before=%d encoded=%d peak_during=%d", extracted, before, encoded, peak)
	t.Logf("peak/extracted=%.3f scratch/extracted=%.3f encoded/extracted=%.3f", float64(peak)/float64(extracted), float64(before)/float64(extracted), float64(encoded)/float64(extracted))
	t.Logf("peak/encoded=%.3f scratch/encoded=%.3f", float64(peak)/float64(encoded), float64(before)/float64(encoded))
	t.Logf("analytic upper bound scratch+encoded=%d (%.3fx extracted, %.3fx encoded)", before+encoded, float64(before+encoded)/float64(extracted), float64(before+encoded)/float64(encoded))
}

// TestContentSavePeakWithinAccountedBound is the regression guard: one
// oversize shard (extracted well past the 512 KiB planning target, as an
// indivisible folder unit can be) must save without its section files and
// encoded output coexisting for the whole copy. The accounted peak is what
// selection budgets: the peak multiplier times the shard's extracted bytes
// plus the fixed scratch slack for in-flight shards.
func TestContentSavePeakWithinAccountedBound(t *testing.T) {
	restoreTarget, restoreConc := contentShardTargetBytes, contentShardBuildConcurrency
	contentShardTargetBytes, contentShardBuildConcurrency = 512<<10, 1
	t.Cleanup(func() { contentShardTargetBytes, contentShardBuildConcurrency = restoreTarget, restoreConc })

	docs := contentSavePeakDocs(t, 240, 1800)
	extracted, before, encoded, peak := contentMeasureSavePeak(t, docs)
	bound := float64(extracted)*contentScopePeakMultiplier + float64(contentScopeScratchSlack())
	if float64(peak) > bound {
		t.Fatalf("save peak %d bytes (%.3fx extracted, %.3fx encoded; scratch %d + encoded %d) exceeds accounted bound %.0f (%.3fx extracted + %d slack)",
			peak, float64(peak)/float64(extracted), float64(peak)/float64(encoded), before, encoded, bound, contentScopePeakMultiplier, contentScopeScratchSlack())
	}
}

// contentSaveHashDocs is a stable fixture: fixed path strings, fixed text,
// no on-disk files (assembly never re-reads the source). Its saved shard hash
// is pinned so the streaming save can change how it consumes section files
// without changing one output byte.
func contentSaveHashDocs(n, wordsPerDoc int) []contentBuildDoc {
	rng := rand.New(rand.NewSource(0xc0ffee))
	vocab := make([]string, 4000)
	for i := range vocab {
		var b strings.Builder
		for k := 0; k < 6+rng.Intn(6); k++ {
			b.WriteByte(byte('a' + rng.Intn(26)))
		}
		vocab[i] = b.String()
	}
	docs := make([]contentBuildDoc, 0, n)
	for i := 0; i < n; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, "doc%05d ", i)
		for k := 0; k < wordsPerDoc; k++ {
			b.WriteString(vocab[rng.Intn(len(vocab))])
			b.WriteByte(' ')
		}
		docs = append(docs, contentBuildDoc{
			path:    fmt.Sprintf("C:/repo/d%02d/f%05d.txt", i%13, i),
			frn:     uint64(1000 + i*3),
			text:    []byte(b.String()),
			modUnix: 1_700_000_000 + int64(i),
			class:   1,
			version: 2,
		})
	}
	return docs
}

// TestContentSaveShardBytesUnchanged pins the saved shard hash. The fixture
// exercises file-backed terms, grams and filter sections, so consuming those
// files during the copy must not alter the bytes.
func TestContentSaveShardBytesUnchanged(t *testing.T) {
	docs := contentSaveHashDocs(60, 400)
	tmp := t.TempDir()
	built, err := assembleContentIndexStreamed(contentStreamTestSource(docs), tmp, contentFilterID{})
	if err != nil {
		t.Fatal(err)
	}
	hdr := contentShardHeader(&contentIndex{Origin: contentOriginUSN}, built)
	hdr.Sections = map[uint32][]byte{contentSectionPaths: encodeContentPaths(built.paths)}
	dir := t.TempDir()
	info, err := contentSaveShard(dir, hdr, contentBuildFiles(built), []string{""})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, info.File))
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(data))
	const want = "ce9f1b5517c751bd24100566046236644463cdaf038baf42f9263ac1cbdf8d46"
	if got != want {
		t.Fatalf("saved shard sha256 = %s; want %s", got, want)
	}
}
