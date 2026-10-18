package main

import (
	"encoding/json"
	"flag"
	"strings"
	"testing"
	"time"
)

// TestResolveContentBudget covers the CLI parse/validation that keeps an
// omitted --budget (no implicit time bound), an explicit 0 (disable the time
// bound), and a positive bound distinct; it rejects negative and sub-millisecond durations
// that would otherwise silently truncate to 0 and be mistaken for "no bound".
func TestResolveContentBudget(t *testing.T) {
	cases := []struct {
		name     string
		d        time.Duration
		provided bool
		wantMS   int
		wantOff  bool
		wantErr  bool
	}{
		{name: "omitted keeps default", d: 0, provided: false, wantMS: 0, wantOff: false},
		{name: "explicit zero disables", d: 0, provided: true, wantMS: 0, wantOff: true},
		{name: "positive honored", d: 150 * time.Millisecond, provided: true, wantMS: 150, wantOff: false},
		{name: "one ms honored", d: time.Millisecond, provided: true, wantMS: 1, wantOff: false},
		{name: "negative rejected", d: -time.Second, provided: true, wantErr: true},
		{name: "sub-ms rejected", d: 500 * time.Microsecond, provided: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms, off, err := resolveContentBudget(tc.d, tc.provided)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got ms=%d disabled=%v", ms, off)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ms != tc.wantMS || off != tc.wantOff {
				t.Fatalf("= (%d, %v); want (%d, %v)", ms, off, tc.wantMS, tc.wantOff)
			}
		})
	}
}

func TestContentBudgetFlagSetDistinguishesOmittedAndZero(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantMS  int
		wantOff bool
		wantErr string
	}{
		{name: "omitted", args: []string{"needle"}},
		{name: "explicit zero", args: []string{"--budget", "0", "needle"}, wantOff: true},
		{name: "explicit zero equals", args: []string{"--budget=0", "needle"}, wantOff: true},
		{name: "positive", args: []string{"--budget", "150ms", "needle"}, wantMS: 150},
		{name: "negative", args: []string{"--budget", "-1s", "needle"}, wantErr: "must be >= 0"},
		{name: "sub-ms", args: []string{"--budget", "500us", "needle"}, wantErr: "at least 1ms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("search", flag.ContinueOnError)
			budget := fs.Duration("budget", 0, "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			ms, disabled, err := contentBudgetFromFlagSet(fs, *budget)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error=%v; want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || ms != tc.wantMS || disabled != tc.wantOff {
				t.Fatalf("selection=(%d,%v,%v); want (%d,%v,nil)", ms, disabled, err, tc.wantMS, tc.wantOff)
			}
		})
	}
}

