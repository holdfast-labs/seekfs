//go:build windows

package main

import (
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// app_windows.go: the main window — layout, message loop, search plumbing.
//
// Layout (top to bottom, left to right):
//   toolbar: back | forward | search box (fills) | Contents toggle | view switcher
//   body:    sidebar (TreeView, 220px) | results (ListView, fills) | preview (320px, collapsible)
//   status:  status bar (result count | query time | content health | index health)
//
// All service I/O runs on the Searcher goroutine; results arrive via a
// posted message (msgSearchResult) so the UI thread never blocks.

// Control IDs.
const (
	idSearchBox = 100
	idBackBtn   = 101
	idFwdBtn    = 102
	idContentsBtn = 103
	idViewBtn     = 104
	idResults     = 200
	idSidebar     = 300
	idPreview     = 400
	idStatusBar   = 500
	idGoBtn       = 106
)

const appWindowClass = "seekfs-native-main"

type app struct {
	hwnd       uintptr
	dpi        int
	dark       bool
	pal        palette
	font       uintptr
	iconSmall  uintptr // system small image list
	iconLarge  uintptr // system large/jumb image list

	// controls
	searchBox   uintptr
	scopeBox    uintptr
	backBtn     uintptr
	fwdBtn      uintptr
	contentsBtn uintptr
	viewBtn     uintptr
	results       uintptr
	resultsHdr    uintptr
	sidebar       uintptr
	preview       uintptr
	previewText   uintptr
	previewHeader uintptr
	statusBar     uintptr

	// state
	settings Settings
	client   *Client
	searcher *Searcher
	history  *History
	icons    *iconCache

	mu       sync.Mutex
	rows     []ResultRow
	sortedRows []ResultRow
	flatList []flatGroup
	volumes  []DBVolume
	sortKey  SortKey
	sortAsc  bool
	view     ViewMode
	contents bool
	seq      int64 // latest delivered seq
	query    string
	under    string
	fuzzyMsg string
	elapsedMS int64
	searchMS float64
	source   string
	content  *contentState
	contentUnavailableMsg string
	indexHealth string
	indexMsg    string
	indexEntries int
	selAnchor int // selection anchor for shift-select

	previewHost previewHost
	previewText2 string // text fallback content (unused; edit holds text)
	previewPath string

	sidebarRoots []sidebarNode
	statusParts []int32

	brushes map[uintptr]uintptr // COLORREF -> HBRUSH cache

	pendingResult *SearchResult
	pendingFlash  string
	tallRows      bool
	tallList      uintptr
	tallListDPI   int

	explicitPreview bool

	accelTable uintptr
}

type sidebarNode struct {
	handle  uintptr
	label   string
	under   string // scope path, "" = everywhere
	query   string // saved search query, "" = scope
	volumes []DBVolume
}

var appInstance *app

// initCommonControls registers the comctl32 classes we use.
func initCommonControls() {
	var icc initCommonControlsEx
	icc.DwSize = uint32(unsafe.Sizeof(icc))
	icc.DwICC = 0x00004000 | // ICC_LISTVIEW_CLASSES
		0x00000002 | // ICC_TREEVIEW_CLASSES
		0x00000001 | // ICC_BAR_CLASSES (status/toolbar)
		0x00000010 // ICC_PROGRESS_CLASS
	_, _, _ = syscall.SyscallN(pInitCommonControlsEx.Addr(), uintptr(unsafe.Pointer(&icc)))
}

func runUI(pipe string, limit int) error {
	settings := loadSettings()
	client := NewClient(pipe)
	searcher := NewSearcher(client, limit, 120*time.Millisecond)

	a := &app{
		settings: settings,
		client:   client,
		searcher: searcher,
		history:  NewHistory(100),
		icons:    newIconCache(),
		sortKey:  sortKeyFromString(settings.SortColumn),
		sortAsc:  settings.SortAsc,
		view:     viewModeFromString(settings.View),
		contents: settings.ContentsMode,
		under:    settings.Under,
		brushes:  map[uintptr]uintptr{},
	}
	if !settings.SortAsc && settings.SortColumn == "" {
		a.sortAsc = true
	}
	if settings.SortColumn == "" {
		a.sortAsc = true
	}
	appInstance = a

	if r, _, _ := syscall.SyscallN(pOleInitialize.Addr(), 0); r != 0 && r != 1 {
		// Non-fatal: drag-drop and preview handlers need it, but the app can
		// still search without OLE.
	}
	defer syscall.SyscallN(pOleUninitialize.Addr())

	initCommonControls()
	startContextMenuService()

	a.searcher.OnDeliver(func(res SearchResult) {
		a.onSearchResultBackground(res)
	})

	if err := a.createMainWindow(); err != nil {
		return err
	}
	a.applyTheme()
	a.layout()
	a.refreshStatus()
	a.refreshSidebar()
	a.refreshGoButton()
	a.updateNavButtons()

	// Initial info handshake (async so startup stays under 200ms to window).
	go a.initialInfo()

	// Message loop with accelerators. When an edit box has focus, only
	// Ctrl+Enter (reveal) translates; all other keys (Ctrl+C/A/F, Enter,
	// Delete) belong to the edit control.
	heartbeat("loop-start")
	var m msgT
	msgCount := 0
	for {
		r, _, _ := syscall.SyscallN(pGetMessageW.Addr(), uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			heartbeat("loop-exit")
			break
		}
		msgCount++
		if msgCount%500 == 0 {
			heartbeat("loop-alive")
		}
		if a.accelTable != 0 && a.shouldTranslateAccelerator(&m) {
			if tr, _, _ := syscall.SyscallN(pTranslateAcceleratorW.Addr(), a.hwnd, a.accelTable, uintptr(unsafe.Pointer(&m))); tr != 0 {
				continue
			}
		}
		// Let the edit control handle tab; dialog-like navigation for buttons.
		_, _, _ = syscall.SyscallN(pTranslateMessage.Addr(), uintptr(unsafe.Pointer(&m)))
		_, _, _ = syscall.SyscallN(pDispatchMessageW.Addr(), uintptr(unsafe.Pointer(&m)))
	}
	a.saveNow()
	return nil
}

type msgT struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

func (a *app) onSearchResultBackground(res SearchResult) {
	a.mu.Lock()
	a.pendingResult = &res
	a.mu.Unlock()
	postMessage(a.hwnd, msgSearchResult, 0, 0)
}

// createMainWindow registers the class and creates the top-level window plus
// all child controls. It shows the window before returning so startup to an
// interactive window stays well under 200ms; the service handshake happens
// after.
func (a *app) createMainWindow() error {
	className, _ := syscall.UTF16PtrFromString(appWindowClass)
	inst := instanceHandle()
	cursor, _, _ := syscall.SyscallN(pLoadCursorW.Addr(), 0, uintptr(idcArrow))
	wc := wndClassEx{
		CbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		LpfnWndProc:   syscall.NewCallback(mainWndProc),
		HInstance:     inst,
		HCursor:       cursor,
		HbrBackground: 0, // painted in WM_ERASEBKGND
		LpszClassName: uintptr(unsafe.Pointer(className)),
	}
	if r, _, _ := syscall.SyscallN(pRegisterClassExW.Addr(), uintptr(unsafe.Pointer(&wc))); r == 0 {
		// Already registered is fine (e.g. tests); anything else is fatal.
		if err := syscall.GetLastError(); err != syscall.Errno(1410) { // ERROR_CLASS_ALREADY_EXISTS
			return err
		}
	}

	x, y, w, h := a.initialRect()
	title, _ := syscall.UTF16PtrFromString("seekfs")
	hwnd, _, _ := syscall.SyscallN(pCreateWindowExW.Addr(),
		uintptr(wsExComposited),
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		uintptr(wsOverlappedWindow|wsClipChildren),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		0, 0, inst, 0)
	if hwnd == 0 {
		return syscall.GetLastError()
	}
	a.hwnd = hwnd
	appInstance = a
	setWindowLong(hwnd, gwlpUserData, uintptr(unsafe.Pointer(a)))

	// DPI before creating children (fonts, image lists).
	a.dpi = windowDPI(hwnd)
	a.dark = systemUsesDarkMode()
	a.pal = paletteFor(a.dark)
	a.font = systemUIFont(a.dpi)
	a.iconSmall = systemImageList(shilSysSmall)
	a.iconLarge = systemImageList(shilLarge)

	a.createToolbar()
	a.createSidebar()
	a.createResults()
	a.createPreview()
	a.createStatusBar()
	a.createAccelerators()

	applyWindowTheme(hwnd, a.dark, []uintptr{a.results, a.sidebar, a.searchBox, a.scopeBox})

	// Show immediately: interactive window first, service handshake after.
	if a.settings.Maximized {
		_, _, _ = syscall.SyscallN(pShowWindow.Addr(), hwnd, swShowMax)
	} else {
		_, _, _ = syscall.SyscallN(pShowWindow.Addr(), hwnd, swShowDefault)
	}
	_, _, _ = syscall.SyscallN(pUpdateWindow.Addr(), hwnd)
	_, _, _ = syscall.SyscallN(pSetFocus.Addr(), a.searchBox)
	return nil
}

func (a *app) initialRect() (int32, int32, int32, int32) {
	s := a.settings
	w, h := int32(s.WindowW), int32(s.WindowH)
	if w < 640 {
		w = 1180
	}
	if h < 400 {
		h = 760
	}
	x, y := int32(s.WindowX), int32(s.WindowY)
	if x == 0 && y == 0 {
		// CW_USEDEFAULT.
		return -2147483648, -2147483648, w, h // 0x80000000
	}
	return x, y, w, h
}

func mainWndProc(hwnd, msg, w, l uintptr) uintptr {
	a := appInstance
	if a == nil || a.hwnd != hwnd {
		r, _, _ := syscall.SyscallN(pDefWindowProcW.Addr(), hwnd, msg, w, l)
		return r
	}
	return a.wndProc(uint32(msg), w, l)
}

const cwUseDefault = -2147483648 // 0x80000000 as int32
