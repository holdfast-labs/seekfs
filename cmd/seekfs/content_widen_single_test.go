package main

import (
	"fmt"
	"os"
	"slices"
	"testing"
)

// contentSharedNameRecords builds a single volume with dirs directories, each
// holding one file named name with body content. Every file share the same
// basename, so a default-order page boundary always lands on a name shared by
// several records.
func contentSharedNameRecords(volume string, dirs int, name, body string) []contentVolRecord {
	recs := make([]contentVolRecord, 0, dirs*2)
	for i := 0; i < dirs; i++ {
		dir := fmt.Sprintf("d%03d", i)
		dirFRN := uint64(i*2) + 100
		dirIndex := int32(len(recs))
		recs = append(recs, contentVolRecord{
			frn: dirFRN, parent: -1, parentFRN: 1,
			name: dir, mode: uint32(os.ModeDir),
			path: volume + `\` + dir,
		})
		recs = append(recs, contentVolRecord{
			frn: dirFRN + 1, parent: dirIndex, parentFRN: dirFRN,
			name: name, size: int64(i + 1), modUnix: int64(1_600_000_000 + i),
			path:    volume + `\` + dir + `\` + name,
			content: body,
		})
	}
	return recs
}

// TestContentWidenSharedNameSingleVolumeStaysBounded guards the single-volume
// default-order content page against the shared-name widen: with one volume
// there is no cross-volume merge, so the volume already orders name ties by
// record id exactly as the unbounded search does. The old clause widened to the
// full window for a byte-identical page, re-reading files (and spending the
// shared disk-verify budget) a second time.
func TestContentWidenSharedNameSingleVolumeStaysBounded(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	uncappedDiskVerify(t)

	vol := newContentRecordVolume(t, "C:", contentSharedNameRecords("C:", 200, "notes.txt", "a shared needle body"))
	vols := []*serviceVolumeIndex{vol}

	prev := contentDiskReadForVerify
	reads := 0
	contentDiskReadForVerify = func(path string, s contentExtractSettings) ([]byte, bool) {
		reads++
		return prev(path, s)
	}
	t.Cleanup(func() { contentDiskReadForVerify = prev })

	const limit = 3
	query := "content:needle"

	run := func(fullCandidates bool) ([]Entry, int) {
		reads = 0
		matches, err := searchServiceVolumes(vols, queryOptions{Query: query, Limit: limit, contentFullCandidates: fullCandidates, Trace: &searchTrace{}}, false)
		if err != nil {
			t.Fatalf("searchServiceVolumes(full=%v): %v", fullCandidates, err)
		}
		return matches, reads
	}

	fullMatches, fullReads := run(true)
	bounded, boundedReads := run(false)

	if len(bounded) != limit {
		t.Fatalf("bounded page = %d hits; want %d", len(bounded), limit)
	}
	// The fixture must actually put a shared name at the page boundary, or the
	// clause under test is not exercised.
	if !vol.nameLowerNameShared(entryLowerName(bounded[limit-1])) {
		t.Fatalf("fixture did not put a shared name at the page boundary: %q", bounded[limit-1].Name)
	}
	// The single-volume page must be byte-identical to the unbounded reference
	// (drop the leading completeness element; the entries are what the page is).
	gotSig := contentBoundedDifferentialSignature(bounded, nil)[1:]
	wantSig := contentBoundedDifferentialSignature(fullMatches, nil)[1:]
	if !slices.Equal(gotSig, wantSig) {
		t.Fatalf("single-volume page diverged:\nbounded=%v\nfull=%v", gotSig, wantSig)
	}
	// The bounded page verifies only its limit; the unbounded reference reads
	// the whole candidate set. The old shared-name widen made the bounded arm a
	// second full-window pass, so it read at least as many files as the reference.
	if boundedReads >= fullReads {
		t.Fatalf("bounded page read %d files; want fewer than the %d a full-window pass needs", boundedReads, fullReads)
	}
}
