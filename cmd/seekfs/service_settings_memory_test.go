package main

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"
)

// largeSettingsStubIndex builds a synthetic in-memory index large enough that
// retaining it after a settings build would be obvious in HeapInuse.
func largeSettingsStubIndex(volume string, n int) *Index {
	idx := &Index{
		Version:      indexVersion,
		Roots:        []string{volume + `\`},
		BuiltAt:      time.Now(),
		Source:       "usn",
		Volume:       volume,
		Compact:      true,
		CompactAttrs: true,
	}
	idx.Records = make([]CompactRecord, n)
	for i := 0; i < n; i++ {
		parent := int32(-1)
		parentFRN := uint64(5)
		if i > 0 {
			parent = int32((i - 1) / 8)
			parentFRN = uint64(parent + 5)
		}
		mode := uint32(0o644)
		size := int64(i % 1_000_000)
		if i%13 == 0 {
			mode = uint32(os.ModeDir | 0o755)
			size = 0
		}
		idx.Records[i] = CompactRecord{
			FRN:       uint64(i + 5),
			ParentFRN: parentFRN,
			Parent:    parent,
			Name:      fmt.Sprintf("node-%06d.dat", i),
			Mode:      mode,
			Size:      size,
			ModUnix:   int64(1_700_000_000_000_000_000 + int64(i)*1_000_000),
		}
	}
	return idx
}

// TestSettingsBuildReleasesBuiltIndex proves the settings-driven build path
// drops the in-memory Index after staging and reloading the mmap index, and
// returns HeapInuse near the pre-build baseline after a GC.
func TestSettingsBuildReleasesBuiltIndex(t *testing.T) {
	preservePluginSelection(t)
	t.Setenv("SEEKFS_CONTENT_SEARCH", "0")
	t.Setenv("SEEKFS_MEMORY_MODE", "lowmem")
	t.Setenv("SEEKFS_NAME_ORDER", "0")
	t.Setenv("SEEKFS_NAME_TRIGRAMS", "0")
	t.Setenv("SEEKFS_DIR", t.TempDir())
	useSyntheticSettingsVolumes(t)
	configPath := settingsTestConfig(t, "default_limit = 10\n")
	s := settingsTestService(t, configPath, nil)
	t.Cleanup(func() {
		s.indexMu.Lock()
		for _, vol := range s.volumes {
			if vol != nil {
				_ = closeIndexMMapRecords(vol.index)
			}
		}
		s.volumes = nil
		s.indexMu.Unlock()
		runtime.GC()
	})

	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	finalized := make(chan struct{})
	const records = 120_000
	useSettingsIndexStub(t, func(volume string) (*Index, error) {
		idx := largeSettingsStubIndex(volume, records)
		runtime.SetFinalizer(idx, func(*Index) { close(finalized) })
		return idx, nil
	})

	add := callSettingsCommand(t, s, serviceCapabilities{ReadOnly: true, Mutate: true}, settingsPatchRequest(t, map[string]any{"volumes": []map[string]any{{"volume": "F:", "indexed": true}}}))
	if !add.OK {
		t.Fatalf("add volume = %+v", add)
	}
	waitSettingsVolumeState(t, s, "F:", "ready")

	s.indexMu.RLock()
	var loaded *Index
	for _, vol := range s.volumes {
		if vol != nil && vol.volume == "F:" {
			loaded = vol.index
		}
	}
	s.indexMu.RUnlock()
	if loaded == nil || loaded.MMapRecords == nil {
		t.Fatalf("settings volume did not install the reloaded mmap index: %+v", loaded)
	}

	deadline := time.Now().Add(15 * time.Second)
	var after runtime.MemStats
	for {
		runtime.GC()
		runtime.ReadMemStats(&after)
		select {
		case <-finalized:
		default:
			if time.Now().After(deadline) {
				t.Fatalf("built Index still reachable after settings build: heap_inuse %.1f MB vs baseline %.1f MB",
					float64(after.HeapInuse)/(1<<20), float64(baseline.HeapInuse)/(1<<20))
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		break
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	const slack = 64 << 20
	if after.HeapInuse > baseline.HeapInuse+slack {
		t.Fatalf("HeapInuse after build+GC = %.1f MB, baseline %.1f MB (slack %d MB)",
			float64(after.HeapInuse)/(1<<20), float64(baseline.HeapInuse)/(1<<20), slack>>20)
	}
}
