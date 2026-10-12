package main

import (
	"image"
	"image/color"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// gstate.go: the Gio app state — service client, search plumbing, rows,
// selection, theme, and every widget handle. The event loop (gmain.go)
// renders from this; the Searcher goroutine delivers results through
// deliver() + window invalidation, so typing never blocks.

// saveDebounce coalesces settings writes.
const saveDebounce = 800 * time.Millisecond

type UI struct {
	win      *app.Window
	invalidate func()

	client   *Client
	searcher *Searcher
	history  *History
	shell    shellBridge
	settings Settings

	mu       sync.Mutex
	rows     []ResultRow // sorted display order
	seq      int64
	query    string
	under    string
	contents bool
	view     ViewMode
	sortKey  SortKey
	sortAsc  bool
	sel      map[int]bool // selected row indices
	focus    int          // focused row (-1 none)
	fuzzyMsg string
	elapsedMS int64
	searchMS float64
	source   string
	content  *contentState
	unavail  string
	flashMsg string
	volumes  []DBVolume
	health   string
	healthMsg string
	entries  int

	// Theme.
	th       *material.Theme
	scheme   Scheme
	dark     bool
	accent   color.NRGBA

	// Widgets.
	searchEd   widget.Editor
	scopeEd    widget.Editor
	backBtn    widget.Clickable
	fwdBtn     widget.Clickable
	clearBtn   widget.Clickable
	contentsCk widget.Clickable
	viewBtns   [3]widget.Clickable
	list       widget.List
	iconList   widget.List
	sideList   widget.List
	previewList widget.List
	saveBtn    widget.Clickable
	cancelBtn  widget.Clickable
	renameEd   widget.Editor

	// Sidebar rows (rebuilt on volumes/saved change).
	sideRows   []sideRow
	sideClicks []widget.Clickable

	// gridCols is the last grid column count (for scroll mapping).
	gridCols int

	// Per-row + header interaction state.
	rowClicks []widget.Clickable
	rowTags   []rowTag
	colClicks []widget.Clickable
	divTags   map[SortKey]*divTag
	anchor    int

	// Column visibility for the current frame (priority-hide when narrow).
	showType  bool
	showMatch bool
	// Effective Name/Folder widths after shrink-to-fit.
	effName   float32
	effFolder float32
	// Preview splitter drag state.
	splitTag splitDrag

	// Preview image cache (bounded).
	imgCache map[string]image.Image

	// Column widths in dp (96-dpi px persisted).
	colW map[string]float32

	// Rename overlay: -1 = closed, else row index.
	renameRow int

	// Type-to-select buffer.
	typeBuf   string
	typeStamp time.Time

	// Drag state.
	dragFrom  int
	dragArmed bool

	saveTimer *time.Timer

	// Theme poll.
	lastDark bool

	// frameCount counts laid-out frames (debug logging).
	frameCount int
}

type sideRow struct {
	kind  sideKind
	label string
	under string // scope target
	query string // saved search to run
	depth int
}

// scrollTo moves the results lists so row i is visible.
func (u *UI) scrollTo(i int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.list.Position.First = i
	u.list.Position.Offset = 0
	cols := u.gridCols
	if cols < 1 {
		cols = 4
	}
	u.iconList.Position.First = i / cols
	u.iconList.Position.Offset = 0
}

type sideKind int

const (
	sideHeader sideKind = iota
	sideScope
	sideSaved
)
