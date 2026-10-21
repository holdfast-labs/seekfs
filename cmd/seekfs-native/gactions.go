package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/widget"
)

// gactions.go: editor events, global keyboard shortcuts, navigation, and
// file verbs (open/reveal/rename/delete/copy).

// handleEditorEvents processes change/submit events for the editors. Called
// at the top of every frame.
func (u *UI) handleEditorEvents(gtx layout.Context) {
	for {
		ev, ok := u.searchEd.Update(gtx)
		if !ok {
			break
		}
		switch ev.(type) {
		case widget.ChangeEvent:
			u.typedQuery(u.searchEd.Text())
		case widget.SubmitEvent:
			u.commitQuery()
		}
	}
	for {
		ev, ok := u.scopeEd.Update(gtx)
		if !ok {
			break
		}
		switch ev.(type) {
		case widget.ChangeEvent:
			// Live-apply scope lightly: only store, search on submit.
			u.mu.Lock()
			u.under = u.scopeEd.Text()
			u.mu.Unlock()
		case widget.SubmitEvent:
			u.mu.Lock()
			u.under = u.scopeEd.Text()
			u.mu.Unlock()
			u.settings.Under = u.scopeEd.Text()
			u.commitQuery()
			u.saveSoon()
		}
	}
	if u.renameRow >= 0 {
		for {
			ev, ok := u.renameEd.Update(gtx)
			if !ok {
				break
			}
			if _, isSubmit := ev.(widget.SubmitEvent); isSubmit {
				u.applyRename()
			}
		}
	}
}

// handleGlobalKeys processes list-level shortcuts. The results area holds
// key focus via the event.Op tag registered during its layout; a single
// empty-name filter receives every key while focused.
func (u *UI) handleGlobalKeys(gtx layout.Context) {
	for {
		ev, ok := gtx.Event(key.Filter{Focus: &u.list})
		if !ok {
			return
		}
		e, ok := ev.(key.Event)
		if !ok || e.State != key.Press {
			continue
		}
		name := e.Name
		mods := e.Modifiers
		if u.settingsOpen {
			switch {
			case name == key.NameEscape:
				u.closeSettings()
			case name == key.NameReturn:
				u.applySettings()
			}
			continue
		}
		switch {
		case name == "," && mods.Contain(key.ModCtrl):
			u.openSettings()
		case name == key.NameReturn && mods.Contain(key.ModCtrl):
			u.revealSelected()
		case name == key.NameReturn:
			u.openSelected()
		case name == key.NameDeleteForward || name == key.NameDeleteBackward && mods.Contain(key.ModCtrl):
			u.deleteSelected()
		case name == key.NameF2:
			u.beginRename()
		case name == key.NameF5:
			u.rerun()
		case name == key.NameDownArrow:
			u.stepFocus(1, mods)
		case name == key.NameUpArrow:
			u.stepFocus(-1, mods)
		case name == key.NameHome:
			u.stepFocus(-1<<30, mods)
		case name == key.NameEnd:
			u.stepFocus(1<<30, mods)
		case name == "C" && mods.Contain(key.ModCtrl):
			u.copySelected()
		case name == "A" && mods.Contain(key.ModCtrl):
			u.selectAll()
		case name == "F" && mods.Contain(key.ModCtrl):
			u.focusSearch(gtx)
		case name == "L" && mods.Contain(key.ModCtrl):
			u.focusSearch(gtx)
		case name == "P" && mods.Contain(key.ModAlt):
			u.togglePreview()
		case name == key.NameEscape:
			if u.renameRow >= 0 {
				u.renameRow = -1
			}
		case len(name) == 1 && !mods.Contain(key.ModCtrl) && !mods.Contain(key.ModAlt):
			u.typeSelect(string(name))
		}
	}
}

// focusSearch moves key focus to the search box.
func (u *UI) focusSearch(gtx layout.Context) {
	gtx.Execute(key.FocusCmd{Tag: &u.searchEd})
}

// stepFocus moves focus/selection by delta rows (keyboard navigation).
func (u *UI) stepFocus(delta int, mods key.Modifiers) {
	u.mu.Lock()
	n := len(u.rows)
	if n == 0 {
		u.mu.Unlock()
		return
	}
	i := u.focus + delta
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	if mods.Contain(key.ModShift) {
		anchor := u.anchor
		if anchor < 0 {
			anchor = i
		}
		lo, hi := anchor, i
		if lo > hi {
			lo, hi = hi, lo
		}
		u.sel = map[int]bool{}
		for k := lo; k <= hi; k++ {
			u.sel[k] = true
		}
	} else {
		u.sel = map[int]bool{i: true}
		u.anchor = i
	}
	u.focus = i
	u.mu.Unlock()
	u.scrollTo(i)
}

// ---- navigation ----

func (u *UI) goBack() {
	u.mu.Lock()
	e, ok := u.history.Back()
	u.mu.Unlock()
	if ok {
		u.restoreHistory(e)
	}
}

func (u *UI) goForward() {
	u.mu.Lock()
	e, ok := u.history.Forward()
	u.mu.Unlock()
	if ok {
		u.restoreHistory(e)
	}
}

