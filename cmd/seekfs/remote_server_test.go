package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"
)

func TestRemoteResponseAllowlist(t *testing.T) {
	resp := serviceResponse{
		OK:              true,
		Count:           3,
		SearchMS:        12.5,
		Source:          "global-name",
		PlannerMode:     "global-name",
		PID:             1234,
		Executable:      "C:\\ProgramData\\seekfs\\seekfs.exe",
		ExecutableHash:  "abc123",
		PipeName:        `\\.\pipe\seekfs-service`,
		ProcessMode:     "windows-service",
		Version:         "1.6.0",
		Commit:          "abc",
		Date:            "today",
		BuildFlavor:     "release",
		BlocksDecoded:   99,
		ScalarDriver:    "x",
		ComponentBounds: "secret-bounds",
		Terms:           []traceTerm{{Term: "x", Kind: "y"}},
		Declines:        []traceDecline{{Source: "s", Reason: "r"}},
		Runtime:         &runtimeMemoryInfo{HeapAllocBytes: 1},
		EligibleVolumes: []string{"C:", "F:"},
		Results:         []string{"C:\\foo\\bar.txt"},
		Rows:            []jsonResult{{Path: "C:\\foo\\bar.txt"}},
		Candidates:      5,
		Decline:         "missing-posting",
		Fallback:        "bounded-scan",
		Entries:         42,
		Loading:         false,
		Fuzzy:           true,
		Complete:        boolPtr(false),
		Health:          "ok",
		HealthMessage:   "volume C: stale: journal replay failed",
		Content: &contentHealth{
			State: "degraded", Partial: true, Incomplete: true, CountDivergent: true,
			DegradedVolumes: []string{"F:"}, BuildError: "secret build error",
			FoldError: "secret fold error", DeltaBytes: 4096,
		},
		DBs: []dbInfo{{Path: "C:\\ProgramData\\seekfs\\indexes\\seekfs_c.gsi", Entries: 100, Volume: "C:", State: "ready", JournalID: 123, Checkpoint: 456, Memory: &residentMemoryInfo{Records: 100}, LastPersistError: "boom", Source: "usn", BuiltAt: "2026-01-01T00:00:00Z", FRNRecords: 100}},
	}

	out := remoteResponseFromService(resp, false)

	// The projection type has no fields for physical internals; verify the
	// marshaled JSON contains none of them.
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal remoteResponse: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, forbidden := range []string{"pid", "executable", "executable_hash", "pipe_name", "process_mode", "runtime", "journal_id", "checkpoint", "component_bounds", "blocks_decoded", "memory", "planner_mode", "candidates", "decline", "fallback", "build_error", "fold_error", "delta_bytes"} {
		if _, ok := raw[forbidden]; ok {
			t.Errorf("remote response leaks forbidden field %q", forbidden)
		}
	}

	// Public fields preserved.
	if out.Count != 3 || !out.OK || len(out.Results) != 1 {
		t.Errorf("public result fields lost: %+v", out)
	}
	if len(out.EligibleVolumes) != 2 || out.EligibleVolumes[0] != "C:" {
		t.Errorf("eligible volumes lost: %+v", out.EligibleVolumes)
	}
	if out.Version != "1.6.0" || out.Commit != "abc" {
		t.Errorf("version/commit should be preserved: %+v", out)
	}
	// Public response semantics retained: info totals, fuzzy flag, and the
	// completeness signal so partial fallback results are not mistaken for
	// exhaustive results.
	if out.Entries != 42 {
		t.Errorf("entries lost: %+v", out)
	}
	if !out.Fuzzy {
		t.Errorf("fuzzy flag lost: %+v", out)
	}
	if out.Complete == nil || *out.Complete {
		t.Errorf("complete signal lost: %+v", out)
	}
	// The internal planner source ("global-name") collapses to the coarse
	// public category rather than leaking the route name.
	if out.Source != "indexed" {
		t.Errorf("source should be coarse category, got %q", out.Source)
	}
	if len(out.DBs) != 1 {
		t.Fatalf("DBs count = %d, want 1", len(out.DBs))
	}
	d := out.DBs[0]
	if d.Volume != "C:" || d.State != "ready" || d.Entries != 100 || d.Source != "usn" {
		t.Errorf("public db summary lost: %+v", d)
	}
	// Health message is coarse-capped to avoid leaking the stale reason/path.
	if out.HealthMessage == "volume C: stale: journal replay failed" {
		t.Errorf("health message leaked internal detail: %q", out.HealthMessage)
	}
	// Content health is projected coarsely: the trust flags and volume labels
	// reach a remote caller, but the error text and sizes do not.
	if out.Content == nil {
		t.Fatal("content health not projected to remote callers")
	}
	if out.Content.State != "degraded" || !out.Content.Partial || !out.Content.Incomplete ||
		!out.Content.CountDivergent || len(out.Content.DegradedVolumes) != 1 || out.Content.DegradedVolumes[0] != "F:" {
		t.Errorf("content health flags lost: %+v", out.Content)
	}
}

