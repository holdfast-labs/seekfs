package main

import (
	"image"
	"image/color"

	"gioui.org/f32"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// gresults.go: the virtualized results area — Details (sortable, resizable
// columns, shell icons, snippet second line with match emphasis), Icons
// grid, and grouped-by-folder. layout.List only lays out visible rows, so
// millions of rows stay smooth.

// layoutResults renders the results pane plus the rename overlay.
func (u *UI) layoutResults(gtx layout.Context) layout.Dimensions {
	s := u.scheme
	// Focusable results area (keyboard shortcuts + type-to-select).
	event.Op(gtx.Ops, &u.list)

	u.mu.Lock()
	rows := u.rows
	view := u.view
	renameRow := u.renameRow
	u.mu.Unlock()

	base := layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if view == ViewIcons {
				return layout.Dimensions{}
			}
			return u.layoutHeader(gtx)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			switch view {
			case ViewIcons:
				return u.layoutIconGrid(gtx, rows)
			case ViewGrouped:
				return u.layoutGrouped(gtx, rows)
			default:
				return u.layoutDetailList(gtx, rows)
			}
		}),
	)
	if renameRow < 0 {
		return base
	}
	// Rename overlay card.
	return layout.Stack{}.Layout(gtx,
		layout.Stacked(func(gtx layout.Context) layout.Dimensions { return base }),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return u.layoutRenameCard(gtx, s)
			})
		}),
	)
}

// ---- Details header ----

var detailCols = []struct {
	key   SortKey
	title string
}{
	{SortName, "Name"},
	{SortFolder, "Folder"},
	{SortSize, "Size"},
	{SortModified, "Date modified"},
	{SortType, "Type"},
	{SortSnippet, "Match"},
}

func (u *UI) layoutHeader(gtx layout.Context) layout.Dimensions {
	th := u.th
	s := u.scheme
	for len(u.colClicks) < len(detailCols) {
		u.colClicks = append(u.colClicks, widget.Clickable{})
	}
	// Header clicks: sort (Match column keeps current sort).
	for i, c := range detailCols {
		if u.colClicks[i].Clicked(gtx) && c.key != SortSnippet {
			u.clickColumn(c.key)
		}
	}
	u.mu.Lock()
	key, asc := u.sortKey, u.sortAsc
	u.mu.Unlock()
	dims := layout.Inset{Left: Dp(8), Right: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx, u.headerCells(th, s, key, asc)...)
	})
	// Header background + bottom hairline.
	return layout.Stack{}.Layout(gtx,
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			fillRect(gtx, s.Header)
			return dims
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.SE.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return hairline(gtx, s.Border)
			})
		}),
	)
}

func (u *UI) headerCells(th *material.Theme, s Scheme, key SortKey, asc bool) []layout.FlexChild {
	var out []layout.FlexChild
	for i, c := range detailCols {
		i, c := i, c
		w := u.colW[c.key.String()]
		title := c.title
		if i < len(detailCols)-1 && c.key != SortSnippet && key == c.key {
			if asc {
				title += " ▲"
			} else {
				title += " ▼"
			}
		}
		out = append(out, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Dp(Dp(w))
			gtx.Constraints.Max.X = gtx.Dp(Dp(w))
			return u.colClicks[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: Dp(7), Bottom: Dp(7), Left: Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return label(th, Sp(12), title, s.Muted).Layout(gtx)
				})
			})
		}))
		// Divider handle (except after last column).
		if i < len(detailCols)-1 {
			out = append(out, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.layoutDivider(gtx, c.key)
			}))
		}
	}
	return out
}

