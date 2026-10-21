package main

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

// TestOverlayApplyIsImmediatelyVisible pins the name-side freshness
// invariant: once applyUSNChanges returns, the very next query observes the
// change. There is no debounce, timer, or persist gate between the overlay
// apply and query visibility — the snapshot publish is synchronous.
func TestOverlayApplyIsImmediatelyVisible(t *testing.T) {
	vol := buildOverlayLimitFixture(t, 5)

	count := func(query string) int {
		t.Helper()
		got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: query, MatchPath: true, Limit: 20}, false)
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		return len(got)
	}

	// Create: a brand-new FRN under the fixture root (FRN 100).
	vol.applyUSNChanges([]usnChange{{FRN: 5000, ParentFRN: 100, USN: 1001, Name: "zzvisbrandnew.txt", Reason: usnReasonFileCreate}})
	if n := count("zzvisbrandnew"); n != 1 {
		t.Fatalf("created file visible count = %d; want 1 (no poll, no sleep)", n)
	}

	// Rename: FRN 200 is needle-00.txt in the fixture.
	vol.applyUSNChanges([]usnChange{
		{FRN: 200, ParentFRN: 100, USN: 1002, Name: "needle-00.txt", Reason: usnReasonRenameOld},
		{FRN: 200, ParentFRN: 100, USN: 1003, Name: "zzvisrenamed.txt", Reason: usnReasonRenameNew},
	})
	if n := count("zzvisrenamed"); n != 1 {
		t.Fatalf("renamed file visible count = %d; want 1", n)
	}
	if n := count("needle-00"); n != 0 {
		t.Fatalf("old name visible count = %d; want 0", n)
	}

	// Delete: FRN 201 is needle-01.txt in the fixture.
	vol.applyUSNChanges([]usnChange{{FRN: 201, ParentFRN: 100, USN: 1004, Name: "needle-01.txt", Reason: usnReasonFileDelete}})
	if n := count("needle-01"); n != 0 {
		t.Fatalf("deleted file visible count = %d; want 0", n)
	}
}

func appendWalkNotifyEntry(buf []byte, action uint32, name string, last bool) []byte {
	utf := utf16.Encode([]rune(name))
	entryLen := 12 + len(utf)*2
	start := len(buf)
	buf = append(buf, make([]byte, entryLen)...)
	if !last {
		binary.LittleEndian.PutUint32(buf[start:], uint32(entryLen))
	}
	binary.LittleEndian.PutUint32(buf[start+4:], action)
	binary.LittleEndian.PutUint32(buf[start+8:], uint32(len(utf)*2))
	for i, c := range utf {
		binary.LittleEndian.PutUint16(buf[start+12+i*2:], c)
	}
	return buf
}

// TestWalkClassifyNotify pins the watcher's fail-safe direction: anything
// that cannot be proven metadata-only — truncation, garbled offsets,
// unknown actions, empty batches — classifies Unknown, and Unknown
// rebuilds exactly like a name change. A missed rebuild would silently
// drop a rename; a spurious one only costs a re-walk.
func TestWalkClassifyNotify(t *testing.T) {
	cases := []struct {
		name  string
		buf   []byte
		class walkNotifyClass
	}{
		{"empty", nil, walkNotifyUnknown},
		{"truncated header", []byte{1, 2, 3}, walkNotifyUnknown},
		{"truncated second entry", appendWalkNotifyEntry(nil, 3, "a.txt", false)[:14], walkNotifyUnknown},
		{"modified only", appendWalkNotifyEntry(nil, 3, "file.txt", true), walkNotifyMeta},
		{"added", appendWalkNotifyEntry(nil, 1, "file.txt", true), walkNotifyName},
		{"removed", appendWalkNotifyEntry(nil, 2, "file.txt", true), walkNotifyName},
		{"renamed old", appendWalkNotifyEntry(nil, 4, "file.txt", true), walkNotifyName},
		{"renamed new", appendWalkNotifyEntry(nil, 5, "file.txt", true), walkNotifyName},
		{"zero action", appendWalkNotifyEntry(nil, 0, "file.txt", true), walkNotifyUnknown},
		{"future action", appendWalkNotifyEntry(nil, 9, "file.txt", true), walkNotifyUnknown},
	}
	for _, tc := range cases {
		if got := walkClassifyNotify(tc.buf); got != tc.class {
			t.Fatalf("%s: class = %d; want %d", tc.name, got, tc.class)
		}
	}
	// A write burst followed by a delete still takes the fast name path.
	mixed := appendWalkNotifyEntry(nil, 3, "a.txt", false)
	mixed = append(mixed, appendWalkNotifyEntry(nil, 2, "b.txt", true)...)
	if got := walkClassifyNotify(mixed); got != walkNotifyName {
		t.Fatalf("MODIFIED+REMOVED: class = %d; want name", got)
	}
	// A garbage offset after a metadata entry must rebuild, never skip.
	garbled := appendWalkNotifyEntry(nil, 3, "a.txt", false)
	binary.LittleEndian.PutUint32(garbled[0:], uint32(1<<30))
	if got := walkClassifyNotify(garbled); got != walkNotifyUnknown {
		t.Fatalf("garbled offset: class = %d; want unknown (rebuild)", got)
	}
	// A short offset that lands mid-buffer must rebuild, never over-read.
	short := appendWalkNotifyEntry(nil, 3, "a.txt", false)
	short = append(short, appendWalkNotifyEntry(nil, 3, "b.txt", true)...)
	binary.LittleEndian.PutUint32(short[0:], uint32(13))
	if got := walkClassifyNotify(short); got != walkNotifyUnknown {
		t.Fatalf("short offset: class = %d; want unknown (rebuild)", got)
	}
}

// TestWalkRebuildPlan pins the debounce split: names and doubt rebuild
// fast; metadata-only batches take the longer coalescing wait but still
// rebuild (Size/ModUnix feed filters, sorts, recency, and the content
// walk reconcile — a skip would leave them all stale).
func TestWalkRebuildPlan(t *testing.T) {
	nameBuf := appendWalkNotifyEntry(nil, 1, "n.txt", true)
	metaBuf := appendWalkNotifyEntry(nil, 3, "m.txt", true)
	doubtBuf := []byte{9, 9, 9}

	reason, d := walkRebuildPlan(nameBuf, false)
	if reason != "watch-change" || d != walkWatcherDebounce {
		t.Fatalf("name plan = (%q, %s); want (watch-change, fast)", reason, d)
	}
	reason, d = walkRebuildPlan(metaBuf, false)
	if reason != "watch-meta" || d != walkWatcherMetaDebounce {
		t.Fatalf("meta plan = (%q, %s); want (watch-meta, meta debounce)", reason, d)
	}
	if walkWatcherMetaDebounce <= walkWatcherDebounce {
		t.Fatalf("meta debounce %s <= name debounce %s; bursts would not coalesce", walkWatcherMetaDebounce, walkWatcherDebounce)
	}
	reason, d = walkRebuildPlan(doubtBuf, false)
	if reason != "watch-unknown" || d != walkWatcherDebounce {
		t.Fatalf("doubt plan = (%q, %s); want (watch-unknown, fast rebuild)", reason, d)
	}
	reason, d = walkRebuildPlan(nil, true)
	if reason != "watch-overflow" || d != walkWatcherDebounce {
		t.Fatalf("overflow plan = (%q, %s); want (watch-overflow, fast rebuild)", reason, d)
	}
}
