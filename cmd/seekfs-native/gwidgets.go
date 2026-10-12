package main

import (
	"image"
	"image/color"
	"math"

	"gioui.org/f32"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// gwidgets.go: small custom widgets for the design system — rounded fills,
// icon buttons with vector glyphs, toggle chips, hairlines, and labels.

// fillRect paints a solid rect over the min constraints. The clip is
// mandatory: a bare PaintOp covers an infinite plane (Gio docs), which
// flattened whole frames during development.
func fillRect(gtx layout.Context, c color.NRGBA) layout.Dimensions {
	size := gtx.Constraints.Min
	defer clip.Rect{Max: size}.Push(gtx.Ops).Pop()
	paint.ColorOp{Color: c}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	return layout.Dimensions{Size: size}
}

// roundedFill paints a rounded-rect fill of size.
func roundedFill(gtx layout.Context, c color.NRGBA, size image.Point, radius unit.Dp) {
	defer clip.RRect{Rect: image.Rectangle{Max: size}, SE: gtx.Dp(radius), SW: gtx.Dp(radius), NW: gtx.Dp(radius), NE: gtx.Dp(radius)}.Push(gtx.Ops).Pop()
	paint.ColorOp{Color: c}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
}

// opRecord records subsequent ops for replay (background-then-content).
func opRecord(gtx layout.Context) opState { return opState{mac: op.Record(gtx.Ops)} }

type opState struct{ mac op.MacroOp }

func (o opState) Stop() op.CallOp { return o.mac.Stop() }

// label is a one-line truncated label.
func label(th *material.Theme, size unit.Sp, txt string, c color.NRGBA) material.LabelStyle {
	l := material.Label(th, size, txt)
	l.Color = c
	l.MaxLines = 1
	l.Truncator = "…"
	return l
}

// ficon is a Segoe Fluent Icons glyph label (16px, secondary color).
func ficon(th *material.Theme, size unit.Sp, glyph string, c color.NRGBA) material.LabelStyle {
	l := material.Label(th, size, glyph)
	l.Color = c
	l.Font.Typeface = fluentFace
	l.MaxLines = 1
	return l
}

// fluentBtn is a square Fluent-icon button with hover, pressed and keyboard
// focus states.
func fluentBtn(gtx layout.Context, th *material.Theme, s Scheme, clk *widget.Clickable, glyph string, enabled, active bool, sz unit.Dp) layout.Dimensions {
	px := gtx.Dp(sz)
	gtx.Constraints.Min = image.Point{X: px, Y: px}
	bg := color.NRGBA{}
	fg := s.Subtle
	if active {
		fg = s.Accent
	} else if !enabled {
		fg = s.Muted
	} else if clk.Hovered() {
		bg = s.Hover
	}
	if clk.Pressed() && enabled {
		bg = s.Pressed
	}
	focused := gtx.Focused(clk)
	return clk.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min = image.Point{X: px, Y: px}
		mac := op.Record(gtx.Ops)
		dims := layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			g := fg
			if !enabled {
				gtx = gtx.Disabled()
			}
			return ficon(th, Sp(16), glyph, g).Layout(gtx)
		})
		call := mac.Stop()
		sz := dims.Size
		if sz.X < px || sz.Y < px {
			sz = image.Point{X: px, Y: px}
		}
		if bg.A != 0 {
			roundedFill(gtx, bg, sz, Dp(6))
		}
		call.Add(gtx.Ops)
		if focused {
			border(gtx, sz, s.Accent, Dp(6), Dp(2))
		}
		return layout.Dimensions{Size: sz}
	})
}

// iconBtn is a square icon button; glyph draws the vector icon centered.
func iconBtn(gtx layout.Context, th *material.Theme, s Scheme, clk *widget.Clickable, glyph func(gtx layout.Context, c color.NRGBA, size image.Point) layout.Dimensions, enabled bool, sz unit.Dp) layout.Dimensions {
	px := gtx.Dp(sz)
	gtx.Constraints.Min = image.Point{X: px, Y: px}
	bg := color.NRGBA{}
	fg := s.Subtle
	if !enabled {
		fg = s.Muted
	} else if clk.Hovered() {
		bg = s.Hover
	}
	if clk.Pressed() && enabled {
		bg = s.Pressed
	}
	return clk.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		if bg.A != 0 {
			roundedFill(gtx, bg, gtx.Constraints.Min, Dp(6))
		}
		if !enabled {
			gtx = gtx.Disabled()
		}
		return glyph(gtx, fg, gtx.Constraints.Min)
	})
}