func TestGenericRemoteMessage(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"loading indexes", "loading indexes"},
		{"service running", "service running"},
		{"service has no search indexes loaded", "service has no search indexes loaded"},
		{"boom: C:\\ProgramData\\seekfs\\indexes\\seekfs_c.gsi: access denied", "request failed"},
		{"permission denied on volume F:", "request failed"},
	}
	for _, tt := range tests {
		if got := genericRemoteMessage(tt.in); got != tt.want {
			t.Errorf("genericRemoteMessage(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestReadRemoteFramePayload(t *testing.T) {
	msg := `{"type":"request","id":7,"v":1,"payload":{"command":"search"}}`
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(msg)))
	buf.Write(hdr[:])
	buf.WriteString(msg)
	r := bufio.NewReader(&buf)
	got, err := readRemoteFramePayload(r)
	if err != nil {
		t.Fatalf("readRemoteFramePayload: %v", err)
	}
	if string(got) != msg {
		t.Errorf("payload mismatch:\n got %s\nwant %s", got, msg)
	}

	if _, err := readRemoteFramePayload(bufio.NewReader(&buf)); err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestReadRemoteFramePayloadTooLarge(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], remoteMaxFrameBytes+1)
	buf.Write(hdr[:])
	r := bufio.NewReader(&buf)
	if _, err := readRemoteFramePayload(r); err == nil {
		t.Fatal("expected error for oversized frame")
	}
}

func TestClampRemoteDeadline(t *testing.T) {
	now := time.Now().UnixNano()
	if got := clampRemoteDeadline(0); got <= now {
		t.Errorf("clampRemoteDeadline(0) = %d, want future default", got)
	}
	farFuture := now + int64(24*time.Hour)
	max := now + int64(serviceQueryTimeout)
	if got := clampRemoteDeadline(farFuture); got > max {
		t.Errorf("clampRemoteDeadline(huge) = %d, want capped at %d", got, max)
	}
	small := now + int64(time.Second)
	if got := clampRemoteDeadline(small); got != small {
		t.Errorf("clampRemoteDeadline(small) = %d, want %d", got, small)
	}
}

func TestRemoteLoopbackServerRoundTrip(t *testing.T) {
	s := &goSearchService{}
	rs := newRemoteLoopbackServer(s, "127.0.0.1:0")
	if err := rs.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer rs.close()
	addr := rs.ln.Addr().String()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := remoteFrame{Type: remoteFrameHello, ID: 1, V: remoteProtocolVersion}
	writeFrame(t, conn, req)
	resp := readFrame(t, conn)
	if resp.Type != remoteFrameResponse || resp.ID != 1 {
		t.Fatalf("hello response = %+v", resp)
	}
	var hello remoteHello
	if err := json.Unmarshal(resp.Payload, &hello); err != nil {
		t.Fatalf("hello payload: %v", err)
	}
	if hello.Proto != remoteProtocolVersion || hello.Mode != "loopback" {
		t.Errorf("hello = %+v", hello)
	}
}

func TestRemoteLoopbackServerRejectsNonLoopback(t *testing.T) {
	s := &goSearchService{}
	rs := newRemoteLoopbackServer(s, "0.0.0.0:0")
	if err := rs.start(); err == nil {
		rs.close()
		t.Fatal("expected non-loopback bind to be rejected")
	}
}

func TestRemoteLoopbackSearchDeniedWhenNoIndexes(t *testing.T) {
	s := &goSearchService{}
	rs := newRemoteLoopbackServer(s, "127.0.0.1:0")
	if err := rs.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer rs.close()
	conn, err := net.Dial("tcp", rs.ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := remoteFrame{Type: remoteFrameRequest, ID: 5, V: remoteProtocolVersion, Payload: mustJSON(serviceRequest{Command: "search", Query: "anything"})}
	writeFrame(t, conn, req)
	resp := readFrame(t, conn)
	if resp.ID != 5 || resp.Type != remoteFrameResponse {
		t.Fatalf("response = %+v", resp)
	}
	var out remoteResponse
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatalf("response payload: %v", err)
	}
	if out.OK {
		t.Error("expected OK=false for service with no indexes")
	}
	if out.Message == "" {
		t.Error("expected a helpful message")
	}
}

