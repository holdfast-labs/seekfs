//go:build windows

package main

import (
	"strings"
	"syscall"
	"unsafe"
)

// results_windows.go: the virtual results ListView.
//
// LVS_OWNERDATA: the control asks for text/icon per row (LVN_GETDISPINFO) and
// never copies the data, so millions of rows stay instant. Type-to-select is
// LVN_ODFINDITEM. Sorting is client-side over ResultRow (service order is
// already relevance/name order; header clicks re-sort). Content snippets
// render under the name via custom-draw with the matched term emphasized.

var resultColumns = []struct {
	key   SortKey
	title string
	width int32 // 96-dpi
	align int32
}{
	{SortName, "Name", 280, lvColFmtLeft},
	{SortFolder, "Folder", 260, lvColFmtLeft},
	{SortSize, "Size", 110, lvColFmtRight},
	{SortModified, "Date modified", 150, lvColFmtLeft},
	{SortType, "Type", 150, lvColFmtLeft},
	{SortSnippet, "Match", 320, lvColFmtLeft},
}

func (a *app) createResults() {
	inst := instanceHandle()
	style := uintptr(wsChild | wsVisible | wsClipSiblings | lvsReport | lvsOwnerData | lvsShowSelAlways | lvsEditLabels)
	if a.view != ViewDetails {
		style = uintptr(wsChild | wsVisible | wsClipSiblings | lvsIcon | lvsOwnerData | lvsShowSelAlways | lvsAutoArrange)
	}
	hwnd, _, _ := syscall.SyscallN(pCreateWindowExW.Addr(), uintptr(wsExClientEdge),
		uintptr(unsafe.Pointer(utf16("SysListView32"))), 0,
		style,
		0, 0, 10, 10,
		a.hwnd, uintptr(idResults), inst, 0)
	a.results = hwnd
	if f := a.font; f != 0 {
		sendMessage(hwnd, 0x0030 /*WM_SETFONT*/, f, 1)
	}
	sendMessage(hwnd, lvmSetExtendedStyle, 0,
		lvExFullRowSelect|lvExDoubleBuffer|lvExLabelTip|lvExInfoTip)
	// Snippet rows need two lines. The ListView row height comes from the
	// small image list; swap in a taller imageless placeholder only while
	// any visible row carries a snippet.
	a.updateRowHeight()
	if a.iconSmall != 0 {
		sendMessage(hwnd, lvmSetImageList, lvsilSmall, a.iconSmall)
	}
	if a.iconLarge != 0 {
		sendMessage(hwnd, lvmSetImageList, lvsilNormal, a.iconLarge)
	}
	a.setupResultColumns()
	subclassControl(hwnd)
	setControlHandler(hwnd, a.resultsProc)
	a.resultsHdr = sendMessage(hwnd, lvmGetHeader, 0, 0)
	a.applyViewMode()
}

const lvsAutoArrange = 0x0100

// setupResultColumns (re)creates the Details columns, restoring widths.
func (a *app) setupResultColumns() {
	// Remove existing.
	for sendMessage(a.results, 0x1200 /*HDM_GETITEMCOUNT*/, 0, 0) != 0 {
		_ = 0
		break
	}
	for i, col := range resultColumns {
		w := col.width
		if saved, ok := a.settings.ColumnWidths[col.key.String()]; ok && saved > 20 {
			w = int32(saved)
		}
		w = scaleDPI(w, a.dpi)
		title, _ := syscall.UTF16PtrFromString(col.title)
		lvc := lvColumn{Mask: 0x0001 /*LVCF_FMT*/ | 0x0002 /*LVCF_WIDTH*/ | 0x0004 /*LVCF_TEXT*/ | 0x0008 /*LVCF_SUBITEM*/,
			Fmt: col.align, CX: w, PszText: title, ISubItem: int32(i)}
		sendMessage(a.results, lvmInsertColumnW, uintptr(i), uintptr(unsafe.Pointer(&lvc)))
	}
	a.updateHeaderSort()
}

