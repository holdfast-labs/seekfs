package main

import (
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// A self-contained file: the test must not depend on a local checkout.
	dir := t.TempDir()
	path := filepath.Join(dir, "about_chrome_preferences.xml")
	var body strings.Builder
	body.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<PreferenceScreen>\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&body, "    <Preference android:key=\"pref_%02d\" android:title=\"Preference title %02d\" />\n", i, i)
	}
	body.WriteString("</PreferenceScreen>\n")
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	setRows(u, []ResultRow{
		{Path: path, Name: "about_chrome_preferences.xml", Dir: dir, Type: "xml", Exists: true},
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

func TestHeadlessFluentGlyphs(t *testing.T) {
	u := testUI(t, true)
	for name, g := range map[string]string{
		"back": iconBack, "forward": iconForward, "search": iconSearch,
		"clear": iconClear, "list": iconList, "grid": iconGrid,
		"group": iconGroup, "contents": iconContents, "saved": iconSaved,
		"add": iconAdd,
	} {
		img := renderHeadless(t, 60, 60, func(gtx layout.Context) layout.Dimensions {
			u.paintBG(gtx)
			return ficon(u.th, Sp(24), g, u.scheme.Text).Layout(gtx)
		})
		n := 0
		for y := 0; y < 60; y++ {
			for x := 0; x < 60; x++ {
				c := img.RGBAAt(x, y)
				if (int(c.R)+int(c.G)+int(c.B))/3 > 120 {
					n++
				}
			}
		}
		t.Logf("glyph %s bright=%d", name, n)
		if n < 5 {
			t.Errorf("fluent glyph %s (%q) rendered blank", name, g)
		}
	}
}

// testSoakEnabled reports whether the opt-in soak tier runs. The soak tier
// re-applies the strict wall-clock budgets that are too noisy for a loaded
// dev box (see TestHeadlessFrameCost) and the full differential matrices in
// cmd/seekfs. CI sets it for the long step; see scripts/ci.ps1 and the
// "Test tiers" section in docs/BENCHMARKS.md.
func testSoakEnabled() bool {
	return os.Getenv("SEEKFS_TEST_SOAK") == "1"
}

func TestHeadlessFrameCost(t *testing.T) {
	if testing.Short() {
		t.Skip("frame cost measurement")
	}
	u := testUI(t, true)
	setRows(u, sampleRows(200))
	r := new(input.Router)
	mk := func(size image.Point) (layout.Context, *op.Ops) {
		ops := new(op.Ops)
		return layout.Context{
			Ops:    ops,
			Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1},
			Constraints: layout.Constraints{
				Min: size,
				Max: size,
			},
			Source: r.Source(),
		}, ops
	}
	for _, size := range []image.Point{image.Pt(1280, 800), image.Pt(2560, 1440)} {
		hw, err := headless.NewWindow(size.X, size.Y)
		if err != nil {
			t.Skipf("headless unavailable: %v", err)
		}
		img := image.NewRGBA(image.Rectangle{Max: size})
		frame := func(i int) (layoutMS, e2eMS float64) {
			gtx, _ := mk(size)
			// Simulate scrolling: advance the list each frame.
			u.list.Position.First = (i * 7) % 150
			u.list.Position.Offset = 0
			start := time.Now()
			u.layout(gtx)
			r.Frame(gtx.Ops)
			layoutMS = float64(time.Since(start).Microseconds()) / 1000.0
			start = time.Now()
			if err := hw.Frame(gtx.Ops); err != nil {
				t.Fatalf("frame: %v", err)
			}
			if err := hw.Screenshot(img); err != nil {
				t.Fatalf("screenshot: %v", err)
			}
			e2eMS = float64(time.Since(start).Microseconds()) / 1000.0
			return layoutMS, e2eMS
		}
		// Warm up caches (fonts, icons, GPU programs); discarded.
		for i := 0; i < 5; i++ {
			frame(i)
		}
		const iters = 30
		layoutMin, e2eMin := 1e9, 1e9
		layoutSum, e2eSum := 0.0, 0.0
		for i := 0; i < iters; i++ {
			layoutMS, e2eMS := frame(5 + i)
			layoutSum += layoutMS
			e2eSum += e2eMS
			if layoutMS < layoutMin {
				layoutMin = layoutMS
			}
			if e2eMS < e2eMin {
				e2eMin = e2eMS
			}
		}
		hw.Release()
		t.Logf("headless render+readback (%dx%d, scrolled): layout min=%.2fms avg=%.2fms, end-to-end min=%.2fms avg=%.2fms over %d frames",
			size.X, size.Y, layoutMin, layoutSum/iters, e2eMin, e2eSum/iters, iters)
		// The CPU layout is deterministic work, so its minimum filters
		// scheduling noise while still moving with real regressions (an
		// accidental O(n^2) in layout raises every sample including the
		// minimum). 10ms keeps headroom over the ~1-3ms this frame costs
		// while tripping on a 3-4x CPU regression in any tier.
		if layoutMin > 10 {
			t.Fatalf("layout min %.2fms exceeds 10ms CPU budget (%dx%d)", layoutMin, size.X, size.Y)
		}
		// The end-to-end frame plus readback crosses the GPU/driver, so its
		// average is load noise (15-24ms on a busy box), not signal. The
		// default tier only trips the smoke cap: the frame path completing
		// at all is the coverage, and 100ms means it is fundamentally
		// broken, never merely busy. The soak tier (SEEKFS_TEST_SOAK=1, run
		// by CI) applies a budget to the minimum instead of the average.
		// GitHub's Windows runners have no GPU, so the frame renders through
		// the software rasterizer and its floor is ~14.7ms there; 25ms still
		// fails a real 2x frame-path regression on that path, and the 10ms
		// layout CPU budget above stays the strict check.
		cap := 100.0
		if testSoakEnabled() {
			cap = 25.0
		}
		if e2eMin > cap {
			t.Fatalf("frame end-to-end min %.2fms exceeds %.0fms budget (%dx%d, soak=%v)",
				e2eMin, cap, size.X, size.Y, testSoakEnabled())
		}
	}
}

func TestHeadlessRowBaselines(t *testing.T) {
	u := testUI(t, true)
	// Keep the x bands deterministic: the on-disk native.json may hold
	// wider persisted columns than this test's fixed pixel bands assume.
	u.mu.Lock()
	u.colW = defaultColW()
	u.mu.Unlock()
	setRows(u, []ResultRow{{
		Path: `C:\proj\active_tab_chrome_pages.go`, Name: "active_tab_chrome_pages.go",
		Dir: `C:\proj\api_test\tab_capture`, Size: 900, HasSize: true,
		Modified: time.Now(), HasMod: true, Type: "go", Exists: true,
	}})
	u.ensureRowState(1)
	img := renderHeadless(t, 1200, 120, func(gtx layout.Context) layout.Dimensions {
		u.paintBG(gtx)
		return u.layoutRow(gtx, u.rows, 0)
	})
	// Text centroid per column band (name 0-260, folder 280-500, size, date).
	centroid := func(x0, x1 int) float64 {
		sum, n := 0.0, 0
		for y := 0; y < 120; y++ {
			for x := x0; x < x1; x++ {
				c := img.RGBAAt(x, y)
				if (int(c.R)+int(c.G)+int(c.B))/3 > 120 {
					sum += float64(y)
					n++
				}
			}
		}
		if n == 0 {
			return -1
		}
		return sum / float64(n)
	}
	nameY := centroid(30, 260)
	folderY := centroid(280, 500)
	sizeY := centroid(510, 610)
	dateY := centroid(620, 790)
	t.Logf("centroids name=%.1f folder=%.1f size=%.1f date=%.1f", nameY, folderY, sizeY, dateY)
	for col, y := range map[string]float64{"folder": folderY, "size": sizeY, "date": dateY} {
		if y < 0 || nameY < 0 {
			t.Fatalf("missing text in name or %s column", col)
		}
		if d := y - nameY; d < -1.5 || d > 1.5 {
			t.Fatalf("%s baseline off by %.1fpx from name", col, d)
		}
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