func TestRemoteZeroRequestIDRejected(t *testing.T) {
	s := &goSearchService{}
	rs := newRemoteLoopbackServer(s, "127.0.0.1:0")
	if err := rs.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer rs.close()
	conn, err := net.Dial("tcp", rs.ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := remoteFrame{Type: remoteFrameRequest, ID: 0, V: remoteProtocolVersion, Payload: mustJSON(serviceRequest{Command: "search"})}
	writeFrame(t, conn, req)
	resp := readFrame(t, conn)
	if resp.Type != remoteFrameError {
		t.Fatalf("expected error frame for id=0, got %+v", resp)
	}
}

func TestRemoteWatchDeltaDenied(t *testing.T) {
	if serviceCommandAllowed("watch-delta", serviceCapabilities{ReadOnly: true, Remote: true}) {
		t.Error("watch-delta must be denied for remote read-only callers")
	}
	if !serviceCommandAllowed("watch-delta", serviceCapabilities{ReadOnly: true}) {
		t.Error("watch-delta must stay allowed for local read-only callers")
	}
}

func TestRemoteDuplicateRequestID(t *testing.T) {
	// Duplicate request ids are rejected deterministically: pre-seed an
	// in-flight state for id 7, then a second request with the same id must be
	// rejected without relying on goroutine scheduling.
	rs := newRemoteLoopbackServer(nil, "127.0.0.1:0")
	a, b := net.Pipe()
	rc := &remoteConn{rs: rs, conn: a, inFlight: make(map[int64]*remoteRequestState)}
	rc.inFlight[7] = &remoteRequestState{done: make(chan struct{})}

	// Read the rejection frame in the background: writes to net.Pipe block
	// until read.
	frames := make(chan remoteFrame, 1)
	errs := make(chan error, 1)
	go func() {
		br := bufio.NewReader(b)
		payload, err := readRemoteFramePayload(br)
		if err != nil {
			errs <- err
			return
		}
		var f remoteFrame
		if err := json.Unmarshal(payload, &f); err != nil {
			errs <- err
			return
		}
		frames <- f
	}()

	payload := mustJSON(serviceRequest{Command: "search"})
	rc.handleRequest(7, payload)

	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatal("frame channel closed early")
		}
		var out remoteResponse
		_ = json.Unmarshal(f.Payload, &out)
		if out.Message != "duplicate request id" {
			t.Errorf("expected duplicate rejection, got %q", out.Message)
		}
	case err := <-errs:
		t.Fatalf("read frame: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for duplicate rejection frame")
	}
}

func TestRemoteShutdownCancelsInFlight(t *testing.T) {
	// shutdown must cancel in-flight state without waiting, so a failed write
	// from inside a request cannot deadlock on its own done channel.
	st := &remoteRequestState{done: make(chan struct{})}
	a, _ := net.Pipe()
	rc := &remoteConn{conn: a, inFlight: map[int64]*remoteRequestState{3: st}}
	rc.shutdown()
	if !st.cancelFlag.Load() {
		t.Error("in-flight request was not cancelled on shutdown")
	}
	// It must not block forever even though done is never closed.
	select {
	case <-time.After(50 * time.Millisecond):
	case <-st.done:
		t.Fatal("shutdown must not wait on done")
	}
}

func TestRemoteCancellation(t *testing.T) {
	st := &remoteRequestState{done: make(chan struct{})}
	if st.requestCancelFunc()() {
		t.Error("cancel flag should be false initially")
	}
	st.cancel()
	if !st.requestCancelFunc()() {
		t.Error("cancel flag should be true after cancel")
	}
}

func TestRemoteClosedRejectsNewRequests(t *testing.T) {
	// Once shutdown begins, a request that was already read (and not yet
	// admitted) must be rejected, so close cannot miss newly admitted work.
	rs := newRemoteLoopbackServer(nil, "127.0.0.1:0")
	a, b := net.Pipe()
	rc := &remoteConn{rs: rs, conn: a, inFlight: make(map[int64]*remoteRequestState)}
	rc.closed.Store(true)

	frames := make(chan remoteFrame, 1)
	errs := make(chan error, 1)
	go func() {
		br := bufio.NewReader(b)
		payload, err := readRemoteFramePayload(br)
		if err != nil {
			errs <- err
			return
		}
		var f remoteFrame
		if err := json.Unmarshal(payload, &f); err != nil {
			errs <- err
			return
		}
		frames <- f
	}()

	rc.handleRequest(9, mustJSON(serviceRequest{Command: "search"}))

	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatal("frame channel closed early")
		}
		var out remoteResponse
		_ = json.Unmarshal(f.Payload, &out)
		if out.Message != "connection closed" {
			t.Errorf("expected connection-closed rejection, got %q", out.Message)
		}
	case err := <-errs:
		t.Fatalf("read frame: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for connection-closed rejection frame")
	}
}

