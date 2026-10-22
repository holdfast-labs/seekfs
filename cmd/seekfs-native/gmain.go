package main

import (
	"os"
	"strings"
	"time"

	"gioui.org/app"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
)

// gmain.go: the Gio event loop — window creation, per-frame layout,
// ViewEvent HWND capture for frame theming, theme polling, result delivery,
// and settings persistence.

// newUI builds the app state. onFrame is called on the event-loop goroutine
// to request a new frame (window invalidation).
func newUI(pipe string, limit int, onFrame func()) *UI {
	settings := loadSettings()
	client := NewClient(pipe)
	u := &UI{
		client:    client,
		searcher:  NewSearcher(client, limit, 120*time.Millisecond),
		history:   NewHistory(100),
		shell:     newShellBridge(),
		settings:  settings,
		th:        newMaterialTheme(),
		sel:       map[int]bool{},
		focus:     -1,
		contents:  settings.ContentsMode,
		view:      viewModeFromString(settings.View),
		sortKey:   sortKeyFromString(settings.SortColumn),
		sortAsc:   settings.SortAsc,
		query:     "",
		under:     settings.Under,
		renameRow: -1,
		colW:      defaultColW(),
	}
	if settings.SortColumn == "" {
		u.sortAsc = true
	}
	for k, v := range settings.ColumnWidths {
		u.colW[k] = float32(v)
	}
	u.invalidate = onFrame
	u.searchEd.SingleLine = true
	u.searchEd.Submit = true
	u.scopeEd.SingleLine = true
	u.scopeEd.Submit = true
	u.scopeEd.SetText(settings.Under)
	u.list.Axis = layout.Vertical
	u.iconList.Axis = layout.Vertical
	u.sideList.Axis = layout.Vertical
	u.previewList.Axis = layout.Vertical
	u.renameEd.SingleLine = true
	u.renameEd.Submit = true
	u.dark = u.shell.SystemDark()
	u.accent = u.shell.SystemAccent()
	// SEEKFS_NATIVE_THEME=dark|light overrides the OS theme (screenshots).
	switch os.Getenv("SEEKFS_NATIVE_THEME") {
	case "dark":
		u.dark = true
	case "light":
		u.dark = false
	}
	dbgLog("newUI dark=%v accent=%v", u.dark, u.accent)
	u.applyScheme()
	u.searcher.OnDeliver(func(res SearchResult) { u.deliver(res) })
	u.rebuildSideRows(nil)
	u.anchor = -1
	return u
}

func defaultColW() map[string]float32 {
	return map[string]float32{
		"name": 240, "folder": 220, "size": 100, "modified": 170, "type": 140, "match": 320,
	}
}

func (u *UI) applyScheme() {
	u.scheme = schemeFor(u.dark, u.accent)
	restyle(u.th, u.scheme)
	// SEEKFS_NATIVE_NOMICA=1 disables all DWM frame calls (diagnosing a
	// white-client issue: Mica/backdrop vs swapchain interaction).
	if os.Getenv("SEEKFS_NATIVE_NOMICA") == "" {
		u.shell.ApplyFrame(u.dark)
	}
	u.lastDark = u.dark
}

// useGDI reports whether to present through the GDI fallback (headless GPU
// render + StretchDIBits) instead of Gio's DXGI swapchain. DXGI presents
// never composite on some real machines (headless dGPU + capture/virtual
// displays report DXGI_STATUS_OCCLUDED forever); GDI is universally visible.
// Default follows SEEKFS_NATIVE_PRESENTER (gdi|dxgi); gdi is the default
// until DXGI is verified on the target machine.
func useGDI() bool {
	switch os.Getenv("SEEKFS_NATIVE_PRESENTER") {
	case "dxgi":
		return false
	case "gdi":
		return true
	default:
		return true
	}
}