// TestContentBudgetWireRoundtrip proves BudgetDisabled survives the CLI ->
// options -> serviceRequest JSON roundtrip, so a resident service honors an
// explicit --budget 0 as "no time bound" instead of applying the default. An
// omitted flag must not serialize the marker, so old clients keep the default.
func TestContentBudgetWireRoundtrip(t *testing.T) {
	opts := queryOptions{Query: "content:needle", Limit: 10, BudgetDisabled: true}
	req := serviceRequestFromOptions(opts, false)
	if !req.BudgetDisabled {
		t.Fatal("BudgetDisabled lost in serviceRequestFromOptions")
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"budget_disabled":true`) {
		t.Fatalf("wire JSON missing budget_disabled: %s", raw)
	}
	var back serviceRequest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !back.BudgetDisabled {
		t.Fatal("BudgetDisabled lost in serviceRequest JSON roundtrip")
	}
	if got := requestToOptionsFromService(back); !got.BudgetDisabled {
		t.Fatal("BudgetDisabled lost in requestToOptionsFromService")
	}

	plain, err := json.Marshal(serviceRequestFromOptions(queryOptions{Query: "content:needle"}, false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "budget_disabled") {
		t.Fatalf("omitted budget serialized a disable marker: %s", plain)
	}
}

func TestServiceRejectsInvalidBudgetFields(t *testing.T) {
	svc := &goSearchService{}
	for _, req := range []serviceRequest{
		{Command: "search", Query: "content:needle", BudgetMS: -1},
		{Command: "search", Query: "content:needle", BudgetMS: 1, BudgetDisabled: true},
	} {
		resp := callTestServiceCommand(t, svc, req)
		if resp.OK || !strings.Contains(resp.Message, "invalid content budget fields") {
			t.Fatalf("request budget=%d disabled=%v response=(ok=%v,message=%q); want invalid-budget rejection", req.BudgetMS, req.BudgetDisabled, resp.OK, resp.Message)
		}
	}
}

// TestContentBudgetFlagValidation proves cmdSearch (the real CLI entry, which
// detects whether --budget was provided) rejects a negative or sub-millisecond
// --budget before any query runs, rather than silently truncating it to 0 and
// changing the caller's selected time policy.
func TestContentBudgetFlagValidation(t *testing.T) {
	if err := cmdSearch([]string{"--budget", "-1s", "needle"}, false); err == nil || !strings.Contains(err.Error(), "--budget must be >= 0") {
		t.Fatalf("negative --budget err = %v; want a range error", err)
	}
	if err := cmdSearch([]string{"--budget", "500us", "needle"}, false); err == nil || !strings.Contains(err.Error(), "at least 1ms") {
		t.Fatalf("sub-ms --budget err = %v; want a minimum error", err)
	}
	if err := cmdContent([]string{"--budget", "-1s"}); err == nil || !strings.Contains(err.Error(), "--budget must be >= 0") {
		t.Fatalf("offline negative --budget err = %v; want the same range error", err)
	}
	if err := cmdContent([]string{"--budget", "500us"}); err == nil || !strings.Contains(err.Error(), "at least 1ms") {
		t.Fatalf("offline sub-ms --budget err = %v; want the same minimum error", err)
	}
}

// TestContentDeadlineRequiresExplicitBudget pins the restored policy: omitted
// and explicit-zero --budget leave time unbounded; a positive explicit value
// arms the deadline. All three choices are content-only, so filename queries
// never arm one.
func TestContentDeadlineRequiresExplicitBudget(t *testing.T) {
	vol := contentFilterV11Fixture(t, matchingBodies(20))
	prevMS, prevDisk := contentDefaultBudgetMS, contentDiskVerifyBudget
	contentDefaultBudgetMS = 0
	contentDiskVerifyBudget = 512
	t.Cleanup(func() {
		contentDefaultBudgetMS = prevMS
		contentDiskVerifyBudget = prevDisk
	})

	// Omitted --budget retains the historical unbounded-time default and 512
	// exact disk-read safety valve.
	defaultTrace := &searchTrace{}
	if _, err := searchServiceVolumes([]*serviceVolumeIndex{vol},
		queryOptions{Query: "content:needle", Limit: 10, Trace: defaultTrace}, false); err != nil {
		t.Fatal(err)
	}
	if !defaultTrace.BudgetDeadline.IsZero() {
		t.Fatal("omitted --budget unexpectedly armed an implicit deadline")
	}
	if defaultTrace.DiskVerifyBudget != 512 {
		t.Fatalf("omitted --content-verify-budget default=%d; want historical 512", defaultTrace.DiskVerifyBudget)
	}

	// Explicit positive --budget wins over the default.
	explicitTrace := &searchTrace{}
	if _, err := searchServiceVolumes([]*serviceVolumeIndex{vol},
		queryOptions{Query: "content:needle", Limit: 10, Trace: explicitTrace, BudgetMS: 12345}, false); err != nil {
		t.Fatal(err)
	}
	if delay := time.Until(explicitTrace.BudgetDeadline); delay < 12*time.Second || delay > 13*time.Second {
		t.Fatalf("explicit --budget not honored: offset %v; want ~12345ms", delay)
	}

	// Explicit --budget 0 remains distinct on the wire, but also leaves time
	// unbounded under this restored default. Disk accounting remains exact.
	offTrace := &searchTrace{}
	all, err := searchServiceVolumes([]*serviceVolumeIndex{vol},
		queryOptions{Query: "content:needle", Limit: 100, Trace: offTrace, BudgetDisabled: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !offTrace.BudgetDeadline.IsZero() {
		t.Fatal("--budget 0 still armed a deadline")
	}
	if offTrace.ContentIncomplete || len(all) != 20 {
		t.Fatalf("--budget 0: hits=%d incomplete=%v; want 20, false", len(all), offTrace.ContentIncomplete)
	}
	contentCountTrace := &searchTrace{}
	count, handled, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "content:needle", Trace: contentCountTrace, BudgetDisabled: true,
	})
	if err != nil || !handled || count != len(all) || !contentCountTrace.BudgetDeadline.IsZero() {
		t.Fatalf("--budget 0 count=(%d, handled=%v, err=%v) deadline=%v; want %d, unarmed", count, handled, err, contentCountTrace.BudgetDeadline, len(all))
	}

	// A name-only query (no content leaf) must never arm the content deadline,
	// for either search or count: the deadline is content-only state.
	nameTrace := &searchTrace{}
	if _, err := searchServiceVolumes([]*serviceVolumeIndex{vol},
		queryOptions{Query: "f0", Limit: 10, Trace: nameTrace}, false); err != nil {
		t.Fatal(err)
	}
	if !nameTrace.BudgetDeadline.IsZero() {
		t.Fatal("name-only search armed a content deadline")
	}
	countTrace := &searchTrace{}
	if _, _, err := countServiceVolumes([]*serviceVolumeIndex{vol},
		queryOptions{Query: "f0", Trace: countTrace}); err != nil {
		t.Fatal(err)
	}
	if !countTrace.BudgetDeadline.IsZero() {
		t.Fatal("name-only count armed a content deadline")
	}
}

// TestContentTruncatedPageResumesWithoutGaps proves a disk-cap-truncated page
// remains truthful: completed full pages have output-offset cursors, while a
// partial terminal page does not promise more and matches form a gap-free prefix.
func TestContentTruncatedPageResumesWithoutGaps(t *testing.T) {
	const files = 400
	const valve = 150
	vol := contentFilterV11Fixture(t, matchingBodies(files))
	prevMS, prevValve := contentDefaultBudgetMS, contentDiskVerifyBudget
	contentDefaultBudgetMS = 0
	contentDiskVerifyBudget = valve
	pinContentDiskVerifyPageScale(t, 1)
	t.Cleanup(func() {
		contentDefaultBudgetMS = prevMS
		contentDiskVerifyBudget = prevValve
	})

	fullTrace := &searchTrace{}
	full, err := searchServiceVolumes([]*serviceVolumeIndex{vol},
		queryOptions{Query: "content:needle", Limit: files, Trace: fullTrace, ContentDiskVerifyBudget: 2 * files}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != files {
		t.Fatalf("uncapped hits=%d; want %d", len(full), files)
	}

	s := &goSearchService{volumes: []*serviceVolumeIndex{vol}, indexes: []*Index{vol.index}}
	page1 := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 100})
	if !page1.OK || len(page1.Results) != 100 || page1.NextCursor == "" {
		t.Fatalf("page1 = %+v; want 100 hits and a next cursor", page1)
	}
	page2 := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 100, Cursor: page1.NextCursor})
	if !page2.OK || len(page2.Results) != valve-100 {
		t.Fatalf("page2 hits=%d; want the %d remaining rows before the valve", len(page2.Results), valve-100)
	}
	if page2.NextCursor != "" {
		t.Fatalf("page2 advertised a cursor %q; the truncated tail must not promise more", page2.NextCursor)
	}
	if page2.Complete != nil && *page2.Complete {
		t.Fatalf("page2 complete=%v; a valve-truncated tail must not report complete", *page2.Complete)
	}

	combined := append(append([]string{}, page1.Results...), page2.Results...)
	if len(combined) != valve {
		t.Fatalf("combined rows=%d; want %d", len(combined), valve)
	}
	seen := map[string]bool{}
	for i, path := range combined {
		if seen[path] {
			t.Fatalf("duplicate row %q across pages at rank %d", path, i)
		}
		seen[path] = true
		if path != full[i].Path {
			t.Fatalf("resume rank %d = %q; want %q (gap-free prefix)", i, path, full[i].Path)
		}
	}
}

// TestContentDiskVerifyBudgetScalesWithPage pins the page-sized default valve:
// pages up to one unit keep the plain valve, larger pages get one valve per
// started unit up to the scale max, and an explicit override is never scaled.
func TestContentDiskVerifyBudgetScalesWithPage(t *testing.T) {
	prev, prevUnit, prevScale := contentDiskVerifyBudget, contentDiskVerifyPageUnit, contentDiskVerifyPageScaleMax
	contentDiskVerifyBudget, contentDiskVerifyPageUnit, contentDiskVerifyPageScaleMax = 512, 32, 16
	t.Cleanup(func() {
		contentDiskVerifyBudget, contentDiskVerifyPageUnit, contentDiskVerifyPageScaleMax = prev, prevUnit, prevScale
	})
	cases := []struct{ limit, override, want int }{
		{0, 0, 512},
		{20, 0, 512},
		{32, 0, 512},
		{33, 0, 1024},
		{100, 0, 2048},
		{520, 0, 8192},
		{100000, 0, 8192},
		{520, 64, 64},
		{20, 4096, 4096},
	}
	for _, c := range cases {
		pq := parsedQuery{Limit: c.limit, ContentDiskVerifyBudget: c.override}
		if got := contentDiskVerifyBudgetOf(pq); got != c.want {
			t.Errorf("limit=%d override=%d: budget %d, want %d", c.limit, c.override, got, c.want)
		}
	}
}

// pinContentDiskVerifyPageScale pins the page-sized valve scale for a test that
// asserts the plain contentDiskVerifyBudget at a page larger than one unit.
func pinContentDiskVerifyPageScale(t *testing.T, scale int) {
	t.Helper()
	prev := contentDiskVerifyPageScaleMax
	contentDiskVerifyPageScaleMax = scale
	t.Cleanup(func() { contentDiskVerifyPageScaleMax = prev })
}