// layoutDivider is a draggable column divider.
func (u *UI) layoutDivider(gtx layout.Context, col SortKey) layout.Dimensions {
	if u.divTags == nil {
		u.divTags = map[SortKey]*divTag{}
	}
	tag, ok := u.divTags[col]
	if !ok {
		tag = &divTag{}
		u.divTags[col] = tag
	}
	event.Op(gtx.Ops, tag)
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: tag, Kinds: pointer.Press | pointer.Drag | pointer.Release})
		if !ok {
			break
		}
		e, ok := ev.(pointer.Event)
		if !ok {
			continue
		}
		switch e.Kind {
		case pointer.Press:
			tag.start = e.Position.X
			tag.w0 = u.colW[col.String()]
			tag.on = true
		case pointer.Drag:
			if tag.on {
				dx := (e.Position.X - tag.start) / gtx.Metric.PxPerDp
				w := tag.w0 + dx
				if w < 40 {
					w = 40
				}
				if w > 900 {
					w = 900
				}
				u.mu.Lock()
				u.colW[col.String()] = w
				u.mu.Unlock()
			}
		case pointer.Release, pointer.Cancel:
			tag.on = false
			u.persistCols()
		}
	}
	gtx.Constraints.Min.X = gtx.Dp(Dp(7))
	gtx.Constraints.Max.X = gtx.Dp(Dp(7))
	gtx.Constraints.Min.Y = gtx.Dp(Dp(20))
	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return vline(gtx, u.scheme.Divider)
	})
}

type divTag struct {
	start float32
	w0    float32
	on    bool
}

func (u *UI) clickColumn(k SortKey) {
	u.mu.Lock()
	if u.sortKey == k {
		u.sortAsc = !u.sortAsc
	} else {
		u.sortKey, u.sortAsc = k, true
	}
	u.mu.Unlock()
	u.settings.SortColumn = u.sortKey.String()
	u.settings.SortAsc = u.sortAsc
	u.resort()
	u.saveSoon()
}

// resort re-sorts rows client-side.
func (u *UI) resort() {
	u.mu.Lock()
	u.sortLocked()
	u.mu.Unlock()
}

func (u *UI) persistCols() {
	u.mu.Lock()
	for k, v := range u.colW {
		u.settings.ColumnWidths[k] = int(v)
	}
	u.mu.Unlock()
	u.saveSoon()
}

// ---- Details list ----

func (u *UI) layoutDetailList(gtx layout.Context, rows []ResultRow) layout.Dimensions {
	th := u.th
	u.ensureRowState(len(rows))
	return material.List(th, &u.list).Layout(gtx, len(rows), func(gtx layout.Context, i int) layout.Dimensions {
		return u.layoutRow(gtx, rows, i)
	})
}

func (u *UI) ensureRowState(n int) {
	for len(u.rowClicks) < n {
		u.rowClicks = append(u.rowClicks, widget.Clickable{})
		u.rowTags = append(u.rowTags, rowTag{})
	}
}

type rowTag struct {
	pressPos f32.Point
	pressRow int
	pressOn  bool
	dragged  bool
}

func (u *UI) layoutRow(gtx layout.Context, rows []ResultRow, i int) layout.Dimensions {
	s := u.scheme
	r := rows[i]
	u.mu.Lock()
	selected := u.sel[i]
	focused := u.focus == i
	contents := u.contents
	query := u.query
	if contents {
		query = toContentQuery(query)
	}
	terms := contentHighlightTerms(query)
	u.mu.Unlock()

	clk := &u.rowClicks[i]
	if c, ok := clk.Update(gtx); ok {
		u.clickRow(i, c.Modifiers, c.NumClicks)
	}
	// Right-click + drag input.
	tag := &u.rowTags[i]
	event.Op(gtx.Ops, tag)
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: tag, Kinds: pointer.Press | pointer.Release | pointer.Drag})
		if !ok {
			break
		}
		if e, ok := ev.(pointer.Event); ok {
			u.rowPointer(i, e, gtx)
		}
	}

	bg := color.NRGBA{}
	if selected {
		bg = s.Selected
	} else if clk.Hovered() {
		bg = s.Hover
	}
	return clk.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		if bg.A != 0 {
			fillRect(gtx, bg)
		}
		row := layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if focused {
					return layout.Inset{Right: Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.X = gtx.Dp(Dp(3))
						gtx.Constraints.Max.X = gtx.Dp(Dp(3))
						return fillRect(gtx, s.FocusBar)
					})
				}
				return layout.Dimensions{}
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.cellIcon(gtx, r, Dp(16))
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = gtx.Dp(Dp(u.colW["name"]))
				gtx.Constraints.Max.X = gtx.Dp(Dp(u.colW["name"]))
				return u.cellNameSnippet(gtx, r, terms)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.cellText(gtx, r.Dir, u.colW["folder"])
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.cellTextRight(gtx, sizeText(r), u.colW["size"])
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.cellText(gtx, modText(r), u.colW["modified"])
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.cellText(gtx, u.typeText(r), u.colW["type"])
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				if r.Snippet == "" {
					return layout.Dimensions{}
				}
				return u.cellSnippet(gtx, r.Snippet, terms)
			}),
		)
		return row
	})
}