// runUI creates the window and runs the event loop. It returns when the
// window closes.
func runUI(pipe string, limit int) error {
	w := new(app.Window)
	w.Option(app.Title("seekfs"))
	gdi := useGDI()
	if gdi {
		w.Option(app.CustomRenderer(true))
	}
	u := newUI(pipe, limit, w.Invalidate)
	u.win = w
	u.gdiPresent = gdi
	if err := startUIAutomation(u); err != nil {
		return err
	}
	defer stopUIAutomation()

	// Restore window size (settings store 96-dpi px; Gio takes dp).
	ww, hh := u.settings.WindowW, u.settings.WindowH
	if ww < 640 {
		ww = 1180
	}
	if hh < 400 {
		hh = 760
	}
	w.Option(app.Size(unit.Dp(float32(ww)/float32(dpScale)), unit.Dp(float32(hh)/float32(dpScale))))

	// Async service handshake so the window is interactive fast.
	go u.initialInfo()

	// Startup query for deterministic screenshots/tests.
	if q := os.Getenv("SEEKFS_NATIVE_QUERY"); q != "" {
		u.searchEd.SetText(q)
		u.commitQuery()
	}

	// Theme poll: re-render when the OS theme or accent changes.
	go u.themePoll(w)
	// Background indexing poll: cheap frames for the progress bar and a
	// settings-get every 500 ms while any drive is indexing.
	go u.indexProgressPoll(w)

	var ops op.Ops
	for {
		e := w.Event()
		switch e := e.(type) {
		case app.DestroyEvent:
			u.saveNow()
			return e.Err
		case app.ViewEvent:
			u.onViewEvent(e)
		case app.ConfigEvent:
			u.onConfig(e)
		case app.FrameEvent:
			uiautoFrameBegin(u, e)
			gtx := app.NewContext(&ops, e)
			u.layout(gtx)
			u.maybeSaveShot(gtx)
			if u.gdiPresent {
				u.presentGDI(gtx)
			}
			e.Frame(gtx.Ops)
			uiautoFrameEnd(u)
		}
	}
}

// onConfig persists window size changes (debounced). Config.Size is in
// device pixels; settings keep 96-dpi px, converted at startup.
func (u *UI) onConfig(e app.ConfigEvent) {
	u.mu.Lock()
	u.settings.WindowW = e.Config.Size.X
	u.settings.WindowH = e.Config.Size.Y
	u.mu.Unlock()
	u.saveSoon()
}

// themePoll watches the OS theme and invalidates on change.
func (u *UI) themePoll(w *app.Window) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		dark := u.shell.SystemDark()
		accent := u.shell.SystemAccent()
		switch os.Getenv("SEEKFS_NATIVE_THEME") {
		case "dark":
			dark = true
		case "light":
			dark = false
		}
		u.mu.Lock()
		changed := dark != u.dark || accent != u.accent
		u.mu.Unlock()
		if changed {
			u.mu.Lock()
			u.dark, u.accent = dark, accent
			u.mu.Unlock()
			dbgLog("theme-change dark=%v accent=%v", dark, accent)
			u.applyScheme()
			w.Invalidate()
		}
	}
}

// indexProgressPoll renders the progress animation at at most 10 Hz and polls
// settings-get every 500 ms while a drive is indexing. It stops polling (but
// keeps the cheap ticker) when no build is active.
func (u *UI) indexProgressPoll(w *app.Window) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	lastPoll := time.Time{}
	for range ticker.C {
		u.mu.Lock()
		active := u.settingsIndexing
		u.mu.Unlock()
		if !active {
			continue
		}
		w.Invalidate()
		if time.Since(lastPoll) < 500*time.Millisecond {
			continue
		}
		lastPoll = time.Now()
		resp, err := u.client.SettingsGet()
		if err == nil && resp.OK && resp.Settings != nil {
			u.postSettingsMerge(resp)
		}
	}
}

// initialInfo runs the service handshake off the UI thread.
func (u *UI) initialInfo() {
	resp, err := u.ensureService()
	u.mu.Lock()
	u.noVolumes = err == nil && len(resp.DBs) == 0
	if err != nil {
		u.health, u.healthMsg = serviceHealthError, err.Error()
	} else if resp.Loading {
		u.health, u.healthMsg = serviceHealthDegraded, "Loading indexes…"
		u.entries = resp.Entries
	} else if resp.OK && resp.Entries > 0 {
		u.health = resp.Health
		if u.health == "" {
			u.health = serviceHealthOK
		}
		u.healthMsg = resp.HealthMessage
		u.entries = resp.Entries
	} else if u.noVolumes {
		u.health, u.healthMsg = serviceHealthEmpty, "No drives indexed yet"
	} else {
		u.health, u.healthMsg = serviceHealthError, firstNonEmpty(resp.Message, "service has no search indexes loaded")
	}
	vols := dbVolumesFrom(resp)
	u.mu.Unlock()
	u.rebuildSideRows(vols)
	u.invalidate()
	go u.seedSettingsSnapshot()
}