func (a *app) resultsHeader() uintptr {
	if a.resultsHdr != 0 {
		return a.resultsHdr
	}
	a.resultsHdr = sendMessage(a.results, lvmGetHeader, 0, 0)
	return a.resultsHdr
}

// applyViewMode switches Details / Icons / Grouped presentation.
func (a *app) applyViewMode() {
	style := getWindowLong(a.results, gwlStyle)
	const viewMask = uintptr(lvsReport | lvsIcon | lvsSmallIcon | lvsList)
	style &^= viewMask
	switch a.view {
	case ViewIcons:
		style |= lvsIcon
	case ViewGrouped:
		style |= lvsReport // grouped renders as Details with group headers
	default:
		style |= lvsReport
	}
	setWindowLong(a.results, gwlStyle, style)
	setWindowText(a.viewBtn, viewButtonLabel(a.view))
	a.refreshRows()
}

// refreshRows re-sorts and pushes the row count to the virtual list.
func (a *app) refreshRows() {
	a.mu.Lock()
	rows := a.rows
	key, asc := a.sortKey, a.sortAsc
	view := a.view
	a.mu.Unlock()
	working := append([]ResultRow(nil), rows...)
	if view == ViewGrouped {
		// Grouped: folder order first, then the active sort within folders.
		sortRows(working, SortFolder, true)
	} else {
		sortRows(working, key, asc)
	}
	a.mu.Lock()
	a.sortedRows = working
	a.mu.Unlock()
	a.rebuildFlatList()
	n := len(a.flatListSnapshot())
	sendMessage(a.results, lvmSetItemCount, uintptr(n), 0)
	a.updateRowHeight()
	invalidate(a.results)
	a.updatePreviewSelection()
}

func (a *app) onResultsNotify(hdr *nmhdr, l uintptr) uintptr {
	switch hdr.Code {
	case lvnGetDispInfoW:
		di := notifyLPARAM[*lvDispInfo](l)
		if uintptr(unsafe.Pointer(di)) != l {
			return 0
		}
		a.onGetDispInfo(di)
		return 0
	case lvnOdfindItemW:
		return a.onFindItem(notifyLPARAM[*lvFindItem](l))
	case lvnItemChanged:
		nm := notifyLPARAM[*nmListView](l)
		if nm.Changed&0x0002 /*LVIF_STATE*/ != 0 && nm.NewState&lvisSelected != 0 {
			a.onSelectionChanged()
		}
		return 0
	case lvnGetInfoTipW:
		a.onGetInfoTip(notifyLPARAM[*lvInfoTip](l))
		return 0
	case lvnBeginLabelEditW:
		return a.onBeginLabelEdit(notifyLPARAM[*lvDispInfo](l))
	case lvnEndLabelEditW:
		return a.onEndLabelEdit(notifyLPARAM[*lvDispInfo](l))
	case lvnKeyDown:
		return a.onResultsKeyDown(notifyLPARAM[*lvKeyDown](l))
	case nmDblClk:
		a.openSelected()
		return 1
	case nmRClick:
		a.showContextMenu()
		return 1
	case nmnCustomDraw:
		// Custom-draw deliberately defaults to CDRF_DODEFAULT. Snippet
		// emphasis renders in the owner-drawn snippet column via the
		// NMLVCUSTOMDRAW path only when the buffer layout is verified;
		// until then the snippet shows as plain second-column text.
		return cdrfDodefault
	case lvnColumnClick:
		nm := notifyLPARAM[*nmListView](l)
		a.onColumnClick(int(nm.ISubItem))
		return 0
	case lvnBeginDrag:
		a.beginDrag()
		return 0
	}
	return 0
}

// lvFindItem mirrors NMLVFINDITEM: hdr, iStart, lvfi.
type lvFindItem struct {
	Hdr    nmhdr
	IStart int32
	_      [4]byte
	Find   lvFindInfo
}

type lvKeyDown struct {
	Hdr   nmhdr
	WVKey uint16
	Flags uint32
}

