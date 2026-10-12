package main

import (
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// gtoolbar.go: back/forward, the search field (leading icon, clear button),
// the folder-scope field, the Contents toggle chip, and the view switcher.

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
					return fluentBtn(gtx, th, s, &u.backBtn, iconBack, canBack, false, Dp(32))
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return fluentBtn(gtx, th, s, &u.fwdBtn, iconForward, canFwd, false, Dp(32))
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
					return ficon(th, Sp(16), iconSearch, s.Muted).Layout(gtx)
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
					return fluentBtn(gtx, th, s, &u.clearBtn, iconClear, true, false, Dp(24))
				})
			}),
		)
	}, gtx.Focused(&u.searchEd))
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
	}, gtx.Focused(&u.scopeEd))
}

// roundedField draws the field background + border around content. The
// border turns accent while the field has keyboard focus.
func (u *UI) roundedField(gtx layout.Context, content layout.Widget, focused bool) layout.Dimensions {
	s := u.scheme
	mac := op.Record(gtx.Ops)
	dims := content(gtx)
	call := mac.Stop()
	sz := dims.Size
	gtx.Constraints.Min = sz
	roundedFill(gtx, s.Field, sz, Dp(8))
	call.Add(gtx.Ops)
	if focused {
		border(gtx, sz, s.Accent, Dp(8), Dp(2))
	} else {
		border(gtx, sz, s.Border, Dp(8), Dp(1))
	}
	return layout.Dimensions{Size: sz}
}

// layoutViewSwitcher renders the 3-way segmented view control.
func (u *UI) layoutViewSwitcher(gtx layout.Context, cur ViewMode) layout.Dimensions {
	glyphs := []string{iconList, iconGrid, iconGroup}
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
func (u *UI) segBtn(gtx layout.Context, clk *widget.Clickable, glyph string, on bool) layout.Dimensions {
	return fluentBtn(gtx, u.th, u.scheme, clk, glyph, true, on, Dp(32))
}
