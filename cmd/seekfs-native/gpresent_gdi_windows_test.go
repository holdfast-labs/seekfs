//go:build windows

package main

import (
	"image"
	"image/color"
	"testing"
)

func TestGdiTargetSize(t *testing.T) {
	cases := []struct {
		name       string
		cur, want  image.Point
		wantTarget image.Point
	}{
		{"initial", image.Point{}, image.Pt(900, 600), image.Pt(1024, 768)},
		{"exact bucket", image.Pt(1024, 768), image.Pt(1024, 768), image.Pt(1024, 768)},
		{"grow one axis", image.Pt(1024, 768), image.Pt(1600, 1000), image.Pt(1792, 1024)},
		{"never shrink", image.Pt(1792, 1024), image.Pt(900, 600), image.Pt(1792, 1024)},
		{"round up", image.Pt(0, 0), image.Pt(1025, 1), image.Pt(1280, 256)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gdiTargetSize(tc.cur, tc.want); got != tc.wantTarget {
				t.Fatalf("gdiTargetSize(%v, %v) = %v, want %v", tc.cur, tc.want, got, tc.wantTarget)
			}
		})
	}
}

func TestGdiConvertRGBAtoBGRA(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			src.SetRGBA(x, y, color.RGBA{
				R: uint8(10 + x*10),
				G: uint8(20 + y*10),
				B: uint8(100 + x*5 + y),
				A: 255,
			})
		}
	}
	before := src.RGBAAt(1, 1)
	sub := src.SubImage(image.Rect(1, 1, 3, 3)).(*image.RGBA)
	dst := make([]byte, 2*2*4)
	if err := convertRGBAtoBGRA(dst, sub); err != nil {
		t.Fatalf("convert: %v", err)
	}
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			want := src.RGBAAt(x+1, y+1)
			i := (y*2 + x) * 4
			got := dst[i : i+4 : i+4]
			if got[0] != want.B || got[1] != want.G || got[2] != want.R || got[3] != want.A {
				t.Fatalf("pixel (%d,%d) = %v, want BGRA %v", x, y, got, []uint8{want.B, want.G, want.R, want.A})
			}
		}
	}
	// The source must stay RGBA (the old presenter swapped it back in place).
	if p := src.RGBAAt(1, 1); p != before {
		t.Fatalf("source pixel changed: %+v, want %+v", p, before)
	}
	if err := convertRGBAtoBGRA(dst[:8], sub); err == nil {
		t.Fatal("expected an error for a too-small destination")
	}
}
