package main

import (
	"fmt"
	"testing"
	"time"
)

// budgetTestVolume is a 200-file corpus where every file matches, with each
// disk verify slowed so a short --budget expires mid-page.
func budgetTestVolume(t *testing.T, delay time.Duration) *serviceVolumeIndex {
	t.Helper()
	files := make([]contentFixtureFile, 200)
	for i := range files {
		files[i] = contentFixtureFile{frn: uint64(i + 2), name: fmt.Sprintf("file-%03d.txt", i), text: "needle in indexed text"}
	}
	vol := newContentQueryVolume(t, files)
	uncappedDiskVerify(t)
	prev := contentDiskReadForVerify
	contentDiskReadForVerify = func(path string, s contentExtractSettings) ([]byte, bool) {
		time.Sleep(delay)
		return prev(path, s)
	}
	t.Cleanup(func() { contentDiskReadForVerify = prev })
	return vol
}

func TestContentTimeBudgetStopsDiskVerify(t *testing.T) {
	vol := budgetTestVolume(t, 2*time.Millisecond)
	vols := []*serviceVolumeIndex{vol}
	full, err := searchServiceVolumes(vols, queryOptions{Query: "content:needle", Limit: 100, Trace: &searchTrace{}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 100 {
		t.Fatalf("unbudgeted = %d hits; want 100", len(full))
	}
	trace := &searchTrace{}
	got, err := searchServiceVolumes(vols, queryOptions{Query: "content:needle", Limit: 100, Trace: trace, BudgetMS: 20}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !trace.BudgetExceeded || !trace.ContentIncomplete {
		t.Fatalf("budget trace: exceeded=%v incomplete=%v; want both true", trace.BudgetExceeded, trace.ContentIncomplete)
	}
	if len(got) == 0 || len(got) >= len(full) {
		t.Fatalf("budgeted = %d hits; want a partial page (0 < n < %d)", len(got), len(full))
	}
	// Partial hits keep the normal result order.
	pos := make(map[string]int, len(full))
	for i, e := range full {
		pos[e.Path] = i
	}
	for i := 1; i < len(got); i++ {
		if pos[got[i-1].Path] >= pos[got[i].Path] {
			t.Fatalf("budgeted hits out of order at %d: %s before %s", i, got[i-1].Path, got[i].Path)
		}
	}
}

func TestContentTimeBudgetZeroAndCountUnchanged(t *testing.T) {
	vol := budgetTestVolume(t, 0)
	vols := []*serviceVolumeIndex{vol}
	trace := &searchTrace{}
	got, err := searchServiceVolumes(vols, queryOptions{Query: "content:needle", Limit: 50, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 || trace.BudgetExceeded {
		t.Fatalf("budget 0 = %d hits, exceeded=%v; want 50, false", len(got), trace.BudgetExceeded)
	}
	// Count ignores budget_ms (no deadline is armed for counts): exact.
	n, ok, err := countServiceVolumes(vols, queryOptions{Query: "content:needle", Trace: &searchTrace{}, BudgetMS: 1})
	if err != nil || !ok || n != 200 {
		t.Fatalf("count with budget = %d, %v, %v; want 200, true, nil", n, ok, err)
	}
}
