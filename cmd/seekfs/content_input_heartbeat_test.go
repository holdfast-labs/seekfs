package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// The UI heartbeat op is local-only: ordinary local callers may report,
// remote callers are rejected before dispatch, like open/recent.
func TestInputHeartbeatOpAuth(t *testing.T) {
	if got := classifyServiceCommand("input-heartbeat"); got != serviceCommandLocalOnly {
		t.Fatalf("classify input-heartbeat = %v, want local-only", got)
	}
	if !serviceCommandAllowed("input-heartbeat", serviceCapabilities{ReadOnly: true}) {
		t.Fatal("local read-only caller must be allowed to heartbeat")
	}
	if serviceCommandAllowed("input-heartbeat", serviceCapabilities{ReadOnly: true, Remote: true}) {
		t.Fatal("remote caller must be denied heartbeat")
	}
	if serviceCommandAllowed("input-heartbeat", serviceCapabilities{}) {
		t.Fatal("caller with no capabilities must be denied heartbeat")
	}

	resetUIHeartbeatForTest()
	t.Cleanup(resetUIHeartbeatForTest)
	svc := &goSearchService{}
	// Remote dispatch is denied at the capability gate.
	var rbuf bytes.Buffer
	svc.handleServiceCommand(&rbuf, servicePrincipal{}, serviceCapabilities{ReadOnly: true, Remote: true},
		&serviceRequest{Command: "input-heartbeat", InputUnixNano: time.Now().UnixNano()})
	var rresp serviceResponse
	if err := json.Unmarshal(rbuf.Bytes(), &rresp); err != nil || rresp.OK {
		t.Fatalf("remote heartbeat must be denied: %v %s", err, rbuf.String())
	}
	// Local dispatch with a missing timestamp is rejected (not recorded).
	var lbuf bytes.Buffer
	svc.handleServiceCommand(&lbuf, servicePrincipal{SID: "S-1-5-21-1"}, serviceCapabilities{ReadOnly: true},
		&serviceRequest{Command: "input-heartbeat"})
	var lresp serviceResponse
	if err := json.Unmarshal(lbuf.Bytes(), &lresp); err != nil || lresp.OK {
		t.Fatalf("heartbeat without timestamp must fail: %v %s", err, lbuf.String())
	}
	if _, ok := uiHeartbeatSample(); ok {
		t.Fatal("rejected heartbeat must not record a sample")
	}
}

// A fresh heartbeat report is sampled; a report with no refresh past the
// expiry reads unavailable again.
func TestInputHeartbeatExpiry(t *testing.T) {
	resetUIHeartbeatForTest()
	t.Cleanup(resetUIHeartbeatForTest)
	oldExpiry := uiHeartbeatExpiry
	uiHeartbeatExpiry = 30 * time.Second
	t.Cleanup(func() { uiHeartbeatExpiry = oldExpiry })

	at := time.Now().Add(-2 * time.Second)
	noteUIHeartbeat(at)
	got, ok := uiHeartbeatSample()
	if !ok || !got.Equal(at) {
		t.Fatalf("fresh heartbeat sample = (%v, %v), want (%v, true)", got, ok, at)
	}
	// Age the receive time past expiry: the report goes stale even though the
	// input time itself is unchanged.
	uiHeartbeatMu.Lock()
	uiHeartbeatReceivedAt = time.Now().Add(-31 * time.Second)
	uiHeartbeatMu.Unlock()
	if _, ok := uiHeartbeatSample(); ok {
		t.Fatal("stale heartbeat (no refresh past expiry) must sample unavailable")
	}
	// A new report revives the signal.
	noteUIHeartbeat(time.Now())
	if _, ok := uiHeartbeatSample(); !ok {
		t.Fatal("refreshed heartbeat must sample available")
	}
}

