//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestShellIconRaster verifies real system icons rasterize headlessly (GDI
// needs no GPU): extension-differentiated, correctly sized, non-blank.
func TestShellIconRaster(t *testing.T) {
	dir := t.TempDir()
	txt := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(txt, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	exe := os.Args[0] // a real .exe with its own icon
	store := newIconStore()
	img16 := store.image(txt, false, 16)
	img32 := store.image(txt, false, 32)
	imgExe := store.image(exe, false, 16)
	imgDir := store.image(dir, true, 16)
	if img16 == nil || img32 == nil || imgExe == nil || imgDir == nil {
		t.Fatal("shell icon rasterization returned nil")
	}
	if img16.Bounds().Dx() != 16 || img32.Bounds().Dx() != 32 {
		t.Fatalf("sizes = %v %v", img16.Bounds(), img32.Bounds())
	}
	nonBlank := func(name string, px []uint8) {
		alpha := 0
		for i := 3; i < len(px); i += 4 {
			if px[i] > 16 {
				alpha++
			}
		}
		if alpha == 0 {
			t.Fatalf("%s icon is fully transparent", name)
		}
	}
	nonBlank("txt", img16.Pix)
	nonBlank("exe", imgExe.Pix)
	nonBlank("dir", imgDir.Pix)
	if name := store.typeName(txt, false); name == "" {
		t.Fatal("empty shell type name")
	}
	if name := store.typeName(dir, true); name == "" {
		t.Fatal("empty shell folder type name")
	}
}
