//go:build windows

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// mftBenchCount returns the record count for the memory benchmark. The normal
// test run never enters a Benchmark function; SEEKFS_MFT_BENCH_N tunes the
// count when the benchmark is run explicitly.
func mftBenchCount() int {
	if v := os.Getenv("SEEKFS_MFT_BENCH_N"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1_000_000
}

type mftBenchStage struct {
	name       string
	beforeHeap uint64
	peakHeap   uint64
	peakSys    uint64
	afterHeap  uint64
	records    int
}

func (s mftBenchStage) report(b *testing.B) {
	b.Helper()
	b.Logf("stage=%-12s records=%d before_heap_mb=%.1f peak_heap_mb=%.1f peak_sys_mb=%.1f after_heap_mb=%.1f bytes_per_record=%.1f",
		s.name, s.records,
		float64(s.beforeHeap)/(1<<20), float64(s.peakHeap)/(1<<20),
		float64(s.peakSys)/(1<<20), float64(s.afterHeap)/(1<<20),
		float64(s.peakHeap)/float64(max(1, s.records)))
}

type mftPeakSampler struct {
	stop chan struct{}
	done chan struct{}
	peak atomic.Uint64
	sys  atomic.Uint64
}

func startMFTMemSampler() *mftPeakSampler {
	s := &mftPeakSampler{stop: make(chan struct{}), done: make(chan struct{})}
	var initial runtime.MemStats
	runtime.ReadMemStats(&initial)
	s.peak.Store(initial.HeapInuse)
	s.sys.Store(initial.HeapSys)
	go func() {
		defer close(s.done)
		t := time.NewTicker(time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				for {
					old := s.peak.Load()
					if m.HeapInuse <= old || s.peak.CompareAndSwap(old, m.HeapInuse) {
						break
					}
				}
				for {
					old := s.sys.Load()
					if m.HeapSys <= old || s.sys.CompareAndSwap(old, m.HeapSys) {
						break
					}
				}
			}
		}
	}()
	return s
}

func (s *mftPeakSampler) finish() (peak, sys uint64) {
	close(s.stop)
	<-s.done
	return s.peak.Load(), s.sys.Load()
}

func measureMFTStage(name string, records int, fn func()) mftBenchStage {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	sampler := startMFTMemSampler()
	fn()
	peak, sys := sampler.finish()
	runtime.GC()
	runtime.ReadMemStats(&after)
	return mftBenchStage{
		name: name, beforeHeap: before.HeapInuse, peakHeap: peak, peakSys: sys,
		afterHeap: after.HeapInuse, records: records,
	}
}

func syntheticMFTName(i int) string {
	n := 8 + i%33
	b := make([]byte, n)
	for j := 0; j < n; j++ {
		b[j] = byte('a' + (i+j*7)%26)
	}
	ext := []string{".txt", ".bin", ".go", ".dat", ".log"}[i%5]
	copy(b[n-len(ext):], ext)
	return string(b)
}

// mftBenchRecord walks the same shape as a real volume: dense record numbers,
// about 8% directories, parent links about eight levels deep.
func mftBenchRecord(i int) synthOpts {
	parent := uint64(5)
	if i > 6 {
		parent = uint64(5 + (i-6)/8)
	}
	if parent >= uint64(i) {
		parent = 5
	}
	isDir := i%13 == 0
	var size int64
	if !isDir {
		size = int64(i % 1_000_000)
	}
	return synthOpts{
		inUse:       true,
		isDir:       isDir,
		name:        syntheticMFTName(i),
		namespace:   fileNameWin32,
		parentFRN:   parent,
		modUnixNano: int64(1_700_000_000_000_000_000 + int64(i)*1_000_000),
		size:        size,
		nonResident: true,
	}
}

// runMFTBuildStages runs the settings-driven name build stages against
// synthetic records. It returns the stages and the retained heap measured
// after this function's locals have gone out of scope.
func runMFTBuildStages(n int, tmpPath string) []mftBenchStage {
	stages := make([]mftBenchStage, 0, 4)
	entries := newMFTTable(uint64(n))
	buf := make([]byte, 1024)
	stages = append(stages, measureMFTStage("accumulate", n, func() {
		for i := 0; i < n; i++ {
			fillSynthMFTRecord(buf, mftBenchRecord(i))
			parseMFTRecordInto(buf, uint64(i), 512, entries)
		}
	}))

	idx := &Index{
		Version:      indexVersion,
		Roots:        []string{`X:\`},
		BuiltAt:      time.Now(),
		Source:       "usn",
		Volume:       "X:",
		Compact:      true,
		CompactAttrs: true,
	}
	stages = append(stages, measureMFTStage("records", entries.count, func() {
		buildRecordsFromMFT(idx, entries)
	}))
	stages = append(stages, measureMFTStage("orders", len(idx.Records), func() {
		buildOrders(idx)
	}))
	stages = append(stages, measureMFTStage("stage", len(idx.Records), func() {
		tmp, err := stageIndexFile(tmpPath, idx)
		if err != nil {
			panic(err)
		}
		if err := os.Remove(tmp); err != nil {
			panic(err)
		}
	}))

	// Drop every build-time reference before returning so the caller can measure
	// what remains resident after a GC.
	entries = nil
	idx = nil
	buf = nil
	runtime.GC()
	return stages
}

func BenchmarkMFTBuildMemory(b *testing.B) {
	n := mftBenchCount()
	dir := os.Getenv("TMP")
	if dir == "" {
		dir = `E:\bench\gotmp`
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	tmpPath := filepath.Join(dir, "mft-memory-bench.gsi")
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	b.ReportAllocs()
	b.ResetTimer()
	var stages []mftBenchStage
	for i := 0; i < b.N; i++ {
		stages = runMFTBuildStages(n, tmpPath)
	}
	b.StopTimer()
	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	b.Logf("retained_after_build_gc heap_inuse_mb=%.1f baseline_heap_inuse_mb=%.1f delta_mb=%.1f heap_released_mb=%.1f",
		float64(m.HeapInuse)/(1<<20), float64(baseline.HeapInuse)/(1<<20),
		float64(int64(m.HeapInuse)-int64(baseline.HeapInuse))/(1<<20), float64(m.HeapReleased)/(1<<20))
	for _, s := range stages {
		s.report(b)
	}
}