// rowPointer handles right-click menu + drag-out initiation.
func (u *UI) rowPointer(i int, e pointer.Event, gtx layout.Context) {
	tag := &u.rowTags[i]
	switch e.Kind {
	case pointer.Press:
		if e.Buttons.Contain(pointer.ButtonSecondary) {
			u.ensureSelected(i)
			u.showMenuForSelection()
		} else {
			tag.pressPos = e.Position
			tag.pressRow = i
			tag.pressOn = true
			tag.dragged = false
			// Focus the list for keyboard nav.
			gtx.Execute(key.FocusCmd{Tag: &u.list})
		}
	case pointer.Drag:
		if tag.pressOn && !tag.dragged {
			dx := e.Position.X - tag.pressPos.X
			dy := e.Position.Y - tag.pressPos.Y
			if dx*dx+dy*dy > 16*16 {
				tag.dragged = true
				u.ensureSelected(i)
				paths := u.selectedPaths()
				_ = u.shell.BeginDrag(paths)
				tag.pressOn = false
			}
		}
	case pointer.Release, pointer.Cancel:
		tag.pressOn = false
	}
}

// clickRow processes left-click selection: plain (single), Ctrl (toggle),
// Shift (range), double-click (open).
func (u *UI) clickRow(i int, mods key.Modifiers, numClicks int) {
	u.mu.Lock()
	n := len(u.rows)
	anchor := u.anchor
	if mods.Contain(key.ModCtrl) {
		if u.sel[i] {
			delete(u.sel, i)
		} else {
			u.sel[i] = true
		}
		u.focus = i
		u.anchor = i
	} else if mods.Contain(key.ModShift) {
		if anchor < 0 {
			anchor = i
		}
		lo, hi := anchor, i
		if lo > hi {
			lo, hi = hi, lo
		}
		u.sel = map[int]bool{}
		for k := lo; k <= hi && k < n; k++ {
			u.sel[k] = true
		}
		u.focus = i
	} else {
		u.sel = map[int]bool{i: true}
		u.focus = i
		u.anchor = i
	}
	u.mu.Unlock()
	if numClicks >= 2 {
		u.openSelected()
	}
}

// ensureSelected selects i if it isn't part of the selection (for
// right-click and drag).
func (u *UI) ensureSelected(i int) {
	u.mu.Lock()
	if !u.sel[i] {
		u.sel = map[int]bool{i: true}
		u.focus = i
		u.anchor = i
	}
	u.mu.Unlock()
}

// showMenuForSelection shows the Explorer shell menu at the cursor.
func (u *UI) showMenuForSelection() {
	paths := u.selectedPaths()
	if len(paths) == 0 {
		return
	}
	x, y, _ := cursorScreenPos()
	u.shell.ShowContextMenu(paths, x, y, func(shown bool, err error) {
		if err != nil {
			u.flash("Menu failed: " + err.Error())
			u.invalidate()
		}
	})
}

// ---- cells ----

func (u *UI) cellIcon(gtx layout.Context, r ResultRow, size unit.Dp) layout.Dimensions {
	px := gtx.Dp(size)
	gtx.Constraints.Min = image.Point{X: px, Y: px}
	gtx.Constraints.Max = image.Point{X: px, Y: px}
	bucket := 16
	if px >= 40 {
		bucket = 48
	} else if px >= 24 {
		bucket = 32
	}
	if im := u.shell.Icon(r.Path, r.IsDir, bucket); im != nil {
		op := paint.NewImageOp(im)
		op.Filter = paint.FilterNearest
		return widget.Image{Src: op, Fit: widget.Contain}.Layout(gtx)
	}
	return layout.Dimensions{Size: image.Point{X: px, Y: px}}
}

