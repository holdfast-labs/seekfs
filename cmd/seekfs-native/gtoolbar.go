package main

import (
	"image"
	"image/color"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// gtoolbar.go: back/forward, the search field (leading icon, clear button),
// the folder-scope field, the Contents toggle chip, and the view switcher.

type glyphFunc func(gtx layout.Context, c color.NRGBA, size image.Point) layout.Dimensions

// layoutToolbar renders the top bar. Clicks and editor events are handled
// inline; the searcher runs on its own goroutine.
func (u *UI) layoutToolbar(gtx layout.Context) layout.Dimensions {
	th := u.th
	s := u.scheme
	u.mu.Lock()
	canBack, canFwd := u.history.CanBack(), u.history.CanForward()
	contents := u.contents
	view := u.view
	u.mu.Unlock()

	if u.backBtn.Clicked(gtx) && canBack {
		u.goBack()
	}
	if u.fwdBtn.Clicked(gtx) && canFwd {
		u.goForward()
	}
	if u.contentsCk.Clicked(gtx) {
		u.toggleContents()
	}
	if u.clearBtn.Clicked(gtx) {
		u.searchEd.SetText("")
		u.typedQuery("")
	}
	for i := range u.viewBtns {
		if u.viewBtns[i].Clicked(gtx) {
			u.setView(ViewMode(i))
		}
	}

	bar := func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return iconBtn(gtx, th, s, &u.backBtn, chevGlyph(true), canBack, Dp(32))
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return iconBtn(gtx, th, s, &u.fwdBtn, chevGlyph(false), canFwd, Dp(32))
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: Dp(8), Right: Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return u.layoutSearchField(gtx)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Right: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return u.layoutScopeField(gtx)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Right: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return chip(gtx, th, s, &u.contentsCk, "Contents", contents)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Right: Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return u.layoutViewSwitcher(gtx, view)
				})
			}),
		)
	}
	dims := layout.Inset{Top: Dp(8), Bottom: Dp(8)}.Layout(gtx, bar)
	return layout.Stack{}.Layout(gtx,
		layout.Stacked(func(gtx layout.Context) layout.Dimensions { return dims }),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.SE.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return hairline(gtx, s.Border)
			})
		}),
	)
}

// layoutSearchField renders the rounded search box with icon + clear.
func (u *UI) layoutSearchField(gtx layout.Context) layout.Dimensions {
	th := u.th
	s := u.scheme
	ed := material.Editor(th, &u.searchEd, "Search")
	ed.Color = s.Text
	ed.HintColor = s.Muted
	ed.SelectionColor = s.Selected
	ed.TextSize = Sp(14)
	return u.roundedField(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					sz := gtx.Dp(Dp(18))
					return searchGlyph(gtx, s.Muted, image.Point{X: sz, Y: sz})
				})
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: Dp(6), Right: Dp(2), Top: Dp(6), Bottom: Dp(6)}.Layout(gtx, ed.Layout)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if u.searchEd.Text() == "" {
					return layout.Dimensions{}
				}
				return layout.Inset{Right: Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return iconBtn(gtx, th, s, &u.clearBtn, xGlyph, true, Dp(24))
				})
			}),
		)
	})
}

// layoutScopeField renders the compact folder-scope box.
func (u *UI) layoutScopeField(gtx layout.Context) layout.Dimensions {
	th := u.th
	s := u.scheme
	gtx.Constraints.Min.X = gtx.Dp(Dp(170))
	gtx.Constraints.Max.X = gtx.Dp(Dp(170))
	ed := material.Editor(th, &u.scopeEd, "Folder")
	ed.Color = s.Text
	ed.HintColor = s.Muted
	ed.SelectionColor = s.Selected
	ed.TextSize = Sp(13)
	return u.roundedField(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Left: Dp(10), Right: Dp(6), Top: Dp(6), Bottom: Dp(6)}.Layout(gtx, ed.Layout)
	})
}

// roundedField draws the field background + border around content.
func (u *UI) roundedField(gtx layout.Context, content layout.Widget) layout.Dimensions {
	s := u.scheme
	mac := op.Record(gtx.Ops)
	dims := content(gtx)
	call := mac.Stop()
	sz := dims.Size
	gtx.Constraints.Min = sz
	roundedFill(gtx, s.Field, sz, Dp(8))
	border(gtx, sz, s.Border, Dp(8), Dp(1))
	call.Add(gtx.Ops)
	return layout.Dimensions{Size: sz}
}

// layoutViewSwitcher renders the 3-way segmented view control.
func (u *UI) layoutViewSwitcher(gtx layout.Context, cur ViewMode) layout.Dimensions {
	glyphs := []glyphFunc{listGlyph, gridGlyph, groupGlyph}
	modes := []ViewMode{ViewDetails, ViewIcons, ViewGrouped}
	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.segBtn(gtx, &u.viewBtns[0], glyphs[0], cur == modes[0])
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.segBtn(gtx, &u.viewBtns[1], glyphs[1], cur == modes[1])
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.segBtn(gtx, &u.viewBtns[2], glyphs[2], cur == modes[2])
		}),
	)
}

// segBtn is one segment of the view switcher.
func (u *UI) segBtn(gtx layout.Context, clk *widget.Clickable, glyph glyphFunc, on bool) layout.Dimensions {
	s := u.scheme
	return clk.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		px := gtx.Dp(Dp(32))
		gtx.Constraints.Min = image.Point{X: px, Y: px}
		bg := color.NRGBA{}
		fg := s.Subtle
		if on {
			bg, fg = s.Selected, s.Text
		} else if clk.Hovered() {
			bg = s.Hover
		}
		if bg.A != 0 {
			roundedFill(gtx, bg, gtx.Constraints.Min, Dp(6))
		}
		return glyph(gtx, fg, gtx.Constraints.Min)
	})
}
