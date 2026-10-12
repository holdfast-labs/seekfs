//go:build seekfs_ui && (production || dev)

package main

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeUIQueryForServicePathFilter(t *testing.T) {
	query, matchPath := normalizeUIQueryForService("path:Downloads ext:png", false)
	if !matchPath {
		t.Fatal("path: did not enable path matching")
	}
	if query != "Downloads ext:png" {
		t.Fatalf("query = %q, want %q", query, "Downloads ext:png")
	}
}

func TestUIServiceArgsUseCurrentPipeAndDBs(t *testing.T) {
	args := uiServiceArgs(`\\.\pipe\seekfs-test`, []string{`C:\idx\c.gsi`, `F:\idx\f.gsi`})
	want := []string{
		"service",
		"-lowmem",
		"-pipe", `\\.\pipe\seekfs-test`,
		"-sddl", defaultServiceSDDL,
		"-db", `C:\idx\c.gsi`,
		"-db", `F:\idx\f.gsi`,
	}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q; args=%v", i, args[i], want[i], args)
		}
	}
}

func TestNormalizeUIQueryForServiceEverythingAliases(t *testing.T) {
	cases := []struct {
		in        string
		wantQuery string
		wantPath  bool
	}{
		{"extension:.go", "ext:go", false},
		{"folder:", "type:dir", false},
		{"file:", "type:file", false},
		{"folder:regex:^src$", "type:dir regex:^src$", false},
		{"sz:>10mb", "size:>10mb", false},
		{"date-modified:today", "dm:today", false},
		{"location:Downloads", "dir:Downloads", true},
		{"name:main.go", "main.go", false},
	}
	for _, tc := range cases {
		gotQuery, gotPath := normalizeUIQueryForService(tc.in, false)
		if gotQuery != tc.wantQuery || gotPath != tc.wantPath {
			t.Fatalf("normalizeUIQueryForService(%q) = (%q, %v), want (%q, %v)", tc.in, gotQuery, gotPath, tc.wantQuery, tc.wantPath)
		}
	}
}

func TestNormalizeUIQueryKeepsDottedSubstringTerms(t *testing.T) {
	cases := []struct {
		in        string
		wantQuery string
		wantPath  bool
	}{
		{".opencode", ".opencode", false},
		{"path:C: .opencode", "C: .opencode", true},
		{"path:Downloads .nrrd", "Downloads .nrrd", true},
	}
	for _, tc := range cases {
		gotQuery, gotPath := normalizeUIQueryForService(tc.in, false)
		if gotQuery != tc.wantQuery || gotPath != tc.wantPath {
			t.Fatalf("normalizeUIQueryForService(%q) = (%q, %v), want (%q, %v)", tc.in, gotQuery, gotPath, tc.wantQuery, tc.wantPath)
		}
	}
}

func TestFrontendDoesNotRewriteDottedSubstringToExtension(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("ui_frontend", "main.js"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ext:${query.slice(1)}") {
		t.Fatal("frontend rewrites single dotted terms to ext:, breaking dotted substring search")
	}
}

func TestIncompleteUIQuerySuppressesTrailingDotAndFieldPrefix(t *testing.T) {
	cases := []string{
		"path:Downloads .",
		"ext:",
		"path:Downloads ext:",
	}
	for _, query := range cases {
		if !incompleteUIQuery(query) {
			t.Fatalf("incompleteUIQuery(%q) = false, want true", query)
		}
	}
	for _, query := range []string{"path:Downloads .nrrd", "path:.nrrd", "Downloads ext:nrrd"} {
		if incompleteUIQuery(query) {
			t.Fatalf("incompleteUIQuery(%q) = true, want false", query)
		}
	}
}

func TestUISearchDoesNotContactServiceForIncompleteQuery(t *testing.T) {
	app := &UIApp{pipeName: `\\.\pipe\seekfs-does-not-exist`, defaultLimit: 200}
	resp := app.search(UISearchRequest{Query: "path:Downloads .", Limit: 200}, 1)
	if !resp.OK || resp.Message != "Keep typing" || len(resp.Results) != 0 {
		t.Fatalf("response = %+v, want immediate keep-typing response", resp)
	}
}

