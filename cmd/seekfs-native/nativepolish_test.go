package main

import (
	"image"
	"testing"

	"gioui.org/layout"
)

// nativepolish_test.go: regressions for the native UI polish pass — preview
// tab/control expansion, stable selected-row x positions, settings header
// subtitle truthfulness, Contents-mode Match visibility, and icon fallbacks.

func TestPreviewSanitizeTabsAndControls(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"\tx", "    x"},
		{"a\tb\tc", "a   b   c"},
		{"abcd\te", "abcd    e"},
		{"a\x01b", "a" + previewControlReplacement + "b"},
		{"ok\r\nnext", "ok\nnext"},
		{"lone\rcr", "lone\ncr"},
	}
	for _, tc := range cases {
		got := sanitizePreviewText(tc.in)
		if got != tc.want {
			t.Fatalf("sanitizePreviewText(%q) = %q, want %q", tc.in, got, tc.want)
		}
		for _, r := range got {
			if r == '\t' || (r < 0x20 && r != '\n') || r == 0x7f {
				t.Fatalf("sanitizePreviewText(%q) left control rune %U in %q", tc.in, r, got)
			}
		}
	}
}

func TestPreviewSanitizeKeepsMatchOffsets(t *testing.T) {
	line := sanitizePreviewText("\t\tneedle")
	if line != "        needle" {
		t.Fatalf("tab expansion = %q, want 8 spaces + needle", line)
	}
	segs := highlightSegments(line, []string{"needle"})
	if len(segs) != 2 || !segs[1].Match || segs[1].Text != "needle" {
		t.Fatalf("segments = %+v, want plain prefix then match", segs)
	}
	if segs[0].Text != "        " {
		t.Fatalf("match offset prefix = %q, want 8 spaces", segs[0].Text)
	}
}

func TestSelectedRowKeepsTextX(t *testing.T) {
	u := testUI(t, false)
	rows := sampleRows(2)
	setRows(u, rows)
	u.mu.Lock()
	rows = append([]ResultRow(nil), u.rows...)
	u.sel = map[int]bool{1: true}
	u.focus = 1
	u.showType = false
	u.showMatch = false
	u.mu.Unlock()
	u.ensureRowState(len(rows))

	img := renderHeadless(t, 600, 64, func(gtx layout.Context) layout.Dimensions {
		u.paintBG(gtx)
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.layoutRow(gtx, rows, 0)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.layoutRow(gtx, rows, 1)
			}),
		)
	})

	firstInk := func(y int) int {
		bg := img.RGBAAt(5, y)
		for x := 4; x < 600; x++ {
			c := img.RGBAAt(x, y)
			dr := int(c.R) - int(bg.R)
			dg := int(c.G) - int(bg.G)
			db := int(c.B) - int(bg.B)
			if dr < 0 {
				dr = -dr
			}
			if dg < 0 {
				dg = -dg
			}
			if db < 0 {
				db = -db
			}
			if dr+dg+db > 24 {
				return x
			}
		}
		return -1
	}
	xPlain := firstInk(16)
	xSelected := firstInk(32 + 16)
	if xPlain < 0 || xSelected < 0 {
		t.Fatalf("no row ink: plain=%d selected=%d", xPlain, xSelected)
	}
	if xPlain != xSelected {
		t.Fatalf("selected row text starts at x=%d, plain row at x=%d", xSelected, xPlain)
	}
}

func TestSettingsHeaderSubtitleOnlyDetailedText(t *testing.T) {
	u, p := settingsTestUI(t, false)
	if got := u.settingsHeaderSubtitle(p); got != "" {
		t.Fatalf("ready volumes subtitle = %q, want empty", got)
	}
	v := &p.model.draft.Volumes[0]
	v.State = "indexing"
	v.Progress = &serviceVolumeProgress{Done: 42, Total: 100}
	if got, want := u.settingsHeaderSubtitle(p), formatIndexingProgress(v.Progress); got != want {
		t.Fatalf("indexing volume subtitle = %q, want %q", got, want)
	}
	v.State = "ready"
	p.load = true
	if got := u.settingsHeaderSubtitle(p); got != "" {
		t.Fatalf("in-flight load subtitle = %q, want empty", got)
	}
	p.load = false
	p.saving = true
	if got := u.settingsHeaderSubtitle(p); got != "" {
		t.Fatalf("generic apply subtitle = %q, want empty", got)
	}
	u.setApplyStep("Installing service…")
	if got := u.settingsHeaderSubtitle(p); got != "Installing service…" {
		t.Fatalf("detailed apply subtitle = %q", got)
	}
}

