package main

import (
	"image"
	"image/color"
	"testing"

	"gioui.org/gpu/headless"
	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
)

func headlessShot(t *testing.T, ops *op.Ops, w, h int) *image.RGBA {
	t.Helper()
	hw, err := headless.NewWindow(w, h)
	if err != nil {
		t.Skipf("headless unavailable: %v", err)
	}
	defer hw.Release()
	if err := hw.Frame(ops); err != nil {
		t.Fatalf("frame: %v", err)
	}
	img := image.NewRGBA(image.Rectangle{Max: image.Point{X: w, Y: h}})
	if err := hw.Screenshot(img); err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	return img
}

func countColors(img *image.RGBA) (int, color.RGBA, color.RGBA) {
	seen := map[color.RGBA]int{}
	var bg color.RGBA
	best := 0
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			c := img.RGBAAt(x, y)
			seen[c]++
			if seen[c] > best {
				best, bg = seen[c], c
			}
		}
	}
	return len(seen), bg, img.RGBAAt(150, 150)
}

func TestHeadlessHandOps(t *testing.T) {
	var ops op.Ops
	red := color.NRGBA{R: 0xFF, A: 0xFF}
	paint.ColorOp{Color: red}.Add(&ops)
	paint.FillShape(&ops, red, clip.Rect(image.Rect(0, 0, 700, 500)).Op())
	img := headlessShot(t, &ops, 800, 600)
	n, bg, mid := countColors(img)
	t.Logf("handops colors=%d bg=%v mid=%v", n, bg, mid)
	if n < 1 {
		t.Fatal("no colors")
	}
}

func TestHeadlessRealLayout(t *testing.T) {
	u := testUI(t, true)
	setRows(u, sampleRows(60))
	r := new(input.Router)
	gtx := layout.Context{
		Ops:         new(op.Ops),
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(image.Pt(1280, 800)),
		Source:      r.Source(),
	}
	dims := u.layout(gtx)
	r.Frame(gtx.Ops)
	t.Logf("layout dims=%v", dims.Size)
	hw, err := headless.NewWindow(1280, 800)
	if err != nil {
		t.Skipf("headless unavailable: %v", err)
	}
	defer hw.Release()
	if err := hw.Frame(gtx.Ops); err != nil {
		t.Fatalf("frame: %v", err)
	}
	img := image.NewRGBA(image.Rectangle{Max: image.Point{X: 1280, Y: 800}})
	if err := hw.Screenshot(img); err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	seen := map[color.RGBA]int{}
	for y := 0; y < 800; y += 8 {
		for x := 0; x < 1280; x += 8 {
			seen[img.RGBAAt(x, y)]++
		}
	}
	t.Logf("layout colors=%d", len(seen))
	if len(seen) < 5 {
		for c, n := range seen {
			t.Logf("  %v x%d", c, n)
		}
		t.Fatal("layout rendered near-uniform; want structured UI")
	}
}

func renderHeadless(t *testing.T, w, h int, layoutFn func(gtx layout.Context) layout.Dimensions) *image.RGBA {
	t.Helper()
	r := new(input.Router)
	gtx := layout.Context{
		Ops:         new(op.Ops),
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(image.Pt(w, h)),
		Source:      r.Source(),
	}
	layoutFn(gtx)
	r.Frame(gtx.Ops)
	hw, err := headless.NewWindow(w, h)
	if err != nil {
		t.Skipf("headless unavailable: %v", err)
	}
	defer hw.Release()
	if err := hw.Frame(gtx.Ops); err != nil {
		t.Fatalf("frame: %v", err)
	}
	img := image.NewRGBA(image.Rectangle{Max: image.Point{X: w, Y: h}})
	if err := hw.Screenshot(img); err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	return img
}

func countDistinct(img *image.RGBA) int {
	seen := map[color.RGBA]bool{}
	for y := 0; y < img.Bounds().Dy(); y += 8 {
		for x := 0; x < img.Bounds().Dx(); x += 8 {
			seen[img.RGBAAt(x, y)] = true
		}
	}
	return len(seen)
}

