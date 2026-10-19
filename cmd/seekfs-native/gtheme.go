package main

import (
	"image/color"
	"os"
	"strconv"
	"sync"

	"gioui.org/font"
	"gioui.org/font/gofont"
	"gioui.org/font/opentype"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

// gtheme.go: the seekfs design system for the Gio UI — light/dark schemes
// following the OS, the system accent color, Segoe UI typefaces, and a 4px
// spacing grid. Every layout constant goes through Dp/Sp so the simulated
// scaling factor (SEEKFS_NATIVE_DPISCALE) exercises the same code paths as
// real DPI scaling.

// dpScale multiplies every dp/sp value. 1.0 in production; set
// SEEKFS_NATIVE_DPISCALE=1.5 for simulated-150% screenshots.
var dpScale = func() float64 {
	if v := os.Getenv("SEEKFS_NATIVE_DPISCALE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0.5 && f < 4 {
			return f
		}
	}
	return 1.0
}()

// Dp scales a dp value by the test multiplier.
func Dp(v float32) unit.Dp { return unit.Dp(float64(v) * dpScale) }

// Sp scales a sp value by the test multiplier.
func Sp(v float32) unit.Sp { return unit.Sp(float64(v) * dpScale) }

// Scheme is the full color set for one mode.
type Scheme struct {
	Dark bool
	// Surfaces.
	BG       color.NRGBA // app background
	Panel    color.NRGBA // sidebar / preview / cards
	Field    color.NRGBA // text fields
	Header   color.NRGBA // list header
	Border   color.NRGBA // hairlines
	Divider  color.NRGBA // column dividers
	Hover    color.NRGBA // hover fill (alpha-blended already)
	Pressed  color.NRGBA
	Selected color.NRGBA // selected row fill
	FocusBar color.NRGBA // focused-row accent bar
	// Text.
	Text     color.NRGBA
	Subtle   color.NRGBA
	Muted    color.NRGBA
	Accent   color.NRGBA // system accent
	OnAccent color.NRGBA
	Match    color.NRGBA // snippet match emphasis
	// Status health.
	OK      color.NRGBA
	Warning color.NRGBA
	Error   color.NRGBA
}

func schemeFor(dark bool, accent color.NRGBA) Scheme {
	if dark {
		return Scheme{
			Dark:     true,
			BG:       nrgba(0x202020),
			Panel:    nrgba(0x2B2B2B),
			Field:    nrgba(0x2D2D2D),
			Header:   nrgba(0x282828),
			Border:   color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0x14},
			Divider:  color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0x10},
			Hover:    color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0x0F},
			Pressed:  color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0x1A},
			Selected: withAlpha(accent, 0x40),
			FocusBar: accent,
			Text:     nrgba(0xFFFFFF),
			Subtle:   nrgba(0xCCCCCC),
			Muted:    nrgba(0x9D9D9D),
			Accent:   accent,
			OnAccent: nrgba(0xFFFFFF),
			Match:    accentLight(accent),
			OK:       nrgba(0x6CCB5F),
			Warning:  nrgba(0xFCE100),
			Error:    nrgba(0xF1707B),
		}
	}
	return Scheme{
		Dark:     false,
		BG:       nrgba(0xF3F3F3),
		Panel:    nrgba(0xFFFFFF),
		Field:    nrgba(0xFFFFFF),
		Header:   nrgba(0xF9F9F9),
		Border:   color.NRGBA{R: 0x00, G: 0x00, B: 0x00, A: 0x14},
		Divider:  color.NRGBA{R: 0x00, G: 0x00, B: 0x00, A: 0x12},
		Hover:    color.NRGBA{R: 0x00, G: 0x00, B: 0x00, A: 0x0D},
		Pressed:  color.NRGBA{R: 0x00, G: 0x00, B: 0x00, A: 0x1A},
		Selected: withAlpha(accent, 0x33),
		FocusBar: accent,
		Text:     nrgba(0x1B1B1B),
		Subtle:   nrgba(0x424242),
		Muted:    nrgba(0x6B6B6B),
		Accent:   accent,
		OnAccent: nrgba(0xFFFFFF),
		Match:    accentDark(accent),
		OK:       nrgba(0x0F7B0F),
		Warning:  nrgba(0x9D5D00),
		Error:    nrgba(0xC42B1C),
	}
}

