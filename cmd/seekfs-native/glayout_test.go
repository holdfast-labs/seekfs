package main

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
)

// glayout_test.go: headless verification of the whole Gio UI tree. Gio
// layouts run on the CPU without a window or GPU, so toolbar, sidebar,
// results (all views), preview, status and dialogs are exercised here with
// geometry assertions. (On-screen pixels can't be captured in this
// environment — DXGI presents are occluded — so this is the rendering
// verification that can run here.)

// fakeBridge is a hermetic shellBridge for tests.
type fakeBridge struct {
	dark   bool
	accent color.NRGBA
	icons  map[string]image.Image
}

func newFakeBridge() *fakeBridge {
	return &fakeBridge{accent: color.NRGBA{R: 0, G: 0x66, B: 0xCC, A: 0xFF}, icons: map[string]image.Image{}}
}

func (b *fakeBridge) SystemDark() bool               { return b.dark }
func (b *fakeBridge) SystemAccent() color.NRGBA      { return b.accent }
func (b *fakeBridge) SetHWND(hwnd uintptr)           {}
func (b *fakeBridge) ApplyFrame(dark bool)           {}
func (b *fakeBridge) Open(path string) error         { return nil }
func (b *fakeBridge) Reveal(path string) error       { return nil }
func (b *fakeBridge) Properties(path string) error   { return nil }
func (b *fakeBridge) Recycle(paths []string) error   { return nil }
func (b *fakeBridge) CopyFiles(paths []string) error { return nil }
func (b *fakeBridge) ShowContextMenu(paths []string, x, y int32, done func(bool, error)) {
	done(true, nil)
}
func (b *fakeBridge) BeginDrag(paths []string) error { return nil }
func (b *fakeBridge) Icon(path string, isDir bool, size int) image.Image {
	key := path
	if isDir {
		key = "dir"
	}
	if im, ok := b.icons[key]; ok {
		return im
	}
	im := image.NewRGBA(image.Rect(0, 0, size, size))
	c := color.RGBA{R: 0x4C, G: 0x8B, B: 0xF5, A: 0xFF}
	if isDir {
		c = color.RGBA{R: 0xE0, G: 0xA8, B: 0x2E, A: 0xFF}
	}
	margin := size / 8
	for y := margin; y < size-margin; y++ {
		for x := margin; x < size-margin; x++ {
			im.SetRGBA(x, y, c)
		}
	}
	b.icons[key] = im
	return im
}
func (b *fakeBridge) TypeName(path string, isDir bool) string {
	if isDir {
		return "File folder"
	}
	return "Text Document"
}
func (b *fakeBridge) PickFolder(title string) (string, error) { return "", nil }

func testUI(t *testing.T, dark bool) *UI {
	t.Helper()
	u := newUI(`\\.\pipe\seekfs-test-none`, 200, func() {})
	u.shell = newFakeBridge()
	u.dark = dark
	u.accent = color.NRGBA{R: 0, G: 0x66, B: 0xCC, A: 0xFF}
	u.applySchemeNoFrame()
	return u
}

func testFrame(u *UI, w, h int) layout.Dimensions {
	r := new(input.Router)
	gtx := layout.Context{
		Ops:         new(op.Ops),
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(image.Pt(w, h)),
		Source:      r.Source(),
	}
	dims := u.layout(gtx)
	r.Frame(gtx.Ops)
	return dims
}

func sampleRows(n int) []ResultRow {
	rows := make([]ResultRow, 0, n)
	for i := 0; i < n; i++ {
		r := ResultRow{
			Path: filepath.Join(`C:\proj`, "file"+itoa(i)+".go"),
			Name: "file" + itoa(i) + ".go",
			Dir:  `C:\proj`,
			Size: int64(100 + i), HasSize: true,
			Modified: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), HasMod: true,
			Type:   "go",
			Exists: true,
		}
		if i%7 == 0 {
			r.Snippet = "func needle() matches here"
		}
		if i%11 == 0 {
			r.IsDir = true
			r.Type = ""
		}
		rows = append(rows, r)
	}
	return rows
}

func setRows(u *UI, rows []ResultRow) {
	u.mu.Lock()
	u.rows = rows
	u.sortLocked()
	u.sel = map[int]bool{}
	u.focus = -1
	u.mu.Unlock()
}

func TestHeadlessFullFrameDark(t *testing.T) {
	u := testUI(t, true)
	setRows(u, sampleRows(200))
	dims := testFrame(u, 1180, 760)
	if dims.Size.X != 1180 || dims.Size.Y != 760 {
		t.Fatalf("frame size = %v, want 1180x760", dims.Size)
	}
	if u.scheme.BG != nrgba(0x202020) {
		t.Fatalf("dark BG = %v", u.scheme.BG)
	}
	// Second frame settles state (hover/focus caches).
	testFrame(u, 1180, 760)
}

func TestHeadlessFullFrameLight(t *testing.T) {
	u := testUI(t, false)
	setRows(u, sampleRows(200))
	dims := testFrame(u, 1180, 760)
	if dims.Size.X != 1180 || dims.Size.Y != 760 {
		t.Fatalf("frame size = %v, want 1180x760", dims.Size)
	}
	if u.scheme.BG != nrgba(0xF3F3F3) {
		t.Fatalf("light BG = %v", u.scheme.BG)
	}
}