func TestHeadlessBisectBG(t *testing.T) {
	u := testUI(t, true)
	img := renderHeadless(t, 1280, 800, func(gtx layout.Context) layout.Dimensions {
		u.paintBG(gtx)
		return layout.Dimensions{Size: image.Pt(1280, 800)}
	})
	got := img.RGBAAt(640, 400)
	t.Logf("bg-only pixel=%v distinct=%d", got, countDistinct(img))
	if got.R != 0x20 || got.G != 0x20 || got.B != 0x20 {
		t.Fatalf("bg-only = %v, want #202020", got)
	}
}

func TestHeadlessBisectToolbar(t *testing.T) {
	u := testUI(t, true)
	setRows(u, sampleRows(5))
	img := renderHeadless(t, 1280, 800, func(gtx layout.Context) layout.Dimensions {
		u.paintBG(gtx)
		d := u.layoutToolbar(gtx)
		return layout.Dimensions{Size: image.Pt(1280, d.Size.Y)}
	})
	n := countDistinct(img)
	t.Logf("toolbar distinct=%d corner=%v", n, img.RGBAAt(640, 20))
	if n < 3 {
		t.Fatalf("toolbar rendered near-uniform (%d colors)", n)
	}
}

func TestHeadlessBisectSidebar(t *testing.T) {
	u := testUI(t, true)
	u.rebuildSideRows([]DBVolume{{Path: `C:\i\c.gsi`, Volume: "C:", Entries: 5}})
	img := renderHeadless(t, 220, 800, func(gtx layout.Context) layout.Dimensions {
		u.paintBG(gtx)
		return u.layoutSidebar(gtx)
	})
	n := countDistinct(img)
	t.Logf("sidebar distinct=%d", n)
	if n < 3 {
		t.Fatalf("sidebar rendered near-uniform (%d colors)", n)
	}
}

func TestHeadlessBisectResults(t *testing.T) {
	u := testUI(t, true)
	setRows(u, sampleRows(60))
	img := renderHeadless(t, 800, 600, func(gtx layout.Context) layout.Dimensions {
		u.paintBG(gtx)
		return u.layoutResults(gtx)
	})
	n := countDistinct(img)
	t.Logf("results distinct=%d", n)
	if n < 5 {
		t.Fatalf("results rendered near-uniform (%d colors)", n)
	}
}

func TestHeadlessBisectStatus(t *testing.T) {
	u := testUI(t, true)
	setRows(u, sampleRows(10))
	u.mu.Lock()
	u.health, u.healthMsg, u.entries = serviceHealthOK, "", 9999
	u.mu.Unlock()
	img := renderHeadless(t, 1280, 60, func(gtx layout.Context) layout.Dimensions {
		u.paintBG(gtx)
		return u.layoutStatus(gtx)
	})
	n := countDistinct(img)
	t.Logf("status distinct=%d sample=%v", n, img.RGBAAt(200, 30))
	if n < 3 {
		t.Fatalf("status rendered near-uniform (%d colors)", n)
	}
}

func TestHeadlessFullNoStatus(t *testing.T) {
	u := testUI(t, true)
	setRows(u, sampleRows(60))
	img := renderHeadless(t, 1280, 800, func(gtx layout.Context) layout.Dimensions {
		u.paintBG(gtx)
		// Full body without status bar.
		dims := layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(u.layoutToolbar),
			layout.Flexed(1, u.layoutBody),
		)
		_ = dims
		return layout.Dimensions{Size: image.Pt(1280, 800)}
	})
	n := countDistinct(img)
	t.Logf("full-no-status distinct=%d", n)
	if n < 10 {
		t.Fatalf("full-no-status near-uniform (%d colors)", n)
	}
}

