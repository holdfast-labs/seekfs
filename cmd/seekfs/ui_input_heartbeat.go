//go:build seekfs_ui && (production || dev)

package main

// UI input heartbeat sender.
//
// seekfs-ui runs in the user's interactive session, where GetLastInputInfo
// sees that session's keyboard/mouse time. The installed service runs in
// session 0 and cannot see it (WTS LastInputTime is zero on this Windows
// build), so the UI reports its session's last-input time to the service
// every uiHeartbeatInterval while the UI process runs (window, tray and
// background alike). The send is fire-and-forget on a background goroutine
// with a short timeout: it never blocks search, render, or Open, and errors
// (service down, pipe busy) are ignored — the next tick retries. When the
// probe itself is unavailable (non-Windows, or GetLastInputInfo failing) the
// tick sends nothing.

import (
	"time"
)

// uiHeartbeatInterval paces the input-heartbeat reports: well under the
// service's 30 s receive-expiry, sparse enough to be noise on the pipe.
var uiHeartbeatInterval = 5 * time.Second

// uiHeartbeatTimeout bounds one report: the loop must never pile up behind a
// wedged pipe.
var uiHeartbeatTimeout = 2 * time.Second

// startInputHeartbeatLoop launches the background heartbeat reporter for the
// UI's pipe. It sends one report immediately, then every interval, until the
// process exits. Safe to call once at startup; a nil app or empty pipe is a
// no-op.
func (a *UIApp) startInputHeartbeatLoop() {
	if a == nil || a.pipeName == "" {
		return
	}
	pipe := a.pipeName
	go func() {
		a.sendInputHeartbeat(pipe)
		ticker := time.NewTicker(uiHeartbeatInterval)
		defer ticker.Stop()
		for range ticker.C {
			a.sendInputHeartbeat(pipe)
		}
	}()
}

// sendInputHeartbeat reads this session's last-input time and reports it. It
// blocks at most uiHeartbeatTimeout and never reports to the caller.
func (a *UIApp) sendInputHeartbeat(pipe string) {
	if pipe == "" {
		return
	}
	at, ok := contentGLIILastInputProbe()
	if !ok || at.IsZero() {
		return
	}
	_, _ = callServiceWithTimeout(pipe, serviceRequest{
		Command:       "input-heartbeat",
		InputUnixNano: at.UnixNano(),
	}, uiHeartbeatTimeout)
}
