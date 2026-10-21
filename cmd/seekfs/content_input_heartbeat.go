package main

// UI input heartbeat: the session-0 input signal.
//
// The installed service runs as LocalSystem in session 0, which never
// receives keyboard/mouse input, and on this Windows build the WTS
// LastInputTime is zero on every session (see §6w). GetLastInputInfo only
// sees the calling session, so the service cannot see interactive input on
// its own. seekfs-ui runs in the user's interactive session and reports its
// session's GetLastInputInfo time to the service over the existing pipe
// protocol (`input-heartbeat` op, local-only like open/recent).
//
// The service keeps the freshest valid report as its input signal and lets
// it expire: with no heartbeat for uiHeartbeatExpiry (30 s) the signal reads
// unavailable again (fail open for decisions, live=false in health). The
// freshest report wins so one UI per session composes without coordination;
// a stale UI that stops heartbeating cannot pin the signal once its report
// ages past the receive-expiry.

import (
	"sync"
	"time"
)

// uiHeartbeatExpiry bounds how long a heartbeat report stays valid without a
// refresh. A var so tests drive expiry without sleeping.
var uiHeartbeatExpiry = 30 * time.Second

var uiHeartbeatMu sync.Mutex
var uiHeartbeatInputAt time.Time
var uiHeartbeatReceivedAt time.Time
var uiHeartbeatHave bool

// noteUIHeartbeat records one UI input-heartbeat report. The reported time is
// the UI session's last-input instant (UTC); a zero time is rejected by the
// caller. The freshest report wins; the receive time gates expiry.
func noteUIHeartbeat(inputAt time.Time) {
	if inputAt.IsZero() {
		return
	}
	now := time.Now()
	uiHeartbeatMu.Lock()
	defer uiHeartbeatMu.Unlock()
	if !uiHeartbeatHave || inputAt.After(uiHeartbeatInputAt) {
		uiHeartbeatInputAt = inputAt
	}
	uiHeartbeatReceivedAt = now
	uiHeartbeatHave = true
}

// uiHeartbeatSample returns the heartbeat input time when a fresh report
// exists (received within uiHeartbeatExpiry). ok is false when no report has
// arrived or the last report is stale.
func uiHeartbeatSample() (t time.Time, ok bool) {
	uiHeartbeatMu.Lock()
	defer uiHeartbeatMu.Unlock()
	if !uiHeartbeatHave {
		return time.Time{}, false
	}
	if time.Since(uiHeartbeatReceivedAt) >= uiHeartbeatExpiry {
		return time.Time{}, false
	}
	return uiHeartbeatInputAt, true
}

// resetUIHeartbeatForTest drops the heartbeat state so tests start clean.
func resetUIHeartbeatForTest() {
	uiHeartbeatMu.Lock()
	defer uiHeartbeatMu.Unlock()
	uiHeartbeatInputAt = time.Time{}
	uiHeartbeatReceivedAt = time.Time{}
	uiHeartbeatHave = false
}
