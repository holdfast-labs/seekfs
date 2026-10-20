package main

import (
	"fmt"
	"testing"
)

// TestContentRegexCommonLiteralHitsWithinDefaultValve pins regex pages with
// strong required literals: files that contain both literals far apart must
// not spend the whole default disk valve before the files that contain the
// pattern. /Tab\w+Model/ extracts the runs "tab" and "model"; the trigram
// intersection is ~600 files here, and only the tab_strip_model files match.
func TestContentRegexCommonLiteralHitsWithinDefaultValve(t *testing.T) {
	const misses = 600
	const hits = 5
	hitNames := []string{
		"tab_strip_model.h",
		"tab_strip_model.cc",
		"tab_strip_model_unittest.cc",
		"tab_strip_model_observer.h",
		"tab_strip_model_order_controller.cc",
	}
	disjuncts := regexRequiredLiteralAlternatives(`Tab\w+Model`)
	if len(disjuncts) != 1 || len(disjuncts[0]) != 2 || disjuncts[0][0] != "tab" || disjuncts[0][1] != "model" {
		t.Fatalf("required literals = %v; want [[tab model]]", disjuncts)
	}
	files := make([]contentFixtureFile, 0, misses+hits)
	frn := uint64(2)
	for i := 0; i < misses; i++ {
		files = append(files, contentFixtureFile{frn, fmt.Sprintf("a%04d.txt", i), "Tab control Model marker"})
		frn++
	}
	for _, name := range hitNames {
		files = append(files, contentFixtureFile{frn, name, "class TabStripModel : public Base {}"})
		frn++
	}
	vol := newContentQueryVolume(t, files)

	prevMS, prevValve := contentDefaultBudgetMS, contentDiskVerifyBudget
	contentDefaultBudgetMS = 0
	contentDiskVerifyBudget = 512
	pinContentDiskVerifyPageScale(t, 1)
	t.Cleanup(func() {
		contentDefaultBudgetMS = prevMS
		contentDiskVerifyBudget = prevValve
	})

	trace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol},
		queryOptions{Query: `content:/Tab\w+Model/`, Limit: 5, Trace: trace}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("default valve: hits=%d source=%q disk_verified=%d candidates=%d; want 5 hits from the tab_strip_model files", len(got), trace.Source, trace.DiskVerified, trace.Candidates)
	}
	if trace.DiskVerified > 32 {
		t.Fatalf("disk_verified=%d; the name hint should keep the page within a few read batches", trace.DiskVerified)
	}
	t.Logf("default valve: hits=%d disk_verified=%d candidates=%d", len(got), trace.DiskVerified, trace.Candidates)
	seen := map[string]bool{}
	for _, e := range got {
		seen[e.Name] = true
	}
	for _, name := range hitNames {
		if !seen[name] {
			t.Fatalf("missing hit %q; got %v", name, seen)
		}
	}
}
