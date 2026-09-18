package main

import "testing"

func TestContentIndexPathForDB(t *testing.T) {
	cases := map[string]string{
		`C:\ProgramData\seekfs\indexes\seekfs_c.gsi`: `C:\ProgramData\seekfs\indexes\seekfs_c.gsx`,
		`F:\idx.gsi`: `F:\idx.gsx`,
		`plain`:      `plain.gsx`,
	}
	for in, want := range cases {
		if got := contentIndexPathForDB(in); got != want {
			t.Errorf("contentIndexPathForDB(%q) = %q; want %q", in, got, want)
		}
	}
	if contentIndexPathForDB("") != "" {
		t.Fatal("empty dbPath must map to empty")
	}
}

func TestContentBaseFRNColumns(t *testing.T) {
	idx := &Index{Derived: indexDerivedSections{
		FRNs:         []uint64{100, 200},
		FRNRecordIDs: []uint32{0, 1},
	}}
	frns, ids, ok := contentBaseFRNColumns(idx)
	if !ok || len(frns) != 2 || len(ids) != 2 {
		t.Fatalf("columns = %v %v %v", frns, ids, ok)
	}
	bad := &Index{Derived: indexDerivedSections{FRNs: []uint64{1}, FRNRecordIDs: nil}}
	if _, _, ok := contentBaseFRNColumns(bad); ok {
		t.Fatal("mismatched FRN columns must not be usable")
	}
	if _, _, ok := contentBaseFRNColumns(nil); ok {
		t.Fatal("nil index must not be usable")
	}
}

func TestRebindContentAfterBaseSwap(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	idx := &Index{Volume: "C:", Derived: indexDerivedSections{
		FRNs:         []uint64{100, 200},
		FRNRecordIDs: []uint32{3, 4},
	}}
	vol := newServiceVolumeIndex(`C:\seekfs_c.gsi`, idx)
	if vol.content == nil {
		t.Fatal("content state should exist when the flag is on")
	}
	cidx := newContentIndex()
	cidx.Docs = []contentDoc{{DocID: 0, FRN: 100}, {DocID: 1, FRN: 200}}
	reader, err := openContentReader(cidx)
	if err != nil {
		t.Fatal(err)
	}
	vol.content.setReady(cidx, reader, nil)

	rebindContentAfterBaseSwap(vol)
	if id, ok := vol.content.resolver.recordID(0); !ok || id != 3 {
		t.Fatalf("doc 0 -> %d, %v; want 3", id, ok)
	}
	if id, ok := vol.content.resolver.recordID(1); !ok || id != 4 {
		t.Fatalf("doc 1 -> %d, %v; want 4", id, ok)
	}
}
