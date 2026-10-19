package main

import (
	"fmt"
	"sync"
	"testing"
)

// refillFixture builds a volume with a pending rename (hidden base row) and a
// regex whose required literal gives many prefilter candidates. hitFirst and
// missFirst control the scan-order split so the first valve reads can be all
// misses or a mix.
func refillFixture(t *testing.T, hitsBefore, misses, hitsAfter int) (*serviceVolumeIndex, map[string]string) {
	t.Helper()
	recs := make([]contentVolRecord, 0, hitsBefore+misses+hitsAfter+1)
	missText := map[string]string{}
	frn := uint64(100)
	add := func(name, body string, miss bool) {
		recs = append(recs, contentVolRecord{frn: frn, parent: -1, parentFRN: 1, name: name, content: body})
		if miss {
			missText[`C:\`+name] = body
		}
		frn++
	}
	for i := 0; i < hitsBefore; i++ {
		add(fmt.Sprintf("a%04d.txt", i), "needle123 hit", false)
	}
	for i := 0; i < misses; i++ {
		add(fmt.Sprintf("b%04d.txt", i), "needle filler", true)
	}
	for i := 0; i < hitsAfter; i++ {
		add(fmt.Sprintf("c%04d.txt", i), "needle123 hit", false)
	}
	renameFRN := frn
	add("zzz-rename.txt", "plain filler", true)
	vol := newContentRecordVolume(t, "C:", recs)
	vol.applyUSNChanges([]usnChange{{FRN: renameFRN, ParentFRN: 1, USN: 9001, Reason: usnReasonRenameNew, Name: "zz-renamed.txt"}})
	if vol.snapshotHiddenBaseIDs().empty() {
		t.Fatal("fixture overlay hides no base row")
	}
	return vol, missText
}

func withVerifyBudget(t *testing.T, valve int) {
	t.Helper()
	overlayPrefixSetup(t)
	prev := contentDiskVerifyBudget
	contentDiskVerifyBudget = valve
	pinContentDiskVerifyPageScale(t, 1)
	t.Cleanup(func() { contentDiskVerifyBudget = prev })
}

// TestContentExplicitBudgetRefillsFromZeroHits proves an explicit budget is
// honored when the capped first pass found no hits: the refill must start at
// the first unverified candidate (the frontier), not require a hit to resume.
func TestContentExplicitBudgetRefillsFromZeroHits(t *testing.T) {
	vol, _ := refillFixture(t, 0, 300, 50)
	withVerifyBudget(t, 40)

	capped := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol},
		queryOptions{Query: `content:/needle[0-9]+/`, Limit: 50, Trace: capped}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || !capped.contentIncomplete() {
		t.Fatalf("default valve: hits=%d incomplete=%v; want a zero-hit incomplete page", len(got), capped.contentIncomplete())
	}
	if !capped.contentFrontierSet {
		t.Fatal("capped pass recorded no verify frontier")
	}

	explicit := &searchTrace{}
	got, err = searchServiceVolumes([]*serviceVolumeIndex{vol},
		queryOptions{Query: `content:/needle[0-9]+/`, Limit: 50, Trace: explicit, ContentDiskVerifyBudget: 1000}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 {
		t.Fatalf("explicit budget 1000: hits=%d (disk verified %d of cap %d); want a full page of 50", len(got), explicit.DiskVerified, explicit.DiskVerifyBudget)
	}
}

// TestContentExplicitBudgetRefillReadsNoCandidateTwice proves the refill
// resumes at the frontier, not after the last emitted hit: every candidate
// verified by the first pass is skipped, and the total stays within the first
// pass plus the reads needed to fill the remainder.
func TestContentExplicitBudgetRefillReadsNoCandidateTwice(t *testing.T) {
	vol, missText := refillFixture(t, 20, 300, 50)
	withVerifyBudget(t, 40)

	prevRead := contentDiskReadForVerify
	var readsMu sync.Mutex
	reads := map[string]int{}
	contentDiskReadForVerify = func(path string, s contentExtractSettings) ([]byte, bool) {
		readsMu.Lock()
		reads[path]++
		readsMu.Unlock()
		return prevRead(path, s)
	}
	t.Cleanup(func() { contentDiskReadForVerify = prevRead })

	explicit := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol},
		queryOptions{Query: `content:/needle[0-9]+/`, Limit: 50, Trace: explicit, ContentDiskVerifyBudget: 1000}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 {
		t.Fatalf("explicit budget 1000: hits=%d; want a full page of 50", len(got))
	}
	readsMu.Lock()
	for path := range missText {
		if reads[path] > 1 {
			readsMu.Unlock()
			t.Fatalf("candidate %q read %d times; the refill must resume at the frontier", path, reads[path])
		}
	}
	readsMu.Unlock()
	batch := 4 * contentParallelWorkers()
	const snippets = 50
	if explicit.DiskVerified > 40+310+batch+snippets {
		t.Fatalf("explicit run verified %d files; want at most the first valve 40 + remainder 310 + batch %d + snippets %d", explicit.DiskVerified, batch, snippets)
	}
}