// chip is a pill toggle (Contents) or action chip, with hover, pressed and
// keyboard focus states.
func chip(gtx layout.Context, th *material.Theme, s Scheme, clk *widget.Clickable, txt string, on bool) layout.Dimensions {
	bg := s.Field
	fg := s.Text
	if on {
		bg, fg = s.Accent, s.OnAccent
	} else if clk.Hovered() {
		bg = s.Hover
	}
	if clk.Pressed() {
		bg = s.Pressed
		if on {
			fg = s.OnAccent
		}
	}
	focused := gtx.Focused(clk)
	return clk.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		mac := op.Record(gtx.Ops)
		dims := layout.Inset{Left: Dp(10), Right: Dp(10), Top: Dp(5), Bottom: Dp(5)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return label(th, Sp(13), txt, fg).Layout(gtx)
		})
		call := mac.Stop()
		sz := dims.Size
		gtx.Constraints.Min = sz
		roundedFill(gtx, bg, sz, Dp(14))
		if focused {
			border(gtx, sz, s.Accent, Dp(14), Dp(2))
		} else {
			border(gtx, sz, s.Border, Dp(14), Dp(1))
		}
		call.Add(gtx.Ops)
		return layout.Dimensions{Size: sz}
	})
}

// border strokes a rounded-rect outline.
func border(gtx layout.Context, size image.Point, c color.NRGBA, radius, width unit.Dp) {
	rr := clip.RRect{Rect: image.Rectangle{Max: size}, SE: gtx.Dp(radius), SW: gtx.Dp(radius), NW: gtx.Dp(radius), NE: gtx.Dp(radius)}
	defer clip.Stroke{Path: rr.Path(gtx.Ops), Width: float32(gtx.Dp(width))}.Op().Push(gtx.Ops).Pop()
	paint.ColorOp{Color: c}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
}

// hairline draws a 1px bottom divider across the min width.
func hairline(gtx layout.Context, c color.NRGBA) layout.Dimensions {
	h := gtx.Dp(Dp(1))
	if h < 1 {
		h = 1
	}
	gtx.Constraints.Min.Y = h
	gtx.Constraints.Max.Y = h
	return fillRect(gtx, c)
}

// vline draws a 1px vertical divider of the min height.
func vline(gtx layout.Context, c color.NRGBA) layout.Dimensions {
	w := gtx.Dp(Dp(1))
	if w < 1 {
		w = 1
	}
	gtx.Constraints.Min.X = w
	gtx.Constraints.Max.X = w
	return fillRect(gtx, c)
}

// ---- vector glyphs (stroke-based, crisp at any scale) ----

func strokeLine(gtx layout.Context, c color.NRGBA, w unit.Dp, from, to f32.Point) {
	var p clip.Path
	p.Begin(gtx.Ops)
	p.MoveTo(from)
	p.LineTo(to)
	defer clip.Stroke{Path: p.End(), Width: float32(gtx.Dp(w))}.Op().Push(gtx.Ops).Pop()
	paint.ColorOp{Color: c}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
}

// chevGlyph draws a < or > chevron.
func chevGlyph(left bool) func(gtx layout.Context, c color.NRGBA, size image.Point) layout.Dimensions {
	return func(gtx layout.Context, c color.NRGBA, size image.Point) layout.Dimensions {
		cx, cy := float32(size.X)/2, float32(size.Y)/2
		d := float32(gtx.Dp(Dp(5)))
		w := Dp(2)
		if left {
			strokeLine(gtx, c, w, f32.Point{X: cx + d, Y: cy - d}, f32.Point{X: cx - d, Y: cy})
			strokeLine(gtx, c, w, f32.Point{X: cx - d, Y: cy}, f32.Point{X: cx + d, Y: cy + d})
		} else {
			strokeLine(gtx, c, w, f32.Point{X: cx - d, Y: cy - d}, f32.Point{X: cx + d, Y: cy})
			strokeLine(gtx, c, w, f32.Point{X: cx + d, Y: cy}, f32.Point{X: cx - d, Y: cy + d})
		}
		return layout.Dimensions{Size: size}
	}
}

