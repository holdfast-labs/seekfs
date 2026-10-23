//go:build uiautomation

package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type fakeAutomationSink struct {
	state      automationState
	actions    []string
	args       []any
	shotPath   string
	shotErr    error
	hwnd       uintptr
	actionFail bool
}

func (f *fakeAutomationSink) AutomationState() automationState { return f.state }
func (f *fakeAutomationSink) AutomationAction(name string, arg any) error {
	f.actions = append(f.actions, name)
	f.args = append(f.args, arg)
	if f.actionFail {
		return errors.New("action refused")
	}
	return nil
}
func (f *fakeAutomationSink) AutomationScreenshot(path string) error {
	f.shotPath = path
	return f.shotErr
}
func (f *fakeAutomationSink) AutomationHWND() uintptr { return f.hwnd }

func decodeAutomationResponse(t *testing.T, raw []byte) automationResponse {
	t.Helper()
	var resp automationResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, raw)
	}
	return resp
}

func TestAutomationStateDispatch(t *testing.T) {
	sink := &fakeAutomationSink{state: automationState{
		Width: 1600, Height: 1000, SettingsOpen: true, SettingsTab: "drives",
		Rows: 7, Query: "needle", Health: "ok", Entries: 20000,
		Contents: true, RowDetails: []automationRow{{Index: 0, Name: "a.txt", Path: `C:\a.txt`, Snippet: "a needle", MatchRanges: [][2]int{{2, 8}}}}, ResultsNotice: "building", ContentDocs: 12,
		StatusContentText: "Content index: building 42% (42 of 100 files)",
		Drives:            []automationDrive{{Volume: "X:", Indexed: true, State: "ready", Entries: 20001}},
	}}
	resp := decodeAutomationResponse(t, runAutomationCommand([]byte(`{"cmd":"state"}`), sink))
	if !resp.OK || resp.State == nil {
		t.Fatalf("state response = %+v", resp)
	}
	if resp.State.Width != 1600 || resp.State.Rows != 7 || len(resp.State.Drives) != 1 {
		t.Fatalf("state = %+v", resp.State)
	}
	if !resp.State.Contents || len(resp.State.RowDetails) != 1 || resp.State.RowDetails[0].Snippet != "a needle" || len(resp.State.RowDetails[0].MatchRanges) != 1 || resp.State.ResultsNotice == "" || resp.State.ContentDocs != 12 {
		t.Fatalf("content state = %+v", resp.State)
	}
	if resp.State.StatusContentText != "Content index: building 42% (42 of 100 files)" {
		t.Fatalf("status content text = %q", resp.State.StatusContentText)
	}
	if resp.State.Drives[0].Volume != "X:" || resp.State.Drives[0].Entries != 20001 {
		t.Fatalf("drive = %+v", resp.State.Drives[0])
	}
}

func TestAutomationActionDispatch(t *testing.T) {
	sink := &fakeAutomationSink{}
	cases := []struct {
		raw  string
		name string
		arg  any
	}{
		{`{"cmd":"action","name":"open-settings","arg":"content"}`, "open-settings", "content"},
		{`{"cmd":"action","name":"toggle-drive","arg":"X:"}`, "toggle-drive", "X:"},
		{`{"cmd":"action","name":"set-install-service","arg":true}`, "set-install-service", true},
		{`{"cmd":"action","name":"set-query","arg":"needle"}`, "set-query", "needle"},
		{`{"cmd":"action","name":"type-query","arg":"needle"}`, "type-query", "needle"},
		{`{"cmd":"action","name":"search-now","arg":"needle"}`, "search-now", "needle"},
		{`{"cmd":"action","name":"set-contents-mode","arg":true}`, "set-contents-mode", true},
		{`{"cmd":"action","name":"set-content-mode","arg":"explicit"}`, "set-content-mode", "explicit"},
		{`{"cmd":"action","name":"apply-settings"}`, "apply-settings", nil},
	}
	for _, tc := range cases {
		resp := decodeAutomationResponse(t, runAutomationCommand([]byte(tc.raw), sink))
		if !resp.OK {
			t.Fatalf("%s: %+v", tc.name, resp)
		}
	}
	if len(sink.actions) != len(cases) {
		t.Fatalf("actions = %v", sink.actions)
	}
	for i, tc := range cases {
		if sink.actions[i] != tc.name {
			t.Fatalf("action %d = %q, want %q", i, sink.actions[i], tc.name)
		}
		if sink.args[i] != tc.arg {
			t.Fatalf("action %s arg = %#v, want %#v", tc.name, sink.args[i], tc.arg)
		}
	}
}

