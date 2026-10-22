package main

// gcontentstatus.go: content-index messaging for the status bar and the
// results notice. The service reports build progress and failures in info's
// content object; these helpers turn that into short UI text.

const contentIncompleteNoticeText = "Content index is still building — results may be incomplete"

// contentIndexBuilding reports whether the content index is still being built
// or reconciled.
func contentIndexBuilding(c *contentState) bool {
	if c == nil {
		return false
	}
	return c.State == contentStateIndexing || c.State == contentStateReconciling
}

// contentIndexStatusText describes a content-index build or failure for the
// status bar. An idle/ready index has no status text.
func contentIndexStatusText(c *contentState) (string, healthTone) {
	if c == nil {
		return "", healthToneMuted
	}
	if c.BuildError != "" {
		return "Content index: " + c.BuildError, healthToneError
	}
	if contentIndexBuilding(c) {
		if c.BuildTotal > 0 {
			pct := 100 * c.BuildDone / c.BuildTotal
			if pct < 0 {
				pct = 0
			}
			if pct > 100 {
				pct = 100
			}
			return "Content index: building " + itoa(pct) + "% (" +
				formatCompactCount(int64(c.BuildDone)) + " of " +
				formatCompactCount(int64(c.BuildTotal)) + " files)", healthToneMuted
		}
		if c.BuildDone > 0 {
			return "Content index: building… (" + formatCompactCount(int64(c.BuildDone)) + " files)", healthToneMuted
		}
		return "Content index: building…", healthToneMuted
	}
	return "", healthToneMuted
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
