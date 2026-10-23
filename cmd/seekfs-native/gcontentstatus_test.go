package main

import (
	"strings"
	"testing"
)

func TestContentIndexBuildingLabel(t *testing.T) {
	cases := []struct {
		name string
		in   *contentState
		want string
	}{
		{
			name: "known total",
			in:   &contentState{State: contentStateIndexing, BuildDone: 1_185_000, BuildTotal: 1_500_000},
			want: "Content index: building 79% (1.2M of 1.5M files)",
		},
		{
			name: "unknown total with progress",
			in:   &contentState{State: contentStateIndexing, BuildDone: 1_200_000},
			want: "Content index: building\u2026 (1.2M files)",
		},
		{
			name: "unknown total without progress",
			in:   &contentState{State: contentStateReconciling},
			want: "Content index: building\u2026",
		},
		{
			name: "ready",
			in:   &contentState{State: contentStateReady, Docs: 1200},
			want: "",
		},
		{
			name: "nil",
			in:   nil,
			want: "",
		},
	}
	for _, tc := range cases {
		if got, tone := contentIndexStatusText(tc.in); got != tc.want {
			t.Fatalf("%s label = %q, want %q", tc.name, got, tc.want)
		} else if tone != healthToneMuted {
			t.Fatalf("%s tone = %d, want muted", tc.name, tone)
		}
	}
}

func TestContentIndexBuildErrorTone(t *testing.T) {
	got, tone := contentIndexStatusText(&contentState{State: contentStateDegraded, BuildError: "disk full"})
	if got != "Content index failed: disk full" {
		t.Fatalf("error label = %q", got)
	}
	if tone != healthToneError {
		t.Fatalf("error tone = %d, want error", tone)
	}
}

func TestContentDegradedReasons(t *testing.T) {
	cases := []struct {
		name string
		in   *contentState
		want string
	}{
		{"empty reason", &contentState{State: contentStateDegraded}, ""},
		{"scope reconcile", &contentState{State: contentStateDegraded, ScopeReconcile: true}, "Content index: updating changes\u2026"},
		{"scope pending", &contentState{State: contentStateDegraded, ScopeReconcilePending: 4}, "Content index: updating changes\u2026"},
		{"extractor refresh", &contentState{State: contentStateDegraded, ExtractorRefresh: true}, "Content index: updating file types\u2026"},
		{"stale", &contentState{State: contentStateDegraded, Stale: true}, "Content index: updating changes\u2026"},
		{"budget partial", &contentState{State: contentStateDegraded, Incomplete: true, ScopeDropped: 3, BudgetBytes: 1 << 20}, "Content index: partial \u2014 size budget reached"},
		{"reserve partial", &contentState{State: contentStateDegraded, Incomplete: true, ScopeDropped: 3, ReserveBytes: 10 << 30}, "Content index: partial \u2014 disk reserve reached"},
		{"partial generic", &contentState{State: contentStateDegraded, Partial: true}, "Content index: partial results"},
		{"sidecar cap", &contentState{State: contentStateDegraded, SidecarCap: 10, SidecarBytes: 10}, "Content index: index size cap reached"},
	}
	for _, tc := range cases {
		got, _ := contentIndexStatusText(tc.in)
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestStatusContentOff(t *testing.T) {
	text, tone := statusContentText(false, nil, nil, "")
	if text != "" || tone != healthToneMuted {
		t.Fatalf("Contents off text = %q tone=%d", text, tone)
	}
}

func TestStatusBarSingleMessageWhileBuilding(t *testing.T) {
	health := &contentState{State: contentStateIndexing, BuildDone: 42, BuildTotal: 100}
	text, tone := statusContentText(true, nil, health,
		"Content index unavailable: content indexing unavailable: the content index is not built or still building")
	if strings.Contains(strings.ToLower(text), "unavailable") || strings.Contains(text, "not built") {
		t.Fatalf("status text repeats the raw unavailable error: %q", text)
	}
	if text != "Content index: building 42% (42 of 100 files)" || tone != healthToneMuted {
		t.Fatalf("status text = %q tone=%d", text, tone)
	}
	if notice := contentIncompleteNotice(true, health); notice == "" {
		t.Fatal("results notice must still explain the building state")
	}
	// Without a health snapshot, the raw building refusal must still stay out.
	text, _ = statusContentText(true, nil, nil,
		"Content index unavailable: content indexing unavailable: the content index is not built or still building")
	if text != "" {
		t.Fatalf("status text without health = %q, want empty", text)
	}
}

func TestStatusBarRealErrorIsPlainAndBounded(t *testing.T) {
	long := "content indexing unavailable: " + strings.Repeat("x", 300)
	text, tone := statusContentText(true, nil, &contentState{State: contentStateDegraded, BuildError: long}, "")
	if !strings.HasSuffix(text, "\u2026") || len([]rune(text)) > 120 {
		t.Fatalf("long error not truncated sensibly: %q", text)
	}
	if tone != healthToneError {
		t.Fatalf("tone = %d, want error", tone)
	}
	if strings.Contains(text, "content indexing unavailable: content indexing unavailable") {
		t.Fatalf("raw duplicated error: %q", text)
	}
}

func TestTruncateStatusText(t *testing.T) {
	if got := truncateStatusText("short", 10); got != "short" {
		t.Fatalf("short = %q", got)
	}
	got := truncateStatusText(strings.Repeat("a", 20), 10)
	if len([]rune(got)) != 10 || !strings.HasSuffix(got, "\u2026") {
		t.Fatalf("truncated = %q", got)
	}
	if got := truncateStatusText("x", 1); got != "x" {
		t.Fatalf("one rune fits = %q", got)
	}
	if got := truncateStatusText("xy", 1); got != "\u2026" {
		t.Fatalf("one rune limit = %q", got)
	}
	if got := truncateStatusText("abc", 0); got != "" {
		t.Fatalf("zero = %q", got)
	}
}

func TestContentIncompleteNotice(t *testing.T) {
	for _, c := range []*contentState{
		{State: contentStateIndexing},
		{State: contentStateReady, Incomplete: true},
		{State: contentStateReady, Partial: true},
	} {
		if got := contentIncompleteNotice(true, c); got != contentIncompleteNoticeText {
			t.Fatalf("notice = %q, want %q", got, contentIncompleteNoticeText)
		}
	}
	if got := contentIncompleteNotice(false, &contentState{Incomplete: true}); got != "" {
		t.Fatalf("notice with Contents off = %q", got)
	}
	if got := contentIncompleteNotice(true, &contentState{State: contentStateReady}); got != "" {
		t.Fatalf("notice for a complete index = %q", got)
	}
}
