package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func enrichmentPaths(entries []Entry) []string {
	paths := make([]string, len(entries))
	for i := range entries {
		paths[i] = entries[i].Path
	}
	return paths
}

func TestContentSnippetEnrichmentSharesDeadlineAndKeepsHits(t *testing.T) {
	vol := contentFilterV11Fixture(t, map[string]string{"only.txt": "needle in verified text"})
	prevDefault := contentDefaultBudgetMS
	contentDefaultBudgetMS = 0
	t.Cleanup(func() { contentDefaultBudgetMS = prevDefault })

	full, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "content:needle", Limit: 1, Trace: &searchTrace{}, BudgetDisabled: true,
	}, false)
	if err != nil || len(full) != 1 {
		t.Fatalf("unbounded reference hits=%d err=%v; want one hit", len(full), err)
	}

	previous := contentDiskReadForVerify
	var reads int
	var firstReadStart time.Time
	contentDiskReadForVerify = func(path string, settings contentExtractSettings) ([]byte, bool) {
		reads++
		if reads == 1 {
			firstReadStart = time.Now()
		}
		// Verification completes after the query deadline. Snippet enrichment
		// must consult the same trace and must not start a second disk read.
		time.Sleep(80 * time.Millisecond)
		return previous(path, settings)
	}
	t.Cleanup(func() { contentDiskReadForVerify = previous })

	trace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "content:needle", Limit: 1, Trace: trace, BudgetMS: 50,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != full[0].Path {
		t.Fatalf("timed search hits=%v; want the verified reference hit %q", enrichmentPaths(got), full[0].Path)
	}
	if reads != 1 || !firstReadStart.Before(trace.BudgetDeadline) {
		t.Fatalf("source reads=%d first-start=%v deadline=%v; want one verification read started before deadline and no enrichment read", reads, firstReadStart, trace.BudgetDeadline)
	}
	if got[0].Snippet != "" {
		t.Fatalf("snippet=%q; optional snippet should be omitted after deadline", got[0].Snippet)
	}
	if !trace.BudgetExceeded || !trace.contentIncomplete() || trace.DiskVerified != 1 {
		t.Fatalf("trace exceeded=%v incomplete=%v reads=%d; want incomplete, one counted read", trace.BudgetExceeded, trace.contentIncomplete(), trace.DiskVerified)
	}

	// Optional enrichment denied by an already-expired deadline is tracked
	// separately and cannot turn a complete verified match into an incomplete
	// query result.
	optionalTrace := &searchTrace{BudgetDeadline: time.Now().Add(-time.Millisecond), DiskVerifyBudget: 10}
	optionalTrace.initDiskBudget()
	reads = 0
	entry := full[0]
	if _, ok := vol.contentTextForEntryOptional(&entry, optionalTrace); ok {
		t.Fatal("expired optional snippet unexpectedly returned text")
	}
	optionalTrace.publishDiskBudget()
	if reads != 0 || optionalTrace.ContentIncomplete || optionalTrace.BudgetExceeded || !optionalTrace.optionalEnrichmentSkipped() {
		t.Fatalf("optional skip reads=%d incomplete=%v exceeded=%v skipped=%v; want skipped enrichment only", reads, optionalTrace.ContentIncomplete, optionalTrace.BudgetExceeded, optionalTrace.optionalEnrichmentSkipped())
	}

	// A bounded direct caller that omits Trace must not silently get an
	// unbounded verification/snippet path; the entry point creates its own trace.
	reads = 0
	noTrace, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "content:needle", Limit: 1, BudgetMS: 50,
	}, false)
	if err != nil || len(noTrace) != 1 || noTrace[0].Path != full[0].Path || noTrace[0].Snippet != "" || reads != 1 {
		t.Fatalf("nil-trace bounded search hits=%v snippet=%q reads=%d err=%v; want verified hit, omitted snippet, one read", enrichmentPaths(noTrace), func() string {
			if len(noTrace) == 0 {
				return ""
			}
			return noTrace[0].Snippet
		}(), reads, err)
	}

	reads = 0
	if _, handled, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "content:needle", BudgetMS: 50,
	}); err == nil || !handled || !strings.Contains(err.Error(), "incomplete") || reads != 1 {
		t.Fatalf("nil-trace bounded count handled=%v reads=%d err=%v; want an incomplete count after one read", handled, reads, err)
	}
}

