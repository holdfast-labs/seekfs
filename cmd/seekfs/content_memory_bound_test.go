package main

// Memory bound: the build's retained per-file metadata stays a small fixed
// cost per in-scope file (the path string plus a compact struct), not a
// corpus-proportional working set. Run with SEEKFS_CONTENT_MEMTEST=1.

import (
	"fmt"
	"os"
	"runtime"
	"testing"
)

func contentMemItems(n int) []contentBuildItem {
	items := make([]contentBuildItem, n)
	for i := 0; i < n; i++ {
		items[i] = contentBuildItem{
			frn:      uint64(i + 1),
			path:     fmt.Sprintf(`c:\work\repo\src\component%03d\file%06d.go`, i%500, i),
			priority: i % 4,
			mod:      int64(1_700_000_000 + i),
			size:     int64(4096 + i%8192),
		}
	}
	return items
}

func contentMemHeap() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

func TestContentBuildMetadataBounded(t *testing.T) {
	if os.Getenv("SEEKFS_CONTENT_MEMTEST") != "1" {
		t.Skip("set SEEKFS_CONTENT_MEMTEST=1")
	}
	const small, large = 20000, 80000
	base := contentMemHeap()
	items1 := contentMemItems(small)
	a1 := contentMemHeap()
	items2 := contentMemItems(large)
	a2 := contentMemHeap()
	runtime.KeepAlive(items1)
	runtime.KeepAlive(items2)

	b1 := float64(a1-base) / small
	b2 := float64(a2-a1) / float64(large-small)
	per := float64(a2-base) / float64(small+large)
	t.Logf("MEM metadata items=%d+%d base=%.1fMB afterSmall=%.1fMB afterLarge=%.1fMB bytesPerItem small=%.1f large=%.1f", small, large, float64(base)/(1<<20), float64(a1)/(1<<20), float64(a2)/(1<<20), b1, b2)
	if per > 200 {
		t.Fatalf("per-file metadata = %.1f B; want <= 200 (struct + path)", per)
	}
	if b2 > b1*1.5+32 {
		t.Fatalf("per-file metadata grew from %.1f B to %.1f B between corpus sizes", b1, b2)
	}
}

func BenchmarkContentBuildItemMetadata(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		items := contentMemItems(20000)
		if len(items) != 20000 {
			b.Fatal("short")
		}
		runtime.KeepAlive(items)
	}
}
