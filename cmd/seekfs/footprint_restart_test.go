package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfiguredVolumeDefersUSNCatchUp pins the restart fast path: a
// USN volume's mmapped base (+ WAL overlay) must be queryable as soon as
// loadConfiguredVolume returns, without the journal catch-up running first.
// Catch-up runs behind serving (backgroundCatchUpVolume). Z: does not exist
// on any test box, so any synchronous catch-up attempt would mark the volume
// stale here — exactly the regression this guards.
func TestLoadConfiguredVolumeDefersUSNCatchUp(t *testing.T) {
	records := []CompactRecord{
		{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
		{FRN: 200, ParentFRN: 100, Parent: 0, Name: "deferred-zz.txt"},
	}
	idx := &Index{
		Version: indexVersion,
		Roots:   []string{`Z:\`},
		Source:  "usn",
		Volume:  "Z:",
		Compact: true,
		Records: records,
	}
	buildOrders(idx)
	db := filepath.Join(t.TempDir(), "deferred.gsi")
	if err := saveIndex(db, idx); err != nil {
		t.Fatalf("save deferred fixture: %v", err)
	}
	_, vol, err := loadConfiguredVolume(db)
	if err != nil {
		t.Fatalf("loadConfiguredVolume: %v", err)
	}
	if !vol.needsBackgroundCatchUp() {
		t.Fatalf("needsBackgroundCatchUp = false for a compact USN volume; want true")
	}
	if vol.state != "ready" {
		t.Fatalf("state = %q right after load; want ready (catch-up must not block first query)", vol.state)
	}
	query := func() int {
		t.Helper()
		got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "deferred-zz", MatchPath: true, Limit: 10}, false)
		if err != nil {
			t.Fatalf("search deferred-zz: %v", err)
		}
		return len(got)
	}
	if n := query(); n != 1 {
		t.Fatalf("base visible count = %d; want 1", n)
	}
	// While the background catch-up marks the volume replaying, the same
	// base stays served (serviceVolumesForQuery admits non-ready volumes).
	vol.state = "replaying"
	if n := query(); n != 1 {
		t.Fatalf("base visible count during replaying = %d; want 1", n)
	}
}
