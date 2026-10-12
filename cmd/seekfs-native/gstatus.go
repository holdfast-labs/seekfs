package main

import (
	"fmt"
	"image/color"

	"gioui.org/layout"
)

// gstatus.go: the bottom status bar — result count/message, query time,
// content health, and index health.

// layoutStatus renders the status bar.
func (u *UI) layoutStatus(gtx layout.Context) layout.Dimensions {
	s := u.scheme
	u.mu.Lock()
	n := len(u.rows)
	msg := u.fuzzyMsg
	elapsed := u.elapsedMS
	searchMS := u.searchMS
	content := u.content
	contentMode := u.contents
	unavail := u.unavail
	health, healthMsg := u.health, u.healthMsg
	entries := u.entries
	flash := u.flashMsg
	u.flashMsg = ""
	u.mu.Unlock()

	countText := fmt.Sprintf("%s result%s", formatCount(n), plural(n))
	if msg != "" {
		countText += " — " + msg
	}
	if unavail != "" {
		countText = unavail
	}
	if flash != "" {
		countText = flash
	}

	timeText := ""
	if elapsed > 0 || searchMS > 0 {
		timeText = fmt.Sprintf("%d ms (service %.0f ms)", elapsed, searchMS)
	}

	contentText := ""
	if contentMode || (content != nil && content.State != "") {
		if unavail != "" {
			contentText = "Contents unavailable"
		} else {
			contentText = contentStatusText(content)
		}
	}

	healthText, healthColor := "Connecting…", s.Muted
	switch health {
	case serviceHealthOK:
		healthText = fmt.Sprintf("Index ready (%s entries)", formatCount(entries))
		healthColor = s.OK
	case serviceHealthDegraded:
		healthText = "Index: " + firstNonEmpty(healthMsg, "degraded")
		healthColor = s.Warning
	case serviceHealthError:
		healthText = "Index: " + firstNonEmpty(healthMsg, "error")
		healthColor = s.Error
	default:
		if healthMsg != "" {
			healthText = healthMsg
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
					return label(u.th, Sp(12), contentText, s.Muted).Layout(gtx)
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

// healthDot draws the status dot.
func (u *UI) healthDot(gtx layout.Context, c color.NRGBA) layout.Dimensions {
	d := gtx.Dp(Dp(8))
	gtx.Constraints.Min.X = d
	gtx.Constraints.Min.Y = d
	roundedFill(gtx, c, gtx.Constraints.Min, Dp(4))
	return layout.Dimensions{Size: gtx.Constraints.Min}
}
