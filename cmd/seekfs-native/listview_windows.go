//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// listview_windows.go: virtual (owner-data) ListView wrapper for the results
// area — LVS_OWNERDATA so millions of rows stay instant, with sortable
// columns, system image lists, full-row select, double buffering and
// Explorer theming.

// ListView messages and styles.
const (
	lvmFirst = 0x1000

	lvmGetItemCount   = lvmFirst + 4
	lvmSetItemCount   = lvmFirst + 47
	lvmSetImageList   = lvmFirst + 3
	lvmInsertColumnW  = lvmFirst + 97
	lvmSetColumnW     = lvmFirst + 96
	lvmGetColumnW     = lvmFirst + 95
	lvmGetItemTextW   = lvmFirst + 115
	lvmSetItemState   = lvmFirst + 43
	lvmGetItemState   = lvmFirst + 44
	lvmGetNextItem    = lvmFirst + 12
	lvmEnsureVisible  = lvmFirst + 19
	lvmGetHeader      = lvmFirst + 31
	lvmSetExtendedStyle = lvmFirst + 54
	lvmGetExtendedStyle = lvmFirst + 55
	lvmSetBkColor     = lvmFirst + 1
	lvmSetTextBkColor = lvmFirst + 38
	lvmSetTextColor   = lvmFirst + 36
	lvmGetSelectionMark = lvmFirst + 66
	lvmSetSelectionMark = lvmFirst + 67
	lvmGetTopIndex    = lvmFirst + 39
	lvmGetCountPerPage = lvmFirst + 40
	lvmScroll         = lvmFirst + 20
	lvmRedrawItems    = lvmFirst + 21
	lvmGetItemRect    = lvmFirst + 14
	lvmSubItemHitTest = lvmFirst + 57
	lvmHitTest        = lvmFirst + 18
	lvmGetEditControl = lvmFirst + 24
	lvmEditLabelW     = lvmFirst + 118
	lvmGetColumnOrderArray = lvmFirst + 59

	lvsReport      = 0x0001
	lvsIcon        = 0x0000
	lvsSmallIcon   = 0x0002
	lvsList        = 0x0003
	lvsSingleSel   = 0x0004
	lvsShowSelAlways = 0x0008
	lvsOwnerData   = 0x1000
	lvsEditLabels  = 0x0200
	lvsNoLabelWrap = 0x0080
	lvsAlignTop    = 0x0000

	lvsilNormal = 0
	lvsilSmall  = 1

	lvscwAutoSizeUseHeader = -2

	lvniSelected = 0x0002
	lvniFocused  = 0x0001

	lvisSelected = 0x0002
	lvisFocused  = 0x0001

	lvifText  = 0x0001
	lvifImage = 0x0002
	lvifParam = 0x0004
	lvifState = 0x0008

	lvfiParam = 0x0001

	lvExGridLines       = 0x00000001
	lvExFullRowSelect   = 0x00000020
	lvExDoubleBuffer    = 0x00010000
	lvExLabelTip        = 0x00004000
	lvExBorderSelect    = 0x00008000
	lvExAutoSizeColumns = 0x10000000
	lvExInfoTip         = 0x00000400

	lvmSetItemPosition32 = lvmFirst + 49
	lvmGetItemSpacing   = lvmFirst + 51

	lvColFmtLeft   = 0x0000
	lvColFmtRight  = 0x0001
	lvColFmtCenter = 0x0002

	lvHdItemFmtLeft  = 0x0000
	lvHdItemFmtRight = 0x0001

	hdnFirst       = int32(-300)
	hdnItemClickW  = hdnFirst - 22
	hdnItemChangedW = hdnFirst - 21
	hdnDividerDblClickW = hdnFirst - 25
	hdnBeginTrackW = hdnFirst - 26
	hdnEndTrackW   = hdnFirst - 27

	lvnFirst            = int32(-100)
	lvnItemChanged      = lvnFirst - 1
	lvnGetDispInfoW     = lvnFirst - 77
	lvnOdfindItemW      = lvnFirst - 79
	lvnBeginLabelEditW  = lvnFirst - 75
	lvnEndLabelEditW    = lvnFirst - 76
	lvnKeyDown          = lvnFirst - 55
	lvnColumnClick      = lvnFirst - 8
	lvnBeginDrag        = lvnFirst - 9
	lvnGetInfoTipW      = lvnFirst - 57

	nmFirst            = int32(0)
	nmClick            = nmFirst - 2
	nmRClick           = nmFirst - 5
	nmDblClk           = nmFirst - 3
	nmnCustomDraw      = nmFirst - 12
	nmSetFocus         = nmFirst - 7
	nmKillFocus        = nmFirst - 8
	nmHover            = nmFirst - 13

	tvnFirst         = int32(-400)
	tvnSelChangedW   = tvnFirst - 51
	tvnGetDispInfoW  = tvnFirst - 62
	nmTVCustomDraw   = nmFirst - 12

	tvsHasButtons      = 0x0001
	tvsHasLines        = 0x0002
	tvsLinesAtRoot     = 0x0004
	tvsShowSelAlways   = 0x0020
	tvsTrackSelect     = 0x0200
	tvsFullRowSelect   = 0x1000
	tvsNoHScroll       = 0x8000
	tvsNoneEvenHeight  = 0x4000

	tvmFirst = 0x1100
	tvmInsertItemW  = tvmFirst + 50
	tvmDeleteItem   = tvmFirst + 1
	tvmGetItemW     = tvmFirst + 62
	tvmSetItemW     = tvmFirst + 63
	tvmGetCount     = tvmFirst + 5
	tvmExpand       = tvmFirst + 2
	tvmGetNextItem  = tvmFirst + 10
	tvmSelectItem   = tvmFirst + 11
	tvmHitTest      = tvmFirst + 17
	tvmSetImageList = tvmFirst + 9
	tvmSetBkColor   = tvmFirst + 29
	tvmSetTextColor = tvmFirst + 30
	tvmEnsureVisible = tvmFirst + 20

	tviRoot  = 0xFFFF0000
	tviFirst = 0xFFFF0001
	tviLast  = 0xFFFF0002

	tveExpand = 0x0002

	tvgnCaret = 0x0009
	tvgnRoot  = 0x0000
	tvgnNext  = 0x0001

	tvifText     = 0x0001
	tvifImage    = 0x0002
	tvifParam    = 0x0004
	tvifState    = 0x0008
	tvifSelectedImage = 0x0020
	tvifChildren = 0x0040

	tvisSelected = 0x0002

	tvhtOnItem = 0x0006

	sbemSetBorder = 0x2000 + 1

	ecLeft = 0x0000

	enChange = 0x0300
	enUpdate = 0x0400

	bmGetCheck = 0x00F0
	bmSetCheck = 0x00F1
	bstChecked   = 0x0001
	bstUnchecked = 0x0000

	tbmFirst = 0x0400
	tbmSetPos = tbmFirst + 5

	hdmFirst = 0x1200
	hdmGetItemCount = hdmFirst + 0
	hdmGetItemW = hdmFirst + 11
	hdmSetItemW = hdmFirst + 12
	hdmLayout = hdmFirst + 5

	hdiFormat = 0x0004
	hdiText   = 0x0002
	hdiWidth  = 0x0001
	hdiBitmap = 0x0010

	hdfLeft   = 0x0000
	hdfRight  = 0x0001
	hdfCenter = 0x0002
	hdfSortUp   = 0x0400
	hdfSortDown = 0x0200
)

