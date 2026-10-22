package main

import "testing"

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
			want: "Content index: building… (1.2M files)",
		},
		{
			name: "unknown total without progress",
			in:   &contentState{State: contentStateReconciling},
			want: "Content index: building…",
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
	if got != "Content index: disk full" {
		t.Fatalf("error label = %q", got)
	}
	if tone != healthToneError {
		t.Fatalf("error tone = %d, want error", tone)
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
