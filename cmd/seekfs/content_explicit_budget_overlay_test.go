package main

import (
	"fmt"
	"testing"
)

// TestContentExplicitBudgetHonoredWithOverlay pins the explicit
// --content-verify-budget on a volume whose pending overlay hides a base row:
// that forces the full-window pass, which is the page verify itself, so the
// default page valve must not override the caller's budget. A capped run must
// also report the cap that actually applied.
func TestContentExplicitBudgetHonoredWithOverlay(t *testing.T) {
	overlayPrefixSetup(t)
	const files = 300
	recs := make([]contentVolRecord, 0, files)
	for i := 0; i < files; i++ {
		body := "filler text"
		if i%5 == 0 {
			body = "needle here"
		}
		recs = append(recs, contentVolRecord{frn: uint64(100 + i), parent: -1, parentFRN: 1, name: fmt.Sprintf("f%04d.txt", i), content: body})
	}
	vol := newContentRecordVolume(t, "C:", recs)
	// Rename one non-matching base file: the overlay now hides a base row.
	vol.applyUSNChanges([]usnChange{{FRN: 101, ParentFRN: 1, USN: 9001, Reason: usnReasonRenameNew, Name: "zz-renamed.txt"}})
	if vol.snapshotHiddenBaseIDs().empty() {
		t.Fatal("fixture overlay hides no base row")
	}
	prev := contentDiskVerifyBudget
	contentDiskVerifyBudget = 40
	pinContentDiskVerifyPageScale(t, 1)
	t.Cleanup(func() { contentDiskVerifyBudget = prev })

	capped := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "content:needle", Limit: 50, Trace: capped}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) >= 50 || !capped.contentIncomplete() {
		t.Fatalf("default valve: hits=%d incomplete=%v; want a short incomplete page", len(got), capped.contentIncomplete())
	}
	if capped.DiskVerifyBudget != int(capped.disk.limit) {
		t.Fatalf("reported cap %d != applied cap %d", capped.DiskVerifyBudget, capped.disk.limit)
	}

	explicit := &searchTrace{}
	got, err = searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "content:needle", Limit: 50, Trace: explicit, ContentDiskVerifyBudget: 1000}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 {
		t.Fatalf("explicit budget 1000: hits=%d (disk verified %d of cap %d); want a full page of 50", len(got), explicit.DiskVerified, explicit.DiskVerifyBudget)
	}
}
