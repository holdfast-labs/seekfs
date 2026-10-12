package main

// Phase 2 (Mode L): versioned length-prefixed JSON framing over a loopback TCP
// listener, connection-scoped request IDs, asynchronous request dispatch with
// real cancellation, and an allowlist-based remote response projection.
// See docs/NETWORK_CLIENT_SERVER_IMPLEMENTATION_SCOPE.md §7 and §8.
//
// The loopback listener is DISABLED by default and must be explicitly enabled
// (e.g. -remote-addr 127.0.0.1:port).  It is the development/proof-of-transport
// path only and is never a privilege boundary: an unauthenticated loopback
// listener is reachable by other local sessions.  Production LAN exposure
// (Mode N) ships in Phase 3 with the broker + auth.

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// remoteProtocolVersion is the wire protocol version for the Mode L transport.
const remoteProtocolVersion = 1

// remoteMaxFrameBytes bounds a single frame; larger frames close the
// connection.
const remoteMaxFrameBytes = 16 * 1024 * 1024

// remoteMaxResultBytes bounds a single encoded response payload before it is
// written, so an oversized result cannot stall the client with no response.
const remoteMaxResultBytes = 8 * 1024 * 1024

// remoteMaxResultLimit bounds the number of result rows a remote caller may
// request.  It is enforced before dispatch so an unauthenticated loopback caller
// cannot force the engine to allocate large result sets (the encoded-response
// bound alone runs too late).  The exact budget is an implementation choice.
const remoteMaxResultLimit = 50_000

// remoteWriteTimeout bounds a single socket write so a client that stops reading
// cannot block cancellation, request cleanup, or new admission behind a slow
// writer.
const remoteWriteTimeout = 15 * time.Second

// remoteConnMaxInFlight bounds outstanding requests per remote connection.
const remoteConnMaxInFlight = 32

// remoteIdleTimeout expires a connection that sends no bytes within the
// window; it bounds idle connections and is not a strict performance threshold.
const remoteIdleTimeout = 10 * time.Minute

// remoteFrameType enumerates the frame kinds in the remote envelope.
type remoteFrameType string

const (
	remoteFrameHello    remoteFrameType = "hello"
	remoteFrameRequest  remoteFrameType = "request"
	remoteFrameResponse remoteFrameType = "response"
	remoteFrameCancel   remoteFrameType = "cancel"
	remoteFrameError    remoteFrameType = "error"
)