// lvInfoTip mirrors NMLVGETINFOTIP: hdr, flags, text buffer, item/subitem.
type lvInfoTip struct {
	Hdr       nmhdr
	Flags     uint32
	PszText   *uint16
	CchTextMax int32
	IItem     int32
	ISubItem  int32
	LParam    uintptr
}

// onGetInfoTip shows the folder path (and snippet) as the row tooltip.
func (a *app) onGetInfoTip(tip *lvInfoTip) {
	i := a.displayRow(int(tip.IItem))
	a.mu.Lock()
	rows := a.sortedRows
	a.mu.Unlock()
	if i < 0 || i >= len(rows) {
		return
	}
	r := rows[i]
	text := r.Path
	if r.Snippet != "" {
		text += "\n" + r.Snippet
	}
	putLVText(tip.PszText, int(tip.CchTextMax), text)
}

// onGetDispInfo serves text + icon index for a virtual row.
func (a *app) onGetDispInfo(di *lvDispInfo) {
	if dispInfoLogCount < 3 {
		dispInfoLogCount++
		heartbeat("dispinfo iitem=" + itoa(int(di.Item.IItem)) + " sub=" + itoa(int(di.Item.ISubItem)) + " mask=" + itoa(int(di.Item.Mask)))
	}
	i := a.displayRow(int(di.Item.IItem))
	if i < 0 {
		return
	}
	a.mu.Lock()
	rows := a.sortedRows
	a.mu.Unlock()
	if i >= len(rows) {
		return
	}
	r := rows[i]
	if di.Item.Mask&lvifText != 0 && di.Item.PszText != nil {
		var s string
		switch int(di.Item.ISubItem) {
		case 0:
			s = r.Name
		case 1:
			s = r.Dir
		case 2:
			if r.HasSize && !r.IsDir {
				s = formatByteSize(r.Size)
			}
		case 3:
			if r.HasMod {
				s = formatSystemTime(r.Modified.Unix(), 0)
			}
		case 4:
			if r.IsDir {
				s = "File folder"
			} else {
				s = a.icons.typeName(r.Path, false)
			}
		case 5:
			s = r.Snippet
		}
		putLVText(di.Item.PszText, int(di.Item.CchTextMax), s)
	}
	if di.Item.Mask&lvifImage != 0 {
		di.Item.IImage = a.icons.iconIndex(r.Path, r.IsDir)
	}
}

// flatGroup holds one folder group flattened for the virtual list.
type flatGroup struct {
	header bool
	dir    string
	row    int // index into sortedRows when !header
}

// flatRows returns the virtual-list mapping: in grouped mode, a header entry
// per folder followed by its row indices; otherwise the identity mapping.
func (a *app) flatRows() []flatGroup {
	a.mu.Lock()
	rows := a.sortedRows
	view := a.view
	a.mu.Unlock()
	if view != ViewGrouped {
		out := make([]flatGroup, len(rows))
		for i := range rows {
			out[i] = flatGroup{row: i}
		}
		return out
	}
	var out []flatGroup
	for _, g := range groupByFolder(rows) {
		out = append(out, flatGroup{header: true, dir: g.Dir})
		for _, idx := range g.Rows {
			out = append(out, flatGroup{dir: g.Dir, row: idx})
		}
	}
	return out
}

// rebuildFlatList recomputes the virtual-list mapping from sortedRows.
func (a *app) rebuildFlatList() {
	a.mu.Lock()
	rows := a.sortedRows
	view := a.view
	a.mu.Unlock()
	var flat []flatGroup
	if view != ViewGrouped {
		flat = make([]flatGroup, len(rows))
		for i := range rows {
			flat[i] = flatGroup{row: i}
		}
	} else {
		for _, g := range groupByFolder(rows) {
			flat = append(flat, flatGroup{header: true, dir: g.Dir})
			for _, idx := range g.Rows {
				flat = append(flat, flatGroup{dir: g.Dir, row: idx})
			}
		}
	}
	a.mu.Lock()
	a.flatList = flat
	a.mu.Unlock()
}

