//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// icons_windows.go: real system shell icons via SHGetFileInfo /
// SHGetImageList. The small and large system image lists are attached
// directly to the ListView/TreeView; per-extension icon indices and type
// names are cached so a million-row result set costs a handful of calls.

// SHGetFileInfo flags.
const (
	shgfiIcon              = 0x000000100
	shgfiDisplayName       = 0x000000200
	shgfiTypeName          = 0x000000400
	shgfiSysIconIndex      = 0x000004000
	shgfiLargeIcon         = 0x000000000
	shgfiSmallIcon         = 0x000000001
	shgfiUseFileAttributes = 0x000000010
	shgfiOpenIcon          = 0x000000002
)

const (
	fileAttributeNormal    = 0x80
	fileAttributeDirectory = 0x10
)

// SHIL values for SHGetImageList.
const (
	shilLarge      = 0
	shilSmall      = 1
	shilExtraLarge = 2
	shilSysSmall   = 3
	shilJumbo      = 4
)

// IID_IImageList {46EB5926-582E-4017-9FDF-E8993DA77547}
var iidImageList = syscall.GUID{
	Data1: 0x46EB5926,
	Data2: 0x582E,
	Data3: 0x4017,
	Data4: [8]byte{0x9F, 0xDF, 0xE8, 0x99, 0x3D, 0xA7, 0x75, 0x47},
}

type shFileInfo struct {
	HIcon         uintptr
	IIcon         int32
	Attributes    uint32
	DisplayName   [260]uint16
	TypeName      [80]uint16
}

var (
	modShlwapi      = syscall.NewLazyDLL("shlwapi.dll")
	pStrFormatByteSizeW = proc(modShlwapi, "StrFormatByteSizeW")
	pGetDateFormatW     = proc(modKernel32, "GetDateFormatW")
	pGetTimeFormatW     = proc(modKernel32, "GetTimeFormatW")
)

// systemImageList returns the shell system image list (usable as HIMAGELIST).
func systemImageList(which int) uintptr {
	var list uintptr
	hr, _, _ := syscall.SyscallN(pSHGetImageList.Addr(), uintptr(which),
		uintptr(unsafe.Pointer(&iidImageList)), uintptr(unsafe.Pointer(&list)))
	if hr != 0 || list == 0 {
		return 0
	}
	return list
}

// iconCache maps extension/dir keys to system image indices + type names.
type iconCache struct {
	mu    sync.Mutex
	index map[string]int32
	types map[string]string
}

func newIconCache() *iconCache {
	return &iconCache{index: map[string]int32{}, types: map[string]string{}}
}

func iconKey(path string, isDir bool) string {
	if isDir {
		return "dir\x00"
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return "file\x00"
	}
	return ext
}

// iconIndex returns the system image-list index for path. It uses
// USEFILEATTRIBUTES so no disk access is needed.
func (c *iconCache) iconIndex(path string, isDir bool) int32 {
	key := iconKey(path, isDir)
	c.mu.Lock()
	if idx, ok := c.index[key]; ok {
		c.mu.Unlock()
		return idx
	}
	c.mu.Unlock()
	var attrs uint32 = fileAttributeNormal
	if isDir {
		attrs = fileAttributeDirectory
	}
	var sfi shFileInfo
	r, _, _ := syscall.SyscallN(pSHGetFileInfoW.Addr(),
		uintptr(unsafe.Pointer(utf16(path))),
		uintptr(attrs),
		uintptr(unsafe.Pointer(&sfi)),
		unsafe.Sizeof(sfi),
		uintptr(shgfiSysIconIndex|shgfiSmallIcon|shgfiUseFileAttributes))
	idx := int32(-1)
	if r != 0 {
		idx = sfi.IIcon
	}
	c.mu.Lock()
	c.index[key] = idx
	c.mu.Unlock()
	return idx
}

