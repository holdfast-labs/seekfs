package main

import (
	"strings"
	"time"
)

// Client talks to the resident seekfs service over its named pipe with the
// existing JSON protocol (serviceRequest/serviceResponse), the same way
// cmd/seekfs/ui.go does via callServiceWithTimeout /
// serviceRequestFromOptions / exchangeServiceJSON. The service side is
// unchanged.

const (
	uiSearchDeadline = 15 * time.Second
	uiServiceTimeout = 20 * time.Second
)

const defaultResultLimit = 200

// Client is a thin service wrapper. The exchange hook performs one
// request/response round trip; when nil the Windows named-pipe transport is
// used. Tests inject a fake.
type Client struct {
	PipeName string
	Timeout  time.Duration
	seqBase  int64

	exchange func(serviceRequest) (serviceResponse, error)
}

func NewClient(pipe string) *Client {
	if pipe == "" {
		pipe = defaultServicePipe
	}
	return &Client{PipeName: pipe, Timeout: uiServiceTimeout, seqBase: time.Now().UnixNano()}
}

func (c *Client) roundTrip(req serviceRequest) (serviceResponse, error) {
	if c.exchange != nil {
		return c.exchange(req)
	}
	return pipeRoundTrip(c.PipeName, req, c.Timeout)
}

// serviceSeq scopes request sequence numbers to this session, mirroring
// UIApp.serviceUISeq in cmd/seekfs/ui.go. Zero selects a synchronous call.
func (c *Client) serviceSeq(seq int64) int64 {
	if seq <= 0 {
		return 0
	}
	return c.seqBase + seq
}

// Info performs the service info handshake used for readiness and health.
func (c *Client) Info() (serviceResponse, error) {
	return c.roundTrip(serviceRequest{Command: "info"})
}

// SettingsGet reads the indexing settings (docs/SETTINGS_API.md).
func (c *Client) SettingsGet() (serviceResponse, error) {
	return c.roundTrip(serviceRequest{Command: "settings-get"})
}

// SettingsSet stages a settings change. Only fields present in patch change.
func (c *Client) SettingsSet(patch serviceSettings) (serviceResponse, error) {
	return c.roundTrip(serviceRequest{Command: "settings-set", Settings: &patch})
}

// SearchParams are the UI-level knobs for one search. Query is the raw typed
// text; ContentsMode rewrites plain words to content: terms first.
type SearchParams struct {
	Query        string
	ContentsMode bool
	MatchPath    bool
	Under        string
	Limit        int
	Seq          int64
}

// Search sends one search request, mirroring UIApp.searchServiceUI:
// Contents rewrite, Everything-alias normalization, then
// serviceRequestFromOptions with the session-scoped sequence number.
func (c *Client) Search(p SearchParams) (serviceResponse, error) {
	query := p.Query
	if p.ContentsMode {
		query = toContentQuery(query)
	}
	serviceQuery, matchPath := normalizeUIQueryForService(query, p.MatchPath)
	limit := p.Limit
	if limit <= 0 {
		limit = defaultResultLimit
	}
	req := serviceRequest{
		Command:      "search",
		Query:        serviceQuery,
		MatchPath:    matchPath || queryLooksLoosePathScoped(serviceQuery),
		Limit:        limit,
		Under:        strings.TrimSpace(p.Under),
		DeadlineUnix: time.Now().Add(uiSearchDeadline).UnixNano(),
		RequestSeq:   c.serviceSeq(p.Seq),
	}
	return c.roundTrip(req)
}

// queryLooksLoosePathScoped reports whether a normalized query is inherently
// path-scoped (a path: filter, a regex, or a path-like token), mirroring
// queryLooksLoosePathScoped in cmd/seekfs/query_parse.go.
func queryLooksLoosePathScoped(query string) bool {
	fields := strings.Fields(query)
	for _, field := range fields {
		if strings.HasPrefix(field, "!") || strings.HasPrefix(field, "-") {
			continue
		}
		raw := strings.TrimLeft(field, "!-")
		if raw == "" {
			continue
		}
		key, _, hasPrefix := strings.Cut(raw, ":")
		if hasPrefix {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "path", "fullpath", "full-path", "full_path", "fullpathname", "full-path-name", "location":
				return true
			case "regex", "regexp", "re":
				return true
			}
			continue
		}
		if strings.ContainsAny(raw, `\/`) {
			return true
		}
	}
	return false
}

// isTimeoutError reports whether err is a service round-trip timeout.
func isTimeoutError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "request timed out")
}