// deliver handles a Searcher result on the searcher goroutine: store under
// lock, then invalidate. Stale sequences are dropped.
func (u *UI) deliver(res SearchResult) {
	u.mu.Lock()
	if res.Seq < u.seq {
		u.mu.Unlock()
		return
	}
	u.seq = res.Seq
	if res.Err != nil {
		if contentUnavailable(res.Err) {
			u.unavail = res.Message
		} else {
			u.fuzzyMsg = res.Message
		}
		u.rows = nil
	} else if !res.OK {
		if contentUnavailableString(res.Message) {
			u.unavail = res.Message
		} else {
			u.fuzzyMsg = res.Message
		}
		u.rows = nil
		u.content = res.Content
	} else {
		u.rows = append([]ResultRow(nil), res.Rows...)
		u.fuzzyMsg = res.Message
		u.elapsedMS = res.ElapsedMS
		u.searchMS = res.SearchMS
		u.source = res.Source
		u.content = res.Content
		u.unavail = ""
		if strings.TrimSpace(res.Query) != "" {
			u.history.Push(HistoryEntry{Query: res.Query, Under: u.under, ContentsMode: res.ContentMode})
		}
	}
	u.sortLocked()
	u.sel = map[int]bool{}
	u.focus = -1
	n := len(u.rows)
	u.mu.Unlock()
	dbgLog("deliver seq=%d okquery=%q rows=%d unavail=%q fuzzy=%q", res.Seq, res.Query, n, u.unavailSafe(), res.Message)
	u.invalidate()
}

func (u *UI) unavailSafe() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.unavail
}

// sortLocked sorts u.rows in place. Caller holds u.mu.
func (u *UI) sortLocked() {
	if u.view == ViewGrouped {
		sortRows(u.rows, SortFolder, true)
	} else {
		sortRows(u.rows, u.sortKey, u.sortAsc)
	}
}

// typedQuery queues a debounced live search from the search box.
func (u *UI) typedQuery(text string) {
	u.mu.Lock()
	u.query = text
	contents, under := u.contents, u.under
	u.mu.Unlock()
	u.searcher.Query(text, contents, under)
}

// commitQuery sends immediately (Enter / scope change / toggle).
func (u *UI) commitQuery() {
	u.mu.Lock()
	text := u.searchEd.Text()
	u.query = text
	contents, under := u.contents, u.under
	u.mu.Unlock()
	u.searcher.SearchNow(text, contents, under)
}

// rerun re-issues the current query text.
func (u *UI) rerun() {
	u.mu.Lock()
	contents, under := u.contents, u.under
	text := u.searchEd.Text()
	u.mu.Unlock()
	u.searcher.SearchNow(text, contents, under)
}

// saveSoon schedules a debounced settings write.
func (u *UI) saveSoon() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.saveTimer != nil {
		u.saveTimer.Stop()
	}
	u.saveTimer = time.AfterFunc(saveDebounce, func() { u.saveNow() })
}

// saveNow captures state and writes the settings file.
func (u *UI) saveNow() {
	u.mu.Lock()
	s := u.settings
	s.SortColumn = u.sortKey.String()
	s.SortAsc = u.sortAsc
	s.View = u.view.String()
	s.ContentsMode = u.contents
	s.Under = u.under
	widths := make(map[string]int, len(u.colW))
	for k, v := range u.colW {
		widths[k] = int(v)
	}
	s.ColumnWidths = widths
	s.Saved = append([]string(nil), u.settings.Saved...)
	u.mu.Unlock()
	_ = saveSettings(s)
}

// paintBG fills the frame background (explicit full-frame clip; a bare
// PaintOp is an infinite plane).
func (u *UI) paintBG(gtx layout.Context) {
	size := gtx.Constraints.Max
	defer clip.Rect{Max: size}.Push(gtx.Ops).Pop()
	paint.ColorOp{Color: u.scheme.BG}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
}