// remoteFrame is the length-prefixed JSON envelope.  payload carries the
// existing serviceRequest JSON body or a remoteResponse payload.
type remoteFrame struct {
	Type    remoteFrameType `json:"type"`
	ID      int64           `json:"id,omitempty"`
	V       int             `json:"v"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// remoteHello carries server capability info returned to a client after a
// successful hello.  It is deliberately minimal: no volumes, roots, remaps, or
// ACL-mode details pre-authentication (Phase 3 adds the authenticated
// handshake).
type remoteHello struct {
	Version string   `json:"version"`
	Commit  string   `json:"commit"`
	Proto   int      `json:"proto"`
	Mode    string   `json:"mode"` // "loopback"
	Modes   []string `json:"modes"`
}

// remoteDBSummary is the public per-volume summary a remote caller may see.  It
// deliberately excludes physical DB paths, journal ids, checkpoints, memory, and
// error detail that could leak server internals.
type remoteDBSummary struct {
	Volume     string `json:"volume,omitempty"`
	Entries    int    `json:"entries,omitempty"`
	Source     string `json:"source,omitempty"`
	BuiltAt    string `json:"built_at,omitempty"`
	State      string `json:"state,omitempty"`
	FRNRecords int    `json:"frn_records,omitempty"`
}

// remoteResponse is the allowlist-based remote projection.  Only fields listed
// here are ever sent to a remote caller; everything else in the internal
// serviceResponse is private by default.
type remoteResponse struct {
	OK              bool                 `json:"ok"`
	Message         string               `json:"message,omitempty"`
	Count           int                  `json:"count,omitempty"`
	SearchMS        float64              `json:"search_ms,omitempty"`
	Source          string               `json:"source,omitempty"`
	EligibleVolumes []string             `json:"eligible_volumes,omitempty"`
	Health          string               `json:"health,omitempty"`
	HealthMessage   string               `json:"health_message,omitempty"`
	Version         string               `json:"version,omitempty"`
	Commit          string               `json:"commit,omitempty"`
	Date            string               `json:"date,omitempty"`
	BuildFlavor     string               `json:"build_flavor,omitempty"`
	Entries         int                  `json:"entries,omitempty"`
	Loading         bool                 `json:"loading,omitempty"`
	Fuzzy           bool                 `json:"fuzzy,omitempty"`
	Complete        *bool                `json:"complete,omitempty"`
	Content         *remoteContentHealth `json:"content,omitempty"`
	Results         []string             `json:"results,omitempty"`
	Rows            []remoteResultRow    `json:"rows,omitempty"`
	DBs             []remoteDBSummary    `json:"dbs,omitempty"`
	WatchVolumes    []watchVolumeCursor  `json:"watch_volumes,omitempty"`
	WatchEvents     []watchDeltaEvent    `json:"watch_events,omitempty"`
}

// remoteContentHealth is the coarse, public projection of content query health.
// Internal detail (build/fold error text, counts, sidecar sizes) stays private;
// a remote caller gets only the trust-relevant flags, so a degraded or
// divergent content answer is visible off-box instead of only inside Complete.
type remoteContentHealth struct {
	State           string   `json:"state,omitempty"`
	Partial         bool     `json:"partial,omitempty"`
	Incomplete      bool     `json:"incomplete,omitempty"`
	CountDivergent  bool     `json:"count_divergent,omitempty"`
	DegradedVolumes []string `json:"degraded_volumes,omitempty"`
}

// remoteResultRow is the public projection of a jsonResult row.  It carries the
// path and the coarse fields a caller needs, and nothing from internal tracing.
type remoteResultRow struct {
	Path     string `json:"path"`
	Size     *int64 `json:"size,omitempty"`
	Modified string `json:"modified,omitempty"`
	IsDir    bool   `json:"is_dir,omitempty"`
}

// remoteLoopbackServer serves the Mode L transport for one loopback address.
type remoteLoopbackServer struct {
	s       *goSearchService
	addr    string
	ln      net.Listener
	mu      sync.Mutex
	conns   map[*remoteConn]struct{}
	stop    chan struct{}
	done    chan struct{}
	wg      sync.WaitGroup // bounds lifetime across close
	connSeq atomic.Int64   // server-wide connection budget counter
}

// newRemoteLoopbackServer creates a Mode L server bound to addr (which must be a
// loopback address).  The caller owns starting it.
func newRemoteLoopbackServer(s *goSearchService, addr string) *remoteLoopbackServer {
	return &remoteLoopbackServer{
		s:     s,
		addr:  addr,
		conns: make(map[*remoteConn]struct{}),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// start binds the loopback listener and begins accepting connections.  It
// returns an error if the address cannot be bound or is not loopback.
func (rs *remoteLoopbackServer) start() error {
	host, _, err := net.SplitHostPort(rs.addr)
	if err != nil {
		return fmt.Errorf("remote addr %q: %w", rs.addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("remote addr %q is not a loopback address; Mode L must bind 127.0.0.1/::1", rs.addr)
	}
	ln, err := net.Listen("tcp", rs.addr)
	if err != nil {
		return fmt.Errorf("remote loopback listen %s: %w", rs.addr, err)
	}
	rs.ln = ln
	rs.wg.Add(1)
	go func() {
		defer rs.wg.Done()
		rs.acceptLoop()
	}()
	return nil
}

func (rs *remoteLoopbackServer) acceptLoop() {
	defer close(rs.done)
	for {
		conn, err := rs.ln.Accept()
		if err != nil {
			select {
			case <-rs.stop:
				return
			default:
				return
			}
		}
		rc := &remoteConn{rs: rs, conn: conn, inFlight: make(map[int64]*remoteRequestState)}
		rs.mu.Lock()
		rs.conns[rc] = struct{}{}
		rs.mu.Unlock()
		rs.wg.Add(1)
		go func() {
			defer rs.wg.Done()
			rc.serve()
		}()
	}
}

// close shuts down the listener, cancels all active work, and waits for the
// connections to finish.
func (rs *remoteLoopbackServer) close() {
	close(rs.stop)
	if rs.ln != nil {
		rs.ln.Close()
	}
	rs.mu.Lock()
	conns := make([]*remoteConn, 0, len(rs.conns))
	for rc := range rs.conns {
		conns = append(conns, rc)
	}
	rs.mu.Unlock()
	for _, rc := range conns {
		rc.close()
	}
	<-rs.done
	rs.wg.Wait()
}

// remoteRequestState tracks one in-flight remote request for connection-scoped
// cancellation.
type remoteRequestState struct {
	cancelFlag atomic.Bool
	done       chan struct{}
}

// requestCancelFunc returns the engine cancel predicate for this request.
func (st *remoteRequestState) requestCancelFunc() func() bool {
	return func() bool { return st.cancelFlag.Load() }
}

// cancel marks the request cancelled.  The engine cancel predicate observes the
// flag on its next poll, so no additional hook is needed.
func (st *remoteRequestState) cancel() {
	st.cancelFlag.Store(true)
}

// remoteConn is a single client connection on the Mode L transport.
type remoteConn struct {
	rs        *remoteLoopbackServer
	conn      net.Conn
	stateMu   sync.Mutex // guards inFlight and closed admission
	writeMu   sync.Mutex // serializes socket writes
	inFlight  map[int64]*remoteRequestState
	closed    atomic.Bool // set once shutdown begins; rejects new requests
	closeOnce sync.Once
}

// close tears down the connection: closes the socket, cancels in-flight work,
// and waits for it to finish before returning.  It is safe to call from any
// goroutine (the once guard ensures a single shutdown).
func (rc *remoteConn) close() {
	rc.closeOnce.Do(func() {
		rc.shutdown()
		rc.waitInFlight()
		rc.rs.mu.Lock()
		delete(rc.rs.conns, rc)
		rc.rs.mu.Unlock()
	})
}

// shutdown marks the connection closed, closes the socket, and cancels
// in-flight work WITHOUT waiting for it.  It is safe to call from within an
// in-flight request (e.g. a failed response write) because it never blocks on
// the caller's own done channel.
func (rc *remoteConn) shutdown() {
	rc.closed.Store(true)
	rc.conn.Close()
	rc.stateMu.Lock()
	inFlight := make([]*remoteRequestState, 0, len(rc.inFlight))
	for _, st := range rc.inFlight {
		inFlight = append(inFlight, st)
	}
	rc.stateMu.Unlock()
	for _, st := range inFlight {
		st.cancel()
	}
}

// waitInFlight blocks until all in-flight requests have finished.  Callers must
// not hold stateMu.
func (rc *remoteConn) waitInFlight() {
	rc.stateMu.Lock()
	inFlight := make([]*remoteRequestState, 0, len(rc.inFlight))
	for _, st := range rc.inFlight {
		inFlight = append(inFlight, st)
	}
	rc.stateMu.Unlock()
	for _, st := range inFlight {
		<-st.done
	}
}

// writeFrame serializes a frame to the connection under the write lock and
// shuts down the connection on any write failure so the client never waits on a
// desynchronized stream.  It never waits for in-flight requests (a failed
// response write can originate from inside one).  Writes are bounded by a
// deadline so a client that stops reading cannot stall the server indefinitely.
func (rc *remoteConn) writeFrame(f remoteFrame) error {
	payload, err := json.Marshal(f)
	if err != nil {
		rc.shutdown()
		return err
	}
	if len(payload) > remoteMaxFrameBytes {
		rc.shutdown()
		return errors.New("response frame exceeds max frame size")
	}
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(len(payload)))
	rc.writeMu.Lock()
	defer rc.writeMu.Unlock()
	rc.conn.SetWriteDeadline(time.Now().Add(remoteWriteTimeout))
	defer rc.conn.SetWriteDeadline(time.Time{})
	if _, err := rc.conn.Write(buf[:]); err != nil {
		rc.shutdown()
		return err
	}
	if _, err := rc.conn.Write(payload); err != nil {
		rc.shutdown()
		return err
	}
	return nil
}

// serve reads frames until the connection closes, dispatching requests
// asynchronously so cancel frames and additional requests can be processed while
// a query runs.  The read loop enforces an idle timeout.
func (rc *remoteConn) serve() {
	defer rc.close()
	rc.conn.SetReadDeadline(time.Now().Add(remoteIdleTimeout))
	r := bufio.NewReader(rc.conn)
	for {
		select {
		case <-rc.rs.stop:
			return
		default:
		}
		payload, err := readRemoteFramePayload(r)
		if err != nil {
			if err != io.EOF {
				_ = rc.writeFrame(remoteFrame{Type: remoteFrameError, V: remoteProtocolVersion, Payload: mustJSON(remoteResponse{OK: false, Message: "protocol error"})})
			}
			return
		}
		rc.conn.SetReadDeadline(time.Now().Add(remoteIdleTimeout))
		var f remoteFrame
		if err := json.Unmarshal(payload, &f); err != nil {
			_ = rc.writeFrame(remoteFrame{Type: remoteFrameError, V: remoteProtocolVersion, Payload: mustJSON(remoteResponse{OK: false, Message: "malformed frame"})})
			return
		}
		if f.V != remoteProtocolVersion {
			_ = rc.writeFrame(remoteFrame{Type: remoteFrameError, V: remoteProtocolVersion, Payload: mustJSON(remoteResponse{OK: false, Message: fmt.Sprintf("unsupported protocol version %d (server v%d)", f.V, remoteProtocolVersion)})})
			return
		}
		switch f.Type {
		case remoteFrameHello:
			rc.handleHello(f.ID)
		case remoteFrameRequest:
			if f.ID <= 0 {
				_ = rc.writeFrame(remoteFrame{Type: remoteFrameError, V: remoteProtocolVersion, Payload: mustJSON(remoteResponse{OK: false, Message: "request id must be a positive integer"})})
				continue
			}
			rc.handleRequest(f.ID, f.Payload)
		case remoteFrameCancel:
			rc.handleCancel(f.ID)
		default:
			_ = rc.writeFrame(remoteFrame{Type: remoteFrameError, V: remoteProtocolVersion, ID: f.ID, Payload: mustJSON(remoteResponse{OK: false, Message: "unexpected frame type " + string(f.Type)})})
		}
	}
}

func (rc *remoteConn) handleHello(id int64) {
	hello := remoteHello{
		Version: version,
		Commit:  commit,
		Proto:   remoteProtocolVersion,
		Mode:    "loopback",
		Modes:   []string{"loopback"},
	}
	_ = rc.writeFrame(remoteFrame{Type: remoteFrameResponse, V: remoteProtocolVersion, ID: id, Payload: mustJSON(hello)})
}

// handleRequest dispatches a remote request asynchronously.  The request id is
// connection-scoped; a duplicate id while one is in flight is rejected.  Once
// shutdown has begun, no new request is admitted so close cannot miss work it
// has already decided not to wait for.
func (rc *remoteConn) handleRequest(id int64, payload json.RawMessage) {
	var req serviceRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		_ = rc.writeFrame(remoteFrame{Type: remoteFrameResponse, V: remoteProtocolVersion, ID: id, Payload: mustJSON(remoteResponse{OK: false, Message: "bad request"})})
		return
	}

	// Bound in-flight work per connection, reject duplicate ids, and refuse new
	// work once shutdown has begun.
	rc.stateMu.Lock()
	if rc.closed.Load() {
		rc.stateMu.Unlock()
		_ = rc.writeFrame(remoteFrame{Type: remoteFrameResponse, V: remoteProtocolVersion, ID: id, Payload: mustJSON(remoteResponse{OK: false, Message: "connection closed"})})
		return
	}
	if len(rc.inFlight) >= remoteConnMaxInFlight {
		rc.stateMu.Unlock()
		_ = rc.writeFrame(remoteFrame{Type: remoteFrameResponse, V: remoteProtocolVersion, ID: id, Payload: mustJSON(remoteResponse{OK: false, Message: "busy: too many in-flight requests"})})
		return
	}
	if _, exists := rc.inFlight[id]; exists {
		rc.stateMu.Unlock()
		_ = rc.writeFrame(remoteFrame{Type: remoteFrameResponse, V: remoteProtocolVersion, ID: id, Payload: mustJSON(remoteResponse{OK: false, Message: "duplicate request id"})})
		return
	}
	st := &remoteRequestState{done: make(chan struct{})}
	rc.inFlight[id] = st
	rc.stateMu.Unlock()

	rc.rs.wg.Add(1)
	go func() {
		defer rc.rs.wg.Done()
		defer close(st.done)
		rc.executeRequest(id, st, &req)
	}()
}

// executeRequest runs one remote request inside a recovery boundary.  It wires
// the connection-scoped cancel into the engine, clamps caller-controlled
// resource bounds, dispatches through the shared handler, and emits only the
// allowlisted remote projection.
func (rc *remoteConn) executeRequest(id int64, st *remoteRequestState, req *serviceRequest) {
	defer func() {
		if r := recover(); r != nil {
			// Do not leak the panic value or stack to the log: they may embed
			// query terms or filesystem paths, bypassing the remote log
			// redaction policy.  Keep the entry generic and keyed by request id.
			serviceLog("remote request panic (id=%d) recovered", id)
			// Do not leak panic text to the caller; emit a generic failure.
			// Guard the write: the connection may itself be broken, and a
			// failure here must not panic again.
			defer func() { _ = recover() }()
			_ = rc.writeFrame(remoteFrame{Type: remoteFrameResponse, V: remoteProtocolVersion, ID: id, Payload: mustJSON(remoteResponse{OK: false, Message: "internal error"})})
		}
		// Ensure in-flight cleanup even on the panic path.
		rc.stateMu.Lock()
		delete(rc.inFlight, id)
		rc.stateMu.Unlock()
	}()

	// Remote callers are read-only.  watch-delta is deferred remotely until
	// Phase 7; the capability gate rejects it here.
	caps := serviceCapabilities{ReadOnly: true, Remote: true}
	principal := servicePrincipal{}

	// Server-owned deadline clamp: a remote client cannot drive an unbounded
	// query by supplying a large deadline_unix.
	req.DeadlineUnix = clampRemoteDeadline(req.DeadlineUnix)

	// Server-owned result-limit clamp: enforce the bound before dispatch so an
	// unauthenticated loopback caller cannot force large result allocations in
	// the engine (the encoded-response check alone runs too late).
	clampRemoteLimit(req)

	// The request_seq field drives the service-global cancellation counter and
	// must not be trusted from a remote caller (it could cancel another
	// client's or the local GUI's in-flight query).  Remote cancellation is
	// connection-scoped via the frame id.
	req.RequestSeq = 0
	req.CancelOverride = st.requestCancelFunc()

	resp := rc.dispatch(caps, principal, req)

	if st.cancelFlag.Load() {
		// The request was cancelled; the client may not expect a response.
		return
	}
	if err := rc.writeRemoteResponse(id, resp, req.CountOnly); err != nil {
		serviceLog("remote response write failed (id=%d)", id)
	}
}

// writeRemoteResponse converts an internal response to the allowlisted remote
// projection, bounds its encoded size, and writes it (closing the connection on
// failure).
func (rc *remoteConn) writeRemoteResponse(id int64, resp serviceResponse, countOnly bool) error {
	out := remoteResponseFromService(resp, countOnly)
	b, err := json.Marshal(out)
	if err != nil {
		rc.shutdown()
		return err
	}
	if len(b) > remoteMaxResultBytes {
		// Send a bounded error instead of stalling the client.
		small := remoteFrame{Type: remoteFrameResponse, V: remoteProtocolVersion, ID: id, Payload: mustJSON(remoteResponse{OK: false, Message: "result too large"})}
		_ = rc.writeFrameNoClose(small)
		rc.shutdown()
		return errors.New("result too large")
	}
	return rc.writeFrame(remoteFrame{Type: remoteFrameResponse, V: remoteProtocolVersion, ID: id, Payload: b})
}

// writeFrameNoClose is a writeFrame variant used only to emit a bounded error
// before closing (it never recurses into close).
func (rc *remoteConn) writeFrameNoClose(f remoteFrame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(len(b)))
	rc.writeMu.Lock()
	defer rc.writeMu.Unlock()
	rc.conn.SetWriteDeadline(time.Now().Add(remoteWriteTimeout))
	defer rc.conn.SetWriteDeadline(time.Time{})
	if _, err := rc.conn.Write(buf[:]); err != nil {
		return err
	}
	_, err = rc.conn.Write(b)
	return err
}

// dispatch runs handleServiceCommand and returns the encoded response by
// capturing the handler's output.
func (rc *remoteConn) dispatch(caps serviceCapabilities, principal servicePrincipal, req *serviceRequest) serviceResponse {
	var buf bytesBuffer
	rc.rs.s.handleServiceCommand(&buf, principal, caps, req)
	var resp serviceResponse
	if err := json.Unmarshal(buf.b, &resp); err != nil {
		return serviceResponse{OK: false, Message: "internal dispatch error"}
	}
	return resp
}

// handleCancel cancels the in-flight request with the matching connection-scoped
// id (if any).
func (rc *remoteConn) handleCancel(id int64) {
	rc.stateMu.Lock()
	st, ok := rc.inFlight[id]
	rc.stateMu.Unlock()
	if ok && st != nil {
		st.cancel()
	}
}

// bytesBuffer is a minimal writer used to capture a single handler response.
type bytesBuffer struct{ b []byte }

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.b = append(b.b, p...)
	return len(p), nil
}

// readRemoteFramePayload reads one 4-byte big-endian length-prefixed JSON frame.
func readRemoteFramePayload(r *bufio.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > remoteMaxFrameBytes {
		return nil, fmt.Errorf("frame length %d out of range", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// remoteResponseFromService builds the allowlist-based remote projection from an
// internal serviceResponse.  Only explicitly public fields are copied; all
// internal diagnostics (planner detail, candidate counts, decline/fallback
// reasons) stay private by default.
func remoteResponseFromService(resp serviceResponse, countOnly bool) remoteResponse {
	out := remoteResponse{
		OK:          resp.OK,
		Message:     genericRemoteMessage(resp.Message),
		Count:       resp.Count,
		SearchMS:    resp.SearchMS,
		Source:      remoteSearchSource(resp.Source, countOnly),
		Health:      resp.Health,
		Version:     resp.Version,
		Commit:      resp.Commit,
		Date:        resp.Date,
		BuildFlavor: resp.BuildFlavor,
		Entries:     resp.Entries,
		Loading:     resp.Loading,
		Fuzzy:       resp.Fuzzy,
		Complete:    resp.Complete,
		Results:     resp.Results,
	}
	if resp.Content != nil {
		out.Content = &remoteContentHealth{
			State:           resp.Content.State,
			Partial:         resp.Content.Partial,
			Incomplete:      resp.Content.Incomplete,
			CountDivergent:  resp.Content.CountDivergent,
			DegradedVolumes: resp.Content.DegradedVolumes,
		}
	}
	// EligibleVolumes are public volume labels (C:, F:), safe to expose.
	out.EligibleVolumes = resp.EligibleVolumes
	// Health message may embed load errors or physical paths; only surface a
	// coarse, non-sensitive health message.
	if resp.Health != "" {
		out.HealthMessage = genericHealthMessage(resp.HealthMessage)
	}
	// Rows: project only the public fields.
	if resp.Rows != nil {
		out.Rows = make([]remoteResultRow, 0, len(resp.Rows))
		for _, row := range resp.Rows {
			out.Rows = append(out.Rows, remoteResultRow{Path: row.Path, Size: row.Size, Modified: row.Modified, IsDir: row.IsDir})
		}
	}
	// DBs: public per-volume summary only.
	if resp.DBs != nil {
		out.DBs = make([]remoteDBSummary, 0, len(resp.DBs))
		for _, info := range resp.DBs {
			out.DBs = append(out.DBs, remoteDBSummary{
				Volume:     info.Volume,
				Entries:    info.Entries,
				Source:     info.Source,
				BuiltAt:    info.BuiltAt,
				State:      info.State,
				FRNRecords: info.FRNRecords,
			})
		}
	}
	return out
}

// remoteSearchSource maps an internal planner source route to a coarse public
// category.  Internal sources are detailed planner names (e.g.
// "global:filename-pngc", "planned:ext-top", "compact-name-order-scan") that
// must not become wire API; remote callers get one of the stable route
// categories: "count", "bounded-scan", or "indexed".  An empty source is
// preserved (it appears on info and error responses and must not acquire a
// false route).  Fuzzy behavior is conveyed by the separate Fuzzy field, not by
// the source route.
func remoteSearchSource(source string, countOnly bool) string {
	if source == "" {
		return ""
	}
	if countOnly {
		return "count"
	}
	switch {
	case source == "count-fast-posting" || source == "count-fast-pngc" || source == "count-fast-pngr" ||
		source == "parallel-name-count" || source == "global:boolean-persisted-count" || source == "planned:or-group-lazy-count" || strings.HasSuffix(source, "-count"):
		return "count"
	case strings.Contains(source, "bounded-scan") || source == "broad-scan" || source == "filesystem-under-fallback" || source == "compact-name-order-scan" || source == "legacy-planner":
		return "bounded-scan"
	default:
		return "indexed"
	}
}

// genericRemoteMessage maps internal failure detail to a stable, non-sensitive
// message.  Internal messages can embed load errors, stale reasons, and physical
// DB paths; remote callers get only coarse categories.
func genericRemoteMessage(msg string) string {
	switch msg {
	case "", "loading indexes", "service running":
		return msg
	}
	if msg == "service has no search indexes loaded" {
		return msg
	}
	// Everything else is internal detail: return a generic error marker.
	return "request failed"
}

// genericHealthMessage keeps only the coarse health phrase, stripping any
// embedded error detail or path.
func genericHealthMessage(msg string) string {
	if msg == "" {
		return ""
	}
	// Health messages that reference specific volumes/paths are collapsed to a
	// generic label; coarse ok/loading/degraded states pass through.
	switch msg {
	case "loading indexes", "indexes loaded", "indexes ready", "service healthy", "ok":
		return msg
	}
	return "unavailable"
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"ok":false,"message":"internal marshal error"}`)
	}
	return b
}

// clampRemoteDeadline caps a client-supplied deadline at the server max so a
// remote caller cannot drive an unbounded query.
func clampRemoteDeadline(deadlineUnix int64) int64 {
	if deadlineUnix <= 0 {
		return time.Now().Add(serviceQueryTimeout - 250*time.Millisecond).UnixNano()
	}
	maxDeadline := time.Now().Add(serviceQueryTimeout).UnixNano()
	if deadlineUnix > maxDeadline {
		return maxDeadline
	}
	return deadlineUnix
}

// clampRemoteLimit enforces the server-owned result-limit bound before dispatch
// so an unauthenticated loopback caller cannot force large result allocations
// in the engine.  A non-positive limit preserves the engine's normal default
// (100 results); only positive values above the maximum are clamped.  Count-only
// requests never materialize result rows, so their limit is left to the engine
// (which ignores it for counting).
func clampRemoteLimit(req *serviceRequest) {
	if req.CountOnly {
		return
	}
	if req.Limit > remoteMaxResultLimit {
		req.Limit = remoteMaxResultLimit
	}
}