// searchGlyph draws a magnifier.
func searchGlyph(gtx layout.Context, c color.NRGBA, size image.Point) layout.Dimensions {
	cx, cy := float32(size.X)/2-1, float32(size.Y)/2-1
	r := float32(gtx.Dp(Dp(6)))
	var p clip.Path
	p.Begin(gtx.Ops)
	const steps = 24
	for i := 0; i <= steps; i++ {
		a := float64(i) / steps * 2 * math.Pi
		x := cx + r*float32(math.Cos(a))
		y := cy + r*float32(math.Sin(a))
		if i == 0 {
			p.MoveTo(f32.Point{X: x, Y: y})
		} else {
			p.LineTo(f32.Point{X: x, Y: y})
		}
	}
	defer clip.Stroke{Path: p.End(), Width: float32(gtx.Dp(Dp(2)))}.Op().Push(gtx.Ops).Pop()
	paint.ColorOp{Color: c}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	e := float32(gtx.Dp(Dp(5)))
	strokeLine(gtx, c, Dp(2), f32.Point{X: cx + r*0.7, Y: cy + r * 0.7}, f32.Point{X: cx + r*0.7 + e, Y: cy + r*0.7 + e})
	return layout.Dimensions{Size: size}
}

// xGlyph draws a clear (×) mark.
func xGlyph(gtx layout.Context, c color.NRGBA, size image.Point) layout.Dimensions {
	cx, cy := float32(size.X)/2, float32(size.Y)/2
	d := float32(gtx.Dp(Dp(4)))
	strokeLine(gtx, c, Dp(2), f32.Point{X: cx - d, Y: cy - d}, f32.Point{X: cx + d, Y: cy + d})
	strokeLine(gtx, c, Dp(2), f32.Point{X: cx + d, Y: cy - d}, f32.Point{X: cx - d, Y: cy + d})
	return layout.Dimensions{Size: size}
}

// listGlyph draws three lines (details view icon).
func listGlyph(gtx layout.Context, c color.NRGBA, size image.Point) layout.Dimensions {
	for i := 0; i < 3; i++ {
		y := float32(size.Y)/2 + float32(i-1)*float32(gtx.Dp(Dp(5)))
		x0 := float32(size.X)/2 - float32(gtx.Dp(Dp(6)))
		x1 := float32(size.X)/2 + float32(gtx.Dp(Dp(6)))
		strokeLine(gtx, c, Dp(2), f32.Point{X: x0, Y: y}, f32.Point{X: x1, Y: y})
	}
	return layout.Dimensions{Size: size}
}

// gridGlyph draws four squares (icons view icon).
func gridGlyph(gtx layout.Context, c color.NRGBA, size image.Point) layout.Dimensions {
	g := float32(gtx.Dp(Dp(5)))
	cx, cy := float32(size.X)/2, float32(size.Y)/2
	for _, dx := range []float32{-1, 1} {
		for _, dy := range []float32{-1, 1} {
			x, y := cx+dx*g, cy+dy*g
			st := clip.Rect{Min: image.Point{X: int(x - g/2), Y: int(y - g/2)}, Max: image.Point{X: int(x + g / 2), Y: int(y + g/2)}}.Push(gtx.Ops)
			paint.ColorOp{Color: c}.Add(gtx.Ops)
			paint.PaintOp{}.Add(gtx.Ops)
			st.Pop()
		}
	}
	return layout.Dimensions{Size: size}
}

// groupGlyph draws a grouped-rows icon (bracketed lines).
func groupGlyph(gtx layout.Context, c color.NRGBA, size image.Point) layout.Dimensions {
	cx := float32(size.X) / 2
	cy := float32(size.Y) / 2
	d := float32(gtx.Dp(Dp(6)))
	strokeLine(gtx, c, Dp(2), f32.Point{X: cx - d, Y: cy - d}, f32.Point{X: cx - d, Y: cy + d})
	for i := -1; i <= 1; i++ {
		y := cy + float32(i)*float32(gtx.Dp(Dp(5)))
		strokeLine(gtx, c, Dp(2), f32.Point{X: cx - d, Y: y}, f32.Point{X: cx + d, Y: y})
	}
	return layout.Dimensions{Size: size}
}
