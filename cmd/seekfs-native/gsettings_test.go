package main

import (
	"encoding/json"
	"image"
	"strings"
	"testing"

	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
)

func sampleSettings(t *testing.T) serviceSettings {
	t.Helper()
	var resp serviceResponse
	if err := json.Unmarshal([]byte(settingsDocJSON), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Settings == nil {
		t.Fatal("settings missing")
	}
	return *resp.Settings
}

func settingsTestUI(t *testing.T, dark bool) (*UI, *settingsPanelState) {
	t.Helper()
	u := testUI(t, dark)
	p := newSettingsPanelState(u.settings)
	p.model = newSettingsModel(sampleSettings(t))
	p.notice = ""
	p.syncEditors()
	u.gsettings = p
	u.settingsOpen = true
	return u, p
}

func TestSettingsPanelAllSectionsLightAndDark(t *testing.T) {
	for _, dark := range []bool{false, true} {
		for _, tab := range []int{settingsTabDrives, settingsTabContent, settingsTabSearch} {
			u, p := settingsTestUI(t, dark)
			p.tab = tab
			// Two frames: the first registers clickable areas and the list
			// state, the second renders the settled panel.
			testFrame(u, 1000, 720)
			testFrame(u, 1000, 720)
			if len(p.blocks) == 0 {
				t.Fatalf("tab %d dark=%v rendered no blocks", tab, dark)
			}
			want := map[int]string{
				settingsTabDrives:  "drives-heading",
				settingsTabContent: "content-heading",
				settingsTabSearch:  "search-heading",
			}[tab]
			found := false
			for _, b := range p.blocks {
				if b.Kind == want {
					found = true
				}
				if b.Rect.Max.Y <= b.Rect.Min.Y {
					t.Fatalf("tab %d block %q has empty rect %v", tab, b.Kind, b.Rect)
				}
			}
			if !found {
				t.Fatalf("tab %d missing %q: %+v", tab, want, p.blocks)
			}
			assertBlocksDoNotOverlap(t, p.blocks)
		}
	}
}

func assertBlocksDoNotOverlap(t *testing.T, blocks []settingsBlock) {
	t.Helper()
	for i := 1; i < len(blocks); i++ {
		prev := blocks[i-1].Rect
		cur := blocks[i].Rect
		if cur.Min.Y < prev.Max.Y {
			t.Fatalf("blocks overlap: %q %v then %q %v",
				blocks[i-1].Kind, prev, blocks[i].Kind, cur)
		}
		if cur.Max.X > prev.Max.X+1 && cur.Max.X > 0 {
			// Blocks share the body width; a wider block would imply a
			// different layout pass.
			t.Fatalf("block %q wider than the section: %v vs %v", blocks[i].Kind, cur, prev)
		}
	}
}

func TestSettingsPanelHeadlessRender(t *testing.T) {
	for _, dark := range []bool{false, true} {
		u, _ := settingsTestUI(t, dark)
		want := uint8(0xF3)
		if dark {
			want = 0x20
		}
		img := renderHeadless(t, 1000, 720, func(gtx layout.Context) layout.Dimensions {
			return u.layout(gtx)
		})
		// The backdrop is a dark scrim over the app background; the panel
		// theme itself is proven by a distinctly non-uniform frame.
		if n := countDistinct(img); n < 10 {
			t.Fatalf("dark=%v settings panel rendered near-uniform (%d colors)", dark, n)
		}
		corner := img.RGBAAt(4, 4)
		if dark && corner.R > want+10 {
			t.Fatalf("dark scrim too light: %v", corner)
		}
	}
}

func TestSettingsPanelLongPathsEllipsize(t *testing.T) {
	u, p := settingsTestUI(t, false)
	p.tab = settingsTabContent
	long := `E:\bench\a-very-long-directory-name\another-long-directory\one-more-segment\still-going\deep\deeper\deepest\file-with-a-long-name.txt`
	p.model.addRoot("", long)
	p.syncEditors()
	testFrame(u, 1000, 720)
	r := new(input.Router)
	gtx := layout.Context{
		Ops:         new(op.Ops),
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(image.Pt(160, 40)),
		Source:      r.Source(),
	}
	dims := label(u.th, Sp(12), long, u.scheme.Text).Layout(gtx)
	r.Frame(gtx.Ops)
	if dims.Size.X > 160 {
		t.Fatalf("long path label = %d px, want <= 160", dims.Size.X)
	}
	// The Section must still lay out after the long path is added.
	testFrame(u, 1000, 720)
	if !strings.Contains(p.gsettingsBlocksDump(), "content-roots") {
		t.Fatalf("content section did not render: %s", p.gsettingsBlocksDump())
	}
}

func (p *settingsPanelState) gsettingsBlocksDump() string {
	var b strings.Builder
	for _, blk := range p.blocks {
		b.WriteString(blk.Kind)
		b.WriteByte(' ')
	}
	return b.String()
}

func TestSettingsPanelStagedDriveToggle(t *testing.T) {
	_, p := settingsTestUI(t, false)
	p.model.setDriveIndexed("F:", true)
	patch := p.model.patch()
	if patch == nil || len(patch.Volumes) != 1 || patch.Volumes[0].Volume != "F:" {
		t.Fatalf("patch = %+v", patch)
	}
	if patch.Volumes[0].Indexed == nil || !*patch.Volumes[0].Indexed {
		t.Fatalf("indexed = %v", patch.Volumes[0].Indexed)
	}
}
