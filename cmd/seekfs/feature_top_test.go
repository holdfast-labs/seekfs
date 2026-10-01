package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestFeatureTopPageIsBoundedAndOrdered(t *testing.T) {
	for _, column := range []string{"", "size", "modified", "extension", "path"} {
		pq := parsedQuery{SortColumn: column}
		h := &featureTopHeap{pq: pq, limit: 7}
		var all []Entry
		for i := 1000; i > 0; i-- {
			name := fmt.Sprintf("file%04d.%s", i, []string{"go", "txt"}[i%2])
			e := Entry{Name: name, Path: `C:\work\` + name, Size: int64(i % 19), ModUnix: int64(i % 17)}
			all = append(all, e)
			if err := h.offer(e); err != nil {
				t.Fatal(err)
			}
			if h.Len() > 7 {
				t.Fatal("retained more than the requested page")
			}
		}
		sortSearchAllEntries(all, pq)
		got := h.sorted()
		for i := range got {
			if got[i].Path != all[i].Path {
				t.Fatalf("sort %q row %d = %s; want %s", column, i, got[i].Path, all[i].Path)
			}
		}
	}
	h := &featureTopHeap{pq: parsedQuery{RootBias: `D:\`}, limit: 1}
	_ = h.offer(Entry{Name: "a.go", Path: `C:\a.go`})
	_ = h.offer(Entry{Name: "z.go", Path: `D:\z.go`})
	if got := h.sorted(); len(got) != 1 || got[0].Path != `D:\z.go` {
		t.Fatalf("root bias: %v", got)
	}
	h = &featureTopHeap{limit: 1}
	if err := h.offer(Entry{Path: strings.Repeat("x", featureResultByteBudget)}); err == nil {
		t.Fatal("result byte budget was not enforced")
	}
}