// layout is the root frame layout.
func (u *UI) layout(gtx layout.Context) layout.Dimensions {
	u.frameCount++
	if u.frameCount <= 5 || u.frameCount%300 == 0 {
		u.mu.Lock()
		dark, n := u.dark, len(u.rows)
		u.mu.Unlock()
		dbgLog("frame=%d dark=%v rows=%d size=%v schemeBG=%v", u.frameCount, dark, n, gtx.Constraints.Max, u.scheme.BG)
	}
	u.handleEditorEvents(gtx)
	u.handleGlobalKeys(gtx)
	u.consumeSettingsPending()
	u.paintBG(gtx)
	body := layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.layoutToolbar(gtx)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return u.layoutBody(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.layoutStatus(gtx)
		}),
	)
	if !u.settingsOpen {
		return body
	}
	return layout.Stack{}.Layout(gtx,
		layout.Stacked(func(gtx layout.Context) layout.Dimensions { return body }),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return u.layoutSettingsOverlay(gtx)
		}),
	)
}

// layoutBody is sidebar | results | splitter | preview. The results list
// never draws outside its own region: columns hide by priority when narrow
// and the area is hard-clipped as a backstop. The preview auto-narrows (to
// 200px) and auto-collapses below a 450px minimum list width.
func (u *UI) layoutBody(gtx layout.Context) layout.Dimensions {
	previewOpen := u.previewOpen()
	previewW := u.previewWidth(gtx)
	totalDP := float32(gtx.Constraints.Max.X) / float32(gtx.Metric.PxPerDp)
	if previewOpen {
		resultsW := totalDP - 220 - 7 - float32(previewW)
		if resultsW < 450 {
			previewW = 200
			resultsW = totalDP - 220 - 7 - 200
			if resultsW < 450 {
				previewOpen = false
			}
		}
	}
	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Dp(Dp(220))
			gtx.Constraints.Max.X = gtx.Dp(Dp(220))
			return u.layoutSidebar(gtx)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return u.layoutResultsClipped(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !previewOpen {
				return layout.Dimensions{}
			}
			return u.layoutSplitter(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !previewOpen {
				return layout.Dimensions{}
			}
			gtx.Constraints.Min.X = gtx.Dp(Dp(float32(previewW)))
			gtx.Constraints.Max.X = gtx.Dp(Dp(float32(previewW)))
			return u.layoutPreview(gtx)
		}),
	)
}

// previewWidth returns the preview pane width in dp (persisted).
func (u *UI) previewWidth(gtx layout.Context) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	w := u.settings.PreviewW
	if w < 200 {
		w = 330
	}
	if w > 600 {
		w = 600
	}
	return w
}

// layoutResultsClipped hard-clips results to their own region so columns
// can never draw under the preview or sidebar.
func (u *UI) layoutResultsClipped(gtx layout.Context) layout.Dimensions {
	size := gtx.Constraints.Max
	defer clip.Rect{Max: size}.Push(gtx.Ops).Pop()
	return u.layoutResults(gtx)
}

type splitDrag struct {
	start float32
	w0    int
	on    bool
}

// layoutSplitter is the draggable preview boundary (7px handle + 1px line).
func (u *UI) layoutSplitter(gtx layout.Context) layout.Dimensions {
	s := u.scheme
	tag := &u.splitTag
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
			tag.w0 = u.previewWidth(gtx)
			tag.on = true
		case pointer.Drag:
			if tag.on {
				dx := (e.Position.X - tag.start) / gtx.Metric.PxPerDp
				w := tag.w0 - int(dx)
				if w < 200 {
					w = 200
				}
				if w > 600 {
					w = 600
				}
				u.mu.Lock()
				u.settings.PreviewW = w
				u.mu.Unlock()
			}
		case pointer.Release, pointer.Cancel:
			if tag.on {
				tag.on = false
				u.saveSoon()
			}
		}
	}
	gtx.Constraints.Min.X = gtx.Dp(Dp(7))
	gtx.Constraints.Max.X = gtx.Dp(Dp(7))
	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return vline(gtx, s.Border)
	})
}

func (u *UI) previewOpen() bool { return u.settings.PreviewOpen }