func TestAutomationActionErrors(t *testing.T) {
	sink := &fakeAutomationSink{actionFail: true}
	resp := decodeAutomationResponse(t, runAutomationCommand([]byte(`{"cmd":"action","name":"apply-settings"}`), sink))
	if resp.OK || resp.Error != "action refused" {
		t.Fatalf("failed action = %+v", resp)
	}
	resp = decodeAutomationResponse(t, runAutomationCommand([]byte(`{"cmd":"action"}`), sink))
	if resp.OK {
		t.Fatalf("missing name = %+v", resp)
	}
	resp = decodeAutomationResponse(t, runAutomationCommand([]byte(`{"cmd":"nope"}`), sink))
	if resp.OK {
		t.Fatalf("unknown cmd = %+v", resp)
	}
	resp = decodeAutomationResponse(t, runAutomationCommand([]byte(`not json`), sink))
	if resp.OK {
		t.Fatalf("bad json = %+v", resp)
	}
}

func TestAutomationScreenshotAndHWND(t *testing.T) {
	dir := t.TempDir()
	sink := &fakeAutomationSink{hwnd: 0x1234}
	path := filepath.Join(dir, "shot.png")
	resp := decodeAutomationResponse(t, runAutomationCommand([]byte(`{"cmd":"screenshot","path":"`+jsonEscaped(path)+`"}`), sink))
	if !resp.OK || sink.shotPath != path {
		t.Fatalf("screenshot = %+v path=%q", resp, sink.shotPath)
	}
	sink.shotErr = errors.New("headless failed")
	resp = decodeAutomationResponse(t, runAutomationCommand([]byte(`{"cmd":"screenshot","path":"x.png"}`), sink))
	if resp.OK || resp.Error != "headless failed" {
		t.Fatalf("screenshot error = %+v", resp)
	}
	resp = decodeAutomationResponse(t, runAutomationCommand([]byte(`{"cmd":"hwnd"}`), sink))
	if !resp.OK || resp.HWND != 0x1234 {
		t.Fatalf("hwnd = %+v", resp)
	}
}

func TestAutomationMetricsResetAndSnapshot(t *testing.T) {
	autoMetricsReset()
	autoMetricsBeginAction("set-query")
	autoMetricsRecordFrame(time.Now(), 12*time.Millisecond)
	autoMetricsRecordFrame(time.Now(), 30*time.Millisecond)
	autoMetricsCompleteActions(time.Now())
	resp := decodeAutomationResponse(t, runAutomationCommand([]byte(`{"cmd":"metrics"}`), &fakeAutomationSink{}))
	if !resp.OK || resp.Metrics == nil {
		t.Fatalf("metrics = %+v", resp)
	}
	if resp.Metrics.Frames.Count != 2 || resp.Metrics.Frames.Max != 30 {
		t.Fatalf("frames = %+v", resp.Metrics.Frames)
	}
	if len(resp.Metrics.Actions) != 1 || resp.Metrics.Actions[0].Name != "set-query" {
		t.Fatalf("actions = %+v", resp.Metrics.Actions)
	}
	resp = decodeAutomationResponse(t, runAutomationCommand([]byte(`{"cmd":"metrics-reset"}`), &fakeAutomationSink{}))
	if !resp.OK {
		t.Fatalf("reset = %+v", resp)
	}
	resp = decodeAutomationResponse(t, runAutomationCommand([]byte(`{"cmd":"metrics"}`), &fakeAutomationSink{}))
	if resp.Metrics.Frames.Count != 0 || len(resp.Metrics.Actions) != 0 {
		t.Fatalf("metrics after reset = %+v", resp.Metrics)
	}
}

func jsonEscaped(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}