func (a *app) flatListSnapshot() []flatGroup {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.flatList
}

// displayRow maps a virtual index to the sorted-row index (-1 for group
// headers or out of range). The flat list is cached per refresh.
func (a *app) displayRow(virt int) int {
	flat := a.flatListSnapshot()
	if virt < 0 || virt >= len(flat) {
		return -1
	}
	if flat[virt].header {
		return -1
	}
	return flat[virt].row
}

func putLVText(dst *uint16, max int, s string) {
	if max <= 1 {
		return
	}
	u, _ := syscall.UTF16FromString(s)
	n := len(u) - 1
	if n > max-1 {
		n = max - 1
	}
	copy(unsafe.Slice(dst, max), u[:n])
	unsafe.Slice(dst, max)[n] = 0
}

// onFindItem implements type-to-select: find the next row whose name starts
// with the typed prefix, wrapping from iStart.
func (a *app) onFindItem(fi *lvFindItem) uintptr {
	a.mu.Lock()
	rows := a.sortedRows
	a.mu.Unlock()
	if fi.Find.Psz == nil || len(rows) == 0 {
		return ^uintptr(0) // -1: not found
	}
	prefix := strings.ToLower(syscall.UTF16ToString(unsafe.Slice(fi.Find.Psz, 260)))
	if prefix == "" {
		return ^uintptr(0)
	}
	start := int(fi.IStart)
	if start < 0 || start >= len(rows) {
		start = 0
	}
	for k := 1; k <= len(rows); k++ {
		i := (start + k) % len(rows)
		if strings.HasPrefix(strings.ToLower(rows[i].Name), prefix) {
			return uintptr(i)
		}
	}
	return ^uintptr(0)
}

func (a *app) onHeaderNotify(hdr *nmhdr) uintptr {
	switch hdr.Code {
	case hdnItemClickW:
		_ = hdr
		return 0
	case hdnEndTrackW, hdnDividerDblClickW:
		a.saveColumnWidths()
		return 0
	}
	return 0
}

// onColumnClick sorts by the clicked column, toggling direction. The Match
// column is display-only and keeps the current sort.
func (a *app) onColumnClick(sub int) {
	if sub < 0 || sub >= len(resultColumns) {
		return
	}
	key := resultColumns[sub].key
	if key == SortSnippet {
		return
	}
	a.mu.Lock()
	if a.sortKey == key {
		a.sortAsc = !a.sortAsc
	} else {
		a.sortKey = key
		a.sortAsc = true
	}
	a.settings.SortColumn = a.sortKey.String()
	a.settings.SortAsc = a.sortAsc
	a.mu.Unlock()
	a.updateHeaderSort()
	a.refreshRows()
	a.saveSoon()
}

func (a *app) updateHeaderSort() {
	hdr := a.resultsHeader()
	if hdr == 0 {
		return
	}
	a.mu.Lock()
	key, asc := a.sortKey, a.sortAsc
	a.mu.Unlock()
	n := sendMessage(hdr, hdmGetItemCount, 0, 0)
	for i := 0; i < int(n); i++ {
		var item hdItem
		item.Mask = hdiFormat
		// HDM_GETITEMW.
		sendMessage(hdr, hdmGetItemW, uintptr(i), uintptr(unsafe.Pointer(&item)))
		item.Fmt &^= hdfSortUp | hdfSortDown
		if i < len(resultColumns) && resultColumns[i].key == key {
			if asc {
				item.Fmt |= hdfSortUp
			} else {
				item.Fmt |= hdfSortDown
			}
		}
		sendMessage(hdr, hdmSetItemW, uintptr(i), uintptr(unsafe.Pointer(&item)))
	}
}

type hdItem struct {
	Mask       uint32
	CXY        int32
	PszText    *uint16
	Hbm        uintptr
	CchTextMax int32
	Fmt        int32
	LParam     uintptr
	IImage     int32
	IOrder     int32
	Type       uint32
	PvFilter   uintptr
	State      uint32
}

