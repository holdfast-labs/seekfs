package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestOnDemandFolderFirstSearch pins the name-side on-demand contract: with
// no index loaded at all, a search scoped --under a real folder still
// answers from a bounded walk of that folder (fsearch indexes the searched
// folder first). Queries without a folder scope keep the no-index error.
func TestOnDemandFolderFirstSearch(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ondemand-zz.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed on-demand folder: %v", err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("seed on-demand subdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "ondemand-zz-deep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed on-demand deep file: %v", err)
	}

	s := &goSearchService{}
	search := func(req *serviceRequest) serviceResponse {
		t.Helper()
		var buf bytes.Buffer
		s.serviceCommandSearch(&buf, serviceCapabilities{}, servicePrincipal{}, req)
		var resp serviceResponse
		if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
			t.Fatalf("decode on-demand response: %v (raw %q)", err, buf.String())
		}
		return resp
	}

	resp := search(&serviceRequest{Command: "search", Query: "ondemand-zz", Under: root, Limit: 20})
	if !resp.OK {
		t.Fatalf("on-demand folder search OK = false (%q); want results from the folder walk", resp.Message)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("on-demand folder search results = %v; want both seeded files", resp.Results)
	}
	if resp.Fallback == "" {
		t.Fatalf("on-demand response missing fallback marker; want filesystem-under-fallback attribution")
	}

	bare := search(&serviceRequest{Command: "search", Query: "ondemand-zz", Limit: 20})
	if bare.OK {
		t.Fatalf("folder-less no-index search OK = true; want the no-index error")
	}
	if bare.Message != "service has no search indexes loaded" {
		t.Fatalf("folder-less no-index message = %q; want the stable error contract", bare.Message)
	}

	counted := search(&serviceRequest{Command: "search", Query: "ondemand-zz", Under: root, Limit: 20, CountOnly: true})
	if counted.OK {
		t.Fatalf("on-demand count OK = true; want the no-index error (counts decline the walk)")
	}
}
