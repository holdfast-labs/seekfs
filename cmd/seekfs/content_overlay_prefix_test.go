package main

import "testing"

// These tests pin option (a) of NOTES.md: an incomplete content page on a
// volume with a pending overlay must stay a rank prefix of the complete
// result. The base verify paths record the first unverified candidate as a
// frontier; mergeOverlayMatches drops every merged entry sorting at or after
// it.

func overlayPrefixSearch(t *testing.T, vol *serviceVolumeIndex, limit, budget int) ([]string, *searchTrace) {
	t.Helper()
	prev := contentDiskVerifyBudget
	contentDiskVerifyBudget = budget
	defer func() { contentDiskVerifyBudget = prev }()
	trace := &searchTrace{}
	matches, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "content:needle", Limit: limit, Trace: trace}, false)
	if err != nil {
		t.Fatalf("search limit=%d budget=%d: %v", limit, budget, err)
	}
	out := make([]string, len(matches))
	for i := range matches {
		out[i] = matches[i].Name
	}
	return out, trace
}

func addContentOverlay(t *testing.T, vol *serviceVolumeIndex, frn uint64, name string) {
	t.Helper()
	vol.applyUSNChanges([]usnChange{{
		FRN: frn, ParentFRN: 1, USN: int64(frn), Reason: usnReasonFileCreate, Name: name,
	}})
	vol.content.deltaView().upsert(contentDeltaDoc{FRN: frn, Text: []byte("needle")})
}

func overlayPrefixSetup(t *testing.T) {
	t.Helper()
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	t.Setenv("SEEKFS_CONTENT_GLOBAL_LANE", "0")
	prev := contentDefaultBudgetMS
	contentDefaultBudgetMS = 0
	t.Cleanup(func() { contentDefaultBudgetMS = prev })
}

func isNamePrefix(short, long []string) bool {
	if len(short) > len(long) {
		return false
	}
	for i := range short {
		if short[i] != long[i] {
			return false
		}
	}
	return true
}

// TestContentOverlayPrefixMinimal is the confirmed scenario from NOTES.md:
// base a/b/c, overlay bc via delta, one disk read. The merged page must stop
// at the unverified base candidate b and return the prefix [a].
func TestContentOverlayPrefixMinimal(t *testing.T) {
	overlayPrefixSetup(t)
	vol := newContentRecordVolume(t, "C:", []contentVolRecord{
		{frn: 2, parent: -1, parentFRN: 1, name: "a.txt", content: "needle"},
		{frn: 3, parent: -1, parentFRN: 1, name: "b.txt", content: "needle"},
		{frn: 4, parent: -1, parentFRN: 1, name: "c.txt", content: "needle"},
	})
	addContentOverlay(t, vol, 999, "bc.txt")

	got, trace := overlayPrefixSearch(t, vol, 10, 1)
	want, _ := overlayPrefixSearch(t, vol, 10, 1<<20)
	if !trace.contentIncomplete() {
		t.Fatalf("capped query not incomplete: got=%v complete=%v", got, want)
	}
	if len(got) != 1 || got[0] != "a.txt" {
		t.Fatalf("capped page = %v; want prefix [a.txt] of complete %v", got, want)
	}
	if !isNamePrefix(got, want) {
		t.Fatalf("capped page %v is not a prefix of complete %v", got, want)
	}
}

func TestContentOverlayPrefixDifferential(t *testing.T) {
	overlayPrefixSetup(t)
	vol := newContentRecordVolume(t, "C:", []contentVolRecord{
		{frn: 10, parent: -1, parentFRN: 1, name: "m10.txt", content: "needle"},
		{frn: 30, parent: -1, parentFRN: 1, name: "m30.txt", content: "needle"},
		{frn: 50, parent: -1, parentFRN: 1, name: "m50.txt", content: "needle"},
		{frn: 70, parent: -1, parentFRN: 1, name: "m70.txt", content: "needle"},
	})
	addContentOverlay(t, vol, 1, "m00.txt")
	addContentOverlay(t, vol, 3, "m20.txt")
	addContentOverlay(t, vol, 5, "m40.txt")
	addContentOverlay(t, vol, 8, "m80.txt")

	for limit := 1; limit <= 4; limit++ {
		complete, completeTrace := overlayPrefixSearch(t, vol, limit, 1<<20)
		if completeTrace.contentIncomplete() {
			t.Fatalf("uncapped limit=%d unexpectedly incomplete", limit)
		}
		for cap := 1; cap <= 6; cap++ {
			got, trace := overlayPrefixSearch(t, vol, limit, cap)
			if !isNamePrefix(got, complete) {
				t.Fatalf("cap=%d limit=%d capped page %v is not a prefix of complete %v (incomplete=%v)",
					cap, limit, got, complete, trace.contentIncomplete())
			}
			if cap >= 4 && !trace.contentIncomplete() {
				if len(got) != len(complete) {
					t.Fatalf("cap=%d limit=%d complete query changed: got=%v want=%v", cap, limit, got, complete)
				}
			}
		}
	}
}

// TestContentOverlayPrefixCompleteUnchanged pins that an uncapped, complete
// content query with a pending overlay keeps its merged order.
func TestContentOverlayPrefixCompleteUnchanged(t *testing.T) {
	overlayPrefixSetup(t)
	vol := newContentRecordVolume(t, "C:", []contentVolRecord{
		{frn: 10, parent: -1, parentFRN: 1, name: "m10.txt", content: "needle"},
		{frn: 30, parent: -1, parentFRN: 1, name: "m30.txt", content: "needle"},
		{frn: 50, parent: -1, parentFRN: 1, name: "m50.txt", content: "needle"},
		{frn: 70, parent: -1, parentFRN: 1, name: "m70.txt", content: "needle"},
	})
	addContentOverlay(t, vol, 1, "m00.txt")
	addContentOverlay(t, vol, 3, "m20.txt")
	addContentOverlay(t, vol, 5, "m40.txt")
	addContentOverlay(t, vol, 8, "m80.txt")

	got, trace := overlayPrefixSearch(t, vol, 4, 1<<20)
	want := []string{"m00.txt", "m10.txt", "m20.txt", "m30.txt"}
	if len(got) != len(want) {
		t.Fatalf("complete page = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("complete page = %v; want %v", got, want)
		}
	}
	if trace.contentIncomplete() {
		t.Fatalf("uncapped complete query marked incomplete: %v", got)
	}
}