func (a *app) saveColumnWidths() {
	hdr := a.resultsHeader()
	if hdr == 0 {
		return
	}
	// LVM_GETCOLUMNWIDTH per subitem.
	for i, col := range resultColumns {
		w := sendMessage(a.results, lvmFirst+29 /*LVM_GETCOLUMNWIDTH*/, uintptr(i), 0)
		if w > 0 {
			// Unscale to 96-dpi units for persistence across DPI changes.
			a.settings.ColumnWidths[col.key.String()] = int((int32(w)*96 + int32(a.dpi)/2) / int32(a.dpi))
		}
	}
	a.saveSoon()
}

// ---- selection ----

func (a *app) selectedIndices() []int {
	var out []int
	i := int(sendMessage(a.results, lvmGetNextItem, ^uintptr(0), lvniSelected))
	for i >= 0 {
		out = append(out, i)
		i = int(sendMessage(a.results, lvmGetNextItem, uintptr(i), lvniSelected))
	}
	return out
}

func (a *app) selectedIndex() int {
	i := int(sendMessage(a.results, lvmGetNextItem, ^uintptr(0), lvniSelected|lvniFocused))
	if i < 0 {
		i = int(sendMessage(a.results, lvmGetNextItem, ^uintptr(0), lvniSelected))
	}
	return i
}

func (a *app) selectedRows() []ResultRow {
	a.mu.Lock()
	rows := a.sortedRows
	a.mu.Unlock()
	var out []ResultRow
	for _, i := range a.selectedIndices() {
		if i >= 0 && i < len(rows) {
			out = append(out, rows[i])
		}
	}
	return out
}

func (a *app) selectedPaths() []string {
	var out []string
	for _, r := range a.selectedRows() {
		out = append(out, r.Path)
	}
	return out
}

func (a *app) selectAll() {
	n := int(sendMessage(a.results, lvmGetItemCount, 0, 0))
	for i := 0; i < n; i++ {
		var item lvItem
		item.Mask = lvifState
		item.IItem = int32(i)
		item.State = lvisSelected
		item.StateMask = lvisSelected
		sendMessage(a.results, lvmSetItemState, uintptr(i), uintptr(unsafe.Pointer(&item)))
	}
}

func (a *app) focusResultsMove(delta int) {
	n := int(sendMessage(a.results, lvmGetItemCount, 0, 0))
	if n == 0 {
		return
	}
	i := a.selectedIndex() + delta
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	var item lvItem
	item.Mask = lvifState
	item.IItem = int32(i)
	item.State = lvisSelected | lvisFocused
	item.StateMask = lvisSelected | lvisFocused
	sendMessage(a.results, lvmSetItemState, uintptr(i), uintptr(unsafe.Pointer(&item)))
	sendMessage(a.results, lvmSetSelectionMark, 0, uintptr(i))
	sendMessage(a.results, lvmEnsureVisible, uintptr(i), 0)
	_, _, _ = syscall.SyscallN(pSetFocus.Addr(), a.results)
}

// onSelectionChanged updates the preview pane and prebuilds the context menu.
func (a *app) onSelectionChanged() {
	a.updatePreviewSelection()
	paths := a.selectedPaths()
	if len(paths) > 0 && len(paths) <= 32 {
		prebuildShellContextMenu(paths)
	}
}

// ---- keyboard on the results list ----

func (a *app) onResultsKeyDown(kd *lvKeyDown) uintptr {
	ctrl := keyDown(vkControlP)
	switch kd.WVKey {
	case vkReturn:
		if ctrl {
			a.revealSelected()
		} else {
			a.openSelected()
		}
		return 1
	case vkDelete:
		a.deleteSelected()
		return 1
	case vkF2:
		a.beginRename()
		return 1
	case uint16('C'):
		if ctrl {
			a.copySelected()
			return 1
		}
	case uint16('A'):
		if ctrl {
			a.selectAll()
			return 1
		}
	case uint16('F'):
		if ctrl {
			_, _, _ = syscall.SyscallN(pSetFocus.Addr(), a.searchBox)
			return 1
		}
	}
	return 0
}