func (u *UI) restoreHistory(e HistoryEntry) {
	u.searchEd.SetText(e.Query)
	u.scopeEd.SetText(e.Under)
	u.mu.Lock()
	u.query, u.under = e.Query, e.Under
	u.contents = e.ContentsMode
	u.mu.Unlock()
	u.settings.ContentsMode = e.ContentsMode
	u.settings.Under = e.Under
	u.searcher.SearchNow(e.Query, e.ContentsMode, e.Under)
}

// ---- view / contents ----

func (u *UI) toggleContents() {
	u.mu.Lock()
	u.contents = !u.contents
	u.mu.Unlock()
	u.settings.ContentsMode = u.contents
	u.saveSoon()
	u.commitQuery()
}

func (u *UI) setView(m ViewMode) {
	u.mu.Lock()
	u.view = m
	u.mu.Unlock()
	u.settings.View = m.String()
	u.saveSoon()
}

func (u *UI) togglePreview() {
	u.settings.PreviewOpen = !u.previewOpen()
	u.saveSoon()
}

// ---- selection ----

func (u *UI) selectedRows() []ResultRow {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []ResultRow
	for i := range u.rows {
		if u.sel[i] {
			out = append(out, u.rows[i])
		}
	}
	return out
}

func (u *UI) selectedPaths() []string {
	var out []string
	for _, r := range u.selectedRows() {
		out = append(out, r.Path)
	}
	return out
}

func (u *UI) focusedRow() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.focus
}

func (u *UI) selectAll() {
	u.mu.Lock()
	for i := range u.rows {
		u.sel[i] = true
	}
	u.mu.Unlock()
}

// typeSelect moves focus/selection to the next row whose name starts with
// the accumulated prefix (500ms window).
func (u *UI) typeSelect(ch string) {
	u.mu.Lock()
	now := time.Now()
	if now.Sub(u.typeStamp) > 500*time.Millisecond {
		u.typeBuf = ""
	}
	u.typeStamp = now
	u.typeBuf += strings.ToLower(ch)
	prefix := u.typeBuf
	rows := u.rows
	start := u.focus
	u.mu.Unlock()
	if len(rows) == 0 || prefix == "" {
		return
	}
	for k := 1; k <= len(rows); k++ {
		i := (start + k) % len(rows)
		if len(rows[i].Name) == 0 {
			continue
		}
		if strings.HasPrefix(strings.ToLower(rows[i].Name), prefix) {
			u.mu.Lock()
			u.focus = i
			u.sel = map[int]bool{i: true}
			u.mu.Unlock()
			u.scrollTo(i)
			return
		}
	}
}

// ---- verbs ----

func (u *UI) openSelected() {
	for _, p := range u.selectedPaths() {
		if err := u.shell.Open(p); err != nil {
			u.flash("Open failed: " + err.Error())
			return
		}
	}
}

func (u *UI) revealSelected() {
	paths := u.selectedPaths()
	if len(paths) == 0 {
		return
	}
	if err := u.shell.Reveal(paths[0]); err != nil {
		u.flash("Reveal failed: " + err.Error())
	}
}

func (u *UI) propertiesSelected() {
	for _, p := range u.selectedPaths() {
		if err := u.shell.Properties(p); err != nil {
			u.flash("Properties failed: " + err.Error())
			return
		}
	}
}

func (u *UI) copySelected() {
	paths := u.selectedPaths()
	if len(paths) == 0 {
		return
	}
	if err := u.shell.CopyFiles(paths); err != nil {
		u.flash("Copy failed: " + err.Error())
		return
	}
	u.flash("Copied " + itoa(len(paths)) + " item(s)")
}

func (u *UI) deleteSelected() {
	paths := u.selectedPaths()
	if len(paths) == 0 {
		return
	}
	if err := u.shell.Recycle(paths); err != nil {
		if !strings.Contains(err.Error(), "cancelled") {
			u.flash("Delete failed: " + err.Error())
		}
		return
	}
	u.flash("Moved " + itoa(len(paths)) + " item(s) to the Recycle Bin")
	u.rerun()
}

// beginRename opens the rename overlay for the focused row.
func (u *UI) beginRename() {
	i := u.focusedRow()
	u.mu.Lock()
	valid := i >= 0 && i < len(u.rows)
	var name string
	if valid {
		name = u.rows[i].Name
	}
	u.mu.Unlock()
	if !valid {
		return
	}
	u.mu.Lock()
	u.renameRow = i
	u.mu.Unlock()
	u.renameEd.SetText(name)
}

// applyRename commits the rename overlay.
func (u *UI) applyRename() {
	u.mu.Lock()
	i := u.renameRow
	u.renameRow = -1
	var oldPath string
	if i >= 0 && i < len(u.rows) {
		oldPath = u.rows[i].Path
	}
	u.mu.Unlock()
	newName := strings.TrimSpace(u.renameEd.Text())
	if oldPath == "" || newName == "" || strings.ContainsAny(newName, `\/:*?"<>|`) {
		if newName != "" {
			u.flash("Invalid file name " + newName)
		}
		return
	}
	if err := os.Rename(oldPath, filepath.Join(filepath.Dir(oldPath), newName)); err != nil {
		u.flash("Rename failed: " + err.Error())
		return
	}
	u.rerun()
}

// flash shows a transient status message.
func (u *UI) flash(s string) {
	u.mu.Lock()
	u.flashMsg = s
	u.mu.Unlock()
}
