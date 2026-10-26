package main

import (
	"fmt"
	"image"
	"image/color"

	"gioui.org/layout"
)

// gstatus.go: the bottom status bar — result count/message, query time,
// content health, and index health.

type healthTone int

const (
	healthToneMuted healthTone = iota
	healthToneOK
	healthToneWarning
	healthToneError
)

// healthStatusText maps the service health to the status bar text and tone.
// The normal first-run state (service up, no drives indexed yet) is neutral,
// not an error.
func healthStatusText(health, msg string, entries int) (string, healthTone) {
	switch health {
	case serviceHealthOK:
		return fmt.Sprintf("Index ready (%s entries)", formatCount(entries)), healthToneOK
	case serviceHealthDegraded:
		return "Index: " + firstNonEmpty(msg, "degraded"), healthToneWarning
	case serviceHealthEmpty:
		return firstNonEmpty(msg, "No drives indexed yet"), healthToneMuted
	case serviceHealthError:
		return "Index: " + firstNonEmpty(msg, "error"), healthToneError
	default:
		if msg != "" {
			return msg, healthToneMuted
		}
		return "Connecting…", healthToneMuted
	}
}

// layoutStatus renders the status bar.
func (u *UI) layoutStatus(gtx layout.Context) layout.Dimensions {
	s := u.scheme
	u.mu.Lock()
	n := len(u.rows)
	msg := u.fuzzyMsg
	elapsed := u.elapsedMS
	searchMS := u.searchMS
	content := u.content
	contentIndex := u.contentHealth
	contentMode := u.contents
	unavail := u.unavail
	health, healthMsg := u.health, u.healthMsg
	entries := u.entries
	flash := u.flashMsg
	snapshot := u.settingsSnapshot
	u.flashMsg = ""
	u.mu.Unlock()

	countText := fmt.Sprintf("%s result%s", formatCount(n), plural(n))
	if msg != "" {
		countText += " \u2014 " + truncateStatusText(msg, 80)
	}
	if unavail != "" && !contentUnavailableString(unavail) {
		countText = truncateStatusText(unavail, 120)
	}
	if flash != "" {
		countText = truncateStatusText(flash, 120)
	}

	timeText := ""
	if elapsed > 0 || searchMS > 0 {
		timeText = fmt.Sprintf("%d ms (service %.0f ms)", elapsed, searchMS)
	}

	contentText, contentTone := statusContentText(contentMode, content, contentIndex, unavail)
	contentColor := s.Muted
	switch contentTone {
	case healthToneWarning:
		contentColor = s.Warning
	case healthToneError:
		contentColor = s.Error
	}

	healthText, healthColor := "Connecting\u2026", s.Muted
	healthText, tone := healthStatusText(health, healthMsg, entries)
	healthText = truncateStatusText(healthText, 96)
	healthColor = s.Muted
	switch tone {
	case healthToneOK:
		healthColor = s.OK
	case healthToneWarning:
		healthColor = s.Warning
	case healthToneError:
		healthColor = s.Error
	}
	if snapshot != nil {
		for _, v := range snapshot.Volumes {
			if v.State == "indexing" {
				healthText = formatIndexingStatus(v.Volume, v.Progress)
				healthColor = s.Warning
				break
			}
		}
	}

	dims := layout.Inset{Left: Dp(12), Right: Dp(12), Top: Dp(6), Bottom: Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return label(u.th, Sp(12), countText, s.Subtle).Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return label(u.th, Sp(12), timeText, s.Muted).Layout(gtx)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return label(u.th, Sp(12), contentText, contentColor).Layout(gtx)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return u.healthDot(gtx, healthColor)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Left: Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return label(u.th, Sp(12), healthText, s.Subtle).Layout(gtx)
							})
						}),
					)
				})
			}),
		)
	})
	return layout.Stack{}.Layout(gtx,
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			fillRect(gtx, s.Panel)
			return dims
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.N.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return hairline(gtx, s.Border)
			})
		}),
	)
}

// healthDot draws the status dot at a fixed 8px.
func (u *UI) healthDot(gtx layout.Context, c color.NRGBA) layout.Dimensions {
	d := gtx.Dp(Dp(8))
	gtx.Constraints.Min = image.Point{X: d, Y: d}
	gtx.Constraints.Max = image.Point{X: d, Y: d}
	roundedFill(gtx, c, gtx.Constraints.Min, Dp(4))
	return layout.Dimensions{Size: gtx.Constraints.Min}
}
