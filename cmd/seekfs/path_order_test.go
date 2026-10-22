//go:build windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func buildZipfPathIndex(t *testing.T, n int) *Index {
	t.Helper()
	common := zipfCommonNames(2048)
	table := newMFTTable(uint64(n))
	buf := make([]byte, 1024)
	for i := 0; i < n; i++ {
		fillSynthMFTRecord(buf, zipfRecord(i, common))
		parseMFTRecordInto(buf, uint64(i), 512, table)
	}
	idx := &Index{
		Version: indexVersion, Roots: []string{`X:\`}, BuiltAt: time.Unix(123, 0),
		Source: "usn", Volume: "X:", Compact: true, CompactAttrs: true,
	}
	buildRecordsFromMFT(idx, table)
	return idx
}

func pathOrderEdgeIndex() *Index {
	idx := &Index{
		Version: indexVersion, Roots: []string{`C:\Users\Alice`}, BuiltAt: time.Unix(123, 0),
		Source: "usn", Volume: "C:", Compact: true, CompactAttrs: true,
	}
	add := func(name string, parent int32, parentFRN, frn uint64) int {
		id := len(idx.Records)
		idx.Records = append(idx.Records, CompactRecord{
			FRN: frn, ParentFRN: parentFRN, Parent: parent, Name: name,
		})
		return id
	}
	root := add(".", -1, 5, 5)
	deep := root
	for i := 0; i < 12; i++ {
		deep = add(string([]byte{'d', 'e', 'e', 'p', byte('a' + i)}), int32(deep), uint64(5+deep), uint64(5+deep+1))
	}
	for _, name := range []string{"a", "a.txt", "ab", "a_b", "a-b", "a0", "Alice"} {
		add(name, int32(root), 5, uint64(100+len(idx.Records)))
	}
	alice := 0
	for i, r := range idx.Records {
		if r.Name == "Alice" {
			alice = i
		}
	}
	add("inside", int32(alice), uint64(5+alice), 500)
	add("Readme.md", int32(root), 5, 601)
	add("README.md", int32(root), 5, 602)
	add("readme.md", int32(root), 5, 603)
	add("Ärger.txt", int32(root), 5, 604)
	add("ärger.txt", int32(root), 5, 605)
	add("ÄRGER.TXT", int32(root), 5, 606)
	add("orphan", -1, 999, 700)
	return idx
}

func comparePathOrderRank(t *testing.T, label string, idx *Index) {
	t.Helper()
	legacyOrder, legacyRank := buildCompactPathOrderRankLegacy(idx)
	order, rank := buildCompactPathOrderRank(idx)
	if len(order) != len(legacyOrder) || len(rank) != len(legacyRank) {
		t.Fatalf("%s: sizes order=%d/%d rank=%d/%d", label, len(order), len(legacyOrder), len(rank), len(legacyRank))
	}
	for i := range legacyOrder {
		if order[i] != legacyOrder[i] {
			na, la := idx.compactRecord(int(order[i])), idx.compactRecord(int(legacyOrder[i]))
			t.Fatalf("%s: order[%d] new=%d(%s p=%d) legacy=%d(%s p=%d)", label, i, order[i], na.Name, na.Parent, legacyOrder[i], la.Name, la.Parent)
		}
	}
	for i := range legacyRank {
		if rank[i] != legacyRank[i] {
			t.Fatalf("%s: rank[%d] new=%d legacy=%d", label, i, rank[i], legacyRank[i])
		}
	}
	if !bytes.Equal(encodeUint32Section(order), encodeUint32Section(legacyOrder)) {
		t.Fatalf("%s: PRNK section bytes differ", label)
	}
}

func TestPathOrderRankMatchesLegacy(t *testing.T) {
	comparePathOrderRank(t, "zipf", buildZipfPathIndex(t, 20_000))
	comparePathOrderRank(t, "edges", pathOrderEdgeIndex())
}

func writePathRankIndex(t *testing.T, src *Index, order, rank []uint32, name string) string {
	t.Helper()
	idx := &Index{
		Version: src.Version, Roots: src.Roots, BuiltAt: src.BuiltAt,
		Source: src.Source, Volume: src.Volume, Compact: src.Compact,
		CompactAttrs: src.CompactAttrs, Records: src.Records,
		Derived: indexDerivedSections{PathOrder: order, PathRank: rank},
	}
	idx.Derived.NameOrder, idx.Derived.NameRank = buildCompactNameOrderRank(src)
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	tmp, err := stageIndexFile(path, idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitStageIndexFile(path, tmp); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPathOrderRankSectionsByteIdentical routes the legacy path arrays through
// the writer and compares the whole index file with the new implementation.
func TestPathOrderRankSectionsByteIdentical(t *testing.T) {
	idx := buildZipfPathIndex(t, 20_000)
	legacyOrder, legacyRank := buildCompactPathOrderRankLegacy(idx)
	legacyPath := writePathRankIndex(t, idx, legacyOrder, legacyRank, "legacy.gsi")
	newPath := writePathRankIndex(t, idx, nil, nil, "new.gsi")
	legacyBytes, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	newBytes, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(legacyBytes, newBytes) {
		t.Fatalf("index files differ: legacy=%d bytes new=%d bytes", len(legacyBytes), len(newBytes))
	}
}