func TestRunNoArgsLaunchesUI(t *testing.T) {
	old := cmdUIRun
	t.Cleanup(func() { cmdUIRun = old })
	var got []string
	cmdUIRun = func(args []string) error {
		got = append([]string(nil), args...)
		return nil
	}
	if err := run(nil); err != nil {
		t.Fatalf("run(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("cmdUI args = %v, want no args", got)
	}
}

func TestRunNoArgsReturnsUIError(t *testing.T) {
	old := cmdUIRun
	t.Cleanup(func() { cmdUIRun = old })
	want := errors.New("ui failed")
	cmdUIRun = func(args []string) error {
		return want
	}
	if err := run(nil); !errors.Is(err, want) {
		t.Fatalf("run(nil) error = %v, want %v", err, want)
	}
}

func TestUIServiceSequenceIsSessionScoped(t *testing.T) {
	app := &UIApp{uiSeqBase: 10_000}
	if got := app.serviceUISeq(1); got != 10_001 {
		t.Fatalf("serviceUISeq(1) = %d, want 10001", got)
	}
	if got := app.serviceUISeq(2); got != 10_002 {
		t.Fatalf("serviceUISeq(2) = %d, want 10002", got)
	}
	if got := app.serviceUISeq(0); got != 0 {
		t.Fatalf("serviceUISeq(0) = %d, want 0 for synchronous calls", got)
	}
}

func TestServiceIdentityMatchesSameDevBuildHash(t *testing.T) {
	expected := testServiceIdentityExpectation(`C:\dev\seekfs.exe`)
	resp := testFreshServiceResponse(expected)
	if !serviceIdentityMatchesExpected(resp, expected) {
		t.Fatalf("serviceIdentityMatchesExpected = false, want true for same dev build hash")
	}
}

func TestServiceIdentityRejectsSamePathDifferentDevBuildHash(t *testing.T) {
	expected := testServiceIdentityExpectation(`C:\dev\seekfs.exe`)
	resp := testFreshServiceResponse(expected)
	resp.ExecutableHash = "old-hash"
	if serviceIdentityMatchesExpected(resp, expected) {
		t.Fatal("serviceIdentityMatchesExpected = true, want false for same path with different dev build hash")
	}
}

func TestServiceIdentityRejectsStaleExecutablePath(t *testing.T) {
	expected := testServiceIdentityExpectation(`C:\dev\seekfs.exe`)
	resp := testFreshServiceResponse(expected)
	resp.Executable = `C:\installed\seekfs.exe`
	if serviceIdentityMatchesExpected(resp, expected) {
		t.Fatal("serviceIdentityMatchesExpected = true, want false for stale executable path")
	}
}

func TestServiceIdentityRejectsMissingIdentityFields(t *testing.T) {
	expected := testServiceIdentityExpectation(`C:\dev\seekfs.exe`)
	resp := testFreshServiceResponse(expected)
	resp.ExecutableHash = ""
	if serviceIdentityMatchesExpected(resp, expected) {
		t.Fatal("missing executable hash was treated as fresh")
	}
	resp = testFreshServiceResponse(expected)
	resp.Date = ""
	if serviceIdentityMatchesExpected(resp, expected) {
		t.Fatal("missing build date was treated as fresh")
	}
	resp = testFreshServiceResponse(expected)
	resp.PipeName = ""
	if serviceIdentityMatchesExpected(resp, expected) {
		t.Fatal("missing pipe name was treated as fresh")
	}
	resp = testFreshServiceResponse(expected)
	resp.PID = 0
	if serviceIdentityMatchesExpected(resp, expected) {
		t.Fatal("missing pid was treated as fresh")
	}
}

func TestServiceIdentityRejectsSCMBinaryMismatch(t *testing.T) {
	expected := testServiceIdentityExpectation(`C:\dev\seekfs.exe`)
	resp := testFreshServiceResponse(expected)
	resp.ProcessMode = "windows-service"
	resp.Executable = `C:\Program Files\seekfs\seekfs.exe`
	resp.ExecutableHash = "installed-hash"
	if serviceIdentityMatchesExpected(resp, expected) {
		t.Fatal("SCM service with mismatched binary was treated as fresh")
	}
}

func TestServiceResponseIsVerifiedStandalone(t *testing.T) {
	if !serviceResponseIsVerifiedStandalone(serviceResponse{ProcessMode: "standalone"}) {
		t.Fatal("standalone service was not verified")
	}
	for _, mode := range []string{"", "windows-service"} {
		if serviceResponseIsVerifiedStandalone(serviceResponse{ProcessMode: mode}) {
			t.Fatalf("process mode %q was treated as verified standalone", mode)
		}
	}
}

func TestServiceInfoResponseIncludesIdentity(t *testing.T) {
	resp := serviceInfoResponseFor(serviceResponse{OK: true}, `\\.\pipe\seekfs-test`, "standalone")
	if resp.PID <= 0 || resp.Executable == "" || resp.ExecutableHash == "" {
		t.Fatalf("service identity missing pid/executable/hash: %+v", resp)
	}
	if resp.Version == "" || resp.Commit == "" || resp.Date == "" || resp.BuildFlavor == "" {
		t.Fatalf("service build identity missing: %+v", resp)
	}
	if resp.PipeName != `\\.\pipe\seekfs-test` || resp.ProcessMode != "standalone" {
		t.Fatalf("service pipe/mode = (%q, %q), want test pipe/standalone", resp.PipeName, resp.ProcessMode)
	}
}

func TestExchangeServiceJSONHalfOpenPipeReturnsError(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		var req serviceRequest
		_ = json.NewDecoder(server).Decode(&req)
	}()
	_, err := exchangeServiceJSON(client, serviceRequest{Command: "info"}, 250*time.Millisecond)
	<-done
	if err == nil {
		t.Fatal("exchangeServiceJSON returned nil error for half-open pipe")
	}
}

