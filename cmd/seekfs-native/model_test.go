package main

import (
	"testing"
	"time"
)

func int64ptr(v int64) *int64 { return &v }

func TestRowFromJSON(t *testing.T) {
	size := int64(42 * 1024)
	row := rowFromJSON(jsonResult{
		Path:     `C:\Downloads\scan.nrrd`,
		Name:     "scan.nrrd",
		Size:     &size,
		Modified: "2026-06-29T10:09:00Z",
		Snippet:  "a needle here",
	})
	if row.Name != "scan.nrrd" || row.Dir != `C:\Downloads` {
		t.Fatalf("row = %+v", row)
	}
	if !row.HasSize || row.Size != size {
		t.Fatalf("size = %v %d", row.HasSize, row.Size)
	}
	if !row.HasMod {
		t.Fatal("modified not parsed")
	}
	if row.Type != "nrrd" {
		t.Fatalf("type = %q, want nrrd", row.Type)
	}
	if row.Snippet != "a needle here" {
		t.Fatalf("snippet = %q", row.Snippet)
	}
	dir := rowFromJSON(jsonResult{Path: `C:\Docs`, Name: "Docs", IsDir: true})
	if !dir.IsDir || dir.Type != "" {
		t.Fatalf("dir row = %+v", dir)
	}
}

func TestRowsPreferServiceRows(t *testing.T) {
	resp := serviceResponse{
		Results: []string{`C:\Downloads\scan.nrrd`},
		Rows:    []jsonResult{{Path: `C:\Downloads\scan.nrrd`, Name: "scan.nrrd", Size: int64ptr(7)}},
	}
	rows := rowsFromServiceResponse(resp)
	if len(rows) != 1 || rows[0].Size != 7 || !rows[0].Exists {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestSortRowsNameDirsFirst(t *testing.T) {
	rows := []ResultRow{
		{Path: `C:\b.txt`, Name: "b.txt"},
		{Path: `C:\A`, Name: "A", IsDir: true},
		{Path: `C:\a.txt`, Name: "a.txt"},
	}
	sortRows(rows, SortName, true)
	if rows[0].Name != "A" || rows[1].Name != "a.txt" || rows[2].Name != "b.txt" {
		t.Fatalf("order = %v", []string{rows[0].Name, rows[1].Name, rows[2].Name})
	}
}

func TestSortRowsSizeAndModified(t *testing.T) {
	rows := []ResultRow{
		{Path: "b", Name: "b", Size: 20, HasSize: true, Modified: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), HasMod: true},
		{Path: "a", Name: "a", Size: 10, HasSize: true, Modified: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), HasMod: true},
	}
	sortRows(rows, SortSize, true)
	if rows[0].Name != "a" {
		t.Fatalf("size order = %v", rows)
	}
	// Modified ascending=true means oldest first after the Explorer-style flip.
	sortRows(rows, SortModified, true)
	if rows[0].Name != "b" {
		t.Fatalf("modified order = %v", rows)
	}
}

func TestSortRowsTypeAndFolder(t *testing.T) {
	rows := []ResultRow{
		{Path: `C:\x\b.go`, Name: "b.go", Dir: `C:\x`, Type: "go"},
		{Path: `C:\a.txt`, Name: "a.txt", Dir: `C:\`, Type: "txt"},
	}
	sortRows(rows, SortFolder, true)
	if rows[0].Dir != `C:\` {
		t.Fatalf("folder order = %+v", rows)
	}
	sortRows(rows, SortType, true)
	if rows[0].Type != "go" {
		t.Fatalf("type order = %+v", rows)
	}
}

func TestGroupByFolder(t *testing.T) {
	rows := []ResultRow{
		{Path: `C:\a\x`, Dir: `C:\a`},
		{Path: `C:\b\y`, Dir: `C:\b`},
		{Path: `C:\a\z`, Dir: `C:\a`},
	}
	groups := groupByFolder(rows)
	if len(groups) != 2 || groups[0].Dir != `C:\a` || len(groups[0].Rows) != 2 {
		t.Fatalf("groups = %+v", groups)
	}
}

func TestContentStatusText(t *testing.T) {
	if got := contentStatusText(nil); got != "" {
		t.Fatalf("nil = %q", got)
	}
	if got := contentStatusText(&contentState{State: "indexing", BuildDone: 5, BuildTotal: 10}); got != "50% indexing contents" {
		t.Fatalf("indexing = %q", got)
	}
	if got := contentStatusText(&contentState{State: "ready", Docs: 1200}); got != "1,200 files indexed" {
		t.Fatalf("ready = %q", got)
	}
	if got := contentStatusText(&contentState{State: "ready", Partial: true}); got != "Contents: partial results" {
		t.Fatalf("partial = %q", got)
	}
	if got := contentStatusText(&contentState{State: "error", BuildError: "disk full"}); got != "Contents: disk full" {
		t.Fatalf("error = %q", got)
	}
}

func TestContentUnavailable(t *testing.T) {
	for _, msg := range []string{
		"content indexing unavailable: the content index is not built or still building",
		"content search is disabled; run seekfs plugin add content to enable it",
	} {
		if !contentUnavailable(errString(msg)) {
			t.Fatalf("%q not detected as content-unavailable", msg)
		}
	}
	if contentUnavailable(errString("connection refused")) {
		t.Fatal("transport error misclassified")
	}
}

func TestSortKeyRoundTrip(t *testing.T) {
	for _, k := range []SortKey{SortName, SortFolder, SortSize, SortModified, SortType} {
		if sortKeyFromString(k.String()) != k {
			t.Fatalf("round trip failed for %v", k)
		}
	}
}