func (u *UI) cellText(gtx layout.Context, txt string, w float32) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Dp(Dp(w))
	gtx.Constraints.Max.X = gtx.Dp(Dp(w))
	return layout.Inset{Left: Dp(4), Right: Dp(4), Top: Dp(5), Bottom: Dp(5)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return label(u.th, Sp(13), txt, u.scheme.Text).Layout(gtx)
	})
}

func (u *UI) cellTextRight(gtx layout.Context, txt string, w float32) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Dp(Dp(w))
	gtx.Constraints.Max.X = gtx.Dp(Dp(w))
	return layout.Inset{Left: Dp(4), Right: Dp(8), Top: Dp(5), Bottom: Dp(5)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		l := label(u.th, Sp(13), txt, u.scheme.Text)
		l.Alignment = text.End
		return l.Layout(gtx)
	})
}

// cellNameSnippet renders name + optional snippet second line.
func (u *UI) cellNameSnippet(gtx layout.Context, r ResultRow, terms []string) layout.Dimensions {
	return layout.Inset{Left: Dp(6), Right: Dp(4), Top: Dp(4), Bottom: Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return label(u.th, Sp(13), r.Name, u.scheme.Text).Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if r.Snippet == "" {
					return layout.Dimensions{}
				}
				return u.snippetSpans(gtx, r.Snippet, terms)
			}),
		)
	})
}

// cellSnippet renders the Match column snippet (single line).
func (u *UI) cellSnippet(gtx layout.Context, snippet string, terms []string) layout.Dimensions {
	return layout.Inset{Left: Dp(4), Right: Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return u.snippetSpans(gtx, snippet, terms)
	})
}

// snippetSpans lays out snippet segments with match emphasis.
func (u *UI) snippetSpans(gtx layout.Context, snippet string, terms []string) layout.Dimensions {
	segs := highlightSegments(snippet, terms)
	if len(segs) == 0 {
		return layout.Dimensions{}
	}
	// Cap spans for pathological snippets.
	if len(segs) > 24 {
		segs = append(segs[:23], Segment{Text: "…"})
	}
	children := make([]layout.FlexChild, 0, len(segs))
	for _, sg := range segs {
		sg := sg
		c := u.scheme.Muted
		if sg.Match {
			c = u.scheme.Match
		}
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := label(u.th, Sp(12), sg.Text, c)
			l.Truncator = ""
			return l.Layout(gtx)
		}))
	}
	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, children...)
}

func sizeText(r ResultRow) string {
	if r.IsDir || !r.HasSize {
		return ""
	}
	return formatSizeShort(r.Size)
}

func modText(r ResultRow) string {
	if !r.HasMod {
		return ""
	}
	return r.Modified.Format("2006-01-02 15:04")
}

func (u *UI) typeText(r ResultRow) string {
	if r.IsDir {
		return "File folder"
	}
	return u.shell.TypeName(r.Path, false)
}

// ---- Icons grid ----

// layoutIconGrid renders rows as a wrapping grid of icon tiles.
func (u *UI) layoutIconGrid(gtx layout.Context, rows []ResultRow) layout.Dimensions {
	th := u.th
	u.ensureRowState(len(rows))
	tileW := gtx.Dp(Dp(104))
	cols := (gtx.Constraints.Max.X + gtx.Dp(Dp(8))) / (tileW + gtx.Dp(Dp(8)))
	if cols < 1 {
		cols = 1
	}
	u.mu.Lock()
	u.gridCols = cols
	u.mu.Unlock()
	nrows := (len(rows) + cols - 1) / cols
	return material.List(th, &u.iconList).Layout(gtx, nrows, func(gtx layout.Context, gi int) layout.Dimensions {
		var cells []layout.FlexChild
		for c := 0; c < cols; c++ {
			idx := gi*cols + c
			if idx >= len(rows) {
				cells = append(cells, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min.X = tileW
					gtx.Constraints.Max.X = tileW
					return layout.Dimensions{Size: image.Point{X: tileW}}
				}))
				continue
			}
			cells = append(cells, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.layoutTile(gtx, rows, idx, tileW)
			}))
		}
		return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, cells...)
	})
}