func testServiceIdentityExpectation(exe string) serviceIdentityExpectation {
	return serviceIdentityExpectation{
		Executable:     exe,
		ExecutableHash: "current-hash",
		Version:        "dev",
		Commit:         "unknown",
		Date:           "unknown",
		BuildFlavor:    "cli,service,lowmem",
		PipeName:       `\\.\pipe\seekfs-test`,
	}
}

func testFreshServiceResponse(expected serviceIdentityExpectation) serviceResponse {
	return serviceResponse{
		OK:             true,
		PID:            1234,
		Executable:     expected.Executable,
		ExecutableHash: expected.ExecutableHash,
		Version:        expected.Version,
		Commit:         expected.Commit,
		Date:           expected.Date,
		BuildFlavor:    expected.BuildFlavor,
		PipeName:       expected.PipeName,
		ProcessMode:    "standalone",
	}
}

func TestUIEverythingQueriesMatchServiceSearchFixture(t *testing.T) {
	idx := commonSearchFixture()
	idx.Records = append(idx.Records,
		CompactRecord{FRN: 17, ParentFRN: 3, Parent: 2, Name: "Downloads", Mode: uint32(os.ModeDir)},
		CompactRecord{FRN: 18, ParentFRN: 17, Parent: 16, Name: "scan.nrrd"},
		CompactRecord{FRN: 19, ParentFRN: 17, Parent: 16, Name: "notes.txt"},
		CompactRecord{FRN: 20, ParentFRN: 2, Parent: 1, Name: "ai.opencode.desktop", Mode: uint32(os.ModeDir)},
	)
	buildOrders(idx)
	vol := newServiceVolumeIndex("fixture.gsi", idx)
	cases := []struct {
		raw       string
		wantQuery string
		wantPath  bool
		wantNames []string
	}{
		{
			raw:       "path:workspace extension:.go",
			wantQuery: "workspace ext:go",
			wantPath:  true,
			wantNames: []string{"main.go", "search_test.go"},
		},
		{
			raw:       "location:Assets extension:.dat",
			wantQuery: "dir:Assets ext:dat",
			wantPath:  true,
			wantNames: []string{"sample.dat"},
		},
		{
			raw:       "folder:Assets",
			wantQuery: "type:dir Assets",
			wantNames: []string{"Assets"},
		},
		{
			raw:       "file:extension:.go",
			wantQuery: "type:file ext:go",
			wantNames: []string{"main.go", "search_test.go"},
		},
		{
			raw:       "full-path-name:Downstream ext:dat",
			wantQuery: "Downstream ext:dat",
			wantPath:  true,
			wantNames: []string{"sibling.dat"},
		},
		{
			raw:       "path:Downloads .nrrd",
			wantQuery: "Downloads .nrrd",
			wantPath:  true,
			wantNames: []string{"scan.nrrd"},
		},
		{
			raw:       "Downloads nrrd",
			wantQuery: "Downloads nrrd",
			wantPath:  false,
			wantNames: nil,
		},
		{
			raw:       "path:Downloads",
			wantQuery: "Downloads",
			wantPath:  true,
			wantNames: []string{"Downloads", "notes.txt", "scan.nrrd"},
		},
		{
			raw:       ".opencode",
			wantQuery: ".opencode",
			wantNames: []string{"ai.opencode.desktop"},
		},
		{
			raw:       "path:C: .opencode",
			wantQuery: "C: .opencode",
			wantPath:  true,
			wantNames: []string{"ai.opencode.desktop"},
		},
		{
			raw:       "ext:opencode",
			wantQuery: "ext:opencode",
			wantNames: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			query, matchPath := normalizeUIQueryForService(tc.raw, false)
			if query != tc.wantQuery || matchPath != tc.wantPath {
				t.Fatalf("normalizeUIQueryForService(%q) = (%q, %v), want (%q, %v)", tc.raw, query, matchPath, tc.wantQuery, tc.wantPath)
			}
			opts := queryOptions{Query: query, MatchPath: matchPath, Limit: 20}
			got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, opts, false)
			if err != nil {
				t.Fatalf("searchServiceVolumes: %v", err)
			}
			if !sameStringSet(namesOf(got), tc.wantNames) {
				t.Fatalf("names = %v, want %v", namesOf(got), tc.wantNames)
			}
		})
	}
}

