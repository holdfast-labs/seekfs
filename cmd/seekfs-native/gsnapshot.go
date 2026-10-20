package main

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"time"

	"gioui.org/gpu/headless"
	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
)

// gsnapshot.go: deterministic headless snapshots — renders the real UI with
// gioui.org/gpu/headless into a PNG. No window, no desktop interaction:
//
//	seekfs-native.exe -snapshot out.png -theme dark -scale 1.5 -size 1280x800 -query chrome
//
// Waits for the first result page before rendering.

// snapshotConfig carries the snapshot flags.
type snapshotConfig struct {
	out      string
	theme    string
	scale    float64
	size     image.Point
	query    string
	view     string
	contents bool
	preview  bool
}

// runSnapshot renders one PNG and exits.
func runSnapshot(pipe string, limit int, cfg snapshotConfig) error {
	if cfg.scale <= 0 {
		cfg.scale = 1
	}
	dpScale = cfg.scale
	u := newUI(pipe, limit, func() {})
	switch cfg.theme {
	case "dark":
		u.dark = true
	case "light":
		u.dark = false
	}
	u.applySchemeNoFrame()
	if cfg.view != "" {
		u.view = viewModeFromString(cfg.view)
	}
	if cfg.contents {
		u.contents = true
	}
	if cfg.preview {
		u.settings.PreviewOpen = true
	}

	// Sync service handshake for the sidebar/volumes + health line.
	if resp, err := u.client.Info(); err == nil {
		u.mu.Lock()
		u.noVolumes = len(resp.DBs) == 0
		if resp.Loading {
			u.health, u.healthMsg = serviceHealthDegraded, "Loading indexes…"
		} else if resp.OK && resp.Entries > 0 {
			u.health = resp.Health
			if u.health == "" {
				u.health = serviceHealthOK
			}
			u.healthMsg = resp.HealthMessage
			u.entries = resp.Entries
		} else {
			u.health, u.healthMsg = serviceHealthError, firstNonEmpty(resp.Message, "service has no search indexes loaded")
		}
		u.mu.Unlock()
		u.rebuildSideRows(dbVolumesFrom(resp))
	} else {
		u.mu.Lock()
		u.health, u.healthMsg = serviceHealthError, err.Error()
		u.mu.Unlock()
	}

	// Run the query and wait for the first delivered page.
	if cfg.query != "" {
		u.searchEd.SetText(cfg.query)
		u.commitQuery()
		deadline := time.Now().Add(20 * time.Second)
		for {
			u.mu.Lock()
			seq := u.seq
			u.mu.Unlock()
			if seq != 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Select the first text-previewable file so the preview pane shows
	// real content; fall back to row 0.
	u.mu.Lock()
	sel := 0
	for i, r := range u.rows {
		if r.IsDir || !textPreviewableExt(r.Path) {
			continue
		}
		if st, err := os.Stat(r.Path); err != nil || st.IsDir() {
			continue
		}
		sel = i
		break
	}
	if len(u.rows) > 0 {
		u.sel = map[int]bool{sel: true}
		u.focus = sel
		fmt.Fprintf(os.Stderr, "snapshot: rows=%d sel=%d path=%q isdir=%v\n", len(u.rows), sel, u.rows[sel].Path, u.rows[sel].IsDir)
	}
	u.mu.Unlock()

	r := new(input.Router)
	gtx := layout.Context{
		Ops:         new(op.Ops),
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(cfg.size),
		Source:      r.Source(),
	}
	dims := u.layout(gtx)
	r.Frame(gtx.Ops)
	_ = dims

	w, err := headless.NewWindow(cfg.size.X, cfg.size.Y)
	if err != nil {
		return fmt.Errorf("headless window: %w", err)
	}
	defer w.Release()
	if err := w.Frame(gtx.Ops); err != nil {
		return fmt.Errorf("headless frame: %w", err)
	}
	img := image.NewRGBA(image.Rectangle{Max: cfg.size})
	if err := w.Screenshot(img); err != nil {
		return fmt.Errorf("headless screenshot: %w", err)
	}
	f, err := os.Create(cfg.out)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return err
	}
	return nil
}