// typeName returns the shell type name ("Text Document", "File folder").
func (c *iconCache) typeName(path string, isDir bool) string {
	key := iconKey(path, isDir)
	c.mu.Lock()
	if t, ok := c.types[key]; ok {
		c.mu.Unlock()
		return t
	}
	c.mu.Unlock()
	var attrs uint32 = fileAttributeNormal
	if isDir {
		attrs = fileAttributeDirectory
	}
	var sfi shFileInfo
	r, _, _ := syscall.SyscallN(pSHGetFileInfoW.Addr(),
		uintptr(unsafe.Pointer(utf16(path))),
		uintptr(attrs),
		uintptr(unsafe.Pointer(&sfi)),
		unsafe.Sizeof(sfi),
		uintptr(shgfiTypeName|shgfiUseFileAttributes))
	name := ""
	if r != 0 {
		name = syscall.UTF16ToString(sfi.TypeName[:])
	}
	c.mu.Lock()
	c.types[key] = name
	c.mu.Unlock()
	return name
}

// formatByteSize formats bytes the way Explorer does (StrFormatByteSizeW).
func formatByteSize(n int64) string {
	var buf [64]uint16
	r, _, _ := syscall.SyscallN(pStrFormatByteSizeW.Addr(),
		uintptr(uint64(n)), uintptr(unsafe.Pointer(&buf[0])), 64)
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:])
}

// formatFileTime formats t with the user's locale short date + short time,
// the way Explorer's Date modified column does.
func formatFileTime(t int64Unix) string {
	return formatSystemTime(t.sec, t.nsec)
}

type int64Unix struct {
	sec  int64
	nsec int64
}

func formatSystemTime(sec, nsec int64) string {
	st := unixToSystemTime(sec)
	var dateBuf [128]uint16
	r, _, _ := syscall.SyscallN(pGetDateFormatW.Addr(),
		0x400, // LOCALE_USER_DEFAULT
		0x1,   // DATE_SHORTDATE
		uintptr(unsafe.Pointer(&st)),
		0, uintptr(unsafe.Pointer(&dateBuf[0])), 128)
	if r == 0 {
		return ""
	}
	var timeBuf [128]uint16
	r, _, _ = syscall.SyscallN(pGetTimeFormatW.Addr(),
		0x400, // LOCALE_USER_DEFAULT
		0x0,   // no flags: short time, no seconds
		uintptr(unsafe.Pointer(&st)),
		0, uintptr(unsafe.Pointer(&timeBuf[0])), 128)
	if r == 0 {
		return syscall.UTF16ToString(dateBuf[:])
	}
	return syscall.UTF16ToString(dateBuf[:]) + " " + syscall.UTF16ToString(timeBuf[:])
}

type systemTime struct {
	Year, Month, DayOfWeek, Day, Hour, Minute, Second, Millis uint16
}

func unixToSystemTime(sec int64) systemTime {
	// Days since 1601-01-01: unix epoch days + 134774.
	const epochDays = 134774
	days := sec/86400 + epochDays
	rem := sec % 86400
	if rem < 0 {
		rem += 86400
		days--
	}
	// Howard Hinnant's civil_from_days.
	z := days + 719468
	era := z / 146097
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	m := mp + 3
	if m > 12 {
		m -= 12
		y++
	}
	// Day of week: 1601-01-01 was a Monday (1).
	dow := (days + 1) % 7
	return systemTime{
		Year: uint16(y), Month: uint16(m), Day: uint16(d),
		DayOfWeek: uint16(dow),
		Hour:      uint16(rem / 3600), Minute: uint16((rem / 60) % 60), Second: uint16(rem % 60),
	}
}

// drawSystemIcon draws the system image-list icon at idx into hdc.
func drawSystemIcon(hSmallList uintptr, hdc uintptr, idx int32, x, y int32) {
	if hSmallList == 0 || idx < 0 {
		return
	}
	_, _, _ = syscall.SyscallN(pImageListDraw.Addr(), hSmallList,
		uintptr(idx), hdc, uintptr(x), uintptr(y), uintptr(0x1 /*ILD_TRANSPARENT*/))
}