func TestUIStrictSpaceSplitDoesNotInferFusedPathExtensions(t *testing.T) {
	cases := []struct {
		raw       string
		wantQuery string
		wantPath  bool
	}{
		{"path:C:.nrrd", "C:.nrrd", true},
		{"path:C:.NRRD", "C:.NRRD", true},
		{"path:.nrrd", "path:.nrrd", true},
		{"path:Downloads.nrrd", "Downloads.nrrd", true},
		{"F: nrrd", "F: nrrd", false},
		{"F: nrrd|raw", "F: nrrd|raw", false},
		{"F: nrrd !raw", "F: nrrd !raw", false},
		{"F: nrrd sort:size", "F: nrrd sort:size", false},
		{"path:F: nrrd", "F: nrrd", true},
		{"pretraining DVT nrrd", "pretraining DVT nrrd", false},
		{"nrrd", "nrrd", false},
		{"ext:nrrd", "ext:nrrd", false},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			gotQuery, gotPath := normalizeUIQueryForService(tc.raw, false)
			if gotQuery != tc.wantQuery || gotPath != tc.wantPath {
				t.Fatalf("normalizeUIQueryForService(%q) = (%q, %v), want (%q, %v)", tc.raw, gotQuery, gotPath, tc.wantQuery, tc.wantPath)
			}
		})
	}
	if gotQuery, gotPath := normalizeUIQueryForService("F: nrrd", true); gotQuery != "F: nrrd" || !gotPath {
		t.Fatalf("explicit UI path mode = (%q, %v), want (F: nrrd, true)", gotQuery, gotPath)
	}
}

func TestServiceIdentityMatchesReleaseBuild(t *testing.T) {
	expected := testServiceIdentityExpectation(filepath.Join(t.TempDir(), "seekfs.exe"))
	expected.Version = "1.2.3"
	expected.Commit = "abc123"
	expected.Date = "2026-07-07T10:00:00Z"
	resp := testFreshServiceResponse(expected)
	if !serviceIdentityMatchesExpected(resp, expected) {
		t.Fatal("matching release identity was rejected")
	}
}

func TestServiceIdentityRejectsUnknownDevBuildWithoutHash(t *testing.T) {
	expected := testServiceIdentityExpectation(filepath.Join(t.TempDir(), "seekfs.exe"))
	resp := testFreshServiceResponse(expected)
	resp.ExecutableHash = ""
	if serviceIdentityMatchesExpected(resp, expected) {
		t.Fatal("unknown dev identity without executable hash was accepted as fresh")
	}
}

func TestServiceIdentityRejectsDateMismatch(t *testing.T) {
	expected := testServiceIdentityExpectation(filepath.Join(t.TempDir(), "seekfs.exe"))
	expected.Version = "1.2.3"
	expected.Commit = "abc123"
	expected.Date = "2026-07-07T10:00:00Z"
	resp := testFreshServiceResponse(expected)
	resp.Date = "2026-07-07T09:00:00Z"
	if serviceIdentityMatchesExpected(resp, expected) {
		t.Fatal("date-mismatched service identity was accepted as fresh")
	}
}

