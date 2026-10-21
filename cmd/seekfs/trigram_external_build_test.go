package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNameGramExternalMatchesInMemory is the equivalence contract for the
// bounded builder: for the same index and posting cap, the external merge must
// produce the same PNGR and PNGC section bytes as the in-memory builder.
func TestNameGramExternalMatchesInMemory(t *testing.T) {
	fixtures := map[string]*Index{
		"dotted": dottedPathBenchmarkIndex(20_000),
		"scan":   buildLargeScanFixture(2000, 50),
	}
	caps := []int{1, 3, 17, 1000, 1 << 30}
	for name, idx := range fixtures {
		for _, maxPosting := range caps {
			t.Run(name, func(t *testing.T) {
				inMem := buildSelectiveCompactNameGramIndex(idx, 3, maxPosting)
				inMemC := optionalSelfNameGramIndex(idx, inMem)
				extR, extC, err := buildNameGramIndexExternal(context.Background(), idx, 3, maxPosting, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				if got, want := encodeGramPostingSection(extR, nil), encodeGramPostingSection(inMem, nil); !bytes.Equal(got, want) {
					t.Fatalf("cap=%d PNGR differs: external=%d bytes in-memory=%d bytes", maxPosting, len(got), len(want))
				}
				var gotC, wantC []byte
				if extC != nil {
					gotC = encodeGramPostingSection(extC, nil)
				}
				if inMemC != nil {
					wantC = encodeGramPostingSection(inMemC, nil)
				}
				if !bytes.Equal(gotC, wantC) {
					t.Fatalf("cap=%d PNGC differs: external=%d bytes in-memory=%d bytes", maxPosting, len(gotC), len(wantC))
				}
			})
		}
	}
}

// TestNameGramExternalSpillsMatchInMemory forces many small spills and checks
// the merge still reproduces the in-memory output.
func TestNameGramExternalSpillsMatchInMemory(t *testing.T) {
	// 8KB spills flush many times even at 20k records; the 50k scale runs
	// in the soak tier (CI) with the same assertions.
	spillRecords := 20_000
	if testSoakEnabled() {
		spillRecords = 50_000
	}
	t.Logf("spill fixture: %d records (soak=%v)", spillRecords, testSoakEnabled())
	idx := dottedPathBenchmarkIndex(spillRecords)
	prev := gramExternalSpillBytes
	gramExternalSpillBytes = 8 << 10
	t.Cleanup(func() { gramExternalSpillBytes = prev })

	for _, maxPosting := range []int{2, 50, 1 << 30} {
		inMem := buildSelectiveCompactNameGramIndex(idx, 3, maxPosting)
		inMemC := optionalSelfNameGramIndex(idx, inMem)
		extR, extC, err := buildNameGramIndexExternal(context.Background(), idx, 3, maxPosting, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if got, want := encodeGramPostingSection(extR, nil), encodeGramPostingSection(inMem, nil); !bytes.Equal(got, want) {
			t.Fatalf("cap=%d PNGR differs under forced spills", maxPosting)
		}
		var gotC, wantC []byte
		if extC != nil {
			gotC = encodeGramPostingSection(extC, nil)
		}
		if inMemC != nil {
			wantC = encodeGramPostingSection(inMemC, nil)
		}
		if !bytes.Equal(gotC, wantC) {
			t.Fatalf("cap=%d PNGC differs under forced spills", maxPosting)
		}
	}
}

func TestShouldUseExternalNameGram(t *testing.T) {
	os.Unsetenv("SEEKFS_MEMORY_MODE")
	os.Unsetenv("SEEKFS_NAME_GRAM_EXTERNAL")
	os.Unsetenv("SEEKFS_NAME_GRAM_EXTERNAL_MIN_RECORDS")
	t.Cleanup(func() { os.Unsetenv("SEEKFS_NAME_GRAM_EXTERNAL"); os.Unsetenv("SEEKFS_NAME_GRAM_EXTERNAL_MIN_RECORDS") })
	if shouldUseExternalNameGram(10) {
		t.Fatal("tiny volumes should stay on the in-memory builder")
	}
	if !shouldUseExternalNameGram(1_000_000) {
		t.Fatal("real volumes should use the external builder by default")
	}
	os.Setenv("SEEKFS_NAME_GRAM_EXTERNAL", "0")
	if shouldUseExternalNameGram(10_000_000) {
		t.Fatal("SEEKFS_NAME_GRAM_EXTERNAL=0 must force in-memory")
	}
	os.Setenv("SEEKFS_NAME_GRAM_EXTERNAL", "1")
	if !shouldUseExternalNameGram(10) {
		t.Fatal("SEEKFS_NAME_GRAM_EXTERNAL=1 must force external")
	}
	os.Unsetenv("SEEKFS_NAME_GRAM_EXTERNAL")
	os.Setenv("SEEKFS_NAME_GRAM_EXTERNAL_MIN_RECORDS", "1000")
	if shouldUseExternalNameGram(500) {
		t.Fatal("below the min-records threshold should use in-memory")
	}
	if !shouldUseExternalNameGram(1500) {
		t.Fatal("above the min-records threshold should use external")
	}
}

// TestNameGramExternalRepeatedGramsMatchInMemory guards against double-counting
// a gram that repeats inside one name (e.g. "aaaa" contains "aaa" twice).  The
// in-memory builder contributes each gram once per record, so the external spill
// must dedup repeats too; both a single spill buffer (one run) and many small
// spills (many runs) are checked.
func TestNameGramExternalRepeatedGramsMatchInMemory(t *testing.T) {
	idx := &Index{Volume: "C:", Source: "usn"}
	for _, name := range []string{"aaaa", "abcabc", "xxxx.txt", "aaaaaa", "bababa", "test", "aAaA", "zzz", "rep" + "rep" + "rep",
		// Long names with many repeated grams so the small-spill config actually
		// flushes mid-buffer with repeated grams in one run.
		strings.Repeat("ab", 700), strings.Repeat("abc", 500)} {
		idx.Records = append(idx.Records, CompactRecord{Name: name, Parent: -1})
	}
	prev := gramExternalSpillBytes
	t.Cleanup(func() { gramExternalSpillBytes = prev })

	for _, cfg := range []struct {
		name  string
		bytes int
	}{
		{"single-run", 1 << 30},
		{"many-runs", 8 << 10},
	} {
		gramExternalSpillBytes = cfg.bytes
		for _, maxPosting := range []int{1, 2, 3, 1000, 1 << 30} {
			inMem := buildSelectiveCompactNameGramIndex(idx, 3, maxPosting)
			extR, _, err := buildNameGramIndexExternal(context.Background(), idx, 3, maxPosting, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			got, want := encodeGramPostingSection(extR, nil), encodeGramPostingSection(inMem, nil)
			if !bytes.Equal(got, want) {
				t.Fatalf("%s cap=%d PNGR differs: external=%d in-memory=%d", cfg.name, maxPosting, len(got), len(want))
			}
		}
	}
}

// TestNameGramBuildersFoldUnicodeLikeQueries guards the case-folding contract:
// the query side grams strings.ToLower(term), so every name-gram builder must
// gram the lowercased name, not the original-case name with ASCII-only folding,
// or a non-ASCII name produces a gram the query can never look up.
func TestNameGramBuildersFoldUnicodeLikeQueries(t *testing.T) {
	idx := &Index{Volume: "C:", Source: "usn"}
	names := []string{"École.txt", "Key.txt", "Ünter.csv", "café", "ABC.dat", "Straße", "ΑΒΓ.log"}
	for _, name := range names {
		idx.Records = append(idx.Records, CompactRecord{Name: name, Parent: -1})
	}
	selective := buildSelectiveNameTrigramIndex(idx, 1<<30)
	ext, _, err := buildNameGramIndexExternal(context.Background(), idx, 3, 1<<30, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	full := buildNameTrigramIndex(idx)
	for _, name := range names {
		for _, gram := range uniqueFixedGramKeysFoldASCII(strings.ToLower(name), 3) {
			if _, ok := selective.counts[gram]; !ok {
				t.Fatalf("selective missing query gram %d for %q", gram, name)
			}
			if _, ok := ext.counts[gram]; !ok {
				t.Fatalf("external missing query gram %d for %q", gram, name)
			}
			if _, ok := full.counts[gram]; !ok {
				t.Fatalf("full missing query gram %d for %q", gram, name)
			}
		}
	}
}

// TestMergeRunKeysRejectsTruncatedRun ensures a short read in a spill run fails
// the build instead of silently producing an incomplete index.
func TestMergeRunKeysRejectsTruncatedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.tmp")
	tbl := newGramSpillTable(8)
	for id := uint32(0); id < 50; id++ {
		tbl.add(0x616263, id)
	}
	if err := writeGramRunGrouped(path, []uint32{0x616263}, tbl); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, fi.Size()-3); err != nil {
		t.Fatal(err)
	}
	err = mergeRunKeys(context.Background(), []string{path}, func(uint32, []uint32) error { return nil })
	if err == nil {
		t.Fatal("mergeRunKeys accepted a truncated run")
	}
}
