package main

import (
	"fmt"
	"strings"
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
	// Partial hits are an exact prefix, not merely sorted: a timed-out parallel
	// batch must not expose a later verified hit past an earlier unverified row.
	for i := range got {
		if got[i].Path != full[i].Path {
			t.Fatalf("budgeted rank %d = %q; want exact full-result prefix %q", i, got[i].Path, full[i].Path)
		}
	}
}

func TestContentTimeBudgetZeroAndCountHonorsBudget(t *testing.T) {
	vol := budgetTestVolume(t, 2*time.Millisecond)
	vols := []*serviceVolumeIndex{vol}
	// No budget: the page fills and nothing is marked exceeded.
	trace := &searchTrace{}
	got, err := searchServiceVolumes(vols, queryOptions{Query: "content:needle", Limit: 50, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 || trace.BudgetExceeded {
		t.Fatalf("budget 0 = %d hits, exceeded=%v; want 50, false", len(got), trace.BudgetExceeded)
	}
	// No budget on the count: exact, complete.
	n, ok, err := countServiceVolumes(vols, queryOptions{Query: "content:needle", Trace: &searchTrace{}})
	if err != nil || !ok || n != 200 {
		t.Fatalf("unbudgeted count = %d, %v, %v; want 200, true, nil", n, ok, err)
	}
	// A count now honors --budget too: a deadline that expires mid-tally (200
	// files x 2ms >> 20ms) refuses rather than returning a partial number.
	ctr := &searchTrace{}
	if _, ok, err := countServiceVolumes(vols, queryOptions{Query: "content:needle", Trace: ctr, BudgetMS: 20}); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("budgeted count err=%v ok=%v; want a refused (incomplete) tally", err, ok)
	}
	if !ctr.BudgetExceeded || !ctr.ContentIncomplete {
		t.Fatalf("budgeted count: exceeded=%v incomplete=%v; want both true", ctr.BudgetExceeded, ctr.ContentIncomplete)
	}
}