func TestRemoteLimitClampedBeforeDispatch(t *testing.T) {
	// The remote limit must be clamped server-side before the engine sees it.
	big := serviceRequest{Command: "search", Limit: 1 << 20}
	clampRemoteLimit(&big)
	if big.Limit != remoteMaxResultLimit {
		t.Errorf("clampRemoteLimit(1<<20) = %d, want %d", big.Limit, remoteMaxResultLimit)
	}
	// A non-positive limit preserves the engine's normal default (100 results);
	// it must not be promoted to the maximum.
	neg := serviceRequest{Command: "search", Limit: -5}
	clampRemoteLimit(&neg)
	if neg.Limit != -5 {
		t.Errorf("clampRemoteLimit(-5) = %d, want unchanged -5", neg.Limit)
	}
	zero := serviceRequest{Command: "search", Limit: 0}
	clampRemoteLimit(&zero)
	if zero.Limit != 0 {
		t.Errorf("clampRemoteLimit(0) = %d, want unchanged 0", zero.Limit)
	}
	// A positive value within the bound passes through unchanged.
	ok := serviceRequest{Command: "search", Limit: 500}
	clampRemoteLimit(&ok)
	if ok.Limit != 500 {
		t.Errorf("clampRemoteLimit(500) = %d, want 500", ok.Limit)
	}
	// Count-only requests do not materialize rows; leave the limit untouched.
	co := serviceRequest{Command: "search", CountOnly: true, Limit: 1 << 20}
	clampRemoteLimit(&co)
	if co.Limit != 1<<20 {
		t.Errorf("count-only limit should be untouched, got %d", co.Limit)
	}
}

func TestRemoteSearchSourceCoarse(t *testing.T) {
	// Detailed planner route names must collapse to stable public categories.
	// Fuzzy behavior is conveyed by the separate Fuzzy field, not the source.
	tests := []struct {
		in        string
		countOnly bool
		want      string
	}{
		{"", false, ""},
		{"", true, ""},
		{"global:filename-pngc", false, "indexed"},
		{"global:filename-pngr", false, "indexed"},
		{"global:filename-trigram", false, "indexed"},
		{"planned:ext-top", false, "indexed"},
		{"planned:boolean", false, "indexed"},
		{"planned", false, "indexed"},
		{"compact-name-order-scan", false, "bounded-scan"},
		{"bounded-scan", false, "bounded-scan"},
		{"global:bounded-scan", false, "bounded-scan"},
		{"filesystem-under-fallback", false, "bounded-scan"},
		{"legacy-planner", false, "bounded-scan"},
		{"count-fast-posting", false, "count"},
		{"count-fast-pngc", false, "count"},
		{"count-fast-pngr", false, "count"},
		{"parallel-name-count", false, "count"},
		{"global:boolean-persisted-count", false, "count"},
		{"planned:or-group-lazy-count", false, "count"},
		// Any count-only request is a count, regardless of internal route.
		{"global:bounded-scan", true, "count"},
		{"planned:or-group-lazy-top", true, "count"},
		{"name-trigram", false, "indexed"},
		{"component-trigram", false, "indexed"},
		{"exact-name", false, "indexed"},
		{"path-component-trigram", false, "indexed"},
	}
	for _, tt := range tests {
		if got := remoteSearchSource(tt.in, tt.countOnly); got != tt.want {
			t.Errorf("remoteSearchSource(%q, countOnly=%v) = %q, want %q", tt.in, tt.countOnly, got, tt.want)
		}
	}
}

func TestRemotePanicContainment(t *testing.T) {
	// A panic inside the dispatch must not terminate the process; the request
	// worker recovers, emits a generic error, and cleans up in-flight state.
	// Point the server at a nil service so handleServiceCommand panics.
	rs := newRemoteLoopbackServer(nil, "127.0.0.1:0")
	if err := rs.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	conn, err := net.Dial("tcp", rs.ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	defer rs.close()

	// Send a search request; the nil service makes dispatch panic internally.
	req := remoteFrame{Type: remoteFrameRequest, ID: 3, V: remoteProtocolVersion, Payload: mustJSON(serviceRequest{Command: "search", Query: "x"})}
	writeFrame(t, conn, req)

	// The server must respond (generic error) rather than crashing.
	resp := readFrame(t, conn)
	if resp.ID != 3 || resp.Type != remoteFrameResponse {
		t.Fatalf("response = %+v", resp)
	}
	var out remoteResponse
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatalf("response payload: %v", err)
	}
	if out.OK {
		t.Error("expected OK=false from a panicking dispatch")
	}
	if out.Message != "internal error" {
		t.Errorf("expected generic internal error, got %q", out.Message)
	}
}

func writeFrame(t *testing.T, w io.Writer, f remoteFrame) {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
}

func readFrame(t *testing.T, r io.Reader) remoteFrame {
	t.Helper()
	br := bufio.NewReader(r)
	payload, err := readRemoteFramePayload(br)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var f remoteFrame
	if err := json.Unmarshal(payload, &f); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	return f
}