func TestContentSnippetEnrichmentSharesDiskCap(t *testing.T) {
	vol := contentFilterV11Fixture(t, map[string]string{"only.txt": "needle in verified text"})
	previous := contentDiskReadForVerify
	reads := 0
	contentDiskReadForVerify = func(path string, settings contentExtractSettings) ([]byte, bool) {
		reads++
		return previous(path, settings)
	}
	t.Cleanup(func() { contentDiskReadForVerify = previous })

	trace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{
		Query: "content:needle", Limit: 1, Trace: trace,
		ContentDiskVerifyBudget: 1, BudgetDisabled: true,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || filepath.Base(got[0].Path) != "only.txt" {
		t.Fatalf("hits=%v; want the single verified file", enrichmentPaths(got))
	}
	if reads != 1 || trace.DiskVerified != 1 || !trace.DiskCapped || trace.contentIncomplete() {
		t.Fatalf("source reads=%d trace reads=%d capped=%v incomplete=%v; want one verify read and optional-cap skip without match incompleteness", reads, trace.DiskVerified, trace.DiskCapped, trace.contentIncomplete())
	}
	if got[0].Snippet != "" {
		t.Fatalf("snippet=%q; cap-exhausted optional enrichment should be omitted", got[0].Snippet)
	}
}

func TestContentLineEnrichmentStopsAtDeadlineAndCap(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	path := filepath.Join(t.TempDir(), "large.txt")
	body := strings.Repeat("needle padding ", 5000)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	pq, err := parseQuery(queryOptions{Query: "content:needle"})
	if err != nil {
		t.Fatal(err)
	}
	matcher := newContentLeafMatcher(pq)
	positive := contentPositiveLeaves(pq)
	previous := contentLineReadChunk
	reads := 0
	contentLineReadChunk = func(f *os.File, p []byte) (int, error) {
		reads++
		time.Sleep(150 * time.Millisecond)
		return f.Read(p)
	}
	t.Cleanup(func() { contentLineReadChunk = previous })

	trace := &searchTrace{BudgetDeadline: time.Now().Add(100 * time.Millisecond), DiskVerifyBudget: 10}
	trace.initDiskBudget()
	if lines, ok := contentFileLines(path, matcher, positive, contentLineOutput{maxPerFile: 10}, trace); ok || len(lines) != 0 {
		t.Fatalf("lines=%v ok=%v; enrichment should be skipped after its first chunk crossed the deadline", lines, ok)
	}
	trace.publishDiskBudget()
	if reads != 1 || !trace.BudgetExceeded || !trace.contentIncomplete() || trace.DiskVerified != 1 {
		t.Fatalf("chunk reads=%d budget=%v incomplete=%v diskreads=%d; want one charged read then deadline stop", reads, trace.BudgetExceeded, trace.contentIncomplete(), trace.DiskVerified)
	}

	// The per-file cap is shared across repeated line enrichments too: once the
	// sole slot is consumed, a second call is rejected before opening/reading.
	reads = 0
	capTrace := &searchTrace{DiskVerifyBudget: 1}
	capTrace.initDiskBudget()
	_, _ = contentFileLines(path, matcher, positive, contentLineOutput{maxPerFile: 10}, capTrace)
	firstPassChunks := reads
	_, _ = contentFileLines(path, matcher, positive, contentLineOutput{maxPerFile: 10}, capTrace)
	capTrace.publishDiskBudget()
	if firstPassChunks == 0 || reads != firstPassChunks || capTrace.DiskVerified != 1 || !capTrace.DiskCapped {
		t.Fatalf("cap chunks=%d+%d diskreads=%d capped=%v; want second file open/read refused after the one shared slot", firstPassChunks, reads-firstPassChunks, capTrace.DiskVerified, capTrace.DiskCapped)
	}
}

func TestServiceReturnsLineEnrichmentBudgetRemainder(t *testing.T) {
	vol := contentFilterV11Fixture(t, map[string]string{"only.txt": "needle in verified text"})
	svc := &goSearchService{volumes: []*serviceVolumeIndex{vol}, indexes: []*Index{vol.index}}
	resp := callTestServiceCommand(t, svc, serviceRequest{
		Command: "search", Query: "content:needle", Limit: 1, BudgetDisabled: true,
	})
	if !resp.OK || len(resp.Results) != 1 {
		t.Fatalf("service response ok=%v hits=%d message=%q", resp.OK, len(resp.Results), resp.Message)
	}
	if resp.ContentDiskVerifyRemaining == nil {
		t.Fatal("service response omitted the remaining content read cap needed by --lines")
	}
	if resp.ContentBudgetDeadlineUnix != 0 {
		t.Fatalf("explicit --budget 0 response deadline=%d; want no time bound", resp.ContentBudgetDeadlineUnix)
	}
	trace := contentLineBudgetTrace(queryOptions{
		BudgetDisabled:          true,
		lineBudgetMetadata:      true,
		lineDiskVerifyRemaining: resp.ContentDiskVerifyRemaining,
	})
	if !trace.BudgetDeadline.IsZero() || trace.disk == nil || trace.disk.limit != int64(*resp.ContentDiskVerifyRemaining) {
		t.Fatalf("client line budget deadline=%v limit=%v; want disabled time and service remainder %d", trace.BudgetDeadline, trace.disk, *resp.ContentDiskVerifyRemaining)
	}
}

func TestOfflineContentReaderHonorsTimeBudget(t *testing.T) {
	vol := contentFilterV11Fixture(t, map[string]string{
		"a.txt": "needle one",
		"b.txt": "needle two",
	})
	reader, err := openContentReader(vol.content.readerView().idx)
	if err != nil {
		t.Fatal(err)
	}
	previous := contentDiskReadForVerify
	reads := 0
	contentDiskReadForVerify = func(path string, settings contentExtractSettings) ([]byte, bool) {
		reads++
		time.Sleep(40 * time.Millisecond)
		return previous(path, settings)
	}
	t.Cleanup(func() { contentDiskReadForVerify = previous })

	hits, complete := reader.searchWithBudget("needle", 100, 10)
	if complete || len(hits) == 0 || len(hits) >= 2 || reads != 1 {
		t.Fatalf("offline hits=%d complete=%v reads=%d; want visible partial, incomplete, one read", len(hits), complete, reads)
	}
}
