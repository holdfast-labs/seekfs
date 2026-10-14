package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testSID_A = "S-1-5-21-1111111111-2222222222-3333333333-1001"
	testSID_B = "S-1-5-21-1111111111-2222222222-3333333333-1002"
)

// Per-user paths stay in the service state dir (SEEKFS_DIR-aware) with one
// file per SID; empty SID is the legacy shared file.
func TestSignalStorePathPerSID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEEKFS_DIR", dir)
	resetSharedSignalStoreForTest()
	defer resetSharedSignalStoreForTest()

	legacy := signalStorePath()
	a := signalStorePathForSID(testSID_A)
	b := signalStorePathForSID(testSID_B)
	if a == b {
		t.Fatal("different SIDs must map to different files")
	}
	if filepath.Dir(a) != dir || filepath.Dir(b) != dir {
		t.Fatalf("per-user files must live in the state dir: %q %q", a, b)
	}
	if signalStorePathForSID("") != legacy {
		t.Fatal("empty SID must map to the legacy shared file")
	}
	if strings.ContainsAny(filepath.Base(a), `/\:`) {
		t.Fatalf("per-user file name must be a plain name: %q", a)
	}
	// Malformed identities cannot escape the state dir.
	evil := signalStorePathForSID(`..\..\..\windows\evil`)
	if filepath.Dir(evil) != dir {
		t.Fatalf("evil SID escaped the state dir: %q", evil)
	}
}

// Two SIDs never share history: opens, combos, recents and ranking evidence
// are all per-store.
func TestSignalStoresIsolatedBySID(t *testing.T) {
	t.Setenv("SEEKFS_DIR", t.TempDir())
	resetSharedSignalStoreForTest()
	defer resetSharedSignalStoreForTest()
	signalStoreDisableACLForTest = true
	defer func() { signalStoreDisableACLForTest = false }()

	a := signalStoreForSID(testSID_A)
	b := signalStoreForSID(testSID_B)
	if a == b {
		t.Fatal("different SIDs must get different stores")
	}
	a.recordOpen(`T:\proj\shared.go`, "shared")
	if b.comboCount("shared", `T:\proj\shared.go`) != 0 {
		t.Fatal("B must not see A's query-history combo")
	}
	if b.frecencyScore(`T:\proj\shared.go`, a.now()) != 0 {
		t.Fatal("B must not see A's frecency score")
	}
	if len(b.recent(10)) != 0 {
		t.Fatal("B must not see A's recent rows")
	}
	if len(a.recent(10)) != 1 {
		t.Fatal("A must see its own open")
	}
	// Same object on repeat lookup (cached per SID+path).
	if signalStoreForSID(testSID_A) != a {
		t.Fatal("same SID must return the cached store")
	}
}

// The service open/recent ops use the caller's SID: A and B keep separate
// histories through the pipe dispatch.
func TestSignalServiceOpenRecentPerUser(t *testing.T) {
	t.Setenv("SEEKFS_DIR", t.TempDir())
	resetSharedSignalStoreForTest()
	defer resetSharedSignalStoreForTest()
	signalStoreDisableACLForTest = true
	defer func() { signalStoreDisableACLForTest = false }()

	svc := &goSearchService{}
	open := func(sid, path, query string) serviceResponse {
		var buf bytes.Buffer
		svc.handleServiceCommand(&buf, servicePrincipal{SID: sid},
			localServiceCapabilities(servicePrincipal{SID: sid}),
			&serviceRequest{Command: "open", Path: path, OpenQuery: query})
		var resp serviceResponse
		if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
			t.Fatalf("decode open: %v", err)
		}
		return resp
	}
	recent := func(sid string) serviceResponse {
		var buf bytes.Buffer
		svc.handleServiceCommand(&buf, servicePrincipal{SID: sid},
			localServiceCapabilities(servicePrincipal{SID: sid}),
			&serviceRequest{Command: "recent", Limit: 10})
		var resp serviceResponse
		if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
			t.Fatalf("decode recent: %v", err)
		}
		return resp
	}

	if r := open(testSID_A, `T:\proj\a.go`, "a"); !r.OK {
		t.Fatalf("A open: %s", r.Message)
	}
	if r := open(testSID_B, `T:\proj\b.go`, "b"); !r.OK {
		t.Fatalf("B open: %s", r.Message)
	}
	ra, rb := recent(testSID_A), recent(testSID_B)
	if !ra.OK || len(ra.Recent) != 1 || ra.Recent[0].Path != normalizeSignalPath(`T:\proj\a.go`) {
		t.Fatalf("A recent = %+v", ra)
	}
	if !rb.OK || len(rb.Recent) != 1 || rb.Recent[0].Path != normalizeSignalPath(`T:\proj\b.go`) {
		t.Fatalf("B recent = %+v", rb)
	}
	// Ranking evidence follows the caller too.
	if got := serviceSignalStoreForSID(testSID_A).comboCount("a", `T:\proj\a.go`); got != 1 {
		t.Fatalf("A combo = %d, want 1", got)
	}
	if got := serviceSignalStoreForSID(testSID_B).comboCount("a", `T:\proj\a.go`); got != 0 {
		t.Fatalf("B combo for A's query = %d, want 0", got)
	}
}

