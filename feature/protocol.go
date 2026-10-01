// Package feature defines seekfs's local companion protocol. A companion reads
// one bounded JSON line from stdin and writes exactly one response to stdout;
// diagnostics belong on stderr. Requests are serialized by the host.
package feature

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

const Version = 1
const MaxFrameBytes = 1 << 20
const PageSize = 256
const MaxQueryMatches = 100000

type Volume struct {
	IndexID    string `json:"index_id"`
	ID         string `json:"id"`
	JournalID  uint64 `json:"journal_id"`
	Generation uint64 `json:"generation"`
	Checkpoint int64  `json:"checkpoint"`
	Cursor     uint64 `json:"cursor"`
}

type Record struct {
	FRN     uint64 `json:"frn"`
	Path    string `json:"path,omitempty"`
	Name    string `json:"name,omitempty"`
	Size    int64  `json:"size,omitempty"`
	ModUnix int64  `json:"mod_unix,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

type Request struct {
	Version       int      `json:"version"`
	ID            uint64   `json:"id"`
	Op            string   `json:"op"`
	Feature       string   `json:"feature,omitempty"`
	DataDir       string   `json:"data_dir,omitempty"`
	Volume        *Volume  `json:"volume,omitempty"`
	Previous      *Volume  `json:"previous,omitempty"`
	Records       []Record `json:"records,omitempty"`
	Query         string   `json:"query,omitempty"`
	CaseSensitive bool     `json:"case_sensitive,omitempty"`
	Limit         int      `json:"limit,omitempty"`
	DeadlineUnix  int64    `json:"deadline_unix,omitempty"`
}

type Response struct {
	Progress     *Progress `json:"progress,omitempty"`
	Capabilities []string  `json:"capabilities,omitempty"`
	Version      int       `json:"version"`
	ID           uint64    `json:"id"`
	OK           bool      `json:"ok"`
	Error        string    `json:"error,omitempty"`
	Message      string    `json:"message,omitempty"`
	Complete     bool      `json:"complete"`
	FRNs         []uint64  `json:"frns,omitempty"`
}

type Progress struct {
	Current int64  `json:"current"`
	Total   int64  `json:"total"`
	Unit    string `json:"unit,omitempty"`
}

func NewScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), MaxFrameBytes)
	return s
}

func Write(w io.Writer, message any) error {
	b, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(b)+1 >= MaxFrameBytes {
		return fmt.Errorf("feature frame exceeds %d bytes", MaxFrameBytes)
	}
	b = append(b, '\n')
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}
