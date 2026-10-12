package main

// Wire types for the seekfs resident-service JSON protocol over a named
// pipe. Field names must match cmd/seekfs exactly (serviceRequest in
// bench.go, serviceResponse in service_protocol.go, jsonResult and
// contentHealth in types.go/content_service.go). The service side is
// unchanged; this package only mirrors the subset the UI needs.

type serviceRequest struct {
	Command       string `json:"command"`
	Query         string `json:"query,omitempty"`
	MatchPath     bool   `json:"match_path,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	CountOnly     bool   `json:"count_only,omitempty"`
	Under         string `json:"under,omitempty"`
	Exists        bool   `json:"exists,omitempty"`
	DeadlineUnix  int64  `json:"deadline_unix,omitempty"`
	RequestSeq    int64  `json:"request_seq,omitempty"`
	CaseSensitive bool   `json:"case_sensitive,omitempty"`
	Fuzzy         bool   `json:"fuzzy,omitempty"`
}

type serviceResponse struct {
	OK            bool          `json:"ok"`
	Message       string        `json:"message,omitempty"`
	Fuzzy         bool          `json:"fuzzy,omitempty"`
	Entries       int           `json:"entries,omitempty"`
	Loading       bool          `json:"loading,omitempty"`
	Count         int           `json:"count,omitempty"`
	SearchMS      float64       `json:"search_ms,omitempty"`
	Source        string        `json:"source,omitempty"`
	Results       []string      `json:"results,omitempty"`
	Rows          []jsonResult  `json:"rows,omitempty"`
	DBs           []dbInfo      `json:"dbs,omitempty"`
	Health        string        `json:"health,omitempty"`
	HealthMessage string        `json:"health_message,omitempty"`
	Content       *contentState `json:"content,omitempty"`
}

type jsonResult struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Volume      string `json:"volume,omitempty"`
	IsDir       bool   `json:"is_dir"`
	Size        *int64 `json:"size,omitempty"`
	Modified    string `json:"modified,omitempty"`
	IndexSource string `json:"index_source,omitempty"`
	Snippet     string `json:"snippet,omitempty"`
	Exists      *bool  `json:"exists,omitempty"`
}

type dbInfo struct {
	Path    string `json:"path"`
	Entries int    `json:"entries"`
	Volume  string `json:"volume,omitempty"`
	State   string `json:"state,omitempty"`
}

// contentState mirrors contentHealth: the content-index telemetry surfaced
// through the service response.
type contentState struct {
	State      string `json:"state,omitempty"`
	Docs       int    `json:"docs,omitempty"`
	Partial    bool   `json:"partial,omitempty"`
	Incomplete bool   `json:"incomplete,omitempty"`
	BuildDone  int    `json:"build_done,omitempty"`
	BuildTotal int    `json:"build_total,omitempty"`
	BuildError string `json:"build_error,omitempty"`
	FoldError  string `json:"fold_error,omitempty"`
}

const defaultServicePipe = `\\.\pipe\seekfs-service`

// Service health values reported via the info response.
const (
	serviceHealthOK       = "ok"
	serviceHealthDegraded = "degraded"
	serviceHealthError    = "error"
)

// Content index states reported by the service.
const (
	contentStateIndexing = "indexing"
	contentStateReady    = "ready"
)