func (u *UI) layoutTile(gtx layout.Context, rows []ResultRow, i, tileW int) layout.Dimensions {
	s := u.scheme
	r := rows[i]
	u.mu.Lock()
	selected := u.sel[i]
	u.mu.Unlock()
	clk := &u.rowClicks[i]
	if c, ok := clk.Update(gtx); ok {
		u.clickRow(i, c.Modifiers, c.NumClicks)
	}
	tag := &u.rowTags[i]
	event.Op(gtx.Ops, tag)
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: tag, Kinds: pointer.Press | pointer.Release | pointer.Drag})
		if !ok {
			break
		}
		if e, ok := ev.(pointer.Event); ok {
			u.rowPointer(i, e, gtx)
		}
	}
	return clk.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = tileW
		gtx.Constraints.Max.X = tileW
		if selected {
			roundedFill(gtx, s.Selected, image.Point{X: tileW, Y: gtx.Constraints.Min.Y}, Dp(8))
		} else if clk.Hovered() {
			roundedFill(gtx, s.Hover, image.Point{X: tileW, Y: gtx.Constraints.Min.Y}, Dp(8))
		}
		return layout.Inset{Top: Dp(8), Bottom: Dp(8), Left: Dp(4), Right: Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return u.cellIcon(gtx, r, Dp(40))
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Top: Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						l := label(u.th, Sp(12), r.Name, u.scheme.Text)
						l.MaxLines = 2
						l.Alignment = text.Middle
						return l.Layout(gtx)
					})
				}),
			)
		})
	})
}

// ---- Grouped ----

// flatItem is one virtual row in grouped mode.
type flatItem struct {
	header bool
	dir    string
	row    int
}

func flattenGrouped(rows []ResultRow) []flatItem {
	var out []flatItem
	for _, g := range groupByFolder(rows) {
		out = append(out, flatItem{header: true, dir: g.Dir})
		for _, idx := range g.Rows {
			out = append(out, flatItem{dir: g.Dir, row: idx})
		}
	}
	return out
}

func (u *UI) layoutGrouped(gtx layout.Context, rows []ResultRow) layout.Dimensions {
	th := u.th
	s := u.scheme
	u.ensureRowState(len(rows))
	flat := flattenGrouped(rows)
	return material.List(th, &u.list).Layout(gtx, len(flat), func(gtx layout.Context, i int) layout.Dimensions {
		it := flat[i]
		if it.header {
			return layout.Inset{Left: Dp(12), Top: Dp(10), Bottom: Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return label(th, Sp(12), it.dir, s.Muted).Layout(gtx)
			})
		}
		return u.layoutRow(gtx, rows, it.row)
	})
}

// ---- rename card ----

func (u *UI) layoutRenameCard(gtx layout.Context, s Scheme) layout.Dimensions {
	th := u.th
	ed := material.Editor(th, &u.renameEd, "New name")
	ed.Color = s.Text
	ed.TextSize = Sp(14)
	if u.saveBtn.Clicked(gtx) {
		u.applyRename()
	}
	if u.cancelBtn.Clicked(gtx) {
		u.mu.Lock()
		u.renameRow = -1
		u.mu.Unlock()
	}
	mac := opRecord(gtx)
	dims := layout.UniformInset(Dp(20)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Dp(Dp(340))
		gtx.Constraints.Max.X = gtx.Dp(Dp(340))
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return label(th, Sp(14), "Rename", s.Text).Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: Dp(12), Bottom: Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return u.roundedField(gtx, func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: Dp(8), Right: Dp(8), Top: Dp(6), Bottom: Dp(6)}.Layout(gtx, ed.Layout)
					})
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceEnd}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return chip(gtx, th, s, &u.cancelBtn, "Cancel", false)
					}),
					layout.Rigid(layout.Spacer{Width: Dp(8)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return chip(gtx, th, s, &u.saveBtn, "Rename", true)
					}),
				)
			}),
		)
	})
	call := mac.Stop()
	roundedFill(gtx, s.Panel, dims.Size, Dp(12))
	border(gtx, dims.Size, s.Border, Dp(12), Dp(1))
	call.Add(gtx.Ops)
	return dims
}

