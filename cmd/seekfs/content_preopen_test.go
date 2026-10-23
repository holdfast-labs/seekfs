package main

// Pre-open inclusion decisions: rules that need only name-index metadata must
// run before the file is opened, so an excluded file costs zero opens. These
// tests use the contentOpenForExtract seam to count actual opens.

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestContentPreOpenSkipAvoidsOpens(t *testing.T) {
	dir := t.TempDir()
	textPath := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(textPath, []byte("hello needle world"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := contentOpenForExtract
	opens := 0
	contentOpenForExtract = func(path string) (*os.File, error) {
		opens++
		return restore(path)
	}
	t.Cleanup(func() { contentOpenForExtract = restore })

	ctx := contentWithExtractSettings(context.Background(), contentDefaultExtractSettings())
	cases := []struct {
		name   string
		path   string
		size   int64
		reason string
		binary bool
	}{
		{"known binary extension", filepath.Join(dir, "photo.png"), 4096, "binary", true},
		{"lockfile", filepath.Join(dir, "package-lock.json"), 4096, contentSkipLockfile, false},
		{"minified bundle", filepath.Join(dir, "app.min.js"), 4096, contentSkipMinified, false},
		{"sourcemap", filepath.Join(dir, "bundle.js.map"), 4096, contentSkipMinified, false},
		{"large json", filepath.Join(dir, "data.json"), 200 << 10, contentSkipLargeJSON, false},
		{"over-cap pdf", filepath.Join(dir, "report.pdf"), 3 << 20, contentSkipSizeCap, false},
		{"over-cap ooxml", filepath.Join(dir, "deck.docx"), 3 << 20, contentSkipSizeCap, false},
	}
	for _, c := range cases {
		opens = 0
		_, res, ok := contentBuildDocSafe(ctx, contentBuildItem{frn: 1, path: c.path, size: c.size})
		if ok {
			t.Errorf("%s: opened and extracted an excluded file", c.name)
		}
		if opens != 0 {
			t.Errorf("%s: %d opens; want 0 (metadata-only skip)", c.name, opens)
		}
		if res.Reason != c.reason || res.Binary != c.binary {
			t.Errorf("%s: result = %+v; want reason %q binary=%v", c.name, res, c.reason, c.binary)
		}
	}

	// A text file is still opened and extracted, and a size-blind call cannot
	// apply the size-gated rules (it opens).
	opens = 0
	doc, res, ok := contentBuildDocSafe(ctx, contentBuildItem{frn: 2, path: textPath, size: 19})
	if !ok || doc.path != textPath || res.Binary || res.Skipped {
		t.Fatalf("text file doc=%+v res=%+v ok=%v; want extracted", doc, res, ok)
	}
	if opens != 1 {
		t.Fatalf("text file opens = %d; want 1", opens)
	}
}

// Within a scope tier the newest files extract first, then path/FRN break ties.
func TestContentBuildItemPriorityOrder(t *testing.T) {
	items := []contentBuildItem{
		{frn: 1, path: `c:\repo\old.cc`, priority: 0, mod: 100},
		{frn: 2, path: `c:\repo\new.cc`, priority: 0, mod: 300},
		{frn: 3, path: `c:\repo\mid.cc`, priority: 0, mod: 200},
		{frn: 4, path: `c:\docs\a.txt`, priority: 2, mod: 999},
		{frn: 5, path: `c:\cfg\a.txt`, priority: 1, mod: 50},
		{frn: 6, path: `c:\repo\new2.cc`, priority: 0, mod: 300},
		{frn: 7, path: `c:\other\a.txt`, priority: 3, mod: 1000},
	}
	sort.Slice(items, func(i, j int) bool { return contentBuildItemLess(items[i], items[j]) })
	want := []uint64{2, 6, 3, 1, 5, 4, 7}
	for i, frn := range want {
		if items[i].frn != frn {
			t.Fatalf("order[%d] frn=%d; want %d (full order %v)", i, items[i].frn, frn, orderFRNs(items))
		}
	}
}

func orderFRNs(items []contentBuildItem) []uint64 {
	out := make([]uint64, len(items))
	for i := range items {
		out[i] = items[i].frn
	}
	return out
}
