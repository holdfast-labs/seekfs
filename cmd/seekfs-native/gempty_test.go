package main

import (
	"image"
	"testing"

	"gioui.org/layout"
)

func countAccentPixels(img *image.RGBA) int {
	n := 0
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			c := img.RGBAAt(x, y)
			if c.B > 0x70 && int(c.B) > int(c.R)+40 && int(c.B) > int(c.G)+20 {
				n++
			}
		}
	}
	return n
}

func renderEmptyState(t *testing.T, dark, noVolumes bool) *image.RGBA {
	t.Helper()
	u := testUI(t, dark)
	u.noVolumes = noVolumes
	return renderHeadless(t, 1000, 700, func(gtx layout.Context) layout.Dimensions {
		return u.layout(gtx)
	})
}

func TestFirstRunEmptyStatePanelLightAndDark(t *testing.T) {
	for _, dark := range []bool{false, true} {
		name := "light"
		if dark {
			name = "dark"
		}
		t.Run(name, func(t *testing.T) {
			generic := renderEmptyState(t, dark, false)
			firstRun := renderEmptyState(t, dark, true)

			if n := countDistinct(firstRun); n < 10 {
				t.Fatalf("first-run panel rendered near-uniform (%d colors)", n)
			}
			if sameImage(generic, firstRun) {
				t.Fatal("first-run panel did not differ from the generic empty state")
			}
			genericAccent := countAccentPixels(generic)
			firstRunAccent := countAccentPixels(firstRun)
			if firstRunAccent <= genericAccent+50 {
				t.Fatalf("first-run action button not visible: accent pixels %d vs %d",
					firstRunAccent, genericAccent)
			}
			bg := firstRun.RGBAAt(4, 4)
			if dark && bg.R > 0x60 {
				t.Fatalf("dark background too light: %v", bg)
			}
			if !dark && bg.R < 0x80 {
				t.Fatalf("light background too dark: %v", bg)
			}
		})
	}
}

func sameImage(a, b *image.RGBA) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	for y := 0; y < a.Bounds().Dy(); y++ {
		for x := 0; x < a.Bounds().Dx(); x++ {
			if a.RGBAAt(x, y) != b.RGBAAt(x, y) {
				return false
			}
		}
	}
	return true
}