type lvColumn struct {
	Mask      uint32
	Fmt       int32
	CX        int32
	PszText   *uint16
	CchTextMax int32
	ISubItem  int32
	IImage    int32
	IOrder    int32
}

type lvItem struct {
	Mask       uint32
	IItem      int32
	ISubItem   int32
	State      uint32
	StateMask  uint32
	PszText    *uint16
	CchTextMax int32
	IImage     int32
	LParam     uintptr
	IIndent    int32
	IGroupID   int32
	CColumns   uint32
	PuColumns  uintptr
}

type lvDispInfo struct {
	Hdr  nmhdr
	Item lvItem
}

// lvFindInfo mirrors NMLVFINDITEM: hdr, iStart, lvfi.
type lvFindInfo struct {
	Flags       uint32
	Psz         *uint16
	LParam      uintptr
	Pt          point
	VkDirection uint32
}

type lvHitTestInfo struct {
	Pt       point
	Flags    uint32
	IItem    int32
	ISubItem int32
	IGroup   int32
}

type nmListView struct {
	Hdr      nmhdr
	IItem    int32
	ISubItem int32
	NewState uint32
	OldState uint32
	Changed  uint32
	PtAction point
	LParam   uintptr
}

// nmCustomDraw mirrors NMCUSTOMDRAW exactly: hdr, dwDrawStage, hdc, rc,
// dwItemSpec, uItemState, lItemParam. DWORD means 32-bit even on 64-bit;
// dwItemSpec/uItemState/lItemParam are pointer-sized (DWORD_PTR/LPARAM).
type nmCustomDraw struct {
	Hdr         nmhdr
	DwDrawStage uint32
	_           [4]byte
	HDC         uintptr
	Rc          rect
	DwItemSpec  uintptr
	UItemState  uintptr
	LItemParam  uintptr
}

