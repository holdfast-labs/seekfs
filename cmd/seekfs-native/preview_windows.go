//go:build windows

package main

import (
	"os"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// preview_windows.go: Quick Look-style preview pane.
//
// Strategy: host Windows preview handlers (IPreviewHandler) for the selected
// file when one is registered; fall back to a plain-text preview (first N
// bytes, encoding-sniffed) for text files and a metadata card otherwise.
//
// Preview handlers are COM objects keyed by extension under
// HKCR\<ext>\shellex\{8895b1c6-b41f-4c1c-a562-0d564af1f615} (the
// IPreviewHandler GUID). Instantiation and DoPreview happen on the UI thread
// inside a dedicated child HWND; unloading happens on selection change.

const maxTextPreviewBytes = 256 * 1024

// CLSID/IPreviewHandler GUIDs.
var (
	iidIPreviewHandler = syscall.GUID{
		Data1: 0x8895b1c6, Data2: 0xb41f, Data3: 0x4c1c,
		Data4: [8]byte{0xa5, 0x62, 0x0d, 0x56, 0x4a, 0xf1, 0xf6, 0x15},
	}
	iidIUnknown = syscall.GUID{
		Data1: 0x00000000, Data2: 0x0000, Data3: 0x0000,
		Data4: [8]byte{0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46},
	}
	iidIInitializeWithFile = syscall.GUID{
		Data1: 0xb7d14566, Data2: 0x0509, Data3: 0x4cce,
		Data4: [8]byte{0xa7, 0xf5, 0xd2, 0xd0, 0xd2, 0xf8, 0x54, 0x33},
	}
	iidIInitializeWithStream = syscall.GUID{
		Data1: 0xb824b49d, Data2: 0x22b1, Data3: 0x4dc1,
		Data4: [8]byte{0xbe, 0x6e, 0x9e, 0x45, 0x6a, 0x8a, 0xe7, 0xf9},
	}
)

// IPreviewHandler vtable slots after IUnknown: SetWindow(3), SetRect(4),
// DoPreview(5), Unload(6), SetFocus(7), QueryFocus(8), TranslateAccelerator(9).
const (
	previewSlotSetWindow = 3
	previewSlotSetRect   = 4
	previewSlotDoPreview = 5
	previewSlotUnload    = 6
)

// previewHandlerCLSID returns the preview-handler CLSID registered for the
// file's extension, or "" when none is registered.
func previewHandlerCLSID(path string) string {
	ext := strings.ToLower(dotExt(path))
	if ext == "" {
		return ""
	}
	keyPath := ext + `\shellex\{8895b1c6-b41f-4c1c-a562-0d564af1f615}`
	k, err := registry.OpenKey(registry.CLASSES_ROOT, keyPath, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("")
	if err != nil || v == "" {
		return ""
	}
	return v
}

func dotExt(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			return path[i:]
		}
		if path[i] == '\\' || path[i] == '/' || path[i] == ':' {
			break
		}
	}
	return ""
}

// textPreviewableExt reports whether the extension is plausibly plain text.
func textPreviewableExt(path string) bool {
	switch strings.ToLower(dotExt(path)) {
	case ".txt", ".log", ".md", ".markdown", ".go", ".c", ".h", ".cpp", ".hpp",
		".cs", ".java", ".js", ".ts", ".tsx", ".jsx", ".py", ".rb", ".rs",
		".toml", ".yaml", ".yml", ".json", ".xml", ".html", ".htm", ".css",
		".sql", ".sh", ".ps1", ".bat", ".cmd", ".ini", ".cfg", ".csv", ".tsv",
		".tex", ".rst", ".diff", ".patch", ".gitignore", ".env", "":
		return true
	}
	return false
}

// readTextPreview reads up to maxTextPreviewBytes and decodes UTF-8/UTF-16.
func readTextPreview(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	buf := make([]byte, maxTextPreviewBytes+1)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "", false
	}
	buf = buf[:n]
	if len(buf) == 0 {
		return "", true
	}
	// BOM sniffing.
	if len(buf) >= 3 && buf[0] == 0xEF && buf[1] == 0xBB && buf[2] == 0xBF {
		return string(buf[3:]), true
	}
	if len(buf) >= 2 && buf[0] == 0xFF && buf[1] == 0xFE {
		return utf16BytesToString(buf[2:], true), true
	}
	if len(buf) >= 2 && buf[0] == 0xFE && buf[1] == 0xFF {
		return utf16BytesToString(buf[2:], false), true
	}
	// Binary sniff: NUL byte in the first 8KB means not text.
	probe := buf
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	for _, b := range probe {
		if b == 0 {
			return "", false
		}
	}
	return string(buf), true
}

