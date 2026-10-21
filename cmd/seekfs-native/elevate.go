package main

import "strings"

// elevate.go: the decision that sends a settings write through the elevated
// CLI helper. The actual ShellExecuteEx call is Windows-only; tests exercise
// this pure decision.

// elevationDecision reports whether a settings-set outcome must be retried
// through the elevated helper. canWrite is the settings-get flag.
func elevationDecision(canWrite bool, resp *serviceResponse) bool {
	if !canWrite {
		return true
	}
	if resp == nil || resp.OK {
		return false
	}
	return isElevationRefusal(resp.Message)
}

func isElevationRefusal(message string) bool {
	msg := strings.ToLower(strings.TrimSpace(message))
	if msg == "" {
		return false
	}
	return strings.Contains(msg, "elevat") ||
		strings.Contains(msg, "administrator") ||
		strings.Contains(msg, "access is denied")
}