// Ranking with per-user stores: the same page reorders for the user with
// history and stays put for the user without.
func TestSignalRankingPerUser(t *testing.T) {
	t.Setenv("SEEKFS_DIR", t.TempDir())
	resetSharedSignalStoreForTest()
	defer resetSharedSignalStoreForTest()
	signalStoreDisableACLForTest = true
	defer func() { signalStoreDisableACLForTest = false }()

	a := serviceSignalStoreForSID(testSID_A)
	b := serviceSignalStoreForSID(testSID_B)
	hit, other := `T:\proj\x\config.yaml`, `T:\proj\y\config.yaml`
	a.recordOpen(hit, "")
	pq := mustParseBlendQuery(t, queryOptions{Query: "config.yaml", Limit: 10})
	pageA := []Entry{blendTestEntry(other, "config.yaml", 0), blendTestEntry(hit, "config.yaml", 0)}
	if got := applySignalReorderWithStore(pageA, pq, a); got[0].Path != hit {
		t.Fatalf("A page must promote its opened file: %v", got)
	}
	pageB := []Entry{blendTestEntry(other, "config.yaml", 0), blendTestEntry(hit, "config.yaml", 0)}
	before := append([]Entry(nil), pageB...)
	if got := applySignalReorderWithStore(pageB, pq, b); got[0] != before[0] || got[1] != before[1] {
		t.Fatalf("B page must be untouched by A's history: %v", got)
	}
	// Empty (ephemeral, no identity) never reorders: parity-safe.
	empty := serviceSignalStoreForSID("")
	pageE := []Entry{blendTestEntry(other, "config.yaml", 0), blendTestEntry(hit, "config.yaml", 0)}
	if got := applySignalReorderWithStore(pageE, pq, empty); got[0].Path != other {
		t.Fatalf("empty store must preserve order: %v", got)
	}
}

// The legacy shared file is claimed by the first user that needs a per-user
// store; later users start empty.
func TestSignalMigrationClaimsLegacy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEEKFS_DIR", dir)
	resetSharedSignalStoreForTest()
	defer resetSharedSignalStoreForTest()
	signalStoreDisableACLForTest = true
	defer func() { signalStoreDisableACLForTest = false }()

	legacy := signalStorePath()
	seed := loadSignalStore(legacy)
	seed.recordOpen(`T:\proj\legacy.go`, "legacy")
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy seed missing: %v", err)
	}
	resetSharedSignalStoreForTest()

	first := signalStoreForSID(testSID_A)
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy must move to the first user, stat err = %v", err)
	}
	if got := first.comboCount("legacy", `T:\proj\legacy.go`); got != 1 {
		t.Fatalf("first user must inherit legacy history, combo = %d", got)
	}
	if _, err := os.Stat(signalStorePathForSID(testSID_A)); err != nil {
		t.Fatalf("per-user file missing: %v", err)
	}

	second := signalStoreForSID(testSID_B)
	if len(second.recent(10)) != 0 {
		t.Fatalf("second user must start empty, got %+v", second.recent(10))
	}
	if got := second.comboCount("legacy", `T:\proj\legacy.go`); got != 0 {
		t.Fatalf("second user must not see legacy history, combo = %d", got)
	}
}

// Per-user files are ACL-restricted to SYSTEM + Administrators + owner.
// The file must exist and stay readable by the owner; on Windows the DACL
// must be protected with exactly those three ACEs. Uses the real process SID
// so the test process keeps access (fake SIDs would lock it out).
func TestSignalStoreACLRestricted(t *testing.T) {
	t.Setenv("SEEKFS_DIR", t.TempDir())
	resetSharedSignalStoreForTest()
	defer resetSharedSignalStoreForTest()

	sid := currentProcessUserSID()
	if sid == "" {
		t.Skip("no process SID on this platform")
	}
	s := signalStoreForSID(sid)
	s.recordOpen(`T:\proj\acl.go`, "")
	path := signalStorePathForSID(sid)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("per-user file missing: %v", err)
	}
	// Owner can still read it back.
	reloaded := loadSignalStore(path)
	if reloaded.frecencyScore(`T:\proj\acl.go`, reloaded.now()) <= 0 {
		t.Fatal("owner must be able to read the restricted file")
	}
	assertSignalStoreACL(t, path, sid)
}