// In session 0 the calling-session probe is skipped, so the heartbeat is the
// only input source that can fire there; the freshest source wins and health
// names it.
func TestGovernorUsesHeartbeatInSession0(t *testing.T) {
	stubGovernorSignals(t)
	resetUIHeartbeatForTest()
	t.Cleanup(resetUIHeartbeatForTest)

	contentCurrentSessionID = func() (uint32, bool) { return 0, true }
	// WTS blind (this Windows build), calling-session input present but
	// ignored in session 0.
	contentWTSLastInput = func() (time.Time, bool) { return time.Time{}, false }
	contentGLIILastInput = func() (time.Time, bool) { return time.Now(), true }
	contentResetInputSample()
	if contentSystemInputActive() {
		t.Fatal("session 0 must ignore calling-session input without a heartbeat")
	}

	hbAt := time.Now()
	noteUIHeartbeat(hbAt)
	contentResetInputSample()
	if !contentSystemInputActive() {
		t.Fatal("session 0 with a fresh heartbeat must read active")
	}
	last, ok, src := contentSampleInputTime()
	if !ok || src != "ui-heartbeat" {
		t.Fatalf("sample = (%v, %v, %q), want ui-heartbeat", last, ok, src)
	}
	h := contentGovernorHealthSnapshot()
	if !h.Input.Live || h.Input.Detail != "ui-heartbeat" {
		t.Fatalf("health input = %+v, want live ui-heartbeat", h.Input)
	}

	// Freshest wins: a newer WTS time beats an older heartbeat, and a newer
	// heartbeat beats WTS. Fixed offsets from one base (not two time.Now()
	// calls) so the ordering is exact even on a coarse or loaded clock.
	base := time.Now()
	noteUIHeartbeat(base)
	contentWTSLastInput = func() (time.Time, bool) { return base.Add(time.Minute), true }
	contentResetInputSample()
	if _, _, src := contentSampleInputTime(); src != "wts" {
		t.Fatalf("newer WTS must win, got source %q", src)
	}
	// And a newer heartbeat beats WTS.
	noteUIHeartbeat(base.Add(2 * time.Minute))
	contentResetInputSample()
	if _, _, src := contentSampleInputTime(); src != "ui-heartbeat" {
		t.Fatalf("newer heartbeat must win, got source %q", src)
	}
}

// The governor backs off while heartbeat input is recent: the pause fires on
// input with no other signal.
func TestGovernorPausesOnHeartbeat(t *testing.T) {
	stubGovernorSignals(t)
	resetUIHeartbeatForTest()
	t.Cleanup(resetUIHeartbeatForTest)

	contentCurrentSessionID = func() (uint32, bool) { return 0, true }
	contentWTSLastInput = func() (time.Time, bool) { return time.Time{}, false }
	contentGLIILastInput = func() (time.Time, bool) { return time.Time{}, false }
	contentInputActiveNow = contentSystemInputActive
	contentResetInputSample()
	if got := contentBuildBusyReason(`E:\idx\vol.gsx`); got != "" {
		t.Fatalf("idle reason = %q, want empty", got)
	}
	noteUIHeartbeat(time.Now())
	contentResetInputSample()
	if got := contentBuildBusyReason(`E:\idx\vol.gsx`); got != "input" {
		t.Fatalf("heartbeat-active reason = %q, want input", got)
	}
	// The input pause length is per build-speed mode: Gentle keeps the full
	// pause, Balanced (the default) shortens it to a quarter.
	contentSetBuildSpeed(contentBuildSpeedGentle)
	if got := contentBuildPauseNow(`E:\idx\vol.gsx`); got != contentBuildBatchPause {
		t.Fatalf("gentle heartbeat-active pause = %v, want %v", got, contentBuildBatchPause)
	}
	contentSetBuildSpeed(contentBuildSpeedBalanced)
	if got := contentBuildPauseNow(`E:\idx\vol.gsx`); got != contentBuildBatchPause/4 {
		t.Fatalf("balanced heartbeat-active pause = %v, want %v", got, contentBuildBatchPause/4)
	}
}

// `loaded --json` names the heartbeat source; with no fresh report it reads
// unavailable (never silently idle).
func TestHeartbeatHealthInLoadedJSON(t *testing.T) {
	stubGovernorSignals(t)
	resetUIHeartbeatForTest()
	t.Cleanup(resetUIHeartbeatForTest)

	contentCurrentSessionID = func() (uint32, bool) { return 0, true }
	contentWTSLastInput = func() (time.Time, bool) { return time.Time{}, false }
	contentGLIILastInput = func() (time.Time, bool) { return time.Time{}, false }
	contentResetInputSample()

	cpuOK, diskOK, battOK := contentCPULastOK.Load(), contentDiskLastOK.Load(), contentBatteryLastOK.Load()
	contentCPULastOK.Store(true)
	contentDiskLastOK.Store(true)
	contentBatteryLastOK.Store(true)
	t.Cleanup(func() {
		contentCPULastOK.Store(cpuOK)
		contentDiskLastOK.Store(diskOK)
		contentBatteryLastOK.Store(battOK)
	})

	s := contentTestService(t)
	var buf bytes.Buffer
	s.serviceCommandInfo(&buf, &serviceRequest{})
	var resp serviceResponse
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("decode loaded response: %v", err)
	}
	if resp.Governor == nil || resp.Governor.Input.Live || resp.Governor.Input.Detail != "unavailable" {
		t.Fatalf("governor without heartbeat = %+v, want unavailable", resp.Governor)
	}

	noteUIHeartbeat(time.Now())
	contentResetInputSample()
	buf.Reset()
	s.serviceCommandInfo(&buf, &serviceRequest{})
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("decode loaded response: %v", err)
	}
	if resp.Governor == nil || !resp.Governor.Input.Live || resp.Governor.Input.Detail != "ui-heartbeat" {
		t.Fatalf("governor with heartbeat = %+v, want live ui-heartbeat", resp.Governor)
	}
}
