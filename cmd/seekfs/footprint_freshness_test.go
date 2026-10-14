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

func TestWalkNotifyHasNameAction(t *testing.T) {
	var empty []byte
	if walkNotifyHasNameAction(empty) {
		t.Fatalf("empty buffer has name action; want false")
	}
	if walkNotifyHasNameAction([]byte{1, 2, 3}) {
		t.Fatalf("truncated buffer has name action; want false")
	}
	modified := appendWalkNotifyEntry(nil, 3, "file.txt", true)
	if walkNotifyHasNameAction(modified) {
		t.Fatalf("MODIFIED-only buffer has name action; want false (no rebuild)")
	}
	for action, name := range map[uint32]string{1: "ADDED", 2: "REMOVED", 4: "RENAMED_OLD", 5: "RENAMED_NEW"} {
		buf := appendWalkNotifyEntry(nil, action, "file.txt", true)
		if !walkNotifyHasNameAction(buf) {
			t.Fatalf("%s buffer lacks name action; want true", name)
		}
	}
	// A write burst followed by a delete still rebuilds.
	mixed := appendWalkNotifyEntry(nil, 3, "a.txt", false)
	mixed = append(mixed, appendWalkNotifyEntry(nil, 2, "b.txt", true)...)
	if !walkNotifyHasNameAction(mixed) {
		t.Fatalf("MODIFIED+REMOVED buffer lacks name action; want true")
	}
	// Corrupt framing declines to rebuild rather than over-reading.
	corrupt := appendWalkNotifyEntry(nil, 3, "a.txt", false)
	binary.LittleEndian.PutUint32(corrupt[0:], uint32(1<<30))
	if walkNotifyHasNameAction(corrupt) {
		t.Fatalf("corrupt-offset buffer has name action; want false")
	}
}
