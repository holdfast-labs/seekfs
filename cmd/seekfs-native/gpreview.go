package main

import (
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"strings"

	"gioui.org/layout"
	"gioui.org/op/paint"
	"gioui.org/widget"
	"gioui.org/widget/material"
	"golang.org/x/image/bmp"
)

// gpreview.go: the Quick Look-style preview pane — file name header, text
// preview with match highlight for text files, rendered images, and a
// metadata card otherwise.

// layoutPreview renders the preview pane for the focused/selected row.
func (u *UI) layoutPreview(gtx layout.Context) layout.Dimensions {
	s := u.scheme
	u.mu.Lock()
	var row *ResultRow
	if u.focus >= 0 && u.focus < len(u.rows) {
		r := u.rows[u.focus]
		row = &r
	} else {
		for i := range u.rows {
			if u.sel[i] {
				r := u.rows[i]
				row = &r
				break
			}
		}
	}
	contents := u.contents
	query := u.query
	if contents {
		query = toContentQuery(query)
	}
	terms := contentHighlightTerms(query)
	u.mu.Unlock()

	dims := layout.Inset{Left: Dp(12), Right: Dp(12), Top: Dp(10), Bottom: Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		if row == nil {
			return label(u.th, Sp(13), "Select a file to preview it here.", s.Muted).Layout(gtx)
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return label(u.th, Sp(14), row.Name, s.Text).Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: Dp(2), Bottom: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return label(u.th, Sp(12), row.Dir, s.Muted).Layout(gtx)
				})
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return u.layoutPreviewBody(gtx, *row, terms)
			}),
		)
	})
	return layout.Stack{}.Layout(gtx,
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			fillRect(gtx, s.Panel)
			return dims
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.W.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return vline(gtx, s.Border)
			})
		}),
	)
}

func (u *UI) layoutPreviewBody(gtx layout.Context, row ResultRow, terms []string) layout.Dimensions {
	// Images first.
	if isImageExt(row.Path) {
		if dims, ok := u.layoutPreviewImage(gtx, row.Path); ok {
			return dims
		}
	}
	if text, ok := readTextPreview(row.Path); ok && textPreviewableExt(row.Path) {
		return u.layoutPreviewText(gtx, text, terms)
	}
	if text, ok := readTextPreview(row.Path); ok && text != "" && isTextBySniff(row.Path) {
		return u.layoutPreviewText(gtx, text, terms)
	}
	return label(u.th, Sp(13), previewMetadata(row, u.shell.TypeName(row.Path, row.IsDir)), u.scheme.Subtle).Layout(gtx)
}

func isImageExt(path string) bool {
	switch strings.ToLower(dotExt(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".bmp":
		return true
	}
	return false
}

// isTextBySniff allows previewing extensionless text (readTextPreview
// already rejected binaries).
func isTextBySniff(path string) bool { return dotExt(path) == "" }

func (u *UI) layoutPreviewImage(gtx layout.Context, path string) (layout.Dimensions, bool) {
	img := u.previewImage(path)
	if img == nil {
		return layout.Dimensions{}, false
	}
	op := paint.NewImageOp(img)
	return widget.Image{Src: op, Fit: widget.Contain}.Layout(gtx), true
}

// previewImage decodes and caches preview images per path.
func (u *UI) previewImage(path string) image.Image {
	u.mu.Lock()
	if im, ok := u.imgCache[path]; ok {
		u.mu.Unlock()
		return im
	}
	u.mu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var img image.Image
	if strings.ToLower(dotExt(path)) == ".bmp" {
		img, err = bmp.Decode(f)
	} else {
		img, _, err = image.Decode(f)
	}
	if err != nil {
		return nil
	}
	// Downscale huge images for GPU sanity.
	if b := img.Bounds(); b.Dx() > 1600 || b.Dy() > 1600 {
		img = downscale(img, 1600)
	}
	u.mu.Lock()
	if u.imgCache == nil {
		u.imgCache = map[string]image.Image{}
	}
	// Bound the cache.
	if len(u.imgCache) > 32 {
		for k := range u.imgCache {
			delete(u.imgCache, k)
			break
		}
	}
	u.imgCache[path] = img
	u.mu.Unlock()
	return img
}

func downscale(img image.Image, max int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	scale := 1
	for w/scale > max || h/scale > max {
		scale *= 2
	}
	dst := image.NewRGBA(image.Rect(0, 0, w/scale, h/scale))
	for y := 0; y < h/scale; y++ {
		for x := 0; x < w/scale; x++ {
			dst.Set(x, y, img.At(b.Min.X+x*scale, b.Min.Y+y*scale))
		}
	}
	return dst
}

func (u *UI) layoutPreviewText(gtx layout.Context, text string, terms []string) layout.Dimensions {
	th := u.th
	s := u.scheme
	if len(text) > 32768 {
		text = text[:32768] + "\n…"
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 400 {
		lines = append(lines[:400], "…")
	}
	return material.List(th, &u.previewList).Layout(gtx, len(lines), func(gtx layout.Context, i int) layout.Dimensions {
		line := lines[i]
		if len(line) > 500 {
			line = line[:500] + "…"
		}
		segs := highlightSegments(line, terms)
		if len(segs) <= 1 || len(terms) == 0 {
			l := material.Body2(th, line)
			l.Color = s.Subtle
			l.TextSize = Sp(12)
			return l.Layout(gtx)
		}
		children := make([]layout.FlexChild, 0, len(segs))
		for _, sg := range segs {
			sg := sg
			c := s.Subtle
			if sg.Match {
				c = s.Match
			}
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				l := material.Body2(th, sg.Text)
				l.Color = c
				l.TextSize = Sp(12)
				l.MaxLines = 1
				return l.Layout(gtx)
			}))
		}
		return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, children...)
	})
}