const vkControlP = 0x11

func keyDown(vk int) bool {
	r, _, _ := syscall.SyscallN(pGetKeyState.Addr(), uintptr(vk))
	return r&0x8000 != 0
}

// resultsProc subclasses the ListView for extra keys (Ctrl+L, arrows at
// edges) and forwards everything else.
func (a *app) resultsProc(hwnd uintptr, msg uint32, w, l uintptr) (uintptr, bool) {
	h := controlHandlers[hwnd]
	if msg == wmKeyDown {
		switch w {
		case uintptr('L'):
			if keyDown(vkControlP) {
				_, _, _ = syscall.SyscallN(pSetFocus.Addr(), a.searchBox)
				return 0, true
			}
		case vkLeft:
			if keyDown(vkControlP) {
				_, _, _ = syscall.SyscallN(pSetFocus.Addr(), a.sidebar)
				return 0, true
			}
		}
	}
	return defControlProc(h, hwnd, msg, w, l), true
}

// ---- custom draw: snippet second line + match emphasis ----

func (a *app) onResultsCustomDraw(cd *nmlvCustomDraw) (out uintptr) {
	// Custom-draw is disabled: the control's NMLVCUSTOMDRAW buffer layout
	// disagrees with every 64-bit-cheap cast tried so far (fault at
	// startup), so default drawing is the safe behavior. Snippets render
	// in the dedicated Match column instead (see resultColumns).
	return cdrfDodefault
}

func (a *app) onResultsCustomDrawDisabled(cd *nmlvCustomDraw) (out uintptr) {
	out = cdrfDodefault
	stage := uint32(0)
	spec, sub := -1, -1
	func() {
		defer func() { _ = recover() }()
		stage = customDrawStageOf(cd)
		spec, sub = customDrawItemOf(cd)
	}()
	if stage == 0 && spec < 0 {
		return out
	}
	a.mu.Lock()
	rows := a.sortedRows
	contents := a.contents
	query := a.query
	if contents {
		query = toContentQuery(query)
	}
	terms := contentHighlightTerms(query)
	a.mu.Unlock()
	switch stage {
	case cddsPrepaint:
		return cdrfNotifyItemDraw
	case cddsItemPrepaint:
		if a.isGroupHeaderVirt(spec) {
			a.drawGroupHeader(cd, spec)
			return cdrfSkipDefault
		}
		i := a.displayRow(spec)
		if i >= 0 && i < len(rows) && rows[i].Snippet != "" && len(terms) > 0 {
			// Ask for subitem notifications so we can draw the snippet.
			return cdrfNotifySubItemDraw
		}
		return cdrfDodefault
	case cddsItemPrepaint | cddsSubItem:
		i := a.displayRow(spec)
		if sub == 0 && i >= 0 && i < len(rows) && rows[i].Snippet != "" {
			a.drawSnippetRow(cd, rows[i], terms)
			return cdrfSkipDefault
		}
		return cdrfDodefault
	}
	return cdrfDodefault
}

// customDrawStageOf reads dwDrawStage + item spec via explicit offsets into
// the NMLVCUSTOMDRAW buffer, independent of the Go struct layouts.
func customDrawStageOf(cd *nmlvCustomDraw) uint32 {
	base := notifyPtr(uintptr(unsafe.Pointer(cd)))
	// NMCUSTOMDRAW: hdr(24) + dwDrawStage(4).
	return *(*uint32)(unsafe.Add(base, 24))
}

// customDrawItemOf reads dwItemSpec (pointer-sized at offset 48) and
// iSubItem (int32 at the NMLVCUSTOMDRAW tail) via explicit offsets.
func customDrawItemOf(cd *nmlvCustomDraw) (spec int, sub int) {
	base := notifyPtr(uintptr(unsafe.Pointer(cd)))
	spec = int(*(*uintptr)(unsafe.Add(base, 48)))
	// NMLVCUSTOMDRAW size = 88 (x64): iSubItem int32 at offset 76.
	sub = int(*(*int32)(unsafe.Add(base, 76)))
	return spec, sub
}

