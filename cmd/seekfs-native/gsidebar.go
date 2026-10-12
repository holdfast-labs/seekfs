package main

import (
	"os"
	"path/filepath"

	"gioui.org/layout"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// gsidebar.go: Favorites/Quick access, indexed volumes with counts, and
// saved searches. Clicking a scope sets the folder scope; clicking a saved
// search runs it.

// sideClicks holds per-row click states, parallel to sideRows.
func (u *UI) layoutSidebar(gtx layout.Context) layout.Dimensions {
	th := u.th
	s := u.scheme
	u.mu.Lock()
	rows := append([]sideRow(nil), u.sideRows...)
	u.mu.Unlock()
	for len(u.sideClicks) < len(rows) {
		u.sideClicks = append(u.sideClicks, widget.Clickable{})
	}
	for i, r := range rows {
		if u.sideClicks[i].Clicked(gtx) {
			u.activateSide(r)
		}
	}
	fillRect(gtx, s.Panel)
	dims := layout.Inset{Top: Dp(8), Bottom: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return material.List(th, &u.sideList).Layout(gtx, len(rows), func(gtx layout.Context, i int) layout.Dimensions {
			return u.layoutSideRow(gtx, rows[i], &u.sideClicks[i])
		})
	})
	// Right hairline divider.
	return layout.Stack{}.Layout(gtx,
		layout.Stacked(func(gtx layout.Context) layout.Dimensions { return dims }),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.E.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return vline(gtx, s.Border)
			})
		}),
	)
}

func (u *UI) layoutSideRow(gtx layout.Context, r sideRow, clk *widget.Clickable) layout.Dimensions {
	th := u.th
	s := u.scheme
	if r.kind == sideHeader {
		return layout.Inset{Left: Dp(16), Right: Dp(8), Top: Dp(10), Bottom: Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			l := label(th, Sp(11), r.label, s.Muted)
			return l.Layout(gtx)
		})
	}
	active := u.sideActive(r)
	focused := gtx.Focused(clk)
	return clk.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		mac := opRecord(gtx)
		dims := layout.Inset{Left: Dp(12 + float32(r.depth)*12), Right: Dp(8), Top: Dp(5), Bottom: Dp(5)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return u.sideIcon(gtx, r)
				}),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return label(th, Sp(13), r.label, s.Text).Layout(gtx)
				}),
			)
		})
		call := mac.Stop()
		// Rounded 4px hover/selection, accent-tinted when active.
		if active || clk.Hovered() {
			bg := s.Hover
			if active {
				bg = s.Selected
			}
			roundedFill(gtx, bg, dims.Size, Dp(4))
		}
		call.Add(gtx.Ops)
		if focused {
			border(gtx, dims.Size, s.Accent, Dp(4), Dp(2))
		}
		return layout.Dimensions{Size: dims.Size}
	})
}

// sideActive highlights the scope matching the current under:.
// sideIcon renders the row icon: shell icon for paths, Fluent glyphs for
// actions (save) where no shell icon applies.
func (u *UI) sideIcon(gtx layout.Context, r sideRow) layout.Dimensions {
	if r.kind == sideHeader {
		return layout.Dimensions{}
	}
	if r.query == "\x00save" {
		return layout.Inset{Right: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return ficon(u.th, Sp(16), iconAdd, u.scheme.Subtle).Layout(gtx)
		})
	}
	if r.query != "" {
		return layout.Inset{Right: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return ficon(u.th, Sp(16), iconSaved, u.scheme.Subtle).Layout(gtx)
		})
	}
	return layout.Inset{Right: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		path := r.under
		if path == "" {
			path = r.label
		}
		return u.cellIcon(gtx, ResultRow{Path: path, IsDir: true}, Dp(16))
	})
}

func (u *UI) sideActive(r sideRow) bool {
	if r.kind != sideScope || r.under == "" {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.under == r.under
}

func (u *UI) activateSide(r sideRow) {
	switch r.kind {
	case sideScope:
		u.scopeEd.SetText(r.under)
		u.mu.Lock()
		u.under = r.under
		u.mu.Unlock()
		u.settings.Under = r.under
		u.saveSoon()
		u.commitQuery()
	case sideSaved:
		if r.query == "\x00save" {
			u.saveCurrentSearch()
			return
		}
		u.searchEd.SetText(r.query)
		u.commitQuery()
	}
}

func (u *UI) saveCurrentSearch() {
	q := u.searchEd.Text()
	if q == "" {
		return
	}
	u.mu.Lock()
	for _, sv := range u.settings.Saved {
		if sv == q {
			u.mu.Unlock()
			return
		}
	}
	u.settings.Saved = append(u.settings.Saved, q)
	u.mu.Unlock()
	u.rebuildSideRows(nil)
	u.saveSoon()
}

// rebuildSideRows reconstructs the sidebar rows; vols nil keeps current.
func (u *UI) rebuildSideRows(vols []DBVolume) {
	u.mu.Lock()
	if vols != nil {
		u.volumes = vols
	}
	saved := append([]string(nil), u.settings.Saved...)
	vols = append([]DBVolume(nil), u.volumes...)
	u.mu.Unlock()
	var rows []sideRow
	rows = append(rows, sideRow{kind: sideHeader, label: "Favorites"})
	for _, f := range quickAccessPaths() {
		rows = append(rows, sideRow{kind: sideScope, label: f.label, under: f.path, depth: 1})
	}
	rows = append(rows, sideRow{kind: sideHeader, label: "Volumes"})
	if len(vols) == 0 {
		rows = append(rows, sideRow{kind: sideScope, label: "No indexes loaded", depth: 1})
	}
	for _, v := range vols {
		under := ""
		if v.Volume != "" {
			under = v.Volume + `\`
		}
		// Explorer-style "Local Disk (E:)" with a muted entry count.
		lbl := v.Volume
		if under != "" {
			if name := volumeLabel(under); name != "" {
				lbl = name
			}
		}
		if lbl == "" {
			lbl = v.Path
		}
		if v.Entries > 0 {
			lbl += "  (" + formatCount(v.Entries) + ")"
		}
		rows = append(rows, sideRow{kind: sideScope, label: lbl, under: under, depth: 1})
	}
	rows = append(rows, sideRow{kind: sideHeader, label: "Saved searches"})
	for _, q := range saved {
		rows = append(rows, sideRow{kind: sideSaved, label: q, query: q, depth: 1})
	}
	rows = append(rows, sideRow{kind: sideSaved, label: "+ Save current search", query: "\x00save", depth: 1})
	u.mu.Lock()
	u.sideRows = rows
	u.mu.Unlock()
}

type quickFav struct{ label, path string }

// quickAccessPaths returns existing well-known folders for Favorites.
func quickAccessPaths() []quickFav {
	var out []quickFav
	home, _ := os.UserHomeDir()
	for _, c := range []quickFav{
		{"Desktop", filepath.Join(home, "Desktop")},
		{"Documents", filepath.Join(home, "Documents")},
		{"Downloads", filepath.Join(home, "Downloads")},
		{"Pictures", filepath.Join(home, "Pictures")},
		{"Music", filepath.Join(home, "Music")},
		{"Videos", filepath.Join(home, "Videos")},
	} {
		if st, err := os.Stat(c.path); err == nil && st.IsDir() {
			out = append(out, c)
		}
	}
	return out
}