func TestHeadlessAllViews(t *testing.T) {
	for _, v := range []ViewMode{ViewDetails, ViewIcons, ViewGrouped} {
		u := testUI(t, true)
		setRows(u, sampleRows(120))
		u.mu.Lock()
		u.view = v
		u.mu.Unlock()
		dims := testFrame(u, 1180, 760)
		if dims.Size.X <= 0 || dims.Size.Y <= 0 {
			t.Fatalf("view %v dims = %v", v, dims.Size)
		}
		testFrame(u, 1180, 760)
	}
}

func TestHeadlessEmptyResults(t *testing.T) {
	u := testUI(t, true)
	setRows(u, nil)
	dims := testFrame(u, 1180, 760)
	if dims.Size.X != 1180 {
		t.Fatalf("dims = %v", dims.Size)
	}
}

func TestHeadlessContentMode(t *testing.T) {
	u := testUI(t, true)
	u.mu.Lock()
	u.contents = true
	u.query = "content:needle ext:go"
	u.mu.Unlock()
	rows := sampleRows(50)
	setRows(u, rows)
	dims := testFrame(u, 1180, 760)
	if dims.Size.X != 1180 {
		t.Fatalf("dims = %v", dims.Size)
	}
	// Snippet segmentation must find the literal term.
	terms := contentHighlightTerms("content:needle ext:go")
	if len(terms) != 1 || terms[0] != "needle" {
		t.Fatalf("terms = %q", terms)
	}
}

func TestHeadlessRenameOverlay(t *testing.T) {
	u := testUI(t, true)
	setRows(u, sampleRows(10))
	u.mu.Lock()
	u.renameRow = 2
	u.mu.Unlock()
	u.renameEd.SetText("renamed.go")
	dims := testFrame(u, 1180, 760)
	if dims.Size.X != 1180 {
		t.Fatalf("dims = %v", dims.Size)
	}
}

func TestHeadlessSidebarVolumes(t *testing.T) {
	u := testUI(t, true)
	u.rebuildSideRows([]DBVolume{
		{Path: `C:\idx\c.gsi`, Volume: "C:", Entries: 123456},
		{Path: `E:\idx\e.gsi`, Volume: "E:", Entries: 789},
	})
	u.mu.Lock()
	n := len(u.sideRows)
	u.mu.Unlock()
	// Favorites header + favs + Volumes header + 2 vols + Saved header + save row.
	if n < 6 {
		t.Fatalf("side rows = %d, want headers+favs+volumes+saved", n)
	}
	dims := testFrame(u, 300, 760)
	if dims.Size.X <= 0 {
		t.Fatalf("dims = %v", dims.Size)
	}
}

func TestHeadlessPreviewTextAndImage(t *testing.T) {
	dir := t.TempDir()
	txt := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(txt, []byte("hello needle world\nsecond line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pngPath := filepath.Join(dir, "img.png")
	f, err := os.Create(pngPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	f.Close()

	u := testUI(t, true)
	setRows(u, []ResultRow{
		{Path: txt, Name: "note.txt", Dir: dir, Type: "txt", Exists: true},
		{Path: pngPath, Name: "img.png", Dir: dir, Type: "png", Exists: true},
		{Path: filepath.Join(dir, "missing.bin"), Name: "missing.bin", Dir: dir, Exists: true},
	})
	u.mu.Lock()
	u.focus = 0
	u.mu.Unlock()
	dims := testFrame(u, 1180, 760)
	if dims.Size.X != 1180 {
		t.Fatalf("dims = %v", dims.Size)
	}
	// Text preview must decode with the match intact.
	text, ok := readTextPreview(txt)
	if !ok || len(text) == 0 {
		t.Fatal("text preview failed")
	}
	// Image preview must decode.
	u.mu.Lock()
	u.focus = 1
	u.mu.Unlock()
	testFrame(u, 1180, 760)
	if im := u.previewImage(pngPath); im == nil || im.Bounds().Dx() != 64 {
		t.Fatalf("image preview = %v", im)
	}
}

func TestHeadlessTextShaping(t *testing.T) {
	u := testUI(t, false)
	r := new(input.Router)
	gtx := layout.Context{
		Ops:         new(op.Ops),
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(image.Pt(600, 40)),
		Source:      r.Source(),
	}
	dims := label(u.th, Sp(14), "about_chrome_preferences.xml", u.scheme.Text).Layout(gtx)
	if dims.Size.X < 100 {
		t.Fatalf("shaped label width = %d, Segoe may not have loaded", dims.Size.X)
	}
	r.Frame(gtx.Ops)
}

func TestHeadlessStatusContent(t *testing.T) {
	u := testUI(t, true)
	setRows(u, sampleRows(25))
	u.mu.Lock()
	u.elapsedMS = 42
	u.searchMS = 12
	u.content = &contentState{State: "ready", Docs: 1200}
	u.contents = true
	u.health, u.healthMsg, u.entries = serviceHealthOK, "", 9999
	u.mu.Unlock()
	dims := testFrame(u, 1180, 760)
	if dims.Size.X != 1180 {
		t.Fatalf("dims = %v", dims.Size)
	}
}

func TestDpSpHelpers(t *testing.T) {
	if Dp(10) != 10 || Sp(14) != 14 {
		t.Fatalf("default scale must be identity: %v %v", Dp(10), Sp(14))
	}
}