func (a *app) isGroupHeaderVirt(vi int) bool {
	flat := a.flatListSnapshot()
	return vi >= 0 && vi < len(flat) && flat[vi].header
}

// customDrawDC reads the hdc + item rect + state from the NMLVCUSTOMDRAW
// buffer via explicit offsets.
func customDrawDC(cd *nmlvCustomDraw) (hdc uintptr, rc rect, state uintptr) {
	base := notifyPtr(uintptr(unsafe.Pointer(cd)))
	hdc = *(*uintptr)(unsafe.Add(base, 32))
	rc = *(*rect)(unsafe.Add(base, 40))
	state = *(*uintptr)(unsafe.Add(base, 64))
	return hdc, rc, state
}

// drawGroupHeader paints a grouped-mode folder header row.
func (a *app) drawGroupHeader(cd *nmlvCustomDraw, vi int) {
	flat := a.flatListSnapshot()
	if vi < 0 || vi >= len(flat) {
		return
	}
	hdc, rc, _ := customDrawDC(cd)
	brush := a.ctlColorBrush(a.pal.headerBg, a.pal.headerText, 0)
	fillRect(hdc, &rc, brush)
	label := flat[vi].dir
	drawTextColored(hdc, label, &rect{
		Left: rc.Left + scaleDPI(6, a.dpi), Top: rc.Top,
		Right: rc.Right - scaleDPI(4, a.dpi), Bottom: rc.Bottom,
	}, a.pal.headerText, dtLeft|dtSingleLine|dtVCenter|dtEndEllipsis|dtNoPrefix)
}

// drawSnippetRow owner-draws a content row: icon + name on line one, the
// snippet with emphasized matches on line two. Colors come from the live
// system theme so selection matches Explorer.
func (a *app) drawSnippetRow(cd *nmlvCustomDraw, row ResultRow, terms []string) {
	hdc, rc, st := customDrawDC(cd)
	selected := st&uintptr(cdisSelected) != 0
	focused := st&uintptr(cdisFocus) != 0
	var textCol, bgCol, grayCol uintptr
	if selected {
		bgCol = getSysColor(sysColorHighlight)
		textCol = getSysColor(sysColorHighlightText)
		grayCol = textCol
		_ = focused
	} else {
		textCol, bgCol = a.pal.text, a.pal.windowBg
		grayCol = a.pal.grayText
	}
	// Fill background.
	brush := a.ctlColorBrush(bgCol, textCol, 0)
	fillRect(hdc, &rc, brush)
	// Taller rows fit two lines: icon vertically centered on the name line.
	iconSize := scaleDPI(16, a.dpi)
	lineH := scaleDPI(17, a.dpi)
	iconX := rc.Left + scaleDPI(2, a.dpi)
	iconY := rc.Top + scaleDPI(2, a.dpi)
	drawSystemIcon(a.iconSmall, hdc, a.icons.iconIndex(row.Path, row.IsDir), iconX, iconY)
	_ = iconSize
	textX := iconX + scaleDPI(20, a.dpi)
	nameRc := rect{Left: textX, Top: rc.Top + scaleDPI(1, a.dpi), Right: rc.Right - scaleDPI(4, a.dpi), Bottom: rc.Top + lineH + scaleDPI(1, a.dpi)}
	drawTextColored(hdc, row.Name, &nameRc, textCol, dtLeft|dtSingleLine|dtEndEllipsis|dtNoPrefix)
	snipRc := rect{Left: textX, Top: rc.Top + lineH + scaleDPI(1, a.dpi), Right: rc.Right - scaleDPI(4, a.dpi), Bottom: rc.Bottom - scaleDPI(1, a.dpi)}
	drawTextWithMatches(hdc, row.Snippet, &snipRc, grayCol, a.pal.accent, terms)
}

