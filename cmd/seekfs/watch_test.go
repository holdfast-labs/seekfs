package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func watchTestEntry(path string, size int64, mtime time.Time) watchEntry {
	return watchEntry{path: path, size: size, mtime: mtime}
}

func TestDiffWatchSnapshots(t *testing.T) {
	t0 := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(2 * time.Second)

	tests := []struct {
		name            string
		prev, next      map[string]watchEntry
		created         []string
		modified        []string
		deleted         []string
		wantNochangeNil bool
	}{
		{
			name: "created only",
			prev: map[string]watchEntry{},
			next: map[string]watchEntry{
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 10, t0),
				`F:\b.txt`: watchTestEntry(`F:\b.txt`, 20, t0),
			},
			created: []string{`F:\a.txt`, `F:\b.txt`},
		},
		{
			name: "modified size change",
			prev: map[string]watchEntry{
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 10, t0),
			},
			next: map[string]watchEntry{
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 99, t0),
			},
			modified: []string{`F:\a.txt`},
		},
		{
			name: "modified mtime-only change",
			prev: map[string]watchEntry{
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 10, t0),
			},
			next: map[string]watchEntry{
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 10, t1),
			},
			modified: []string{`F:\a.txt`},
		},
		{
			name: "unchanged is not modified",
			prev: map[string]watchEntry{
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 10, t0),
			},
			next: map[string]watchEntry{
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 10, t0),
			},
			created: nil, modified: nil, deleted: nil,
		},
		{
			name: "deleted only",
			prev: map[string]watchEntry{
				`F:\gone.txt`: watchTestEntry(`F:\gone.txt`, 5, t0),
			},
			next:    map[string]watchEntry{},
			deleted: []string{`F:\gone.txt`},
		},
		{
			name: "mixed",
			prev: map[string]watchEntry{
				`F:\kept.txt`:    watchTestEntry(`F:\kept.txt`, 1, t0),
				`F:\changed.txt`: watchTestEntry(`F:\changed.txt`, 2, t0),
				`F:\removed.txt`: watchTestEntry(`F:\removed.txt`, 3, t0),
			},
			next: map[string]watchEntry{
				`F:\kept.txt`:    watchTestEntry(`F:\kept.txt`, 1, t0),
				`F:\changed.txt`: watchTestEntry(`F:\changed.txt`, 4, t1),
				`F:\added.txt`:   watchTestEntry(`F:\added.txt`, 5, t1),
			},
			created:  []string{`F:\added.txt`},
			modified: []string{`F:\changed.txt`},
			deleted:  []string{`F:\removed.txt`},
		},
		{
			name: "empty prev means all created",
			prev: nil,
			next: map[string]watchEntry{
				`F:\z.txt`: watchTestEntry(`F:\z.txt`, 1, t0),
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 2, t0),
			},
			created: []string{`F:\a.txt`, `F:\z.txt`},
		},
		{
			name: "empty next means all deleted",
			prev: map[string]watchEntry{
				`F:\z.txt`: watchTestEntry(`F:\z.txt`, 1, t0),
				`F:\a.txt`: watchTestEntry(`F:\a.txt`, 2, t0),
			},
			next:    nil,
			deleted: []string{`F:\a.txt`, `F:\z.txt`},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gotCreated, gotModified, gotDeleted := diffWatchSnapshots(tc.prev, tc.next)
			if !reflect.DeepEqual(gotCreated, tc.created) {
				t.Errorf("created = %v, want %v", gotCreated, tc.created)
			}
			if !reflect.DeepEqual(gotModified, tc.modified) {
				t.Errorf("modified = %v, want %v", gotModified, tc.modified)
			}
			if !reflect.DeepEqual(gotDeleted, tc.deleted) {
				t.Errorf("deleted = %v, want %v", gotDeleted, tc.deleted)
			}
		})
	}
}

func TestDiffWatchSnapshotsDeterminism(t *testing.T) {
	t0 := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	next := make(map[string]watchEntry)
	for _, p := range []string{`F:\c.txt`, `F:\a.txt`, `F:\b.txt`} {
		next[p] = watchTestEntry(p, 1, t0)
	}
	first, _, _ := diffWatchSnapshots(nil, next)
	for i := 0; i < 50; i++ {
		again, _, _ := diffWatchSnapshots(nil, next)
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("diff not deterministic: %v vs %v", first, again)
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i-1] >= first[i] {
			t.Fatalf("created not sorted: %v", first)
		}
	}
}

func watchDeltaFixture(t *testing.T) *serviceVolumeIndex {
	t.Helper()
	idx := &Index{
		Source:  "usn",
		Volume:  "F:",
		Compact: true,
		Records: []CompactRecord{
			{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)},
			{FRN: 200, ParentFRN: 100, Parent: 0, Name: "existing.txt", Size: 5, ModUnix: 1000},
		},
	}
	buildOrders(idx)
	vol := newServiceVolumeIndex(`F:\state.gsi`, idx)
	return vol
}