func utf16BytesToString(b []byte, littleEndian bool) string {
	n := len(b) / 2
	u := make([]uint16, n)
	for i := 0; i < n; i++ {
		if littleEndian {
			u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		} else {
			u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		}
	}
	return syscall.UTF16ToString(u)
}

// ---- hosted preview handler ----

var (
	pCoCreateInstance = proc(modOle32, "CoCreateInstance")
	pCreateStreamOnHGlobal = proc(modOle32, "CreateStreamOnHGlobal")
)

// previewHost owns one hosted IPreviewHandler instance.
type previewHost struct {
	mu      sync.Mutex
	handler unsafe.Pointer // IPreviewHandler
	hwnd    uintptr        // host child window
	path    string
	active  bool
}

func (h *previewHost) unload() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.handler != nil {
		callVtbl(h.handler, previewSlotUnload)
		releaseCom(h.handler)
		h.handler = nil
	}
	h.active = false
	h.path = ""
}

// tryAttachHandler instantiates the registered preview handler for path and
// runs DoPreview in hwnd. Returns false when no handler could be attached
// (caller falls back to text preview).
func (h *previewHost) tryAttachHandler(hwnd uintptr, rc rect, path string) bool {
	h.unload()
	clsidStr := previewHandlerCLSID(path)
	if clsidStr == "" {
		return false
	}
	clsid, err := windows.GUIDFromString(clsidStr)
	if err != nil {
		return false
	}
	var handler unsafe.Pointer
	hr, _, _ := syscall.SyscallN(pCoCreateInstance.Addr(),
		uintptr(unsafe.Pointer(&clsid)), 0,
		uintptr(0x1 /*CLSCTX_INPROC_SERVER*/),
		uintptr(unsafe.Pointer(&iidIPreviewHandler)),
		uintptr(unsafe.Pointer(&handler)))
	if hr != 0 || handler == nil {
		return false
	}
	// Initialize with the file path when supported.
	var initFile unsafe.Pointer
	hr = callVtbl(handler, 0 /*QueryInterface*/, uintptr(unsafe.Pointer(&iidIInitializeWithFile)), uintptr(unsafe.Pointer(&initFile)))
	if hr == 0 && initFile != nil {
		// IInitializeWithFile::Initialize(path, STGM_READ).
		vtbl := *(*unsafe.Pointer)(initFile)
		fn := *(*uintptr)(unsafe.Add(vtbl, 3*unsafe.Sizeof(uintptr(0))))
		p, _ := syscall.UTF16PtrFromString(path)
		hr, _, _ = syscall.SyscallN(fn, uintptr(initFile), uintptr(unsafe.Pointer(p)), uintptr(0))
		releaseCom(initFile)
		if hr != 0 {
			releaseCom(handler)
			return false
		}
	} else {
		// No file init: give up (stream init needs an IStream over the file;
		// handlers that only support streams are rare for our fallback path).
		releaseCom(handler)
		return false
	}
	if callVtbl(handler, previewSlotSetWindow, hwnd, uintptr(unsafe.Pointer(&rc))) != 0 {
		releaseCom(handler)
		return false
	}
	if callVtbl(handler, previewSlotDoPreview) != 0 {
		callVtbl(handler, previewSlotUnload)
		releaseCom(handler)
		return false
	}
	h.mu.Lock()
	h.handler = handler
	h.hwnd = hwnd
	h.path = path
	h.active = true
	h.mu.Unlock()
	return true
}

// resizePreview forwards a new rect to the hosted handler.
func (h *previewHost) resize(rc rect) {
	h.mu.Lock()
	handler := h.handler
	h.mu.Unlock()
	if handler != nil {
		callVtbl(handler, previewSlotSetRect, uintptr(unsafe.Pointer(&rc)))
	}
}