func fillRect(hdc uintptr, rc *rect, brush uintptr) {
	_, _, _ = syscall.SyscallN(pFillRect.Addr(), hdc, uintptr(unsafe.Pointer(rc)), brush)
}

func drawTextColored(hdc uintptr, s string, rc *rect, color uintptr, flags uintptr) {
	_, _, _ = syscall.SyscallN(pSetTextColor.Addr(), hdc, color)
	_, _, _ = syscall.SyscallN(pSetBkMode.Addr(), hdc, transparent)
	_, _, _ = syscall.SyscallN(pDrawTextW.Addr(), hdc, uintptr(unsafe.Pointer(utf16(s))), uintptr(len([]rune(s))), uintptr(unsafe.Pointer(rc)), flags)
}

func drawTextWithMatches(hdc uintptr, s string, rc *rect, base, match uintptr, terms []string) {
	// Simple approach: draw the whole line in gray, then overdraw each
	// matched run in the accent color using extent measurement. For
	// correctness with proportional fonts, measure prefix widths with
	// DrawText(DT_CALCRECT) per segment boundary.
	bounds := matchSegmentBounds(s, terms)
	drawTextColored(hdc, s, rc, base, dtLeft|dtSingleLine|dtEndEllipsis|dtNoPrefix)
	if len(bounds) == 0 {
		return
	}
	for _, b := range bounds {
		prefix := s[:b[0]]
		hit := s[b[0]:b[1]]
		x := textWidth(hdc, prefix)
		w := textWidth(hdc, prefix+hit) - x
		segRc := rect{Left: rc.Left + x, Top: rc.Top, Right: rc.Left + x + w, Bottom: rc.Bottom}
		drawTextColored(hdc, hit, &segRc, match, dtLeft|dtSingleLine|dtNoPrefix)
	}
}

var pGetTextExtentPoint32W = proc(modGdi32, "GetTextExtentPoint32W")
var pDrawTextWEx = proc(modUser32, "DrawTextW")

func textWidth(hdc uintptr, s string) int32 {
	if s == "" {
		return 0
	}
	u, _ := syscall.UTF16FromString(s)
	var sz textSize
	_, _, _ = syscall.SyscallN(pGetTextExtentPoint32W.Addr(), hdc, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&sz)))
	return sz.CX
}

type textSize struct{ CX, CY int32 }

// updateRowHeight toggles a taller row height when any current row carries a
// content snippet, so owner-drawn name+snippet rows fit. Implemented by
// swapping the small image list: the list row height tracks the small icon
// height, so a 1x32 placeholder doubles it. Restored to the system image
// list when no snippets are visible.
func (a *app) updateRowHeight() {
	a.mu.Lock()
	rows := a.sortedRows
	a.mu.Unlock()
	needTall := false
	for _, r := range rows {
		if r.Snippet != "" {
			needTall = true
			break
		}
	}
	a.mu.Lock()
	tall := a.tallRows
	a.mu.Unlock()
	if needTall == tall {
		return
	}
	a.mu.Lock()
	a.tallRows = needTall
	a.mu.Unlock()
	if needTall {
		h := a.tallImageList()
		sendMessage(a.results, lvmSetImageList, lvsilSmall, h)
	} else {
		sendMessage(a.results, lvmSetImageList, lvsilSmall, a.iconSmall)
	}
}

// tallImageList returns a cached 1x32 imageless list at the current DPI.
func (a *app) tallImageList() uintptr {
	a.mu.Lock()
	h, dpi := a.tallList, a.tallListDPI
	a.mu.Unlock()
	if h != 0 && dpi == a.dpi {
		return h
	}
	if h != 0 {
		_, _, _ = syscall.SyscallN(pImageListDestroy.Addr(), h)
	}
	size := scaleDPI(32, a.dpi)
	nh, _, _ := syscall.SyscallN(pImageListCreate.Addr(),
		1, uintptr(size), uintptr(0x00000001 /*ILC_COLOR*/), 0, 0)
	a.mu.Lock()
	a.tallList = nh
	a.tallListDPI = a.dpi
	a.mu.Unlock()
	return nh
}


