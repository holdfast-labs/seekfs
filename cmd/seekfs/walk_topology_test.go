package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A walk index persists its folder topology; the service must adopt it so
// under: scopes use O(1) interval checks instead of parent-chain walks, and
// the intervals must agree with the parent chain for every pair.
func TestWalkIndexAdoptsPersistedSubtreeIntervals(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		`a\b\c\one.h`, `a\b\two.h`, `a\three.cc`, `d\e\four.h`, `d\five.txt`, `six.h`,
	} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	db := filepath.Join(t.TempDir(), "walk.gsi")
	if err := cmdIndex([]string{"-root", root, "-db", db}); err != nil {
		t.Fatal(err)
	}
	_, vols, _, err := loadConfiguredVolumes([]string{db})
	if err != nil {
		t.Fatal(err)
	}
	vol := vols[0]
	count := vol.index.compactRecordCount()
	if vol.index.Source != "walk" {
		t.Fatalf("source = %q, want walk", vol.index.Source)
	}
	if len(vol.subtreeStart) != count || len(vol.childOffsets) != count+1 {
		t.Fatalf("topology not adopted: subtreeStart=%d childOffsets=%d records=%d", len(vol.subtreeStart), len(vol.childOffsets), count)
	}
	for id := 0; id < count; id++ {
		for rootID := 0; rootID < count; rootID++ {
			got := vol.isDescendantOrSelf(id, rootID)
			want := walkIsDescendantOrSelf(vol, id, rootID)
			if got != want {
				t.Fatalf("isDescendantOrSelf(%d, %d) = %v, parent chain says %v", id, rootID, got, want)
			}
		}
	}
	under := filepath.Join(root, "a")
	matches, err := searchServiceVolumes(vols, queryOptions{Query: ".h", Under: under, Limit: 20}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("under %s .h = %d results, want 2: %v", under, len(matches), matches)
	}
}

// A path term matches any path containing it, so the subtree under a
// directory whose name only contains the term ("main_content") must match
// just like the subtree under an exact "content" component.
func TestPathTermCoversPartialComponentSubtrees(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		`browser\content\main.cc`, `browser\main_content\ui\main.h`, `browser\main_content\ui\other.h`, `browser\other\main.cc`,
	} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	db := filepath.Join(t.TempDir(), "walk.gsi")
	if err := cmdIndex([]string{"-root", root, "-db", db}); err != nil {
		t.Fatal(err)
	}
	_, vols, _, err := loadConfiguredVolumes([]string{db})
	if err != nil {
		t.Fatal(err)
	}
	// content\main.cc, main_content\ui\main.h (dir main_content and ui hold
	// "content" but not "main" outside the subtree path itself).
	opts := queryOptions{Query: "path:content main", Limit: 100}
	matches, err := searchServiceVolumes(vols, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(matches))
	for _, m := range matches {
		got = append(got, m.Path)
	}
	var want []string
	for _, rel := range []string{`browser\content\main.cc`, `browser\main_content`, `browser\main_content\ui`, `browser\main_content\ui\main.h`, `browser\main_content\ui\other.h`} {
		want = append(want, filepath.Join(root, rel))
	}
	if !sameStringSet(got, want) {
		t.Fatalf("path:content main = %v\nwant %v", got, want)
	}
	count, ok, err := countServiceVolumes(vols, opts)
	if err != nil || !ok || count != len(want) {
		t.Fatalf("count = %d ok=%v err=%v, want %d", count, ok, err, len(want))
	}

	// Single-term path queries take the interval-coverage count and top-N
	// lanes, which must also include partial-name directory subtrees.
	single := queryOptions{Query: "path:content", Limit: 100}
	want = want[:0]
	for _, rel := range []string{`browser\content`, `browser\content\main.cc`, `browser\main_content`, `browser\main_content\ui`, `browser\main_content\ui\main.h`, `browser\main_content\ui\other.h`} {
		want = append(want, filepath.Join(root, rel))
	}
	matches, err = searchServiceVolumes(vols, single, false)
	if err != nil {
		t.Fatal(err)
	}
	got = got[:0]
	for _, m := range matches {
		got = append(got, m.Path)
	}
	if !sameStringSet(got, want) {
		t.Fatalf("path:content = %v\nwant %v", got, want)
	}
	count, ok, err = countServiceVolumes(vols, single)
	if err != nil || !ok || count != len(want) {
		t.Fatalf("path:content count = %d ok=%v err=%v, want %d", count, ok, err, len(want))
	}
}

func walkIsDescendantOrSelf(vol *serviceVolumeIndex, id, rootID int) bool {
	for cur, depth := id, 0; cur >= 0 && depth < 1024; depth++ {
		if cur == rootID {
			return true
		}
		cur = int(vol.index.compactRecord(cur).Parent)
	}
	return false
}