func TestMatchColumnVisibleForContentSnippets(t *testing.T) {
	u := testUI(t, false)
	u.mu.Lock()
	u.contents = true
	u.colW = defaultColW()
	u.rows = []ResultRow{{Name: "a.go", Path: `C:\a.go`, Snippet: "needle"}}
	u.mu.Unlock()

	// 1100px window minus the 220px sidebar.
	u.fitColumns(880)
	if !u.showMatchNow() {
		t.Fatal("Contents + snippets must show Match at a 1100px window")
	}
	if u.showTypeNow() {
		t.Fatal("Contents mode must hide Type before Match")
	}
	// 1280px window minus the 220px sidebar.
	u.fitColumns(1060)
	if !u.showMatchNow() {
		t.Fatal("Contents + snippets must show Match at a 1280px window")
	}

	u.mu.Lock()
	u.rows[0].Snippet = ""
	u.mu.Unlock()
	u.fitColumns(880)
	if u.showMatchNow() {
		t.Fatal("Match must not take contents priority without snippets")
	}

	u.mu.Lock()
	u.rows[0].Snippet = "needle"
	u.mu.Unlock()
	u.fitColumns(600)
	if u.showMatchNow() {
		t.Fatal("Match must hide when even shrunk columns cannot fit it")
	}
}

func TestFakeBridgeIconIsVisible(t *testing.T) {
	im := newFakeBridge().Icon(`C:\x.txt`, false, 16)
	if im == nil {
		t.Fatal("fake bridge Icon returned nil")
	}
	opaque := 0
	for i := 3; i < len(im.(*image.RGBA).Pix); i += 4 {
		if im.(*image.RGBA).Pix[i] > 16 {
			opaque++
		}
	}
	if opaque == 0 {
		t.Fatal("fake bridge icon is fully transparent")
	}
}

type nilIconBridge struct{ *fakeBridge }

func (nilIconBridge) Icon(string, bool, int) image.Image { return nil }

func TestCellIconFallbackGlyph(t *testing.T) {
	u := testUI(t, false)
	u.shell = nilIconBridge{newFakeBridge()}
	img := renderHeadless(t, 32, 32, func(gtx layout.Context) layout.Dimensions {
		return u.cellIcon(gtx, ResultRow{Path: `C:\missing.ext`, Name: "missing.ext"}, Dp(16))
	})
	ink := 0
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			c := img.RGBAAt(x, y)
			if c.A > 16 {
				ink++
			}
		}
	}
	if ink == 0 {
		t.Fatal("nil shell icon rendered no fallback glyph")
	}
}

func TestEmptyContentPresentation(t *testing.T) {
	unavailable := "Content index unavailable: content indexing unavailable: the content index is not built or still building"
	disabled := "content search is disabled; run seekfs plugin add content to enable it"
	pluginMissing := "content: plugin is not installed; run seekfs plugin add content"
	cases := []struct {
		name    string
		unavail string
		health  *contentState
		kind    emptyContentKind
		title   string
		hint    string
	}{
		{"building with percent", unavailable, &contentState{State: contentStateIndexing, BuildDone: 31, BuildTotal: 100}, emptyContentBuilding, "Content index is building \u2014 31%", "Matches appear as files are indexed"},
		{"building without percent", unavailable, &contentState{State: contentStateIndexing}, emptyContentBuilding, "Content index is building", "Matches appear as files are indexed"},
		{"not built but enabled", unavailable, nil, emptyContentBuilding, "Content index is building", "Matches appear as files are indexed"},
		{"off disabled", disabled, nil, emptyContentOff, "Content search is off", "Turn it on in Settings \u203a Content search"},
		{"plugin missing", pluginMissing, nil, emptyContentOff, "Content search is off", "Turn it on in Settings \u203a Content search"},
		{"real build error", unavailable, &contentState{State: contentStateDegraded, BuildError: "disk full"}, emptyContentError, "Content index error", "disk full"},
		{"no content issue", "", nil, emptyContentNone, "", ""},
	}
	for _, tc := range cases {
		got := emptyContentPresentation(tc.unavail, tc.health)
		if got.Kind != tc.kind || got.Title != tc.title || got.Hint != tc.hint {
			t.Errorf("%s = %+v, want kind=%d title=%q hint=%q", tc.name, got, tc.kind, tc.title, tc.hint)
		}
	}
}