func TestServiceIdentityRejectsMissingExecutable(t *testing.T) {
	expected := testServiceIdentityExpectation(filepath.Join(t.TempDir(), "seekfs.exe"))
	resp := testFreshServiceResponse(expected)
	resp.Executable = ""
	if serviceIdentityMatchesExpected(resp, expected) {
		t.Fatal("service identity without executable was accepted as fresh")
	}
}

func TestServiceIdentityRejectsExecutableMismatch(t *testing.T) {
	dir := t.TempDir()
	expected := testServiceIdentityExpectation(filepath.Join(dir, "seekfs.exe"))
	resp := testFreshServiceResponse(expected)
	resp.Executable = filepath.Join(dir, "old.exe")
	if serviceIdentityMatchesExpected(resp, expected) {
		t.Fatal("service identity with executable mismatch was accepted as fresh")
	}
}

func TestUIResultsPreferServiceRowsOverPathOnlyFallback(t *testing.T) {
	size := int64(42 * 1024)
	resp := serviceResponse{
		Results: []string{`C:\Downloads\scan.nrrd`},
		Rows: []jsonResult{{
			Path:     `C:\Downloads\scan.nrrd`,
			Name:     "scan.nrrd",
			Size:     &size,
			Modified: "2026-06-29T10:09:00Z",
		}},
	}
	got := uiResultsFromServiceResponse(resp)
	if len(got) != 1 {
		t.Fatalf("results = %d, want 1", len(got))
	}
	if got[0].Path != `C:\Downloads\scan.nrrd` || got[0].Name != "scan.nrrd" || got[0].Size != size || !got[0].Exists {
		t.Fatalf("result = %+v, want indexed row metadata", got[0])
	}
	if got[0].Dir != `C:\Downloads` {
		t.Fatalf("dir = %q, want C:\\Downloads", got[0].Dir)
	}
}

func TestEntryToJSONUsesIndexedSizeAndModifiedTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.json")
	mod := time.Date(2026, 6, 29, 13, 10, 47, 0, time.Local)
	row := entryToJSON(Entry{Path: path, Name: "metrics.json", Size: 6, ModUnix: mod.UnixNano()})
	if row.Size == nil || *row.Size != 6 {
		t.Fatalf("size = %v, want indexed size 6", row.Size)
	}
	if row.Modified == "" {
		t.Fatal("indexed modified time was not serialized")
	}
}

func TestPollServiceInfoReadyGivesUpOnDeadPipe(t *testing.T) {
	// Against a pipe with no listener the handshake must fail fast (two quick
	// attempts), not hang: ensureServiceReady spawns a service on failure, and
	// must not be stalled by it.
	app := &UIApp{pipeName: `\\.\pipe\seekfs-test-nonexistent-pipe`}
	start := time.Now()
	_, err := app.pollServiceInfoReady()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error polling a nonexistent pipe")
	}
	if elapsed > 30*time.Second {
		t.Fatalf("dead-pipe poll took %s, want a bounded fast failure", elapsed)
	}
}

func TestIsServiceTimeoutErrorMapping(t *testing.T) {
	if !isServiceTimeoutError(errors.New("service request timed out after 500ms")) {
		t.Fatal("timeout text must classify as a service timeout")
	}
	if isServiceTimeoutError(errors.New(`open \\.\pipe\x: The system cannot find the file specified.`)) {
		t.Fatal("missing-pipe error must not classify as a service timeout")
	}
}

func TestUIResultCarriesContentSnippet(t *testing.T) {
	got := uiResultFromJSON(jsonResult{Path: `C:\src\main.go`, Snippet: "func needle() {}"})
	if got.Snippet != "func needle() {}" || got.Name != "main.go" {
		t.Fatalf("uiResultFromJSON = %+v; want the snippet and name carried", got)
	}
}

func TestUIContentStatusFromHealth(t *testing.T) {
	if uiContentStatusFrom(nil) != nil {
		t.Fatal("no content health must yield no UI content status")
	}
	got := uiContentStatusFrom(&contentHealth{State: contentStateIndexing, BuildDone: 5, BuildTotal: 10, FoldError: "disk full"})
	if got.State != contentStateIndexing || got.BuildDone != 5 || got.BuildTotal != 10 || got.Error != "disk full" {
		t.Fatalf("uiContentStatusFrom = %+v", got)
	}
}
