//go:build windows

package main

import (
	"image"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// icons_gio_windows.go: real system shell icons rasterized to image.Image
// for the Gio UI. Icons resolve by extension with SHGetFileInfoW
// (USEFILEATTRIBUTES, no disk access), render into a 32bpp top-down DIB with
// DrawIconEx, and cache per extension key at 16/32/48px.

// SHGetFileInfo flags.
const (
	gShgfiIcon              = 0x000000100
	gShgfiSmallIcon         = 0x000000001
	gShgfiLargeIcon         = 0x000000000
	gShgfiUseFileAttributes = 0x000000010
	gShgfiTypeName          = 0x000000400

	gFileAttributeNormal    = 0x80
	gFileAttributeDirectory = 0x10

	gDiNormal = 0x0003
	gBiRGB    = 0
)

type gShFileInfo struct {
	HIcon         uintptr
	IIcon         int32
	Attributes    uint32
	DisplayName   [260]uint16
	TypeName      [80]uint16
}

type gBitmapInfoHeader struct {
	BiSize          uint32
	BiWidth         int32
	BiHeight        int32
	BiPlanes        uint16
	BiBitCount      uint16
	BiCompression   uint32
	BiSizeImage     uint32
	BiXPelsPerMeter int32
	BiYPelsPerMeter int32
	BiClrUsed       uint32
	BiClrImportant  uint32
}

type gBitmapInfo struct {
	Header gBitmapInfoHeader
	Colors [3]uint32
}

// iconStore caches rasterized shell icons.
type iconStore struct {
	mu    sync.Mutex
	imgs  map[iconStoreKey]*image.RGBA
	types map[string]string
}

type iconStoreKey struct {
	key  string
	size int
}

func newIconStore() *iconStore {
	return &iconStore{imgs: map[iconStoreKey]*image.RGBA{}, types: map[string]string{}}
}

func gIconKey(path string, isDir bool) string {
	if isDir {
		return "dir\x00"
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return "file\x00"
	}
	return ext
}

// image returns the rasterized shell icon at size px (16, 32 or 48).
func (s *iconStore) image(path string, isDir bool, size int) *image.RGBA {
	if size != 16 && size != 32 && size != 48 {
		size = 16
	}
	key := iconStoreKey{key: gIconKey(path, isDir), size: size}
	s.mu.Lock()
	if img, ok := s.imgs[key]; ok {
		s.mu.Unlock()
		return img
	}
	s.mu.Unlock()
	img := renderShellIcon(path, isDir, size)
	if img == nil {
		return nil
	}
	s.mu.Lock()
	s.imgs[key] = img
	s.mu.Unlock()
	return img
}

// typeName returns the shell type name for path.
func (s *iconStore) typeName(path string, isDir bool) string {
	key := gIconKey(path, isDir)
	s.mu.Lock()
	if t, ok := s.types[key]; ok {
		s.mu.Unlock()
		return t
	}
	s.mu.Unlock()
	var attrs uint32 = gFileAttributeNormal
	if isDir {
		attrs = gFileAttributeDirectory
	}
	var sfi gShFileInfo
	r, _, _ := syscall.SyscallN(pSHGetFileInfoW.Addr(),
		uintptr(unsafe.Pointer(utf16(path))),
		uintptr(attrs),
		uintptr(unsafe.Pointer(&sfi)),
		unsafe.Sizeof(sfi),
		uintptr(gShgfiTypeName|gShgfiUseFileAttributes))
	name := ""
	if r != 0 {
		name = syscall.UTF16ToString(sfi.TypeName[:])
	}
	s.mu.Lock()
	s.types[key] = name
	s.mu.Unlock()
	return name
}

func renderShellIcon(path string, isDir bool, size int) *image.RGBA {
	var attrs uint32 = gFileAttributeNormal
	if isDir {
		attrs = gFileAttributeDirectory
	}
	flags := uintptr(gShgfiIcon | gShgfiUseFileAttributes)
	if size <= 16 {
		flags |= gShgfiSmallIcon
	} else {
		flags |= gShgfiLargeIcon
	}
	var sfi gShFileInfo
	r, _, _ := syscall.SyscallN(pSHGetFileInfoW.Addr(),
		uintptr(unsafe.Pointer(utf16(path))),
		uintptr(attrs),
		uintptr(unsafe.Pointer(&sfi)),
		unsafe.Sizeof(sfi),
		flags)
	if r == 0 || sfi.HIcon == 0 {
		return nil
	}
	defer syscall.SyscallN(pDestroyIcon.Addr(), sfi.HIcon)

	hdc, _, _ := syscall.SyscallN(pGetDC.Addr(), 0)
	if hdc == 0 {
		return nil
	}
	defer syscall.SyscallN(pReleaseDC.Addr(), 0, hdc)
	mem, _, _ := syscall.SyscallN(pCreateCompatibleDC.Addr(), hdc)
	if mem == 0 {
		return nil
	}
	defer syscall.SyscallN(pDeleteDC.Addr(), mem)

	bmi := gBitmapInfo{Header: gBitmapInfoHeader{
		BiSize:      uint32(unsafe.Sizeof(gBitmapInfoHeader{})),
		BiWidth:     int32(size),
		BiHeight:    -int32(size),
		BiPlanes:    1,
		BiBitCount:  32,
		BiCompression: gBiRGB,
	}}
	var bits unsafe.Pointer
	hbmp, _, _ := syscall.SyscallN(pCreateDIBSection.Addr(), hdc,
		uintptr(unsafe.Pointer(&bmi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbmp == 0 || bits == nil {
		return nil
	}
	defer syscall.SyscallN(pDeleteObject.Addr(), hbmp)

	oldBmp, _, _ := syscall.SyscallN(pSelectObject.Addr(), mem, hbmp)
	syscall.SyscallN(pDrawIconEx.Addr(), mem, 0, 0, sfi.HIcon,
		uintptr(size), uintptr(size), 0, 0, gDiNormal)
	syscall.SyscallN(pSelectObject.Addr(), mem, oldBmp)

	raw := unsafe.Slice((*byte)(bits), size*size*4)
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			o := (y*size + x) * 4
			b, g, rr, a := raw[o], raw[o+1], raw[o+2], raw[o+3]
			// DIB bits are premultiplied; RGBA expects straight alpha.
			if a > 0 && a < 255 {
				b = byte(uint32(b) * 255 / uint32(a))
				g = byte(uint32(g) * 255 / uint32(a))
				rr = byte(uint32(rr) * 255 / uint32(a))
			}
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = rr, g, b, a
		}
	}
	return img
}
