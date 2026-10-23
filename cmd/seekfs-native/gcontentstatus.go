package main

import "strings"

// gcontentstatus.go: content-index messaging for the status bar and the
// results notice. The service reports build progress and failures in info's
// content object; these helpers turn that into short UI text.

const contentIncompleteNoticeText = "Content index is still building \u2014 results may be incomplete"

// contentIndexBuilding reports whether the content index is still being built
// or reconciled.
func contentIndexBuilding(c *contentState) bool {
	if c == nil {
		return false
	}
	return c.State == contentStateIndexing || c.State == contentStateReconciling
}

func contentBuildPercent(c *contentState) int {
	pct := 0
	if c != nil && c.BuildTotal > 0 {
		pct = 100 * c.BuildDone / c.BuildTotal
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

// contentBuildStatusText is the status-bar text while a build runs.
func contentBuildStatusText(c *contentState) string {
	if c.BuildTotal > 0 {
		return "Content index: building " + itoa(contentBuildPercent(c)) + "% (" +
			formatCompactCount(int64(c.BuildDone)) + " of " +
			formatCompactCount(int64(c.BuildTotal)) + " files)"
	}
	if c.BuildDone > 0 {
		return "Content index: building\u2026 (" + formatCompactCount(int64(c.BuildDone)) + " files)"
	}
	return "Content index: building\u2026"
}

// contentDegradedReason names a user-meaningful reason for a degraded or
// reconciling index. Empty means there is nothing useful to say.
func contentDegradedReason(c *contentState) string {
	if c == nil {
		return ""
	}
	switch {
	case c.IntegrityError != "":
		return "index damaged; rebuilding\u2026"
	case c.FoldError != "":
		return "updating changes failed"
	case c.ScopeReconcile || c.ScopeReconcilePending > 0:
		return "updating changes\u2026"
	case c.ExtractorRefresh || c.StaleExtractorDocs > 0:
		return "updating file types\u2026"
	case c.Stale:
		return "updating changes\u2026"
	case c.Incomplete || c.Partial:
		if c.ScopeDropped > 0 && c.BudgetBytes > 0 {
			return "partial \u2014 size budget reached"
		}
		if c.ScopeDropped > 0 && c.ReserveBytes > 0 {
			return "partial \u2014 disk reserve reached"
		}
		return "partial results"
	case c.SidecarCap > 0 && c.SidecarBytes >= c.SidecarCap:
		return "index size cap reached"
	default:
		return ""
	}
}

// contentIndexStatusText describes a content-index build or failure for the
// status bar. An idle/ready index has no status text, and a degraded index
// with no user-meaningful reason stays silent instead of showing "degraded".
func contentIndexStatusText(c *contentState) (string, healthTone) {
	if c == nil {
		return "", healthToneMuted
	}
	if c.BuildError != "" {
		return "Content index failed: " + truncateStatusText(c.BuildError, 96), healthToneError
	}
	if contentIndexBuilding(c) {
		if c.State == contentStateReconciling {
			if reason := contentDegradedReason(c); reason != "" {
				return "Content index: " + reason, healthToneWarning
			}
		}
		return contentBuildStatusText(c), healthToneMuted
	}
	if c.State == contentStateDegraded {
		if reason := contentDegradedReason(c); reason != "" {
			tone := healthToneWarning
			if c.IntegrityError != "" || c.FoldError != "" {
				tone = healthToneError
			}
			return "Content index: " + reason, tone
		}
		return "", healthToneMuted
	}
	if c.Incomplete || c.Partial {
		if reason := contentDegradedReason(c); reason != "" {
			return "Content index: " + reason, healthToneWarning
		}
	}
	return "", healthToneMuted
}

// statusContentText selects the one status-bar message for Contents mode. A
// content-unavailable search error while the index is still building is not
// repeated in the status bar: the results notice already explains it and the
// status bar keeps the build percentage.
func statusContentText(contentsMode bool, searchContent, health *contentState, unavail string) (string, healthTone) {
	if health != nil {
		if text, tone := contentIndexStatusText(health); text != "" {
			return text, tone
		}
	}
	if unavail != "" {
		if contentUnavailableString(unavail) {
			if contentIndexBuilding(health) {
				return contentBuildStatusText(health), healthToneMuted
			}
			if contentsMode {
				return "", healthToneMuted
			}
			return "Content index unavailable", healthToneMuted
		}
		return truncateStatusText(unavail, 120), healthToneWarning
	}
	if contentsMode && searchContent != nil {
		if text, tone := contentIndexStatusText(searchContent); text != "" {
			return text, tone
		}
		if searchContent.State == contentStateReady {
			return contentStatusText(searchContent), healthToneMuted
		}
	}
	return "", healthToneMuted
}

// contentHealthDetail is the full status text for the Settings panel (where it
// can wrap). It keeps the raw reason visible without crowding the status bar.
func contentHealthDetail(c *contentState) string {
	if c == nil {
		return ""
	}
	if c.BuildError != "" {
		return "Content index build failed: " + strings.TrimSpace(c.BuildError)
	}
	if c.FoldError != "" {
		return "Content index update failed: " + strings.TrimSpace(c.FoldError)
	}
	if c.IntegrityError != "" {
		return "Content index damaged: " + strings.TrimSpace(c.IntegrityError)
	}
	if text, _ := contentIndexStatusText(c); text != "" {
		return text
	}
	return ""
}

// truncateStatusText limits one status-bar segment. It counts runes so UTF-8
// names are never cut mid-rune.
func truncateStatusText(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "\u2026"
	}
	return string(r[:max-1]) + "\u2026"
}

// contentIncompleteNotice is the results banner shown when Contents mode is on
// and the content index cannot answer completely yet.
func contentIncompleteNotice(contentsMode bool, c *contentState) string {
	if !contentsMode || c == nil {
		return ""
	}
	if c.Incomplete || c.Partial || contentIndexBuilding(c) {
		return contentIncompleteNoticeText
	}
	return ""
}