func nrgba(rgb uint32) color.NRGBA {
	return color.NRGBA{R: uint8(rgb >> 16), G: uint8(rgb >> 8), B: uint8(rgb), A: 0xFF}
}

func withAlpha(c color.NRGBA, a uint8) color.NRGBA { c.A = a; return c }

// accentLight lightens the accent for match text on dark backgrounds.
func accentLight(c color.NRGBA) color.NRGBA {
	mix := func(v uint8) uint8 { return uint8(int(v) + (255-int(v))/2) }
	return color.NRGBA{R: mix(c.R), G: mix(c.G), B: mix(c.B), A: 0xFF}
}

// accentDark darkens the accent for match text on light backgrounds.
func accentDark(c color.NRGBA) color.NRGBA {
	mix := func(v uint8) uint8 { return uint8(int(v) * 3 / 4) }
	return color.NRGBA{R: mix(c.R), G: mix(c.G), B: mix(c.B), A: 0xFF}
}

// ---- fonts ----

var (
	fontOnce sync.Once
	fontColl []font.FontFace
)

// Segoe Fluent icon codepoints (SegoeIcons.ttf).
const (
	iconBack     = "\ue72b"
	iconForward  = "\ue72a"
	iconSearch   = "\ue721"
	iconClear    = "\ue711"
	iconList     = "\ue8fd"
	iconGrid     = "\ue80a"
	iconGroup    = "\ue8a4"
	iconContents = "\ue8a5"
	iconSaved    = "\ue728"
	iconAdd      = "\ue710"
	iconFolder   = "\ue8b7"
	iconFile     = "\ue7b8"
	iconSettings = "\ue713"
)

// fluentFace is the typeface name for Segoe Fluent Icons.
const fluentFace = "Segoe Fluent Icons"

// typefaces returns Segoe UI (+bold) and Segoe Fluent Icons prepended to
// the bundled Go fonts as fallback. On non-Windows systems (or when the
// files are missing) the Go fonts plus Gio's system-font fallback render
// instead.
func typefaces() []font.FontFace {
	fontOnce.Do(func() {
		var faces []font.FontFace
		for _, path := range []string{
			`C:\Windows\Fonts\segoeui.ttf`,
			`C:\Windows\Fonts\segoeuib.ttf`,
			`C:\Windows\Fonts\SegoeIcons.ttf`,
		} {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			parsed, err := opentype.ParseCollection(data)
			if err != nil || len(parsed) == 0 {
				continue
			}
			faces = append(faces, parsed...)
		}
		fontColl = append(faces, gofont.Collection()...)
	})
	return fontColl
}

// newShaper builds a text shaper preferring Segoe UI with system fallback
// (so CJK filenames still render).
func newShaper() *text.Shaper {
	return text.NewShaper(text.WithCollection(typefaces()))
}

// newMaterialTheme builds a material theme wired to our shaper, palette and
// base text size. The returned theme is restyled per mode by restyle().
func newMaterialTheme() *material.Theme {
	th := material.NewTheme()
	th.Shaper = newShaper()
	th.Face = "Segoe UI"
	th.TextSize = Sp(14)
	return th
}

// applySchemeNoFrame restyles without touching a window (headless —
// snapshots and tests).
func (u *UI) applySchemeNoFrame() {
	u.scheme = schemeFor(u.dark, u.accent)
	restyle(u.th, u.scheme)
	u.lastDark = u.dark
}

// restyle applies the scheme to a material theme.
func restyle(th *material.Theme, s Scheme) {
	th.Palette = material.Palette{
		Bg:         s.Panel,
		Fg:         s.Text,
		ContrastBg: s.Accent,
		ContrastFg: s.OnAccent,
	}
}