func TestHeadlessStatusFirst(t *testing.T) {
	u := testUI(t, true)
	setRows(u, sampleRows(10))
	u.mu.Lock()
	u.health, u.healthMsg, u.entries = serviceHealthOK, "", 9999
	u.mu.Unlock()
	img := renderHeadless(t, 1280, 800, func(gtx layout.Context) layout.Dimensions {
		u.paintBG(gtx)
		// Status FIRST, then toolbar: if toolbar survives, status overdraws;
		// if both gone, the status op stream corrupts decoding.
		gtx.Constraints.Min = image.Pt(1280, 60)
		gtx.Constraints.Max = image.Pt(1280, 60)
		_ = u.layoutStatus(gtx)
		gtx.Constraints.Min = image.Pt(1280, 740)
		gtx.Constraints.Max = image.Pt(1280, 740)
		_ = u.layoutToolbar(gtx)
		return layout.Dimensions{Size: image.Pt(1280, 800)}
	})
	n := countDistinct(img)
	t.Logf("status-first distinct=%d", n)
}

func TestHeadlessStatusBgOnly(t *testing.T) {
	u := testUI(t, true)
	img := renderHeadless(t, 1280, 60, func(gtx layout.Context) layout.Dimensions {
		u.paintBG(gtx)
		s := u.scheme
		dims := layout.Dimensions{Size: image.Pt(1280, 60)}
		_ = layout.Stack{}.Layout(gtx,
			layout.Stacked(func(gtx layout.Context) layout.Dimensions {
				fillRect(gtx, s.Panel)
				return dims
			}),
		)
		return dims
	})
	n := countDistinct(img)
	t.Logf("status-bg-only distinct=%d sample=%v", n, img.RGBAAt(640, 30))
}

func TestHeadlessHairlineAlone(t *testing.T) {
	u := testUI(t, true)
	img := renderHeadless(t, 1280, 60, func(gtx layout.Context) layout.Dimensions {
		return hairline(gtx, u.scheme.Border)
	})
	n := countDistinct(img)
	t.Logf("hairline-alone distinct=%d sample0=%v sample30=%v", n, img.RGBAAt(640, 0), img.RGBAAt(640, 30))
}

func TestHeadlessLabelAlone(t *testing.T) {
	u := testUI(t, false)
	img := renderHeadless(t, 400, 40, func(gtx layout.Context) layout.Dimensions {
		return label(u.th, Sp(12), "Index ready (9,999 entries)", u.scheme.Text).Layout(gtx)
	})
	n := countDistinct(img)
	t.Logf("label-alone distinct=%d", n)
	if n < 2 {
		t.Fatalf("label invisible (%d colors)", n)
	}
}

func TestHeadlessListRowsVisible(t *testing.T) {
	u := testUI(t, true)
	setRows(u, sampleRows(60))
	img := renderHeadless(t, 800, 600, func(gtx layout.Context) layout.Dimensions {
		u.paintBG(gtx)
		return u.layoutResults(gtx)
	})
	// The header occupies roughly the top 30px; rows must paint below it.
	text := 0
	for y := 60; y < 590; y += 4 {
		for x := 8; x < 792; x += 4 {
			c := img.RGBAAt(x, y)
			lum := (int(c.R) + int(c.G) + int(c.B)) / 3
			if lum > 120 {
				text++
			}
		}
	}
	t.Logf("bright row-area pixels=%d", text)
	if text < 50 {
		t.Fatalf("no result rows painted (%d bright pixels)", text)
	}
}