type nmlvCustomDraw struct {
	Nmcd     nmCustomDraw
	ClrText  uint32
	ClrTextBk uint32
	ISubItem int32
	DwItemType uint32
	ClrFace  uint32
	IIconEffect int32
	IIconPhase  int32
	IPartID    int32
	IStateID   int32
	RcText     rect
	UAlign     uint32
}

const (
	cddsPrepaint  = 0x00000001
	cddsItem      = 0x00010000
	cddsItemPrepaint = cddsItem | cddsPrepaint
	cddsSubItem   = 0x00020000
	cdrfDodefault = 0x00000000
	cdrfNotifyItemDraw = 0x00000020
	cdrfNotifySubItemDraw = 0x00000020
	cdrfNewFont   = 0x00000002
	cdrfSkipDefault = 0x00000004
	cdisSelected  = 0x0001
	cdisFocus     = 0x0010
)

type tvItem struct {
	Mask           uint32
	HItem          uintptr
	State          uint32
	StateMask      uint32
	PszText        *uint16
	CchTextMax     int32
	IImage         int32
	ISelectedImage int32
	CChildren      int32
	LParam         uintptr
}

type tvInsertStruct struct {
	HParent      uintptr
	HInsertAfter uintptr
	Item         tvItem
}

type tvHitTestInfo struct {
	Pt    point
	Flags uint32
	HItem uintptr
}

type tvDispInfo struct {
	Hdr  nmhdr
	Item tvItem
}

// wndproc plumbing for controls: each subclassed control gets a handler that
// returns (result, handled). Unhandled messages go to the original proc.
var controlHandlers = map[uintptr]controlHandler{}

type controlHandler struct {
	fn   func(hwnd uintptr, msg uint32, w, l uintptr) (uintptr, bool)
	orig uintptr
}

func setControlHandler(hwnd uintptr, fn func(hwnd uintptr, msg uint32, w, l uintptr) (uintptr, bool)) {
	h := controlHandlers[hwnd]
	h.fn = fn
	controlHandlers[hwnd] = h
}

func controlWndProc(hwnd uintptr, msg uint32, w, l uintptr) uintptr {
	if h, ok := controlHandlers[hwnd]; ok && h.fn != nil {
		if r, handled := h.fn(hwnd, msg, w, l); handled {
			return r
		}
		if h.orig != 0 {
			r, _, _ := syscall.SyscallN(h.orig, hwnd, uintptr(msg), w, l)
			return r
		}
	}
	r, _, _ := syscall.SyscallN(pDefWindowProcW.Addr(), hwnd, uintptr(msg), w, l)
	return r
}

var controlWndProcCallback = syscall.NewCallback(func(hwnd, msg, w, l uintptr) uintptr {
	return controlWndProc(hwnd, uint32(msg), w, l)
})

// notifyLPARAM reinterprets a WM_NOTIFY lParam as the notification struct
// pointer. The pointer comes from the control and stays valid for the
// dispatch duration; it is never retained.
//
// NOTE: the generic-typed nil-pointer dereference form trips a Go runtime
// fault on windows/amd64 when inlined, so all notification casts funnel
// through this single non-generic helper.
func notifyLPARAM[T any](l uintptr) T {
	return *(*T)(notifyPtr(l))
}

func notifyPtr(l uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&l))
}

// subclassControl replaces a control's wndproc with the shared dispatcher and
// records the original proc for unhandled messages.
func subclassControl(hwnd uintptr) {
	h := controlHandlers[hwnd]
	orig := setWindowLong(hwnd, gwlWndProc, controlWndProcCallback)
	h.orig = orig
	controlHandlers[hwnd] = h
}

func defControlProc(h controlHandler, hwnd uintptr, msg uint32, w, l uintptr) uintptr {
	if h.orig != 0 {
		r, _, _ := syscall.SyscallN(h.orig, hwnd, uintptr(msg), w, l)
		return r
	}
	r, _, _ := syscall.SyscallN(pDefWindowProcW.Addr(), hwnd, uintptr(msg), w, l)
	return r
}