func TestServiceWatchDeltaCreatesModifiesDeletes(t *testing.T) {
	vol := watchDeltaFixture(t)
	// Baseline cursor: no changes yet.
	cursors, events, err := serviceWatchDelta([]*serviceVolumeIndex{vol}, []watchVolumeCursor{{Volume: "F:", Seq: 0}}, parsedQuery{}, &searchTrace{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 || len(cursors) != 1 || cursors[0].Seq != 0 {
		t.Fatalf("baseline delta = (%+v, %v)", cursors, events)
	}
	// Create a file.
	vol.applyUSNChanges([]usnChange{{FRN: 301, ParentFRN: 100, USN: 10, Reason: usnReasonFileCreate, Name: "alpha.txt"}})
	cursors, events, err = serviceWatchDelta([]*serviceVolumeIndex{vol}, []watchVolumeCursor{{Volume: "F:", Seq: 0}}, parsedQuery{}, &searchTrace{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cursors) != 1 || cursors[0].Seq != 1 {
		t.Fatalf("after create cursors = %+v, want seq 1", cursors)
	}
	if len(events) != 1 || events[0].Event != "created" || events[0].Path != `F:\alpha.txt` {
		t.Fatalf("after create events = %+v, want created F:\\alpha.txt", events)
	}
	// Delta from cursor 1: nothing new.
	_, events, err = serviceWatchDelta([]*serviceVolumeIndex{vol}, []watchVolumeCursor{{Volume: "F:", Seq: 1}}, parsedQuery{}, &searchTrace{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("delta from cursor 1 = %+v, want none", events)
	}
	// Modify the created file (size change).
	vol.applyUSNChanges([]usnChange{{FRN: 301, ParentFRN: 100, USN: 11, Reason: usnReasonFileCreate, Name: "alpha.txt"}})
	vol.overlay.records[1].Size = 42
	_, events, err = serviceWatchDelta([]*serviceVolumeIndex{vol}, []watchVolumeCursor{{Volume: "F:", Seq: 1}}, parsedQuery{}, &searchTrace{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Event != "modified" || events[0].Path != `F:\alpha.txt` {
		t.Fatalf("after modify events = %+v, want modified F:\\alpha.txt", events)
	}
	// Delete the file.
	vol.applyUSNChanges([]usnChange{{FRN: 301, USN: 12, Reason: usnReasonFileDelete}})
	_, events, err = serviceWatchDelta([]*serviceVolumeIndex{vol}, []watchVolumeCursor{{Volume: "F:", Seq: 2}}, parsedQuery{}, &searchTrace{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Event != "deleted" || events[0].Path != `F:\alpha.txt` {
		t.Fatalf("after delete events = %+v, want deleted F:\\alpha.txt", events)
	}
}

func TestServiceWatchDeltaResetOnCursorBeyondWatermark(t *testing.T) {
	vol := watchDeltaFixture(t)
	cursors, _, err := serviceWatchDelta([]*serviceVolumeIndex{vol}, []watchVolumeCursor{{Volume: "F:", Seq: 9999}}, parsedQuery{}, &searchTrace{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cursors) != 1 || !cursors[0].Reset {
		t.Fatal("cursor beyond watermark did not signal reset")
	}
}

func TestServiceWatchDeltaFiltersByQuery(t *testing.T) {
	vol := watchDeltaFixture(t)
	vol.applyUSNChanges([]usnChange{{FRN: 301, ParentFRN: 100, USN: 10, Reason: usnReasonFileCreate, Name: "alpha.txt"}})
	vol.applyUSNChanges([]usnChange{{FRN: 302, ParentFRN: 100, USN: 11, Reason: usnReasonFileCreate, Name: "beta.log"}})
	pq, err := parseQuery(queryOptions{Query: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	_, events, err := serviceWatchDelta([]*serviceVolumeIndex{vol}, []watchVolumeCursor{{Volume: "F:", Seq: 0}}, pq, &searchTrace{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Event != "created" || events[0].Path != `F:\alpha.txt` {
		t.Fatalf("query-filtered events = %+v, want only alpha.txt", events)
	}
	// ext:txt filter also isolates alpha.
	pq2, err := parseQuery(queryOptions{Query: "ext:txt"})
	if err != nil {
		t.Fatal(err)
	}
	_, events, err = serviceWatchDelta([]*serviceVolumeIndex{vol}, []watchVolumeCursor{{Volume: "F:", Seq: 0}}, pq2, &searchTrace{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Event != "created" || events[0].Path != `F:\alpha.txt` {
		t.Fatalf("ext-filtered events = %+v, want only alpha.txt", events)
	}
}

func TestServiceWatchDeltaRenameEmitsDeletedAndCreated(t *testing.T) {
	vol := watchDeltaFixture(t)
	vol.applyUSNChanges([]usnChange{{FRN: 301, ParentFRN: 100, USN: 10, Reason: usnReasonFileCreate, Name: "oldname.txt"}})
	// Baseline established at cursor 1: oldname.txt exists.
	cursors, _, err := serviceWatchDelta([]*serviceVolumeIndex{vol}, []watchVolumeCursor{{Volume: "F:", Seq: 0}}, parsedQuery{}, &searchTrace{})
	if err != nil {
		t.Fatal(err)
	}
	// Rename after the baseline.
	vol.applyUSNChanges([]usnChange{{FRN: 301, ParentFRN: 100, USN: 11, Reason: usnReasonRenameNew, Name: "newname.txt"}})
	_, events, err := serviceWatchDelta([]*serviceVolumeIndex{vol}, cursors, parsedQuery{}, &searchTrace{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("rename events = %+v, want deleted+created pair", events)
	}
	// Poll-diff emits created, then deleted (both sorted by path); the delta
	// must match that ordering.
	if events[0].Event != "created" || events[0].Path != `F:\newname.txt` ||
		events[1].Event != "deleted" || events[1].Path != `F:\oldname.txt` {
		t.Fatalf("rename events = %+v, want created newname + deleted oldname", events)
	}
}

func TestServiceWatchDeltaCreatedThenDeletedInWindowIsSilent(t *testing.T) {
	vol := watchDeltaFixture(t)
	vol.applyUSNChanges([]usnChange{{FRN: 301, ParentFRN: 100, USN: 10, Reason: usnReasonFileCreate, Name: "alpha.txt"}})
	vol.applyUSNChanges([]usnChange{{FRN: 301, USN: 11, Reason: usnReasonFileDelete}})
	_, events, err := serviceWatchDelta([]*serviceVolumeIndex{vol}, []watchVolumeCursor{{Volume: "F:", Seq: 0}}, parsedQuery{}, &searchTrace{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("created-then-deleted window events = %+v, want none", events)
	}
}

func TestServiceWatchDeltaMultiVolume(t *testing.T) {
	volA := watchDeltaFixture(t)
	idxB := &Index{
		Source:  "usn",
		Volume:  "C:",
		Compact: true,
		Records: []CompactRecord{{FRN: 100, ParentFRN: 100, Parent: -1, Name: ".", Mode: uint32(os.ModeDir)}},
	}
	buildOrders(idxB)
	volB := newServiceVolumeIndex(`C:\state.gsi`, idxB)
	volA.applyUSNChanges([]usnChange{{FRN: 301, ParentFRN: 100, USN: 10, Reason: usnReasonFileCreate, Name: "onA.txt"}})
	volB.applyUSNChanges([]usnChange{{FRN: 401, ParentFRN: 100, USN: 20, Reason: usnReasonFileCreate, Name: "onB.txt"}})
	cursors, events, err := serviceWatchDelta([]*serviceVolumeIndex{volA, volB}, []watchVolumeCursor{{Volume: "F:", Seq: 0}, {Volume: "C:", Seq: 0}}, parsedQuery{}, &searchTrace{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("multi-volume events = %+v, want 2", events)
	}
	byVol := map[string]int{}
	for _, e := range events {
		byVol[e.Volume+`|`+e.Path]++
	}
	if _, ok := byVol["F:"+`|`+`F:\onA.txt`]; !ok {
		t.Fatalf("missing volume A event: %+v", events)
	}
	if _, ok := byVol["C:"+`|`+`C:\onB.txt`]; !ok {
		t.Fatalf("missing volume B event: %+v", events)
	}
	if len(cursors) != 2 {
		t.Fatalf("multi-volume cursors = %+v, want 2", cursors)
	}
}

func TestRunWatchExecSubstitution(t *testing.T) {
	dir := t.TempDir()
	// The event path contains a space: it has to reach the command as a single
	// argument rather than being split on whitespace.
	spaced := filepath.Join(dir, "with space")
	if err := os.MkdirAll(spaced, 0o755); err != nil {
		t.Fatal(err)
	}
	delta := filepath.Join(spaced, "delta1.txt")

	log := filepath.Join(dir, "log.txt")
	script := filepath.Join(dir, "w.ps1")
	if err := os.WriteFile(script, []byte("param($p) Add-Content -Path $env:OUTLOG -Value $p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTLOG", log)
	runWatchExec(`powershell -NoProfile -ExecutionPolicy Bypass -File "`+script+`" {}`, delta, false)

	// PowerShell can take several seconds to start on a loaded CI runner, and
	// the temp dir vanishes as soon as the test returns, so poll generously.
	deadline := time.Now().Add(30 * time.Second)
	var b []byte
	var err error
	for time.Now().Before(deadline) {
		b, err = os.ReadFile(log)
		if err == nil && len(b) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || len(b) == 0 {
		t.Fatalf("exec log not written: %v (content=%q)", err, b)
	}
	if !strings.Contains(string(b), delta) {
		t.Fatalf("exec log = %q, want the whole path %q", b, delta)
	}
}

func TestSplitCommandLine(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`prog a b`, []string{"prog", "a", "b"}},
		{`prog "C:\Program Files\x"`, []string{"prog", `C:\Program Files\x`}},
		{`prog  "a b"   c`, []string{"prog", "a b", "c"}},
		{`prog {}`, []string{"prog", "{}"}},
		{`prog --file={} tail`, []string{"prog", "--file={}", "tail"}},
		{`prog ""`, []string{"prog", ""}},
		{``, nil},
		{`   `, nil},
	}
	for _, tc := range cases {
		if got := splitCommandLine(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitCommandLine(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}