func TestHeadlessStatusGeometry(t *testing.T) {
	u := testUI(t, true)
	setRows(u, sampleRows(25))
	u.mu.Lock()
	u.elapsedMS = 42
	u.searchMS = 12
	u.health, u.healthMsg, u.entries = serviceHealthOK, "", 9999
	u.mu.Unlock()
	img := renderHeadless(t, 1280, 60, func(gtx layout.Context) layout.Dimensions {
		u.paintBG(gtx)
		return u.layoutStatus(gtx)
	})
	bg := img.RGBAAt(640, 5)
	firstX := -1
	for x := 0; x < 1280; x++ {
		for y := 0; y < 60; y++ {
			if img.RGBAAt(x, y) != bg {
				firstX = x
				break
			}
		}
		if firstX >= 0 {
			break
		}
	}
	t.Logf("status first non-bg x=%d bg=%v", firstX, bg)
	if firstX < 0 || firstX > 40 {
		t.Fatalf("status text starts at x=%d, want near left padding 12", firstX)
	}
}

func TestHeadlessPreviewBody(t *testing.T) {
	u := testUI(t, true)
	u.mu.Lock()
	u.settings.PreviewOpen = true
	u.mu.Unlock()
	setRows(u, []ResultRow{
		{Path: `E:\bench\chromium\chrome\android\java\res\xml\about_chrome_preferences.xml`, Name: "about_chrome_preferences.xml", Dir: `E:\bench\chromium\chrome\android\java\res\xml`, Type: "xml", Exists: true},
	})
	u.mu.Lock()
	u.focus = 0
	u.sel = map[int]bool{0: true}
	u.mu.Unlock()
	img := renderHeadless(t, 1280, 800, func(gtx layout.Context) layout.Dimensions {
		u.paintBG(gtx)
		return u.layout(gtx)
	})
	// Preview occupies the right 330px; its body must contain text pixels.
	text := 0
	for y := 150; y < 700; y += 4 {
		for x := 960; x < 1270; x += 4 {
			c := img.RGBAAt(x, y)
			if (int(c.R)+int(c.G)+int(c.B))/3 > 120 {
				text++
			}
		}
	}
	t.Logf("preview body bright pixels=%d", text)
	if text < 100 {
		t.Fatalf("preview body empty (%d bright pixels)", text)
	}
}

func TestHeadlessRowHeight32(t *testing.T) {
	u := testUI(t, true)
	setRows(u, sampleRows(5))
	u.ensureRowState(5)
	r := new(input.Router)
	gtx := layout.Context{
		Ops:         new(op.Ops),
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Constraints{Min: image.Pt(900, 0), Max: image.Pt(900, 2000)},
		Source:      r.Source(),
	}
	dims := u.layoutRow(gtx, u.rows, 0)
	r.Frame(gtx.Ops)
	t.Logf("row0 (snippet) height=%d", dims.Size.Y)
	gtx2 := layout.Context{
		Ops:    new(op.Ops),
		Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Constraints{
			Min: image.Pt(900, 0),
			Max: image.Pt(900, 2000),
		},
		Source: r.Source(),
	}
	dims1 := u.layoutRow(gtx2, u.rows, 1)
	r.Frame(gtx2.Ops)
	t.Logf("row1 (plain) height=%d", dims1.Size.Y)
	if dims.Size.Y != 32 || dims1.Size.Y != 32 {
		t.Fatalf("row heights = %d/%d, want 32px at 100%%", dims.Size.Y, dims1.Size.Y)
	}
}

func TestFluentIconsLoad(t *testing.T) {
	found := false
	for _, f := range typefaces() {
		if string(f.Font.Typeface) == fluentFace {
			found = true
			break
		}
	}
	if !found {
		names := []string{}
		for _, f := range typefaces() {
			names = append(names, string(f.Font.Typeface))
		}
		t.Fatalf("Segoe Fluent Icons not in collection; have %q", names)
	}
}

func TestHeadlessTextMetrics(t *testing.T) {
	u := testUI(t, false)
	r := new(input.Router)
	mk := func() layout.Context {
		return layout.Context{
			Ops:    new(op.Ops),
			Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1},
			Constraints: layout.Constraints{
				Min: image.Pt(0, 0),
				Max: image.Pt(600, 200),
			},
			Source: r.Source(),
		}
	}
	gtx := mk()
	d13 := label(u.th, Sp(13), "file0.go", u.scheme.Text).Layout(gtx)
	r.Frame(gtx.Ops)
	gtx2 := mk()
	d12 := label(u.th, Sp(12), "file0.go", u.scheme.Text).Layout(gtx2)
	r.Frame(gtx2.Ops)
	t.Logf("13sp height=%d 12sp height=%d", d13.Size.Y, d12.Size.Y)
}

func TestHeadlessCellHeights(t *testing.T) {
	u := testUI(t, true)
	setRows(u, sampleRows(5))
	r := new(input.Router)
	mk := func() layout.Context {
		return layout.Context{
			Ops:    new(op.Ops),
			Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1},
			Constraints: layout.Constraints{
				Min: image.Pt(0, 0),
				Max: image.Pt(900, 2000),
			},
			Source: r.Source(),
		}
	}
	gtx := mk()
	dName := u.cellText(gtx, "file0.go", 240)
	r.Frame(gtx.Ops)
	gtx = mk()
	dIcon := u.cellIcon(gtx, u.rows[0], Dp(16))
	r.Frame(gtx.Ops)
	t.Logf("namecell=%v iconcell=%v", dName.Size, dIcon.Size)
}

func TestFitColumnsShrink(t *testing.T) {
	u := testUI(t, true)
	u.mu.Lock()
	u.colW["name"] = 280
	u.colW["folder"] = 260
	u.colW["size"] = 110
	u.colW["modified"] = 170
	u.colW["type"] = 140
	u.mu.Unlock()
	// Wide: everything fits.
	u.fitColumns(1200)
	if !u.showTypeNow() || !u.showMatchNow() {
		t.Fatal("wide window must show all columns")
	}
	// Narrow: type+match hide, essentials shrink to fit.
	u.fitColumns(700)
	if u.showTypeNow() || u.showMatchNow() {
		t.Fatal("narrow window must hide type+match")
	}
	effName, effFolder := u.effWidths()
	// Fixed: size 110 + modified 170 + chrome 48 = 328; avail 700 leaves
	// 372 for name+folder (from 540) — folder yields first.
	if effName+effFolder > 372+1 {
		t.Fatalf("name+folder = %v+%v, want <= 372", effName, effFolder)
	}
	if effFolder != 80 && effName != 280 {
		t.Logf("shrink distribution: name=%v folder=%v", effName, effFolder)
	}
	if effFolder < 80 || effName < 120 {
		t.Fatalf("below minimums: name=%v folder=%v", effName, effFolder)
	}
}

func TestHeadlessBisectFullNoStatus(t *testing.T) {
	u := testUI(t, true)
	setRows(u, sampleRows(60))
	img := renderHeadless(t, 1280, 800, func(gtx layout.Context) layout.Dimensions {
		u.paintBG(gtx)
		gtx.Constraints.Min.Y = 740
		gtx.Constraints.Max.Y = 740
		d1 := u.layoutToolbar(gtx)
		_ = d1
		return layout.Dimensions{Size: image.Pt(1280, 800)}
	})
	t.Logf("toolbar-only distinct=%d", countDistinct(img))
}

func TestHeadlessDarkFill(t *testing.T) {
	var ops op.Ops
	dark := color.NRGBA{R: 0x20, G: 0x20, B: 0x20, A: 0xFF}
	paint.ColorOp{Color: dark}.Add(&ops)
	paint.FillShape(&ops, dark, clip.Rect(image.Rect(0, 0, 800, 600)).Op())
	img := headlessShot(t, &ops, 800, 600)
	got := img.RGBAAt(400, 300)
	t.Logf("darkfill pixel=%v", got)
	wantR := uint8(0x20)
	if got.R < wantR-8 || got.R > wantR+8 || got.G != got.R || got.B != got.R {
		t.Fatalf("dark fill read back as %v, want ~#202020 (sRGB round-trip)", got)
	}
}
